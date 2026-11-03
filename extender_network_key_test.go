//go:build !ios && !android && !js

package sdk

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

// A key of the network's own when the operator refuses this installation's
// extender key as another network's (EXTENDER.md C2c, extender_network_key.go).

// A space with local state, as an app has, over a fresh storage directory.
func newTestExtenderKeySpace(t *testing.T) *NetworkSpace {
	t.Helper()
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey(testExtenderKeySpaceHost, "main"),
		func(values *NetworkSpaceValues) {},
	)
	if networkSpace.asyncLocalState == nil {
		t.Fatal("the space keeps no local state")
	}
	return networkSpace
}

const testExtenderKeySpaceHost = "space.example"

func testExtenderKeyPublicKey(t *testing.T, seed []byte) []byte {
	t.Helper()
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// A client jwt of one network and client, unsigned as far as the space cares:
// it reads the claims without verifying them, as the operator verifies them.
func testExtenderClientJwt(t *testing.T, networkId connect.Id, clientId connect.Id) string {
	t.Helper()
	now := time.Now()
	token := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"network_id": networkId.String(),
		"client_id":  clientId.String(),
		"device_id":  connect.NewId().String(),
		"iat":        now.Unix(),
		"exp":        now.Add(24 * time.Hour).Unix(),
	})
	byJwt, err := token.SignedString([]byte("test"))
	if err != nil {
		t.Fatal(err)
	}
	return byJwt
}

// The network of a client jwt is what the role's key is chosen by, and a jwt
// that names none chooses `.extender_key`.
func TestExtenderKeyNetworkIdOfJwt(t *testing.T) {
	networkId := connect.NewId()
	if id := extenderKeyNetworkIdOfJwt(testExtenderClientJwt(t, networkId, connect.NewId())); id != networkId {
		t.Fatalf("network id = %s, expected %s", id, networkId)
	}
	for _, byJwt := range []string{"", "not a jwt"} {
		if id := extenderKeyNetworkIdOfJwt(byJwt); id != (connect.Id{}) {
			t.Fatalf("network id of %q = %s, expected none", byJwt, id)
		}
	}
}

// The space presents `.extender_key` for a network it made no key for, and the
// key it made for a network while the role runs for that network: a device that
// goes back to the account that holds `.extender_key` presents it again, and
// `.extender_key` itself is never replaced.
func TestNetworkSpacePresentsTheKeyOfTheNetworkTheRoleRunsFor(t *testing.T) {
	networkSpace := newTestExtenderKeySpace(t)
	holderNetworkId := connect.NewId()
	otherNetworkId := connect.NewId()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	defaultSeed := networkSpace.extenderIdentityKeySeed()
	networkSpace.setExtenderKeyNetwork(holderNetworkId)
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, defaultSeed) {
		t.Fatal("the space presents another key than .extender_key for a network it made none for")
	}

	networkSpace.setExtenderKeyNetwork(otherNetworkId)
	madeSeed := networkSpace.renewExtenderKeyForNetwork(otherNetworkId, testExtenderKeyPublicKey(t, defaultSeed), now)
	if len(madeSeed) == 0 || bytes.Equal(madeSeed, defaultSeed) {
		t.Fatal("the space made no key of the network's own for the refused .extender_key")
	}
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, madeSeed) {
		t.Fatal("the space does not present the key it made for the network the role runs for")
	}

	// back to the account that holds .extender_key, and to the other again
	networkSpace.setExtenderKeyNetwork(holderNetworkId)
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, defaultSeed) {
		t.Fatal("the space does not present .extender_key again for the network that holds it")
	}
	networkSpace.setExtenderKeyNetwork(otherNetworkId)
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, madeSeed) {
		t.Fatal("the space does not present the network's own key again")
	}

	localState := networkSpace.asyncLocalState.GetLocalState()
	if seed, err := localState.GetOrCreateExtenderKeySeed(); err != nil || !bytes.Equal(seed, defaultSeed) {
		t.Fatal("making a key of the network's own replaced .extender_key")
	}
	if seed, createTime, ok := localState.getExtenderNetworkKeySeed(otherNetworkId); !ok ||
		!bytes.Equal(seed, madeSeed) || !createTime.Equal(now) {
		t.Fatalf("the stored key of the network = %t at %s, expected the made key at %s", ok, createTime, now)
	}
	if _, _, ok := localState.getExtenderNetworkKeySeed(holderNetworkId); ok {
		t.Fatal("the space stored a key for the network that holds .extender_key")
	}
	// a cold read of the same files, as the next process makes
	cold := newLocalState(context.Background(), filepath.Dir(localState.localStorageDir))
	defer cold.Close()
	if seed, _, ok := cold.getExtenderNetworkKeySeed(otherNetworkId); !ok || !bytes.Equal(seed, madeSeed) {
		t.Fatal("the key of the network did not persist")
	}
}

// A key is made only for the key the space presents for the network, at most
// one a day for a network, and never by a space with no local state.
func TestNetworkSpaceRenewsANetworksKeyOnlyOnceADay(t *testing.T) {
	networkSpace := newTestExtenderKeySpace(t)
	networkId := connect.NewId()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	networkSpace.setExtenderKeyNetwork(networkId)
	defaultSeed := networkSpace.extenderIdentityKeySeed()

	madeSeed := networkSpace.renewExtenderKeyForNetwork(networkId, testExtenderKeyPublicKey(t, defaultSeed), now)
	if len(madeSeed) == 0 {
		t.Fatal("the space made no key for the refused .extender_key")
	}
	// a refusal of the key the space presented before, which a role posted
	// before it was replaced
	if seed := networkSpace.renewExtenderKeyForNetwork(networkId, testExtenderKeyPublicKey(t, defaultSeed), now); seed != nil {
		t.Fatal("the space made a key for a refusal of a key it no longer presents")
	}
	// the made key refused within the day: the refusal stands
	if seed := networkSpace.renewExtenderKeyForNetwork(
		networkId, testExtenderKeyPublicKey(t, madeSeed), now.Add(extenderNetworkKeyRenewTimeout-time.Second)); seed != nil {
		t.Fatal("the space made a second key for the network within the day")
	}
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, madeSeed) {
		t.Fatal("a refusal within the day changed the network's key")
	}
	// and after it
	secondSeed := networkSpace.renewExtenderKeyForNetwork(
		networkId, testExtenderKeyPublicKey(t, madeSeed), now.Add(extenderNetworkKeyRenewTimeout))
	if len(secondSeed) == 0 || bytes.Equal(secondSeed, madeSeed) || bytes.Equal(secondSeed, defaultSeed) {
		t.Fatal("the space made no new key for the network a day later")
	}
	if seed := networkSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, secondSeed) {
		t.Fatal("the space does not present the network's new key")
	}

	// no network named
	if seed := networkSpace.renewExtenderKeyForNetwork(connect.Id{}, testExtenderKeyPublicKey(t, secondSeed), now); seed != nil {
		t.Fatal("the space made a key for no network")
	}

	// a space with no local state: its embedder keeps the identity
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urlSpace := newTestProvideExtenderUrlSpace(ctx)
	defer urlSpace.Close()
	urlSeed := urlSpace.extenderIdentityKeySeed()
	if seed := urlSpace.renewExtenderKeyForNetwork(networkId, testExtenderKeyPublicKey(t, urlSeed), now); seed != nil {
		t.Fatal("a space with no local state made a key")
	}
	if seed := urlSpace.extenderIdentityKeySeed(); !bytes.Equal(seed, urlSeed) {
		t.Fatal("a space with no local state changed its identity")
	}
}
