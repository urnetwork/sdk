package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// Native interfaces and the provider packetizer use the same inner mtu;
// carrier framing must not lower either side of the SDK contract.
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
		t.Fatalf("SDK_TUNNEL_MTU_NOT_1280: got=%d", got)
	}
	if int32(connect.DefaultMtu) != got {
		t.Fatalf("SDK_PACKET_MTU_NOT_1280: packetizer=%d interface=%d", connect.DefaultMtu, got)
	}
}
