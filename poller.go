package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const deviceSyncEvery = 6 * time.Hour

// Poller periodically reads every thermometer's current state from Govee and
// caches it in MySQL.
type Poller struct {
	govee    *GoveeClient
	store    *Store
	interval time.Duration
	trigger  chan struct{}

	mu     sync.Mutex
	status PollStatus
}

type PollStatus struct {
	LastPoll       time.Time
	NextPoll       time.Time
	LastDeviceSync time.Time
	LastErr        string
	Online         map[string]bool // device ID -> online at last poll
	Polling        bool
}

func NewPoller(g *GoveeClient, s *Store, interval time.Duration) *Poller {
	return &Poller{
		govee:    g,
		store:    s,
		interval: interval,
		trigger:  make(chan struct{}, 1),
		status:   PollStatus{Online: map[string]bool{}},
	}
}

func (p *Poller) Status() PollStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	st.Online = make(map[string]bool, len(p.status.Online))
	for k, v := range p.status.Online {
		st.Online[k] = v
	}
	return st
}

// TriggerNow requests an immediate poll; it never blocks.
func (p *Poller) TriggerNow() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *Poller) Run(ctx context.Context) {
	for {
		p.pollOnce(ctx)
		next := time.Now().Add(p.interval)
		p.mu.Lock()
		p.status.NextPoll = next
		p.mu.Unlock()
		if !waitUntil(ctx, next, p.trigger) {
			return
		}
	}
}

func (p *Poller) syncDevices(ctx context.Context) error {
	devs, err := p.govee.Devices(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, d := range devs {
		if !d.IsThermometer() {
			continue
		}
		name := d.DeviceName
		if name == "" {
			name = d.SKU + " " + d.Device
		}
		if err := p.store.UpsertDevice(ctx, d.Device, d.SKU, name); err != nil {
			return err
		}
		n++
	}
	log.Printf("device sync: %d thermometer(s)", n)
	p.mu.Lock()
	p.status.LastDeviceSync = time.Now()
	p.mu.Unlock()
	return nil
}

func (p *Poller) pollOnce(ctx context.Context) {
	started := time.Now()
	p.mu.Lock()
	p.status.Polling = true
	needSync := time.Since(p.status.LastDeviceSync) > deviceSyncEvery
	p.mu.Unlock()

	var errs []string
	if needSync {
		if err := p.syncDevices(ctx); err != nil {
			errs = append(errs, "device sync: "+err.Error())
		}
	}

	devs, err := p.store.Devices(ctx)
	if err != nil {
		errs = append(errs, "load devices: "+err.Error())
	}
	ok, failed := 0, 0
	online := map[string]bool{}
	ts := started.UTC().Truncate(time.Second)
	for _, d := range devs {
		if err := p.pollDevice(ctx, d, ts, online); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", d.Name, err))
			continue
		}
		ok++
	}

	errText := strings.Join(errs, "; ")
	if errText != "" {
		log.Printf("poll: %s", errText)
	}
	if err := p.store.LogPoll(ctx, started, time.Since(started), ok, failed, errText); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("poll log: %v", err)
	}

	p.mu.Lock()
	p.status.Polling = false
	p.status.LastPoll = started
	p.status.LastErr = errText
	for k, v := range online {
		p.status.Online[k] = v
	}
	p.mu.Unlock()
}

func (p *Poller) pollDevice(ctx context.Context, d Device, ts time.Time, online map[string]bool) error {
	st, err := p.govee.State(ctx, d.SKU, d.ID)
	if err != nil {
		return err
	}
	online[d.ID] = st.Online
	// Govee: when offline, the returned values are stale and "have no
	// reference significance", so don't cache them.
	if !st.Online {
		return nil
	}
	if st.TempF == nil || *st.TempF < -60 || *st.TempF > 180 {
		return fmt.Errorf("no valid temperature in response")
	}
	r := Reading{DeviceID: d.ID, TS: ts, TempF: *st.TempF, Source: "api"}
	if st.Humidity != nil && *st.Humidity >= 0 && *st.Humidity <= 100 {
		r.Humidity = sql.NullFloat64{Float64: *st.Humidity, Valid: true}
	}
	return p.store.InsertReading(ctx, r)
}
