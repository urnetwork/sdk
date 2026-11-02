package sdk

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// extender_seed_mirrors.go — where a network space fetches its seed bundle
// (connect EXTENDER-UPDATE2.md §2.3).
//
// A mirror is an https server that serves the network's current artifact at
// one fixed path, `/v1/seed.bin`, as the raw serialized envelope. It is
// contacted directly, never through an extender, since the pass runs exactly
// when no extender is reachable yet. A mirror is a distribution point and
// nothing more: what it serves is verified under the root keys the space
// already holds, so it can withhold or replay an artifact but never forge one,
// and it carries no root key, network host, gossip url or resolver setting.
//
// A mirror entry may name an address to dial in place of resolving its host,
// which is the route that needs no dns at all: the connection goes to the
// configured address, and the certificate is still verified for the mirror's
// own host, the name in its url, which is also the sni. An entry without an
// address is resolved by the space's own dialer -- the system resolver, or the
// resolver an embedder configured -- so where dns is what failed, only the
// entries with an address can answer. The seed pass changes no resolver
// setting of the user's. One mirror reachable at several addresses is several
// entries with one url.
//
// The certificate is verified against the roots of the space's tls settings,
// the same roots the control route is verified against: by default connect's
// pinned Let's Encrypt set, plus any roots the embedder added. A mirror must
// present a chain to one of them.
//
// The mirrors of a space are a setting of the space, `ExtenderSeedMirrors`,
// set by whoever owns its values: the bundled space of a build or the app.
// A space that configures none takes the bundled table below for the host its
// extender network is keyed by, as its root keys do. The table is empty until
// the operator's mirrors exist; which hosts it names, and whether the api
// origin's `/v1/seed.bin` behind the load balancer addresses is among them, is
// an owner decision. A url-only space reads it by the host derived from its
// api url, like its root keys. The setting is not an extender value a user
// learned, so "Reset extenders" keeps it, as it keeps the bootstrap DoH
// servers. A space a cloud host shares among its hosted devices runs no seed
// pass at all.

// The path every mirror serves the artifact at. A later envelope version gets
// a path of its own.
const extenderSeedMirrorPath = "/v1/seed.bin"

// Mirror entries one space fetches from in a pass. A pass shares one budget
// across its mirrors, so more of them only divides it further.
const extenderSeedMaxMirrorCount = 8

// One mirror of a space's seed bundle as its values configure it.
type ExtenderSeedMirrorValue struct {
	// The https url of the mirror: a dns host, whose name the certificate is
	// verified for, and a path. A url with no path is the fixed artifact
	// path, `/v1/seed.bin`.
	Url string `json:"url,omitempty"`
	// An ip literal, optionally with a port, to dial in place of resolving
	// the url's host: the dns-independent route. Without a port the url's
	// port is used, 443 by default. Empty resolves the host.
	Address string `json:"address,omitempty"`
}

// The bundled mirrors, by the normalized host a space's extender network is
// keyed by. Empty: the operator's mirrors are not deployed yet (an owner
// decision and an operations step).
var bundledExtenderSeedMirrorValues = map[string][]ExtenderSeedMirrorValue{}

// The configured mirrors of one space, trimmed and without blank entries, else
// the bundled ones for the host its extender network is keyed by. A copy.
func extenderSeedMirrorValues(key *NetworkSpaceKey, values *NetworkSpaceValues) []ExtenderSeedMirrorValue {
	mirrorValues := []ExtenderSeedMirrorValue{}
	for _, mirrorValue := range values.ExtenderSeedMirrors {
		trimmed := ExtenderSeedMirrorValue{
			Url:     strings.TrimSpace(mirrorValue.Url),
			Address: strings.TrimSpace(mirrorValue.Address),
		}
		if trimmed.Url == "" && trimmed.Address == "" {
			continue
		}
		mirrorValues = append(mirrorValues, trimmed)
	}
	if 0 < len(mirrorValues) {
		return mirrorValues
	}
	networkHost := normalExtenderSeedNetworkHost(extenderNetworkHostName(key, values))
	return append(mirrorValues, bundledExtenderSeedMirrorValues[networkHost]...)
}

// Whether two spaces' values configure different mirrors, judged by the
// effective entries, so whitespace or a blank entry changes nothing.
func extenderSeedMirrorValuesChanged(key *NetworkSpaceKey, previous *NetworkSpaceValues, next *NetworkSpaceValues) bool {
	return !slices.Equal(extenderSeedMirrorValues(key, previous), extenderSeedMirrorValues(key, next))
}

// One mirror entry as a pass dials it.
type extenderSeedMirror struct {
	// the artifact url, https, with the fixed path when the config named none
	url string
	// the host the certificate is verified for, and the sni
	serverName string
	// what to dial in place of resolving serverName; invalid resolves it
	address netip.AddrPort
}

// The mirrors a space's values configure, valid ones only, without repeats,
// at most extenderSeedMaxMirrorCount. An entry that does not parse is left out
// rather than dialed.
func spaceExtenderSeedMirrors(key *NetworkSpaceKey, values *NetworkSpaceValues) []*extenderSeedMirror {
	mirrors := []*extenderSeedMirror{}
	for _, mirrorValue := range extenderSeedMirrorValues(key, values) {
		if extenderSeedMaxMirrorCount <= len(mirrors) {
			break
		}
		mirror, err := parseExtenderSeedMirror(mirrorValue)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(mirrors, mirror.equal) {
			continue
		}
		mirrors = append(mirrors, mirror)
	}
	return mirrors
}

// Reads one mirror entry. The url must be https with a dns host and nothing
// but a path: the host is what the certificate is verified for, so an ip
// literal there would verify nothing the address does not already say, and
// user info, a query or a fragment have no meaning for a fixed artifact. The
// address is an ip literal, optionally with a port; without one it takes the
// url's port, 443 by default.
func parseExtenderSeedMirror(mirrorValue ExtenderSeedMirrorValue) (*extenderSeedMirror, error) {
	parsedUrl, err := url.Parse(strings.TrimSpace(mirrorValue.Url))
	if err != nil {
		return nil, err
	}
	if parsedUrl.Scheme != "https" {
		return nil, fmt.Errorf("an extender seed mirror is https")
	}
	if parsedUrl.User != nil || parsedUrl.RawQuery != "" || parsedUrl.Fragment != "" || parsedUrl.Opaque != "" {
		return nil, fmt.Errorf("an extender seed mirror url is a host and a path")
	}
	serverName := strings.ToLower(strings.TrimSuffix(parsedUrl.Hostname(), "."))
	if !isDottedHostName(serverName) {
		return nil, fmt.Errorf("an extender seed mirror names a dns host")
	}
	port := 443
	if portText := parsedUrl.Port(); portText != "" {
		port, err = strconv.Atoi(portText)
		if err != nil || port <= 0 || 65535 < port {
			return nil, fmt.Errorf("an extender seed mirror port is not a port")
		}
	}
	path := parsedUrl.EscapedPath()
	if path == "" || path == "/" {
		path = extenderSeedMirrorPath
	}
	host := serverName
	if parsedUrl.Port() != "" {
		host = net.JoinHostPort(serverName, strconv.Itoa(port))
	}
	mirror := &extenderSeedMirror{
		// the path is already escaped, so it is joined as it stands
		url:        fmt.Sprintf("https://%s%s", host, path),
		serverName: serverName,
	}
	if addressText := strings.TrimSpace(mirrorValue.Address); addressText != "" {
		address, err := parseExtenderSeedMirrorAddress(addressText, port)
		if err != nil {
			return nil, err
		}
		mirror.address = address
	}
	return mirror, nil
}

// One configured address: an ip literal, with or without a port, v6 in
// brackets when it has one. The unspecified, multicast and link-local
// addresses are refused; nothing serves a mirror there.
func parseExtenderSeedMirrorAddress(addressText string, defaultPort int) (netip.AddrPort, error) {
	var address netip.AddrPort
	if addrPort, err := netip.ParseAddrPort(addressText); err == nil {
		address = addrPort
	} else if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(addressText, "["), "]")); err == nil {
		address = netip.AddrPortFrom(addr, uint16(defaultPort))
	} else {
		return netip.AddrPort{}, fmt.Errorf("an extender seed mirror address is an ip literal")
	}
	addr := address.Addr().Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.Zone() != "" {
		return netip.AddrPort{}, fmt.Errorf("an extender seed mirror address is not dialable")
	}
	if address.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("an extender seed mirror address names port 0")
	}
	return netip.AddrPortFrom(addr, address.Port()), nil
}

func (self *extenderSeedMirror) equal(other *extenderSeedMirror) bool {
	return self.url == other.url && self.address == other.address
}

// Whether the entry dials without resolving its host, the route §2.3 asks at
// least one mirror of a deployment to have.
func (self *extenderSeedMirror) dnsIndependent() bool {
	return self.address.IsValid()
}

// The address in the form connect's mirror takes it, empty to resolve.
func (self *extenderSeedMirror) addressText() string {
	if !self.address.IsValid() {
		return ""
	}
	return self.address.String()
}
