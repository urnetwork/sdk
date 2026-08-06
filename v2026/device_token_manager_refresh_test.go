package sdk

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The shape of deviceTokenManager.run(), in miniature: capture the notify channel at the
// TOP of each iteration, wait, then do slow work (the /auth/refresh http call).
//
// `levelTriggered` selects the fix. With it, a request made DURING the slow work survives
// via the pending flag. Without it — the monitor's edge alone — the request is lost:
// NotifyAll closes a channel the loop has already stopped listening to, and the loop goes
// back to sleep for the full timeout (in production, ~16 days).
func runRefreshLoop(levelTriggered bool) int {
	waiting := make(chan struct{}, 4)
	working := make(chan struct{}, 4)

	monitor := connect.NewMonitor()
	var pending atomic.Bool

	refreshes := 0
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 2; i += 1 {
			notify := monitor.NotifyChannel()

			if !levelTriggered || !pending.Load() {
				// tell the test we are parked on the channel, so it can signal without
				// racing us
				waiting <- struct{}{}
				select {
				case <-notify:
				case <-time.After(750 * time.Millisecond): // stands in for the long sleep
					return
				}
			}

			if levelTriggered {
				// consume BEFORE the work: a request arriving during it must set the flag
				// again and be serviced next pass
				pending.Store(false)
			}

			if i == 0 {
				// we are now "inside the http call" — the window where a request used to
				// vanish
				working <- struct{}{}
				time.Sleep(120 * time.Millisecond)
			}
			refreshes += 1
		}
	}()

	// request #1
	<-waiting
	pending.Store(true)
	monitor.NotifyAll()

	// request #2, made while the loop is INSIDE the refresh
	<-working
	pending.Store(true)
	monitor.NotifyAll()

	<-done
	return refreshes
}

// TestEdgeTriggerAloneLosesTheRequest is the CONTROL: it proves the bug was real. With the
// monitor's edge alone, the request made during an in-flight refresh is dropped.
func TestEdgeTriggerAloneLosesTheRequest(t *testing.T) {
	refreshes := runRefreshLoop(false)

	if refreshes != 1 {
		t.Fatalf("edge-only: got %d refreshes, expected the second request to be LOST (1)", refreshes)
	}
}

// TestRefreshRequestSurvivesAnInFlightRefresh pins the fix: the same request is honored.
//
// This matters because an app asks for a refresh right after an upgrade, to pick up the
// new `pro` claim. Dropping it meant the claim stayed stale for ~16 days, and nothing
// anywhere said so.
func TestRefreshRequestSurvivesAnInFlightRefresh(t *testing.T) {
	refreshes := runRefreshLoop(true)

	if refreshes != 2 {
		t.Fatalf("level-triggered: got %d refreshes, want 2 — the request made during an in-flight refresh was dropped", refreshes)
	}
}
