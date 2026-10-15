package sdk

import (
	"testing"
	"time"
)

// The DeviceLocalSettings duration tunables are exposed to gomobile as
// int64-millis accessor pairs (Go keeps time.Duration internally; the millis
// are purely the Sdk translation). This pins the round-trip and the defaults
// so a rename or unit slip can't silently strand the bound accessors.
func TestDeviceLocalSettingsMillisAccessors(t *testing.T) {
	s := DefaultDeviceLocalSettings()

	// defaults surface through the bound view
	if got := s.GetSendTimeoutMillis(); got != 5000 {
		t.Fatalf("default SendTimeout = %dms, want 5000", got)
	}
	if got := s.GetNetContractStatusMillis(); got != 10000 {
		t.Fatalf("default NetContractStatus = %dms, want 10000", got)
	}
	if got := s.GetBlockActionWindowMillis(); got != 300000 {
		t.Fatalf("default BlockActionWindow = %dms, want 300000", got)
	}
	if got := s.GetContractStatsEpochMillis(); got != 1000 {
		t.Fatalf("default ContractStatsEpoch = %dms, want 1000", got)
	}
	if got := s.GetNetworkPeersEpochMillis(); got != 1000 {
		t.Fatalf("default NetworkPeersEpoch = %dms, want 1000", got)
	}

	// sets land on the Go fields with millisecond semantics
	s.SetSendTimeoutMillis(250)
	if s.SendTimeout != 250*time.Millisecond {
		t.Fatalf("SendTimeout = %v, want 250ms", s.SendTimeout)
	}
	s.SetNetContractStatusMillis(1500)
	if s.NetContractStatusDuration != 1500*time.Millisecond {
		t.Fatalf("NetContractStatusDuration = %v, want 1.5s", s.NetContractStatusDuration)
	}
	s.SetBlockActionWindowMillis(60000)
	if s.BlockActionWindowDuration != time.Minute {
		t.Fatalf("BlockActionWindowDuration = %v, want 1m", s.BlockActionWindowDuration)
	}
	s.SetContractStatsEpochMillis(2000)
	s.SetNetworkPeersEpochMillis(3000)
	if got := s.GetContractStatsEpochMillis(); got != 2000 {
		t.Fatalf("ContractStatsEpoch readback = %d", got)
	}
	if got := s.GetNetworkPeersEpochMillis(); got != 3000 {
		t.Fatalf("NetworkPeersEpoch readback = %d", got)
	}

	// sub-millisecond values truncate only in the bound view, never in Go
	s.SendTimeout = 1500 * time.Microsecond
	if got := s.GetSendTimeoutMillis(); got != 1 {
		t.Fatalf("sub-ms bound view = %d, want 1 (truncated)", got)
	}
	if s.SendTimeout != 1500*time.Microsecond {
		t.Fatalf("Go field must be untouched by the bound read")
	}
}
