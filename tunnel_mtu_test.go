package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// The tunnel mtu the apps configure is connect's interface mtu (1280, so the
// interface can carry IPv6), and the packetizer contract stays strictly
// inside it so a full packet written into the tunnel always fits
// (connect/IPV6.md C1).
func TestDefaultTunnelMtuIsTheInterfaceMtu(t *testing.T) {
	got := GetDefaultTunnelMtu()
	if got != int32(connect.DefaultTunnelMtu) {
		t.Fatalf(
			"SDK tunnel MTU=%d does not match connect interface MTU=%d",
			got,
			connect.DefaultTunnelMtu,
		)
	}
	if got != 1280 {
		t.Fatalf("SDK tunnel MTU=%d want=1280 (the IPv6 minimum link mtu)", got)
	}
	if int32(connect.DefaultMtu) > got {
		t.Fatalf("packetizer MTU=%d exceeds the interface MTU=%d", connect.DefaultMtu, got)
	}
}
