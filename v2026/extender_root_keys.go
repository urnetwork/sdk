package sdk

import "strings"

// extender_root_keys.go — the bundled extender root public keys (EXTENDER.md
// B4, F1).
//
// A client needs a trust anchor before it has ever reached the operator: the
// first records it hears arrive over an untrusted extender, and a record
// signed by a key it does not accept is worthless. The bundled table is that
// anchor, keyed by the space host so one binary can carry the keys of every
// space it ships with.
//
// A hello answer replaces whatever is in force (B4), because hello arrives
// over the platform's pinned TLS even when it travelled through an extender,
// so the bundled table only has to cover the very first contact.
//
// The table carries the operator key of every host this binary ships with. The
// keys are the `root_public_keys_hex` of the operator's vault `extender.yml`,
// and an entry is added or replaced here when a host is added or a key rotated
// -- a rotation ships both keys until the old one is dropped, which is why the
// value is a list. A host the table does not name -- a development operator, or
// someone else's space -- accepts no record until its first hello, which is the
// intended fail-closed behavior.

// The urnetwork operator's root key. Both of its hosts are the one operator
// signing with the one key, so the key is named once rather than repeated.
const urnetworkExtenderRootPublicKeyHex = "ee6519b0df7618cea222631c4fff0c5cafe9bd594d63424162db3bb7a1cb544a"

// Hex ed25519 public keys, by space host.
var bundledExtenderRootPublicKeyHexes = map[string][]string{
	"bringyour.com": {urnetworkExtenderRootPublicKeyHex},
	"ur.network":    {urnetworkExtenderRootPublicKeyHex},
}

// The bundled keys of one host, or none. The lookup is case insensitive and
// ignores a trailing dot, so a host written either way finds its entry.
func bundledExtenderRootPublicKeys(hostName string) []string {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostName), "."))
	if normalized == "" {
		return []string{}
	}
	rootPublicKeys, ok := bundledExtenderRootPublicKeyHexes[normalized]
	if !ok {
		return []string{}
	}
	return append([]string{}, rootPublicKeys...)
}
