package sdk

// TunnelDnsSetting is the DNS configuration the platform applies to the TUN
// interface (Android `addDnsServer`, Apple `NEDNSSettings`/`NEDNSOverHTTPSSettings`).
// The device exposes it so the platform does not hardcode DNS.
//
// IMPORTANT: for the UpgradeMux to intercept and upgrade DNS, the OS resolver must
// be PLAIN DNS (UDP/TCP :53) — the mux claims port 53, so a DoH OS resolver (:443)
// would bypass it. The apps therefore use plain DNS 1.1.1.1.
type TunnelDnsSetting struct {
	// Doh selects DNS-over-HTTPS (true) vs. plain DNS on :53 (false).
	Doh bool
	// Server is the resolver IP, e.g. "1.1.1.1".
	Server string
	// DohUrl is the DoH endpoint used when Doh is true, e.g. "https://1.1.1.1/dns-query".
	DohUrl string
}

func DefaultTunnelDnsSetting() *TunnelDnsSetting {
	return &TunnelDnsSetting{
		Doh:    false,
		Server: "1.1.1.1",
	}
}
