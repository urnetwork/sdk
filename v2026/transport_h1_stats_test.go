package sdk

import (
	"bytes"
	"encoding/gob"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestH1ConnectionStatsRpcRoundTripAndLegacyDefaults(t *testing.T) {
	stats := packetStatsFromConnect(testConnectTransportPacketStats())
	want := connect.H1ConnectionStatsSnapshot{WebSocketConnectionCount: 2, H1PlusConnectionCount: 3}
	applyH1ConnectionStats(stats, want)
	wired := gobRoundTrip(t, &DeviceRemotePacketStats{PacketStats: newPacketStatsRpc(stats, true)})
	got := wired.PacketStats.toPacketStats(true)
	if connections := h1ConnectionStatsFromPacketStats(got); connections != want {
		t.Fatalf("RPC lost active H1 selection: got %+v want %+v", connections, want)
	}
	assertSdkTransportStatsReconcile(t, got)
	for _, row := range got.TransportStats.getAll() {
		if row.TransportType != TransportTypeH1 && (row.H1PlusConnectionCount != 0 || row.H1WebSocketConnectionCount != 0) {
			t.Fatal("H1 negotiation leaked to another carrier")
		}
	}
	// An old peer's Gob shape omits the additive fields. Absence must mean H1,
	// never an assumed successful H1+ upgrade.
	legacy := struct {
		TransportType string
		Stats         *PacketStatsRpc
	}{TransportTypeH1, &PacketStatsRpc{RemoteEgressByteCount: 42}}
	var wire bytes.Buffer
	if err := gob.NewEncoder(&wire).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	var decoded TransportPacketStatsRpc
	if err := gob.NewDecoder(&wire).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.H1PlusConnectionCount != 0 || decoded.H1WebSocketConnectionCount != 0 || decoded.Stats.RemoteEgressByteCount != 42 {
		t.Fatalf("legacy stats changed: %+v", decoded)
	}
}

func TestH1ConnectionStatsDistributionFollowsLiveSelectionWhileIdle(t *testing.T) {
	vc := &ContractViewController{sampleInterval: time.Second, windowDuration: time.Minute}
	series := &throughputSeries{}
	now := time.Unix(1_000, 0)
	sample := func(webSocket, plus int64) *TransportShare {
		t.Helper()
		stats := packetStatsFromConnect(&connect.PacketStats{})
		applyH1ConnectionStats(stats, connect.H1ConnectionStatsSnapshot{WebSocketConnectionCount: webSocket, H1PlusConnectionCount: plus})
		now = now.Add(time.Second)
		vc.sampleSeriesWithLock(series, stats, now, false)
		distribution := vc.transportDistributionWithLock(series, now, map[TransportType]bool{TransportTypeH1: true})
		if distribution.Active || distribution.ByteCount != 0 || distribution.Shares.Len() != len(transportTypes()) {
			t.Fatal("negotiation changed traffic totals or transport vocabulary")
		}
		for _, share := range distribution.Shares.getAll() {
			if share.TransportType == TransportTypeH1 {
				if share.H1WebSocketConnectionCount != webSocket || share.H1PlusConnectionCount != plus || !share.Enabled {
					t.Fatalf("live selection not mapped to H1 share: %+v", share)
				}
				return share
			}
		}
		t.Fatal("H1 share disappeared")
		return nil
	}
	sample(0, 0)
	throughputSeriesNotifyWithLock(series)
	sample(0, 1)
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("idle successful H1+ negotiation did not update UI")
	}
	sample(0, 1)
	if throughputSeriesNotifyWithLock(series) {
		t.Fatal("unchanged idle connection caused repeated UI updates")
	}
	sample(1, 1) // mixed window still exposes its active H1+ member
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("mixed window was not published")
	}
	sample(1, 0) // H1+ gone, plain fallback remains
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("idle WebSocket fallback left a stale H1+ label")
	}
	sample(0, 0)
	if !throughputSeriesNotifyWithLock(series) {
		t.Fatal("disconnect was not published")
	}
	sample(0, 1)
	throughputSeriesNotifyWithLock(series)
	for i := range 2 {
		now = now.Add(time.Second)
		vc.sampleSeriesWithLock(series, nil, now, false)
		if got := throughputSeriesNotifyWithLock(series); got != (i == 0) {
			t.Fatalf("missing stats notification at poll %d: %v", i, got)
		}
		if got := h1ConnectionStatsFromPacketStats(series.latestPacketStats); got != (connect.H1ConnectionStatsSnapshot{}) {
			t.Fatal("missing stats retained live H1+ activity")
		}
	}
	// Client and provider series read their own snapshots.
	provider := &throughputSeries{latestPacketStats: packetStatsFromConnect(&connect.PacketStats{})}
	applyH1ConnectionStats(provider.latestPacketStats, connect.H1ConnectionStatsSnapshot{H1PlusConnectionCount: 7})
	if h1ConnectionStatsFromPacketStats(series.latestPacketStats).H1PlusConnectionCount != 0 {
		t.Fatal("provider activity leaked into client stats")
	}
}
