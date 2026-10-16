package sdk

// tunnel_address_ipv6.go — the IPv6 address the platform assigns to the TUN
// interface (connect/IPV6.md C2). The apps pair it with a ::/0 route and the
// link-local, ULA, multicast and loopback exclusions.
//
// A ULA (fc00::/7) is used for the same reason the v4 address is RFC1918: it
// is private, so nothing on the internet can be confused with it, and
// libwebrtc classifies it as private so the browser's mDNS obfuscation masks
// it in peer discovery. The interface id is randomized per device like the
// v4 host so the address is not a fingerprint. The apps exclude fc00::/7 from
// the tunnel, which keeps a home network's own ULA traffic local; the
// interface's own /64 carries nothing but this host, so that exclusion costs
// nothing.

import (
	"crypto/rand"
	"encoding/binary"
	"net/netip"
)

// tunnelLocalIpv6Prefix is the fixed /48 every device draws its tunnel
// address from. The hex spells "urne" (urnetwork) so the range is
// recognizable in a route table.
var tunnelLocalIpv6Prefix = netip.MustParsePrefix("fd00:7572:6e65::/48")

// TunnelLocalPrefixLengthIpv6 is the prefix length the platform assigns with
// the tunnel's IPv6 address.
const TunnelLocalPrefixLengthIpv6 = 64

// GetTunnelLocalPrefixLengthIpv6 is the gomobile-friendly form of
// TunnelLocalPrefixLengthIpv6.
func GetTunnelLocalPrefixLengthIpv6() int {
	return TunnelLocalPrefixLengthIpv6
}

// randomTunnelLocalIpv6 draws a random subnet id and a random non-zero
// interface id inside the fixed /48.
func randomTunnelLocalIpv6() netip.Addr {
	var randomBytes [10]byte
	for {
		if _, err := rand.Read(randomBytes[:]); err != nil {
			panic(err)
		}
		// bytes 8..15 are the interface id; zero is the subnet-router anycast
		// address and must not be assigned to a host
		if binary.BigEndian.Uint64(randomBytes[2:10]) != 0 {
			break
		}
	}
	addr := tunnelLocalIpv6Prefix.Addr().As16()
	copy(addr[6:16], randomBytes[:])
	return netip.AddrFrom16(addr)
}

// TunnelLocalAddressIpv6 returns the IPv6 address the platform assigns to the
// TUN interface, drawn once per device from the fixed ULA /48.
func (self *DeviceLocal) TunnelLocalAddressIpv6() string {
	return self.tunnelLocalAddressIpv6.String()
}
