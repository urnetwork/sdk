//go:build !ios_extension && !js

package sdk

// A listen's grant lifetime through SidecarSockets (owner decision 687; QD-1419): ListenExpires hands its milliseconds
// to the core's backend as a duration, which fills SocketListenSettings.Expires or TlsListenSettings.Expires, 0 as 0,
// and refuses a negative one and one past what a duration holds before the backend sees anything; Listen and the
// Go-only ListenContext ask none; and a listener's GetGrantExpiresMillis is the grant end its core listener keeps, 0
// for none. The core's half, that Expires reaches the tls/ registration and the app listener answers its
// registration's end, is the sidecar repo's TestTlsComponentListenAsksItsExpires. Synthetic values only.

import (
	"errors"
	"math"
	"testing"
	"time"
)

// ListenExpires's lifetime reaches the backend, 0 as 0; Listen and ListenContext ask none.
func TestSidecarSocketsListenExpiresAsksItsGrantsLifetime(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, _ := newTestSidecarSockets(t, backend)
	asked, err := sockets.ListenExpires("asked.sockettest", 0, (3 * 24 * time.Hour).Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	defer asked.Close()
	none, err := sockets.ListenExpires("none.sockettest", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer none.Close()
	bound, err := sockets.Listen("bound.sockettest", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	goOnly, err := sockets.ListenContext(t.Context(), "go.sockettest")
	if err != nil {
		t.Fatal(err)
	}
	defer goOnly.Close()
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	want := []time.Duration{3 * 24 * time.Hour, 0, 0, 0}
	if len(backend.lifetimes) != len(want) {
		t.Fatalf("the listens asked %v", backend.lifetimes)
	}
	for i, lifetime := range want {
		if backend.lifetimes[i] != lifetime {
			t.Errorf("listen %d asked %v, not %v", i, backend.lifetimes[i], lifetime)
		}
	}
}

// A negative lifetime and one past what a duration holds are refused before the backend sees a listen.
func TestSidecarSocketsListenExpiresRefusesALifetimeOutOfRange(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, _ := newTestSidecarSockets(t, backend)
	for _, expiresMillis := range []int64{-1, math.MinInt64, math.MaxInt64, math.MaxInt64/1000000 + 1} {
		if _, err := sockets.ListenExpires("refused.sockettest", 0, expiresMillis); !errors.Is(err, errSidecarSocketListenExpires) {
			t.Errorf("a lifetime of %d ms: %v", expiresMillis, err)
		}
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if len(backend.lifetimes) != 0 {
		t.Errorf("a refused lifetime reached the backend: %v", backend.lifetimes)
	}
}

// GetGrantExpiresMillis is the grant end the core's listener keeps, in milliseconds since the epoch, and 0 for one
// with none.
func TestSidecarSocketListenerGetGrantExpiresMillis(t *testing.T) {
	end := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	backend := newTestSidecarSocketsBackend()
	backend.addressGrantEnds = map[string]time.Time{"granted.sockettest": end}
	sockets, _ := newTestSidecarSockets(t, backend)
	granted, err := sockets.ListenExpires("granted.sockettest", 0, (24 * time.Hour).Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	defer granted.Close()
	standing, err := sockets.Listen("standing.sockettest", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer standing.Close()
	if got := granted.GetGrantExpiresMillis(); got != end.UnixMilli() {
		t.Errorf("the granted listen's end %d, not %d", got, end.UnixMilli())
	}
	if got := standing.GetGrantExpiresMillis(); got != 0 {
		t.Errorf("a listen with no end of its own names %d", got)
	}
}
