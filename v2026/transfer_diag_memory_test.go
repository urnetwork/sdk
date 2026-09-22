package sdk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func carrierDiagnosticSnapshotFixture() connect.PlatformTransportBudgetHierarchyStats {
	pair := connect.PlatformTransportBudgetHandoffStats{
		ID: 7, FromClass: "h1", ToClass: "h3_explicit", H1ByteCount: 256 * 1024,
		ByteCount: 256 * 1024, TransportCount: 1, Owner: "device",
	}
	return connect.PlatformTransportBudgetHierarchyStats{
		Root: connect.PlatformTransportBudgetStats{
			TotalByteCount: 8 * 1024 * 1024, UsedByteCount: 3 * 1024 * 1024,
			MaxTransportCount: 16, UsedTransportCount: 3, PendingH1Count: 2, PendingH1ByteCount: 512 * 1024,
			ReservedByteCount: 6 * 1024 * 1024, ReleasedByteCount: 3 * 1024 * 1024,
			ActiveHandoffCount: 1, ActiveHandoffByteCount: pair.ByteCount, ActiveHandoffTransportCount: pair.TransportCount,
			ActiveHandoffID: pair.ID, ActiveHandoffFromClass: pair.FromClass, ActiveHandoffToClass: pair.ToClass,
			ActiveHandoffH1ByteCount: pair.H1ByteCount,
		},
		Budget: connect.PlatformTransportBudgetStats{
			TotalByteCount: 5 * 1024 * 1024, UsedByteCount: 512 * 1024,
			MaxTransportCount: 16, UsedTransportCount: 2, PendingH1Count: 1, PendingH1ByteCount: 256 * 1024,
			ReservedByteCount: 1024 * 1024, ReleasedByteCount: 512 * 1024,
		},
		RootHandoff: pair, BudgetHandoff: pair,
	}
}

func decodeCarrierDiagnostic(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestTransferDiagCarrierBudgetsPreserveRootAndDeviceEvidence(t *testing.T) {
	snapshot := carrierDiagnosticSnapshotFixture()
	root, child := transferDiagCarrierBudgets(snapshot, 123)
	rootPayload := decodeCarrierDiagnostic(t, transferDiagMemory{
		Part: "memory", UnixMillis: 123, transferDiagProcessCarrierBudget: root,
	})
	childPayload := decodeCarrierDiagnostic(t, child)
	for key, want := range map[string]any{
		"transport_budget_total_bytes":             float64(8 * 1024 * 1024),
		"transport_budget_used_bytes":              float64(3 * 1024 * 1024),
		"transport_budget_reserved_bytes":          float64(6 * 1024 * 1024),
		"transport_budget_released_bytes":          float64(3 * 1024 * 1024),
		"transport_budget_pending_h1":              float64(2),
		"transport_budget_pending_h1_bytes":        float64(512 * 1024),
		"transport_budget_active_handoff_count":    float64(1),
		"transport_budget_active_handoff_id":       float64(7),
		"transport_budget_active_handoff_from":     "h1",
		"transport_budget_active_handoff_to":       "h3_explicit",
		"transport_budget_active_handoff_h1_bytes": float64(256 * 1024),
		"transport_budget_active_handoff_slots":    float64(1),
		"transport_budget_pair_owner":              "device",
	} {
		if got := rootPayload[key]; got != want {
			t.Errorf("root %s = %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]any{
		"part": "memory_device_transport", "unix_millis": float64(123),
		"device_transport_budget_total_bytes":             float64(5 * 1024 * 1024),
		"device_transport_budget_used_bytes":              float64(512 * 1024),
		"device_transport_budget_reserved_bytes":          float64(1024 * 1024),
		"device_transport_budget_released_bytes":          float64(512 * 1024),
		"device_transport_budget_pending_h1":              float64(1),
		"device_transport_budget_pending_h1_bytes":        float64(256 * 1024),
		"device_transport_budget_active_handoff_count":    float64(0),
		"device_transport_budget_active_handoff_id":       float64(0),
		"device_transport_budget_active_handoff_from":     "",
		"device_transport_budget_active_handoff_to":       "",
		"device_transport_budget_active_handoff_h1_bytes": float64(0),
		"device_transport_budget_active_handoff_slots":    float64(0),
		"device_transport_budget_pair_id":                 float64(7),
		"device_transport_budget_pair_from":               "h1",
		"device_transport_budget_pair_to":                 "h3_explicit",
		"device_transport_budget_pair_h1_bytes":           float64(256 * 1024),
		"device_transport_budget_pair_slots":              float64(1),
		"device_transport_budget_pair_owner":              "device",
	} {
		if got := childPayload[key]; got != want {
			t.Errorf("child %s = %v, want %v", key, got, want)
		}
	}
	if rootPayload["unix_millis"] != childPayload["unix_millis"] {
		t.Fatal("atomic budget parts have different timestamps")
	}
}

func TestTransferDiagMemoryPartsStayBelowLogcatLimit(t *testing.T) {
	snapshot := carrierDiagnosticSnapshotFixture()
	snapshot.RootAdditionalHandoff = snapshot.RootHandoff
	snapshot.RootAdditionalHandoff.ID++
	snapshot.BudgetAdditionalHandoff = snapshot.RootAdditionalHandoff
	root, child := transferDiagCarrierBudgets(snapshot, 123)
	for _, part := range []any{
		transferDiagMemory{Part: "memory", UnixMillis: 123, transferDiagProcessCarrierBudget: root},
		child,
		transferDiagTransferBudget(&DeviceLocalMemoryUsage{}, 123),
	} {
		payload := decodeCarrierDiagnostic(t, part)
		// Test large lifetime counters too; logcat must not truncate the child
		// proof once counters grow beyond the short-run fixture values.
		for key, value := range payload {
			if _, ok := value.(float64); ok {
				payload[key] = float64((1 << 53) - 1)
			} else if strings.HasSuffix(key, "_from") || strings.HasSuffix(key, "_to") {
				payload[key] = "h3_explicit"
			} else if strings.HasSuffix(key, "_owner") {
				payload[key] = "other_device"
			}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > 3500 {
			t.Fatalf("memory diagnostic part is %d bytes, leaving too little logcat prefix headroom", len(encoded))
		}
	}
}

func TestTransferDiagCarrierBudgetsPreserveAdditionalPairEvidence(t *testing.T) {
	snapshot := carrierDiagnosticSnapshotFixture()
	second := snapshot.RootHandoff
	second.ID, second.ToClass = 8, "h1"
	snapshot.RootAdditionalHandoff = second
	snapshot.BudgetAdditionalHandoff = snapshot.RootHandoff
	snapshot.BudgetHandoff = second
	root, child := transferDiagCarrierBudgets(snapshot, 123)
	for _, test := range []struct {
		prefix string
		part   any
		want   connect.PlatformTransportBudgetHandoffStats
	}{
		{"transport_budget_", root, snapshot.RootAdditionalHandoff},
		{"device_transport_budget_", child, snapshot.BudgetAdditionalHandoff},
	} {
		payload := decodeCarrierDiagnostic(t, test.part)
		for suffix, want := range map[string]any{
			"id": float64(test.want.ID), "from": test.want.FromClass, "to": test.want.ToClass,
			"h1_bytes": float64(test.want.H1ByteCount), "bytes": float64(test.want.ByteCount),
			"slots": float64(test.want.TransportCount), "owner": test.want.Owner,
		} {
			if value, present := payload[test.prefix+"additional_pair_"+suffix]; !present || value != want {
				t.Errorf("%s additional %s = %v (present %t), want %v", test.prefix, suffix, value, present, want)
			}
		}
	}
}

func TestTransferDiagTransferHierarchyEvidenceAndZeroTeardown(t *testing.T) {
	device, _ := transferMemoryTestDevice(t, 20*1024*1024)
	memory := device.transferMemory
	device.applyProvideMemorySharesWithLock(true)
	const clientBytes, providerBytes, natBytes = 1024, 2048, 4096
	connect.AssertEqual(t, memory.client.TryReserve(clientBytes), true)
	connect.AssertEqual(t, memory.provider.TryReserve(providerBytes), true)
	connect.AssertEqual(t, memory.nat.TryReserve(natBytes), true)
	payload := decodeCarrierDiagnostic(t, transferDiagTransferBudget(device.MemoryUsed(), 123))
	for key, want := range map[string]any{
		"part": "memory_device_transfer", "unix_millis": float64(123),
		"transfer_root_total_bytes":     float64(13 * 1024 * 1024),
		"transfer_root_used_bytes":      float64(clientBytes + providerBytes + natBytes),
		"transfer_root_reserved_bytes":  float64(clientBytes + providerBytes + natBytes),
		"transfer_root_released_bytes":  float64(0),
		"client_transfer_total_bytes":   float64(9 * 1024 * 1024),
		"client_transfer_used_bytes":    float64(clientBytes),
		"provider_transfer_total_bytes": float64(2 * 1024 * 1024),
		"provider_transfer_used_bytes":  float64(providerBytes),
		"nat_budget_total_bytes":        float64(2 * 1024 * 1024),
		"nat_budget_used_bytes":         float64(natBytes),
		"nat_budget_reserved_bytes":     float64(natBytes),
		"nat_budget_released_bytes":     float64(0),
		"pack_queue_total_bytes":        float64(device.settings.ClientSettings.ReceiveBufferSettings.PackQueueBudget.TotalByteCount()),
		"pack_queue_used_bytes":         float64(0),
	} {
		if got, present := payload[key]; !present || got != want {
			t.Errorf("%s = %v (present %t), want %v", key, got, present, want)
		}
	}
	memory.client.Release(clientBytes)
	memory.provider.Release(providerBytes)
	memory.nat.Release(natBytes)
	payload = decodeCarrierDiagnostic(t, transferDiagTransferBudget(device.MemoryUsed(), 124))
	if payload["transfer_root_used_bytes"] != float64(0) || payload["transfer_root_reserved_bytes"] != payload["transfer_root_released_bytes"] ||
		payload["nat_budget_used_bytes"] != float64(0) || payload["nat_budget_reserved_bytes"] != payload["nat_budget_released_bytes"] {
		t.Fatalf("zero teardown evidence missing: %+v", payload)
	}
}

func TestTransferDiagDisabledRemainsInert(t *testing.T) {
	previous := transferDiagLogSeconds
	transferDiagLogSeconds = ""
	defer func() { transferDiagLogSeconds = previous }()
	device := &DeviceLocal{}
	if allocations := testing.AllocsPerRun(100, func() { device.startTransferDiag() }); allocations != 0 {
		t.Fatalf("disabled diagnostics allocated %.2f objects/run", allocations)
	}
	if device.transferDiagStats != nil || transferDiagInterval() != 0 {
		t.Fatal("disabled diagnostics installed runtime state")
	}
}
