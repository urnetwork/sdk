package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestDefaultTunnelMtuMatchesConnectPacketizer(t *testing.T) {
	got := GetDefaultTunnelMtu()
	if got != int32(connect.DefaultMtu) {
		t.Fatalf(
			"SDK tunnel MTU=%d does not match connect MTU=%d",
			got,
			connect.DefaultMtu,
		)
	}
	if got != 1100 {
		t.Fatalf("SDK tunnel MTU=%d want=1100", got)
	}
}
