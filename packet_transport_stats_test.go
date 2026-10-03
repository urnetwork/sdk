package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

func testConnectTransportPacketStats() *connect.PacketStats {
	return &connect.PacketStats{
		RemoteEgressPacketCount:  3,
		RemoteEgressByteCount:    300,
		RemoteIngressPacketCount: 4,
		RemoteIngressByteCount:   400,
		TransportStats: map[connect.TransportType]*connect.PacketStats{
			connect.TransportTypeH3: {
				RemoteEgressPacketCount:  2,
				RemoteEgressByteCount:    200,
				RemoteIngressPacketCount: 1,
				RemoteIngressByteCount:   100,
			},
			connect.TransportTypeH1: {
				RemoteEgressPacketCount:  1,
				RemoteEgressByteCount:    100,
				RemoteIngressPacketCount: 3,
				RemoteIngressByteCount:   300,
			},
		},
	}
}

func assertSdkTransportStatsReconcile(t *testing.T, stats *PacketStats) {
	t.Helper()
	if stats == nil || stats.TransportStats == nil {
		t.Fatal("missing transport stats")
	}
	var egressPackets int64
	var egressBytes ByteCount
	var ingressPackets int64
	var ingressBytes ByteCount
	for _, transportStats := range stats.TransportStats.getAll() {
		if transportStats == nil || transportStats.Stats == nil {
			t.Fatal("nil transport stats row")
		}
		if transportStats.Stats.TransportStats != nil {
			t.Fatal("transport row recursively contains transport stats")
		}
		egressPackets += transportStats.Stats.RemoteEgressPacketCount
		egressBytes += transportStats.Stats.RemoteEgressByteCount
		ingressPackets += transportStats.Stats.RemoteIngressPacketCount
		ingressBytes += transportStats.Stats.RemoteIngressByteCount
	}
	if egressPackets != stats.RemoteEgressPacketCount ||
		egressBytes != stats.RemoteEgressByteCount ||
		ingressPackets != stats.RemoteIngressPacketCount ||
		ingressBytes != stats.RemoteIngressByteCount {
		t.Fatalf("transport sums do not reconcile with aggregate: %+v", stats)
	}
}

func TestPacketStatsTransportMappingIsStableAndReconciles(t *testing.T) {
	stats := packetStatsFromConnect(testConnectTransportPacketStats())
	wantTypes := []TransportType{
		TransportTypeH3,
		TransportTypeH1,
		TransportTypeDns,
		TransportTypeDnsPump,
		TransportTypeP2p,
		TransportTypeUnknown,
	}
	if stats.TransportStats.Len() != len(wantTypes) {
		t.Fatalf("transport row count = %d, want %d", stats.TransportStats.Len(), len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if got := stats.TransportStats.Get(index).TransportType; got != wantType {
			t.Fatalf("transport type[%d] = %q, want %q", index, got, wantType)
		}
	}
	assertSdkTransportStatsReconcile(t, stats)
}

func TestPacketStatsTransportRpcRoundTrip(t *testing.T) {
	stats := packetStatsFromConnect(testConnectTransportPacketStats())
	wired := gobRoundTrip(t, &DeviceRemotePacketStats{
		PacketStats: newPacketStatsRpc(stats, true),
	})
	got := wired.PacketStats.toPacketStats(true)
	if got.TransportStats.Len() != stats.TransportStats.Len() {
		t.Fatalf("rpc transport row count = %d, want %d", got.TransportStats.Len(), stats.TransportStats.Len())
	}
	for index := 0; index < stats.TransportStats.Len(); index += 1 {
		gotRow := got.TransportStats.Get(index)
		wantRow := stats.TransportStats.Get(index)
		if gotRow.TransportType != wantRow.TransportType || *gotRow.Stats != *wantRow.Stats {
			t.Fatalf("rpc row[%d] = %+v, want %+v", index, gotRow, wantRow)
		}
	}
	assertSdkTransportStatsReconcile(t, got)
}

func TestAddConnectPacketStatsDoesNotAliasBaseTransportMap(t *testing.T) {
	base := testConnectTransportPacketStats()
	baseH3 := *base.TransportStats[connect.TransportTypeH3]
	combined := *base
	addConnectPacketStats(&combined, &connect.PacketStats{
		RemoteEgressPacketCount: 1,
		RemoteEgressByteCount:   50,
		TransportStats: map[connect.TransportType]*connect.PacketStats{
			connect.TransportTypeH3: {
				RemoteEgressPacketCount: 1,
				RemoteEgressByteCount:   50,
			},
		},
	})
	if base.TransportStats[connect.TransportTypeH3].RemoteEgressPacketCount != baseH3.RemoteEgressPacketCount ||
		base.TransportStats[connect.TransportTypeH3].RemoteEgressByteCount != baseH3.RemoteEgressByteCount {
		t.Fatal("combining a live epoch mutated the persisted base transport stats")
	}
	if combined.TransportStats[connect.TransportTypeH3].RemoteEgressPacketCount != 3 {
		t.Fatalf("combined H3 stats = %+v", combined.TransportStats[connect.TransportTypeH3])
	}
}
