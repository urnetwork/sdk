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
// The table ships EMPTY. Filling it is an operations decision: the keys are
// generated with the vault's `extender.yml` (`root_public_keys_hex`), and one
// entry per shipped host is added here when they are. Until then a space with
// no configured `ExtenderRootPublicKeys` accepts no record until its first
// hello, which is the intended fail-closed behavior.

// Hex ed25519 public keys, by space host. Operations fills this in.
var bundledExtenderRootPublicKeyHexes = map[string][]string{}

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
