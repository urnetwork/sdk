package sdk

// TunnelDnsSetting is the DNS configuration the platform applies to the TUN
// interface (Android `addDnsServer`, Apple `NEDNSSettings`). The device exposes it
// so the platform does not hardcode DNS.
//
// IMPORTANT: the platform must apply PLAIN DNS (UDP/TCP :53) only and must NOT
// enable OS-level encrypted DNS (DoH/DoT) for the tunnel. The UpgradeMux claims
// port 53 and upgrades plaintext DNS to DoH itself; an OS-level encrypted resolver
// (:443/:853) would bypass the mux and hide queries from it. The platforms
// therefore apply the default plain-DNS resolver list (see DefaultTunnelDnsSetting
// and defaultTunnelDnsServersIpv4).
type TunnelDnsSetting struct {
	// Doh selects DNS-over-HTTPS (true) vs. plain DNS on :53 (false). Retained for
	// the binding surface, but the platforms no longer enable OS-level DoH for the
	// tunnel (the mux performs the plaintext->DoH upgrade in-tunnel), so this is
	// inert.
	Doh bool
	// Server, when non-empty, is a single explicit resolver IP override, e.g.
	// "1.1.1.1". Empty (the default) means the platform applies the default
	// plain-DNS resolver list (defaultTunnelDnsServersIpv4).
	Server string
	// DohUrl is the DoH endpoint that was used when Doh is true, e.g.
	// "https://1.1.1.1/dns-query". Inert; see Doh.
	DohUrl string
}

// defaultTunnelDnsServersIpv4 are the plain-DNS (:53) resolver IPs the platform
// applies to the TUN by default. Order is deliberate: Quad9 (9.9.9.9) leads so the
// OS does not opportunistically upgrade the tunnel resolver to encrypted DNS.
// Android auto-upgrades well-known public resolvers (notably Cloudflare 1.1.1.1 and
// Google 8.8.8.8) to DoT/DoH; once encrypted, queries leave on :443/:853 and the
// UpgradeMux can no longer see them. Leading with a resolver the OS does not
// auto-upgrade keeps the whole set on plain :53, so the mux can intercept and run
// our own DoH upgrade. 1.1.1.1 follows as a fast, reliable secondary.
var defaultTunnelDnsServersIpv4 = []string{"9.9.9.9", "1.1.1.1"}

// defaultTunnelDnsServersIpv6 is the IPv6 counterpart. There is no default IPv6
// tunnel resolver, so the platform applies none unless one is configured.
var defaultTunnelDnsServersIpv6 = []string{}

// defaultTunnelDnsServers returns the default plain-DNS resolver IPs for one
// address family.
func defaultTunnelDnsServers(ipv6 bool) []string {
	if ipv6 {
		return defaultTunnelDnsServersIpv6
	}
	return defaultTunnelDnsServersIpv4
}

// DefaultTunnelDnsSetting is the DNS config applied to the TUN by default: plain
// DNS (no OS-level encryption) with no single-server override, so the platform
// applies the full default resolver list (see defaultTunnelDnsServersIpv4).
func DefaultTunnelDnsSetting() *TunnelDnsSetting {
	return &TunnelDnsSetting{
		Doh: false,
	}
}
