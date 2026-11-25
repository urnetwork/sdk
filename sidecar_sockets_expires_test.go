//go:build !ios_extension && !js

package sdk

// A listen's grant lifetime through SidecarSockets (owner decision 687): the listen beneath the bound calls hands the
// lifetime it is given to the core's backend, which fills SocketListenSettings.Expires or TlsListenSettings.Expires
// with it, and the bound Listen and the Go-only ListenContext ask none, the requests rule's expiresDefault, since
// §18.5 binds no lifetime for an app to ask yet (question listen-expires-surfaces-sdk-listen-expiry). The core's half,
// that Expires reaches the tls/ registration, is the sidecar repo's TestTlsComponentListenAsksItsExpires.

import (
	"testing"
	"time"
)

// The lifetime given reaches the backend; Listen and ListenContext ask none.
func TestSidecarSocketsListenAsksItsGrantsLifetime(t *testing.T) {
	backend := newTestSidecarSocketsBackend()
	sockets, _ := newTestSidecarSockets(t, backend)
	asked, err := sockets.listen(t.Context(), "asked.sockettest", 0, 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer asked.Close()
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
	if len(backend.lifetimes) != 3 || backend.lifetimes[0] != 3*24*time.Hour || backend.lifetimes[1] != 0 || backend.lifetimes[2] != 0 {
		t.Errorf("the listens asked %v", backend.lifetimes)
	}
}
