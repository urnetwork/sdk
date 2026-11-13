// Diagnostic ACK failure fields remain opt-in, bounded and distinct from live totals.
package sdk

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Provider copies and newly generated window settings inherit only their device's opt-in.
func TestTransferDiagAckRouteFailuresOptIn(t *testing.T) {
	previousLog := transferDiagLogSeconds
	transferDiagLogSeconds = ""
	defer func() { transferDiagLogSeconds = previousLog }()
	previousSnapshots := SetTransferDiagnosticSnapshotsEnabled(false)
	defer SetTransferDiagnosticSnapshotsEnabled(previousSnapshots)
	disabled := connect.DefaultClientSettings()
	if allocations := testing.AllocsPerRun(100, func() {
		if prepareTransferDiag(disabled) != nil || disabled.EnableAckRouteWriteDiagnostics {
			t.Fatal("disabled diagnostics installed ACK failure counters")
		}
	}); allocations != 0 {
		t.Fatalf("disabled preparation allocated %g objects", allocations)
	}
	SetTransferDiagnosticSnapshotsEnabled(true)
	enabled := connect.DefaultClientSettings()
	stats := prepareTransferDiag(enabled)
	provider := newDeviceClientSettings(enabled, "", nil)
	window := connect.DefaultClientSettings()
	device := &DeviceLocal{transferDiagStats: stats}
	device.attachTransferDiag(window)
	for _, settings := range []*connect.ClientSettings{enabled, provider, window} {
		if !settings.EnableAckRouteWriteDiagnostics {
			t.Fatal("device/provider/window construction omitted diagnostic opt-in")
		}
	}
	SetTransferDiagnosticSnapshotsEnabled(false)
	newDisabled := connect.DefaultClientSettings()
	if prepareTransferDiag(newDisabled) != nil || newDisabled.EnableAckRouteWriteDiagnostics || disabled.EnableAckRouteWriteDiagnostics || !enabled.EnableAckRouteWriteDiagnostics {
		t.Fatal("later opt-out changed another device's captured settings")
	}
	if other := prepareTransferDiag(connect.DefaultClientSettings()); other != nil {
		t.Fatal("independent disabled device inherited diagnostic state")
	}
}

// The production JSON structs expose lifetime evidence without repurposing old live fields.
func TestTransferDiagAckRouteFailureJsonContract(t *testing.T) {
	quality := &connect.AckRouteWriteFailureStatsSnapshot{
		ErrorCount: 5, RouteTimeoutCount: 1, CallerCanceledCount: 1, CallerDeadlineCount: 1,
		SelectorClosedCount: 1, OtherCount: 1, BlockedCount: 2, WaitDuration: 9 * time.Second, MaxWait: 7 * time.Second,
	}
	speed := &connect.AckRouteWriteFailureStatsSnapshot{ErrorCount: 1, SelectorClosedCount: 1}
	state := decodeCarrierDiagnostic(t, transferDiagState{
		Part: "state", UnixMillis: 123,
		QualityAckRouteWriteLifetimeFailures: quality,
		SpeedAckRouteWriteLifetimeFailures:   speed,
	})
	for _, c := range []struct {
		key  string
		want *connect.AckRouteWriteFailureStatsSnapshot
	}{
		{key: "quality_ack_route_write_lifetime_failures", want: quality},
		{key: "speed_ack_route_write_lifetime_failures", want: speed},
	} {
		got, ok := state[c.key].(map[string]any)
		if !ok || !reflect.DeepEqual(got, decodeCarrierDiagnostic(t, c.want)) {
			t.Fatalf("%s did not retain its distinct owner evidence: %+v", c.key, got)
		}
	}
	row := decodeCarrierDiagnostic(t, transferDiagReceive{
		Part: "window_receive", UnixMillis: 123, Window: "quality",
		Receive: connect.ClientReceiveStatsSnapshot{
			AckRouteWriteErrorCount:         2,
			AckRouteWriteTimeoutByTransport: map[connect.TransportType]uint64{connect.TransportTypeH1: 1},
			AckRouteWriteFailures:           quality,
		},
	})
	receive := row["receive"].(map[string]any)
	if receive["AckRouteWriteErrorCount"] != float64(2) || receive["AckRouteWriteFailures"].(map[string]any)["ErrorCount"] != float64(5) {
		t.Fatal("lifetime/failure evidence replaced the serialized live total")
	}
	emptyState := decodeCarrierDiagnostic(t, transferDiagState{Part: "state"})
	emptyReceive := decodeCarrierDiagnostic(t, transferDiagReceive{Part: "window_receive"})["receive"].(map[string]any)
	if emptyState["quality_ack_route_write_lifetime_failures"] != nil || emptyState["speed_ack_route_write_lifetime_failures"] != nil || emptyReceive["AckRouteWriteFailures"] != nil {
		t.Fatal("disabled/empty counters fabricated new JSON evidence")
	}
}

// Fill fixed counters to their longest decimal representation without arbitrary payloads.
func fillAckFailureDiagnosticNumbers(value reflect.Value) {
	switch value.Kind() {
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			fillAckFailureDiagnosticNumbers(value.Field(i))
		}
	case reflect.Array:
		for i := 0; i < value.Len(); i++ {
			fillAckFailureDiagnosticNumbers(value.Index(i))
		}
	case reflect.Uint, reflect.Uint64, reflect.Uint32:
		if value.CanSet() {
			value.SetUint(math.MaxUint64 >> (64 - value.Type().Bits()))
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		if value.CanSet() {
			value.SetInt(math.MaxInt64 >> (64 - value.Type().Bits()))
		}
	}
}

// Existing logcat parts retain prefix headroom even with maximum-width new counters.
func TestTransferDiagAckRouteFailureRowsStayBounded(t *testing.T) {
	failure := &connect.AckRouteWriteFailureStatsSnapshot{}
	fillAckFailureDiagnosticNumbers(reflect.ValueOf(failure).Elem())
	receive := connect.ClientReceiveStatsSnapshot{AckRouteWriteFailures: failure}
	fillAckFailureDiagnosticNumbers(reflect.ValueOf(&receive).Elem())
	receive.AckRouteWriteCountByTransport = map[connect.TransportType]uint64{}
	receive.AckRouteWriteWaitByTransport = map[connect.TransportType]time.Duration{}
	receive.AckRouteWriteTimeoutByTransport = map[connect.TransportType]uint64{}
	for _, transport := range []connect.TransportType{
		connect.TransportTypeUnknown, connect.TransportTypeH1, connect.TransportTypeH3,
		connect.TransportTypeH3Dns, connect.TransportTypeH3DnsPump, connect.TransportTypeP2p,
	} {
		receive.AckRouteWriteCountByTransport[transport] = math.MaxUint64
		receive.AckRouteWriteWaitByTransport[transport] = time.Duration(math.MaxInt64)
		receive.AckRouteWriteTimeoutByTransport[transport] = math.MaxUint64
	}
	state := transferDiagState{
		Part: "state", ClientID: connect.NewId().String(), LocationClientID: connect.NewId().String(),
		LocationName:                         "synthetic diagnostic location",
		QualityAckRouteWriteLifetimeFailures: failure, SpeedAckRouteWriteLifetimeFailures: failure,
	}
	fillAckFailureDiagnosticNumbers(reflect.ValueOf(&state).Elem())
	for _, row := range []any{
		state,
		transferDiagReceive{Part: "window_receive", UnixMillis: math.MaxInt64, Window: "quality", Destination: connect.NewId().String(), Receive: receive},
	} {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > 3500 {
			t.Fatalf("%T diagnostic row is %d bytes, exceeds existing 3500-byte prefix-headroom bound", row, len(encoded))
		}
	}
}
