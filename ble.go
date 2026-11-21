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

	"tinygo.org/x/bluetooth"
)

// BLECollector downloads each sensor's stored per-minute history over
// Bluetooth (the same request the Govee app's "Get All Data" makes) and caches
// it in MySQL. The sensors keep months of readings, so this only needs to run
// now and then while the computer is near them.
type BLECollector struct {
	store   *Store
	every   time.Duration
	trigger chan struct{}
	adapter *bluetooth.Adapter

	mu     sync.Mutex
	status BLEStatus
}

type BLEStatus struct {
	Enabled bool
	Running bool
	Current string // device being synced
	LastRun time.Time
	NextRun time.Time
	Err     string // adapter-level problem
	Results []BLEResult
}

type BLEResult struct {
	Name     string
	Readings int   // readings downloaded
	New      int64 // readings not already cached
	Took     time.Duration
	Err      string
}

func NewBLECollector(store *Store, every time.Duration) *BLECollector {
	return &BLECollector{
		store:   store,
		every:   every,
		trigger: make(chan struct{}, 1),
		adapter: bluetooth.DefaultAdapter,
		status:  BLEStatus{Enabled: true},
	}
}

func (c *BLECollector) Status() BLEStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.Results = append([]BLEResult(nil), c.status.Results...)
	return st
}

func (c *BLECollector) TriggerNow() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

func (c *BLECollector) setStatus(f func(*BLEStatus)) {
	c.mu.Lock()
	f(&c.status)
	c.mu.Unlock()
}

func (c *BLECollector) Run(ctx context.Context, initialDelay time.Duration) {
	if err := c.adapter.Enable(); err != nil {
		log.Printf("bluetooth: %v (collector disabled)", err)
		c.setStatus(func(s *BLEStatus) { s.Err = "Bluetooth unavailable: " + err.Error() })
		return
	}
	// Give the API poller a moment to register devices on a fresh database.
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialDelay):
	case <-c.trigger:
	}
	for {
		c.syncAll(ctx)
		next := time.Now().Add(c.every)
		c.setStatus(func(s *BLEStatus) { s.NextRun = next })
		if !waitUntil(ctx, next, c.trigger) {
			return
		}
	}
}

func (c *BLECollector) syncAll(ctx context.Context) {
	c.setStatus(func(s *BLEStatus) { s.Running = true; s.Results = nil })
	defer c.setStatus(func(s *BLEStatus) { s.Running = false; s.Current = ""; s.LastRun = time.Now() })

	devs, err := c.store.Devices(ctx)
	if err != nil {
		c.setStatus(func(s *BLEStatus) { s.Err = err.Error() })
		return
	}
	c.setStatus(func(s *BLEStatus) { s.Err = "" })
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		c.setStatus(func(s *BLEStatus) { s.Current = d.Name })
		start := time.Now()
		n, added, err := c.syncDevice(ctx, d)
		r := BLEResult{Name: d.Name, Readings: n, New: added, Took: time.Since(start).Round(time.Second)}
		if err != nil {
			r.Err = err.Error()
			log.Printf("bluetooth %s: %v", d.Name, err)
		} else {
			log.Printf("bluetooth %s: %d readings downloaded, %d new (%s)", d.Name, n, added, r.Took)
		}
		c.setStatus(func(s *BLEStatus) { s.Results = append(s.Results, r) })
	}
}

// bleNameMatches reports whether an advertised name belongs to the device:
// Govee advertises e.g. "GV5179_1A2B", the SKU digits and the last two bytes
// of the device ID.
func bleNameMatches(d Device, name string) bool {
	id := strings.ReplaceAll(d.ID, ":", "")
	if len(id) < 4 || name == "" {
		return false
	}
	return strings.HasSuffix(name, "_"+id[len(id)-4:]) && strings.Contains(name, strings.TrimPrefix(d.SKU, "H"))
}

// scanFor scans until a device advertising a matching name is seen.
func (c *BLECollector) scanFor(d Device, timeout time.Duration) (bluetooth.Address, error) {
	var (
		mu    sync.Mutex
		found *bluetooth.Address
		once  sync.Once
	)
	stop := func() { once.Do(func() { _ = c.adapter.StopScan() }) }
	timer := time.AfterFunc(timeout, stop)
	defer timer.Stop()
	err := c.adapter.Scan(func(_ *bluetooth.Adapter, r bluetooth.ScanResult) {
		if !bleNameMatches(d, r.LocalName()) {
			return
		}
		mu.Lock()
		if found == nil {
			a := r.Address
			found = &a
		}
		mu.Unlock()
		stop()
	})
	if err != nil {
		return bluetooth.Address{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	if found == nil {
		return bluetooth.Address{}, fmt.Errorf("not heard within %s (out of range?)", timeout)
	}
	return *found, nil
}

// connect uses the remembered Bluetooth address when there is one (no scan
// needed), and scans for the device otherwise or if that fails.
func (c *BLECollector) connect(ctx context.Context, d Device) (bluetooth.Device, error) {
	params := bluetooth.ConnectionParams{ConnectionTimeout: bluetooth.NewDuration(20 * time.Second)}
	if d.BLEAddress != "" {
		var a bluetooth.Address
		a.Set(d.BLEAddress)
		if dev, err := c.adapter.Connect(a, params); err == nil {
			return dev, nil
		}
	}
	a, err := c.scanFor(d, 60*time.Second)
	if err != nil {
		return bluetooth.Device{}, err
	}
	if a.String() != d.BLEAddress {
		if err := c.store.SetBLEAddress(ctx, d.ID, a.String()); err != nil {
			log.Printf("bluetooth: remember address: %v", err)
		}
	}
	return c.adapter.Connect(a, params)
}

type bleNotify struct {
	uuid string
	data []byte
}

func (c *BLECollector) syncDevice(ctx context.Context, d Device) (int, int64, error) {
	since, err := c.store.LatestDetailed(ctx, d.ID)
	if err != nil {
		return 0, 0, err
	}
	minutes := maxHistoryMinutes
	if !since.IsZero() {
		// A few minutes of overlap; already-cached minutes are skipped.
		minutes = min(maxHistoryMinutes, int(time.Since(since)/time.Minute)+5)
	}
	if minutes < 10 {
		return 0, 0, nil // synced moments ago
	}

	dev, err := c.connect(ctx, d)
	if err != nil {
		return 0, 0, fmt.Errorf("connect: %w", err)
	}
	defer dev.Disconnect()

	svcs, err := dev.DiscoverServices(nil)
	if err != nil {
		return 0, 0, fmt.Errorf("discover services: %w", err)
	}
	chars := map[string]bluetooth.DeviceCharacteristic{}
	for _, s := range svcs {
		u := s.UUID().String()
		if u != uuidIRService && u != uuidAuthService {
			continue
		}
		cs, err := s.DiscoverCharacteristics(nil)
		if err != nil {
			return 0, 0, fmt.Errorf("discover characteristics: %w", err)
		}
		for _, ch := range cs {
			chars[ch.UUID().String()] = ch
		}
	}
	for _, u := range []string{uuidIRCommand, uuidIRData} {
		if _, ok := chars[u]; !ok {
			return 0, 0, fmt.Errorf("characteristic %s missing (not a supported Govee thermometer?)", u)
		}
	}

	inbox := make(chan bleNotify, 8192)
	for _, u := range []string{uuidAuthNotify, uuidIRDevice, uuidIRCommand, uuidIRData} {
		ch, ok := chars[u]
		if !ok {
			continue
		}
		if err := ch.EnableNotifications(func(buf []byte) {
			select {
			case inbox <- bleNotify{u, append([]byte(nil), buf...)}:
			default: // full; the transfer will come up short and be retried next run
			}
		}); err != nil {
			return 0, 0, fmt.Errorf("enable notifications: %w", err)
		}
	}

	waitFor := func(uuid string, timeout time.Duration) ([]byte, error) {
		deadline := time.After(timeout)
		for {
			select {
			case n := <-inbox:
				if n.uuid == uuid {
					return n.data, nil
				}
			case <-deadline:
				return nil, fmt.Errorf("no reply on %s", uuid[len(uuid)-4:])
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	// Auth handshake on encrypted devices: the reply to e7 01 carries the
	// session key used for everything after.
	var key []byte
	if aw, ok := chars[uuidAuthWrite]; ok {
		for _, step := range []byte{0x01, 0x02} {
			if _, err := aw.WriteWithoutResponse(goveeCrypt(goveePSK, goveePacket(0xe7, step), false)); err != nil {
				return 0, 0, fmt.Errorf("auth write: %w", err)
			}
			raw, err := waitFor(uuidAuthNotify, 10*time.Second)
			if err != nil {
				return 0, 0, fmt.Errorf("auth: %w", err)
			}
			rx := goveeCrypt(goveePSK, raw, true)
			if rx[0] != 0xe7 || rx[1] != step {
				return 0, 0, fmt.Errorf("auth: unexpected reply %x", rx[:2])
			}
			if step == 0x01 {
				key = append([]byte(nil), rx[2:18]...)
			}
		}
	}
	enc := func(p []byte) []byte {
		if key == nil {
			return p
		}
		return goveeCrypt(key, p, false)
	}
	dec := func(p []byte) []byte {
		if key == nil || len(p) != 20 {
			return p
		}
		return goveeCrypt(key, p, true)
	}

	cmd := chars[uuidIRCommand]
	start := time.Now().Truncate(time.Minute)
	if _, err := cmd.Write(enc(historyRequest(minutes))); err != nil {
		return 0, 0, fmt.Errorf("history request: %w", err)
	}

	var samples []bleSample
	lastKeepAlive := minutes
	for done := false; !done; {
		select {
		case n := <-inbox:
			p := dec(n.data)
			switch n.uuid {
			case uuidIRData:
				first, s := decodeHistoryPacket(p)
				samples = append(samples, s...)
				if first < 7 {
					done = true
				} else if lastKeepAlive-first >= 450 {
					// Keep long transfers alive, as the Govee app does.
					lastKeepAlive = first
					if _, err := cmd.Write(enc(goveePacket(0xaa, 0x01))); err != nil {
						return 0, 0, fmt.Errorf("keep-alive: %w", err)
					}
				}
			case uuidIRCommand:
				if len(p) >= 2 && p[0] == 0xee && p[1] == 0x01 {
					done = true // device says the transfer is complete
				}
			}
		case <-time.After(20 * time.Second):
			done = true
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	if len(samples) == 0 {
		return 0, 0, errors.New("no history received")
	}

	rs := make([]Reading, 0, len(samples))
	for _, s := range samples {
		if s.TempC < -40 || s.TempC > 70 || s.Hum <= 0 || s.Hum > 100 {
			continue // unset or corrupt slot
		}
		rs = append(rs, Reading{
			DeviceID: d.ID,
			TS:       start.Add(-time.Duration(s.Offset) * time.Minute),
			TempF:    round(s.TempC*9/5+32+d.TempOffsetF, 2),
			Humidity: sql.NullFloat64{Float64: round(s.Hum+d.HumOffset, 1), Valid: true},
			Source:   "ble",
		})
	}
	if len(rs) == 0 {
		return len(samples), 0, errors.New("history contained no valid readings")
	}
	added, err := c.store.InsertReadingsIgnore(ctx, rs)
	if err != nil {
		return len(rs), 0, err
	}
	// Minute data now covers this span, so the coarse API snapshots in it
	// are redundant.
	if err := c.store.DeleteAPIBetween(ctx, d.ID, rs[0].TS, rs[len(rs)-1].TS); err != nil {
		log.Printf("bluetooth %s: prune API readings: %v", d.Name, err)
	}
	return len(rs), added, nil
}
