package sdk

import (
	"net/netip"
	"testing"
)

// Every drawn address sits inside the fixed ULA /48, never uses the zero
// interface id, and differs across draws.
func TestRandomTunnelLocalIpv6(t *testing.T) {
	seen := map[netip.Addr]bool{}
	for range 64 {
		addr := randomTunnelLocalIpv6()
		if !addr.Is6() || addr.Is4In6() {
			t.Fatalf("tunnel ipv6 address %v is not a native v6 address", addr)
		}
		if !tunnelLocalIpv6Prefix.Contains(addr) {
			t.Fatalf("tunnel ipv6 address %v is outside %v", addr, tunnelLocalIpv6Prefix)
		}
		if !addr.IsPrivate() {
			t.Fatalf("tunnel ipv6 address %v is not a ULA", addr)
		}
		b := addr.As16()
		zeroInterface := true
		for _, x := range b[8:16] {
			if x != 0 {
				zeroInterface = false
				break
			}
		}
		if zeroInterface {
			t.Fatalf("tunnel ipv6 address %v has the subnet-router anycast interface id", addr)
		}
		seen[addr] = true
	}
	if len(seen) < 2 {
		t.Fatalf("tunnel ipv6 addresses are not randomized")
	}
	if GetTunnelLocalPrefixLengthIpv6() != 64 {
		t.Fatalf("prefix length = %d, want 64", GetTunnelLocalPrefixLengthIpv6())
	}
}
