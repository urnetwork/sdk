//go:build !ios && !android && !js

package sdk

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The provider extender role on a key another network holds (EXTENDER.md C2c).

// The operator's refusal of a key another network holds, as it first sent it
// (server controller/extender_controller.go), with no `refusal` code.
const testProvideExtenderKeyOtherNetworkRefusal = "This extender key is registered to another network, " +
	"so it cannot be activated for this one. Generate a new extender key, or ask support to " +
	"move the key to this network."

// A device whose `.extender_key` another network holds -- the device signed in
// to another account, or its state copied to a host under another account --
// serves a key of its network's own and activates under it, and `.extender_key`
// stays as it was. Before, the role posted the refused key on every step of its
// backoff for as long as it ran, and never activated: the refusal told the
// person to make a new key, which no app has an action for.
func TestDeviceLocalProviderExtenderServesAKeyOfItsNetworksOwn(t *testing.T) {
	clientId := connect.NewId()
	var refusedPublicKey ed25519.PublicKey
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		byJwt:    testExtenderClientJwt(t, connect.NewId(), clientId),
		clientId: clientId,
		prepare: func(fixture *testProvideExtenderFixture) {
			seed, err := fixture.networkSpace.asyncLocalState.GetLocalState().GetOrCreateExtenderKeySeed()
			if err != nil {
				t.Fatal(err)
			}
			refusedPublicKey = ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
			fixture.operator.setKeyRefusal(refusedPublicKey, testProvideExtenderKeyOtherNetworkRefusal)
		},
	})

	// the first posts carry `.extender_key`, which the operator refuses
	if post := fixture.waitPost(); post.args.PublicKeyHex != testHexPublicKey(refusedPublicKey) {
		t.Fatalf("the first post carried %s, expected .extender_key's %s",
			post.args.PublicKeyHex, testHexPublicKey(refusedPublicKey))
	}

	// the role starts again on a key of its own, with the clock where it was:
	// no hold or backoff is waited out for it
	deadline := time.After(60 * time.Second)
	var renewedPublicKeyHex string
	for renewedPublicKeyHex == "" {
		select {
		case post := <-fixture.operator.posts:
			if post.args.PublicKeyHex != testHexPublicKey(refusedPublicKey) {
				renewedPublicKeyHex = post.args.PublicKeyHex
			}
		case <-deadline:
			t.Fatalf("the role never posted a key other than the refused one (%d v4 and %d v6 posts)",
				fixture.operator.postCount(4), fixture.operator.postCount(6))
		}
	}
	status := fixture.waitStatus("activated on the new key", func(status *ExtenderProvideStatus) bool {
		return status.State == ExtenderProvideStateActive && status.ActivatedV4 && status.ActivatedV6
	})
	if status.ErrorCase != "" {
		t.Fatalf("status = %+v, expected active with no error case", status)
	}
	if role := fixture.extender(); role == nil || testHexPublicKey(role.publicKey) != renewedPublicKeyHex {
		t.Fatal("the running role is not on the key it activated")
	}

	// `.extender_key` stays as it was, for the account that holds it
	seed, err := fixture.networkSpace.asyncLocalState.GetLocalState().GetOrCreateExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	if publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey); !bytes.Equal(publicKey, refusedPublicKey) {
		t.Fatal("the role replaced .extender_key")
	}
}

// The network's own key is what the device presents to the mesh and to an
// embedder from then on, and it is kept for the network across a restart.
func TestDeviceLocalProviderExtenderKeepsItsNetworksKey(t *testing.T) {
	networkId := connect.NewId()
	clientId := connect.NewId()
	var refusedPublicKey ed25519.PublicKey
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		byJwt:    testExtenderClientJwt(t, networkId, clientId),
		clientId: clientId,
		prepare: func(fixture *testProvideExtenderFixture) {
			seed, err := fixture.networkSpace.asyncLocalState.GetLocalState().GetOrCreateExtenderKeySeed()
			if err != nil {
				t.Fatal(err)
			}
			refusedPublicKey = ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
			fixture.operator.setKeyRefusal(refusedPublicKey, testProvideExtenderKeyOtherNetworkRefusal)
		},
	})
	fixture.waitStatus("activated on the new key", func(status *ExtenderProvideStatus) bool {
		return status.State == ExtenderProvideStateActive && status.ActivatedV4 && status.ActivatedV6
	})
	role := fixture.extender()
	if role == nil || bytes.Equal(role.publicKey, refusedPublicKey) {
		t.Fatal("the role is not on a key of the network's own")
	}

	localState := fixture.networkSpace.asyncLocalState.GetLocalState()
	seed, _, ok := localState.getExtenderNetworkKeySeed(networkId)
	if !ok {
		t.Fatal("the network's key is not kept")
	}
	if publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey); !bytes.Equal(publicKey, role.publicKey) {
		t.Fatal("the kept key of the network is not the role's")
	}
	// the mesh identity is the role's (B1)
	if !bytes.Equal(fixture.networkSpace.extenderIdentityKeySeed(), seed) {
		t.Fatal("the space presents another identity than the role's")
	}
}

func testHexPublicKey(publicKey []byte) string {
	return hex.EncodeToString(publicKey)
}
