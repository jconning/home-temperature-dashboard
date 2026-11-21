package main

import (
	"context"
	"time"
)

// wallCheckEvery is how often waitUntil looks at the wall clock.
const wallCheckEvery = time.Minute

// waitUntil blocks until the wall-clock time reaches due, a trigger arrives, or
// ctx ends (it then returns false). Go timers on macOS stop counting while
// the computer sleeps, so a plain Ticker can run hours late after a night of
// sleep. This checks the wall clock every minute instead, so a job that came
// due during sleep runs within a minute of waking.
func waitUntil(ctx context.Context, due time.Time, trigger <-chan struct{}) bool {
	due = due.Round(0) // drop the monotonic reading; compare wall time only
	check := time.NewTicker(wallCheckEvery)
	defer check.Stop()
	for {
		if !time.Now().Round(0).Before(due) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-trigger:
			return true
		case <-check.C:
		}
	}
}
