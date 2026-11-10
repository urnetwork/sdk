package sdk

import (
	"crypto/ed25519"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The bundled root key table (EXTENDER.md B4, F1).
//
// The table is the trust anchor a client has before it has ever reached the
// operator, so what is asserted here is that the shipped hosts actually carry a
// key, that the key is a real ed25519 public key rather than a hex string that
// only looks like one, and that a host this binary does not ship stays empty. A
// typo in the table is otherwise invisible: every record would simply fail to
// verify until the first hello replaced the anchor.

// The host this binary ships resolves to the operator's root key, and a host it
// does not ship resolves to none, the user root included, since no platform
// space resolves under it (sidecar DESIGN §12.4).
func TestBundledExtenderRootPublicKeys(t *testing.T) {
	for _, hostName := range []string{"bringyour.com"} {
		rootPublicKeyHexes := bundledExtenderRootPublicKeys(hostName)
		if !slices.Equal(rootPublicKeyHexes, []string{urnetworkExtenderRootPublicKeyHex}) {
			t.Errorf("%s bundles %v, want the operator root key", hostName, rootPublicKeyHexes)
		}
		// the lookup takes the host as a space carries it, which may be upper
		// case or fully qualified
		qualified := bundledExtenderRootPublicKeys(strings.ToUpper(hostName) + ".")
		if !slices.Equal(qualified, rootPublicKeyHexes) {
			t.Errorf("%s bundles %v qualified, want %v", hostName, qualified, rootPublicKeyHexes)
		}
		// and the space resolves to it when it configures no keys of its own
		spaceRootPublicKeyHexes := ExtenderRootPublicKeys(
			NewNetworkSpaceKey(hostName, "main"),
			&NetworkSpaceValues{},
		)
		if !slices.Equal(spaceRootPublicKeyHexes, rootPublicKeyHexes) {
			t.Errorf("the %s space resolves %v, want %v", hostName, spaceRootPublicKeyHexes, rootPublicKeyHexes)
		}
	}

	for _, hostName := range []string{"space.example", "ur.example", "ur.network", "UR.NETWORK.", ""} {
		if bundled := bundledExtenderRootPublicKeys(hostName); len(bundled) != 0 {
			t.Errorf("%q bundles %v, want none", hostName, bundled)
		}
	}
}

// The apps' bundled space is still keyed by the user root, with every service
// under its migration host: it takes the operator's key through that host. A
// space keyed by the root with no migration host would resolve its services
// under the root, where none lives, so it takes no key and accepts no record
// until its first hello.
func TestBundledSpaceKeyedByTheRootTakesTheMigrationHostKey(t *testing.T) {
	key := NewNetworkSpaceKey("ur.network", "main")
	migrated := ExtenderRootPublicKeys(key, &NetworkSpaceValues{MigrationHostName: "bringyour.com"})
	if !slices.Equal(migrated, []string{urnetworkExtenderRootPublicKeyHex}) {
		t.Errorf("the migrated space resolves %v, want the operator root key", migrated)
	}
	if unmigrated := ExtenderRootPublicKeys(key, &NetworkSpaceValues{}); len(unmigrated) != 0 {
		t.Errorf("a space under the root resolves %v, want none", unmigrated)
	}
}

// The bundled key parses as the ed25519 public key a record is verified
// against, through the same parser the directory uses.
func TestBundledExtenderRootPublicKeyParses(t *testing.T) {
	rootPublicKey, err := connect.ParseExtenderPublicKeyHex(urnetworkExtenderRootPublicKeyHex)
	if err != nil {
		t.Fatalf("the bundled root key is not readable: %v", err)
	}
	if len(rootPublicKey) != ed25519.PublicKeySize {
		t.Fatalf("the bundled root key is %d bytes, want %d", len(rootPublicKey), ed25519.PublicKeySize)
	}
}
