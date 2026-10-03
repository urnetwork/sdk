package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// Keeping the store behind an unexported LocalState helper must not weaken the
// durable generation and signed-history ratchets it supplies to connect.
func TestLocalStatePeerClientKeyPinStorePersistsRatchet(t *testing.T) {
	if store := (&LocalState{}).peerClientKeyPinStore(); store != nil {
		t.Fatal("local state without private storage returned a shared pin store")
	}

	localStorageDir := t.TempDir()
	store := (&LocalState{localStorageDir: localStorageDir}).peerClientKeyPinStore()
	if store == nil {
		t.Fatal("local state with private storage did not return a pin store")
	}
	peerId := connect.NewId()
	pin := connect.ClientKeyPin{Generation: 7}
	pin.DomainDigest[0] = 1
	pin.Signer[0] = 2
	pin.PublicKey[0] = 3
	store.SetPeerClientKeyPin(peerId, pin)

	rewind := pin
	rewind.Generation--
	rewind.PublicKey[0] = 4
	store.SetPeerClientKeyPin(peerId, rewind)
	store.SetSignedHistorySeen()

	reloaded := (&LocalState{localStorageDir: localStorageDir}).peerClientKeyPinStore()
	got, ok := reloaded.GetPeerClientKeyPin(peerId)
	if !ok || got != pin {
		t.Fatalf("reloaded pin = %+v, present = %v; want %+v", got, ok, pin)
	}
	if !reloaded.SignedHistorySeen() {
		t.Fatal("signed-history tier did not persist")
	}
}
