package sdk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The trust anchor and the identity material (EXTENDER.md B1, B4, K6).
//
// Everything here is the fail-closed half of the extender network: what a
// client accepts a record under, and what it activates and peers as. A defect
// in either is silent -- a refused anchor accepts nothing until the next
// hello, a replaced identity is revoked as fast as it activates -- so each
// rule is pinned on its own rather than observed through a working network.

// The anchor in force is replaced only by a configured set that resolves to a
// key. A set that does not resolve is not an anchor: installing it would
// refuse every record until the next hello, so what is in force stays (B4).
func TestExtenderRootKeysReplaceOnlyOnAResolvableSet(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})

	// the bundled table names no key for a synthetic host, so this space
	// starts with no anchor at all
	if rootKeyHexes := testExtenderRootKeyHexes(networkSpace); 0 < len(rootKeyHexes) {
		t.Fatalf("root keys = %v, expected none for a host the bundled table does not name", rootKeyHexes)
	}

	setRootPublicKeys := func(rootPublicKeys []string) {
		t.Helper()
		updated := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
			values.ExtenderRootPublicKeys = slices.Clone(rootPublicKeys)
		})
		if updated != networkSpace {
			t.Fatalf("a root key change rebuilt the space instead of restarting it in place")
		}
	}

	setRootPublicKeys([]string{testExtenderRootPublicKeyHex})
	if rootKeyHexes := testExtenderRootKeyHexes(networkSpace); !slices.Equal(
		rootKeyHexes,
		[]string{testExtenderRootPublicKeyHex},
	) {
		t.Fatalf("root keys = %v, expected the configured one", rootKeyHexes)
	}

	// every one of these is a real value change, so the apply path runs; none
	// of them may take the anchor away
	cases := []struct {
		name           string
		rootPublicKeys []string
	}{
		{name: "not hex", rootPublicKeys: []string{"nonsense"}},
		{name: "too short", rootPublicKeys: []string{"00112233"}},
		{name: "one bad key in the set", rootPublicKeys: []string{
			testExtenderRootPublicKeyHex,
			"00112233445566778899aabbccddeeff",
		}},
		{name: "cleared", rootPublicKeys: nil},
		{name: "only whitespace", rootPublicKeys: []string{"   "}},
	}
	for _, c := range cases {
		setRootPublicKeys(c.rootPublicKeys)
		if rootKeyHexes := testExtenderRootKeyHexes(networkSpace); !slices.Equal(
			rootKeyHexes,
			[]string{testExtenderRootPublicKeyHex},
		) {
			t.Errorf("%s: root keys = %v, expected the anchor in force to stand", c.name, rootKeyHexes)
		}
		// back to the good anchor, so the next case is a transition off a
		// known key rather than off whatever this one left. The manager
		// rebuilds a space for an update that changes nothing, so every step
		// here has to be a real change.
		setRootPublicKeys([]string{testExtenderRootPublicKeyHex})
	}

	// a second resolvable key does replace it, so the assertions above are
	// about the refusal and not about an anchor nothing can move
	otherSeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	otherPublicKey, err := connect.ExtenderPublicKeyFromSeed(otherSeed)
	if err != nil {
		t.Fatal(err)
	}
	otherPublicKeyHex := hex.EncodeToString(otherPublicKey)
	setRootPublicKeys([]string{otherPublicKeyHex})
	if rootKeyHexes := testExtenderRootKeyHexes(networkSpace); !slices.Equal(
		rootKeyHexes,
		[]string{otherPublicKeyHex},
	) {
		t.Fatalf("root keys = %v, expected the replacement", rootKeyHexes)
	}
}

// The anchor one space's directory holds right now, hex encoded.
func testExtenderRootKeyHexes(networkSpace *NetworkSpace) []string {
	rootKeySet := networkSpace.extenderDirectory.RootKeys()
	if rootKeySet == nil {
		return nil
	}
	rootKeyHexes := []string{}
	for _, rootPublicKey := range rootKeySet.PublicKeys() {
		rootKeyHexes = append(rootKeyHexes, hex.EncodeToString(rootPublicKey))
	}
	return rootKeyHexes
}

// An embedder's extender identity is taken only when it is a usable seed. A
// seed of the wrong length is refused here rather than by the role, which
// would silently run on a generated identity the embedder never persists
// (B1, G2).
func TestExtenderKeyMaterialRefusesAnInvalidSeed(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}

	keyMaterial := NewDeviceLocalKeyMaterial(nil, nil, nil)
	if !keyMaterial.IsEmpty() {
		t.Fatal("a key material with nothing in it did not read as empty")
	}
	keyMaterial.SetExtenderKeySeed(seed)
	if !bytes.Equal(keyMaterial.GetExtenderKeySeed(), seed) {
		t.Fatal("the extender identity was not carried")
	}
	// an extender identity alone is material worth persisting, so it must not
	// read as nothing to save
	if keyMaterial.IsEmpty() {
		t.Fatal("a key material carrying only an extender identity read as empty")
	}

	invalidSeeds := [][]byte{
		{0x01},
		bytes.Clone(seed[:len(seed)-1]),
		append(bytes.Clone(seed), 0x00),
	}
	for _, invalidSeed := range invalidSeeds {
		keyMaterial.SetExtenderKeySeed(invalidSeed)
		if !bytes.Equal(keyMaterial.GetExtenderKeySeed(), seed) {
			t.Errorf(
				"a %d byte seed replaced the identity: %x",
				len(invalidSeed),
				keyMaterial.GetExtenderKeySeed(),
			)
		}
	}

	// the getter hands back a copy, so a caller that keeps the slice cannot
	// rewrite the identity the device runs under
	carried := keyMaterial.GetExtenderKeySeed()
	carried[0] ^= 0xff
	if !bytes.Equal(keyMaterial.GetExtenderKeySeed(), seed) {
		t.Fatal("the extender identity was handed out by reference")
	}

	// an empty seed clears it, which is what a space with local state carries
	keyMaterial.SetExtenderKeySeed(nil)
	if 0 < len(keyMaterial.GetExtenderKeySeed()) {
		t.Fatal("an empty seed did not clear the identity")
	}
	if !keyMaterial.IsEmpty() {
		t.Fatal("a cleared key material did not read as empty")
	}

	// the nil receiver is the no-key-material case every constructor allows
	var noKeyMaterial *DeviceLocalKeyMaterial
	noKeyMaterial.SetExtenderKeySeed(seed)
	if 0 < len(noKeyMaterial.GetExtenderKeySeed()) {
		t.Fatal("a nil key material carried an identity")
	}
	if !noKeyMaterial.IsEmpty() {
		t.Fatal("a nil key material did not read as empty")
	}
}

// The persisted identity is created once and reused, and a file that is not a
// seed is replaced rather than failing the launch: an install that cannot read
// its own key is better off with a new one than with none (B1).
func TestExtenderKeySeedReplacesACorruptKeyFile(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	keyPath := filepath.Join(localState.localStorageDir, extenderKeyFileName)

	seed, err := localState.GetOrCreateExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect.ExtenderPublicKeyFromSeed(seed); err != nil {
		t.Fatalf("the created seed is not an identity: %v", err)
	}
	again, err := localState.GetOrCreateExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, seed) {
		t.Fatal("the identity was rewritten on the second read")
	}
	// the stored document is the hex seed, which is what every other reader of
	// this file expects
	keyState := &extenderKeyState{}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(keyBytes, keyState); err != nil {
		t.Fatal(err)
	}
	if keyState.SeedHex != connect.ExtenderKeySeedHex(seed) {
		t.Fatalf("stored seed = %q, expected the created one", keyState.SeedHex)
	}

	corruptions := []struct {
		name     string
		keyBytes string
	}{
		{name: "empty", keyBytes: ""},
		{name: "not json", keyBytes: "{"},
		{name: "no seed", keyBytes: `{}`},
		{name: "not hex", keyBytes: `{"seed_hex":"nonsense"}`},
		{name: "wrong length", keyBytes: `{"seed_hex":"0011"}`},
	}
	previous := seed
	for _, c := range corruptions {
		if err := os.WriteFile(keyPath, []byte(c.keyBytes), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		replaced, err := localState.GetOrCreateExtenderKeySeed()
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if _, err := connect.ExtenderPublicKeyFromSeed(replaced); err != nil {
			t.Errorf("%s: the replacement is not an identity: %v", c.name, err)
			continue
		}
		if bytes.Equal(replaced, previous) {
			t.Errorf("%s: a corrupt key file was read as the previous identity", c.name)
		}
		// and the replacement is what the next launch reads
		stored, err := localState.GetOrCreateExtenderKeySeed()
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !bytes.Equal(stored, replaced) {
			t.Errorf("%s: the replacement was not persisted", c.name)
		}
		previous = replaced
	}
}

// The legacy single extender overrides discovery outright, so a settings
// change that sets or clears it has to reach the strategy: a cleared one that
// stayed behind would keep every dial on an address the user removed (K6).
func TestNetExtenderChangeReachesTheClientStrategy(t *testing.T) {
	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); 0 < len(customExtenders) {
		t.Fatalf("custom extenders = %v, expected none", customExtenders)
	}

	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.NetExtender = &NetExtender{Ip: "192.0.2.1", Secret: "secret"}
	})
	customExtenders := networkSpace.clientStrategy.CustomExtenders()
	if len(customExtenders) != 1 ||
		customExtenders[netip.MustParseAddr("192.0.2.1")] != "secret" {
		t.Fatalf("custom extenders = %v, expected the configured one", customExtenders)
	}

	// a moved address replaces it rather than adding to it
	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.NetExtender = &NetExtender{Ip: "198.51.100.7", Secret: "other"}
	})
	customExtenders = networkSpace.clientStrategy.CustomExtenders()
	if len(customExtenders) != 1 ||
		customExtenders[netip.MustParseAddr("198.51.100.7")] != "other" {
		t.Fatalf("custom extenders = %v, expected the replacement alone", customExtenders)
	}

	// an address that does not parse configures nothing rather than the last
	// one that did
	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.NetExtender = &NetExtender{Ip: "not an address", Secret: "other"}
	})
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); 0 < len(customExtenders) {
		t.Fatalf("custom extenders = %v, expected none for an unparseable address", customExtenders)
	}

	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.NetExtender = &NetExtender{Ip: "192.0.2.1", Secret: "secret"}
	})
	networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.NetExtender = nil
	})
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); 0 < len(customExtenders) {
		t.Fatalf("custom extenders = %v, expected the cleared extender to be gone", customExtenders)
	}
}
