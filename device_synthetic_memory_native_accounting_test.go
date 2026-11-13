// Compact native refusals use the same causal accounting as the memory soak.

//go:build darwin || linux || android || ios

package sdk

import "testing"

// An actual compact native offer can refuse an unavailable window without a
// security block or redundant TCP state. The source keeps retry ownership.
func TestSyntheticPacketRejectionsAccountForNativeUnavailableRoute(t *testing.T) {
	f := newNativeIngressDeviceFixture(t)
	packet, _ := nativeIngressDevicePackets(t)
	accepted := f.device.SendPacketBatch(packetBatchTestEncode([][]byte{packet}))
	if accepted != 0 {
		t.Fatalf("unavailable native window accepted %d packets", accepted)
	}
	stats := f.remote.PacketStats()
	if stats.BlockEgressPacketCount != 0 || f.remote.TcpCollapseDropCount() != 0 {
		t.Fatal("actual native refusal was attributed to policy or TCP collapse")
	}
	if count := f.remote.NativeAdmissionRefusalCount(); count != 1 {
		t.Fatalf("native refusal count=%d, want 1", count)
	}
	if err := syntheticPacketRejectionError(1, stats.BlockEgressPacketCount, int64(f.remote.TcpCollapseDropCount()), int64(f.remote.NativeAdmissionRefusalCount())); err != nil {
		t.Fatalf("native-ready-only refusal has an actual source recovery owner: %v", err)
	}
}
