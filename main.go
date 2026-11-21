// Command govee-hvac collects readings from Govee H5179 thermo-hygrometers
// (per-minute history over Bluetooth, plus snapshots from the Govee cloud API),
// caches them in MySQL and serves server-rendered temperature charts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8179", "HTTP listen address")
	keyFile := flag.String("key-file", "config/govee_api_key", "file holding the Govee API key (or set GOVEE_API_KEY)")
	dsnFile := flag.String("dsn-file", "config/db_dsn", "file holding the MySQL DSN (or set GOVEE_DSN)")
	poll := flag.Duration("poll", 15*time.Minute, "how often to poll the Govee cloud API (its values refresh only about hourly)")
	useBLE := flag.Bool("ble", true, "download per-minute history from the sensors over Bluetooth")
	bleEvery := flag.Duration("ble-every", 3*time.Hour, "how often to sync over Bluetooth (sensors store months of data)")
	tz := flag.String("tz", "Local", "time zone for display and CSV imports, e.g. America/Chicago")
	flag.Parse()

	key, err := secret("GOVEE_API_KEY", *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	dsn, err := secret("GOVEE_DSN", *dsnFile)
	if err != nil {
		log.Fatal(err)
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatalf("bad -tz: %v", err)
	}
	if *poll < 30*time.Second {
		log.Fatal("-poll must be at least 30s")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// At login MySQL may still be starting, so keep trying for a while.
	var store *Store
	for attempt := 1; ; attempt++ {
		if store, err = OpenStore(ctx, dsn); err == nil {
			break
		}
		if attempt == 24 || ctx.Err() != nil {
			log.Fatal(err)
		}
		log.Printf("%v; retrying in 5s", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	defer store.Close()

	poller := NewPoller(NewGoveeClient(key), store, *poll)
	go poller.Run(ctx)

	var ble *BLECollector
	if *useBLE {
		ble = NewBLECollector(store, *bleEvery)
		go ble.Run(ctx, 15*time.Second)
	}

	srv, err := NewServer(store, poller, ble, loc)
	if err != nil {
		log.Fatal(err)
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("HVAC thermal survey listening on %s (polling every %s)", *addr, *poll)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// secret reads a value from the environment, falling back to a file.
func secret(env, file string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s not set and %s unreadable: %w", env, file, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", file)
	}
	return v, nil
}
