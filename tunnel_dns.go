package sdk

import "github.com/urnetwork/connect"

// TunnelDnsSetting is the DNS configuration the platform applies to the TUN
// interface (Android `addDnsServer`, Apple `NEDNSSettings`). The device exposes it
// so the platform does not hardcode DNS.
//
// IMPORTANT: the platform must apply PLAIN DNS (UDP/TCP :53) only and must NOT
// enable OS-level encrypted DNS (DoH/DoT) for the tunnel. The UpgradeMux claims
// port 53 and upgrades plaintext DNS to DoH itself; an OS-level encrypted resolver
// (:443/:853) would bypass the mux and hide queries from it. The platforms
// therefore apply the resolver's dedicated upgrade-mask address. The mask must
// not equal the address assigned to the TUN: kernels classify an interface's own
// address as local delivery and never put those DNS packets on the TUN read side.
type TunnelDnsSetting struct {
	// Doh selects DNS-over-HTTPS (true) vs. plain DNS on :53 (false). Retained for
	// the binding surface, but the platforms no longer enable OS-level DoH for the
	// tunnel (the mux performs the plaintext->DoH upgrade in-tunnel), so this is
	// inert.
	Doh bool
	// Server, when non-empty, is a single explicit resolver IP override, e.g.
	// "1.1.1.1". Empty (the default) means the platform applies the default
	// plain-DNS identity supplied by the owning DeviceLocal.
	Server string
	// DohUrl is the DoH endpoint that was used when Doh is true, e.g.
	// "https://1.1.1.1/dns-query". Inert; see Doh.
	DohUrl string
}

// defaultTunnelDnsServersIpv4 is the context-free fallback for callers that do
// not have live resolver settings. It is not an upstream resolver: UpgradeMux
// claims the query before the destination is reached and performs the actual
// resolution over DoH.
var defaultTunnelDnsServersIpv4 = []string{connect.DefaultDnsUpgradeMaskAddress}

// defaultTunnelDnsServersIpv6 is the IPv6 counterpart. There is no default IPv6
// tunnel resolver, so the platform applies none unless one is configured.
var defaultTunnelDnsServersIpv6 = []string{}

// GetDefaultTunnelDnsAddressIpv4 exposes the context-free fallback to native
// platform bindings.
func GetDefaultTunnelDnsAddressIpv4() string {
	return connect.DefaultDnsUpgradeMaskAddress
}

// defaultTunnelDnsServers returns the default plain-DNS resolver IPs for one
// address family.
func defaultTunnelDnsServers(ipv6 bool) []string {
	if ipv6 {
		return defaultTunnelDnsServersIpv6
	}
	return defaultTunnelDnsServersIpv4
}

// DefaultTunnelDnsSetting is plain DNS (no OS-level encryption) with no
// single-server override, so DeviceLocal advertises DnsUpgradeMaskAddress.
func DefaultTunnelDnsSetting() *TunnelDnsSetting {
	return &TunnelDnsSetting{
		Doh: false,
	}
}
