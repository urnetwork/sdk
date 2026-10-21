package sdk

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The mobile boundary is whole milliseconds; saturating the affinity setting
// preserves disabled signs without changing the other duration conversions.
func TestSdkAffinityMaxTimeMillisRoundTrip(t *testing.T) {
	const maximumMillis = int64(math.MaxInt64) / int64(time.Millisecond)
	cases := []struct {
		input int64
		want  int64
	}{
		{math.MinInt64, -maximumMillis},
		{-maximumMillis - 1, -maximumMillis},
		{-maximumMillis, -maximumMillis},
		{-60_000, -60_000},
		{-1, -1},
		{0, 0},
		{1, 1},
		{1_500, 1_500},
		{60_000, 60_000},
		{90_000, 90_000},
		{maximumMillis - 1, maximumMillis - 1},
		{maximumMillis, maximumMillis},
		{maximumMillis + 1, maximumMillis},
		{math.MaxInt64, maximumMillis},
	}
	for _, test := range cases {
		settings := ReliabilitySettings{
			AffinityMaxTimeMillis:    test.input,
			SendStallTimeoutMillis:   3_000,
			HeartbeatIntervalMillis:  60_000,
			PerformanceAwareAffinity: true,
		}
		before := settings
		converted := settings.toConnect()
		want := time.Duration(test.want) * time.Millisecond
		if converted.AffinityMaxTime != want || (converted.AffinityMaxTime <= 0) != (test.input <= 0) {
			t.Fatalf("input %d: affinity = %v, want %v with original disabled sign", test.input, converted.AffinityMaxTime, want)
		}
		if converted.SendStallTimeout != 3*time.Second || converted.HeartbeatInterval != time.Minute || !converted.PerformanceAwareAffinity {
			t.Fatalf("input %d changed another setting: %+v", test.input, converted)
		}
		roundTrip := reliabilitySettingsFromConnect(converted)
		wantSettings := before
		wantSettings.AffinityMaxTimeMillis = test.want
		if *roundTrip != wantSettings || settings != before {
			t.Fatalf("input %d: round trip %+v, source %+v", test.input, roundTrip, settings)
		}
		roundTrip.AffinityMaxTimeMillis = 7
		if converted.AffinityMaxTime != want || settings != before {
			t.Fatalf("input %d: returned SDK settings alias an input", test.input)
		}
	}
}

// The default comes from Connect; nil conversion stays an all-zero value and
// nanosecond inputs lose only their unrepresentable millisecond remainder.
func TestSdkAffinityMaxTimeDefaultAndDurationBounds(t *testing.T) {
	defaults := connect.ReliabilitySettingsFrom(connect.DefaultMultiClientSettings())
	if defaults.AffinityMaxTime != time.Minute {
		t.Fatalf("Connect default = %v, want one minute", defaults.AffinityMaxTime)
	}
	if got := reliabilitySettingsFromConnect(defaults).AffinityMaxTimeMillis; got != 60_000 {
		t.Fatalf("SDK default = %d ms, want 60000", got)
	}
	if got := reliabilitySettingsFromConnect(nil); *got != (ReliabilitySettings{}) {
		t.Fatalf("nil conversion invented defaults: %+v", got)
	}
	for _, duration := range []time.Duration{math.MinInt64, -time.Millisecond, -1, 0, 1, time.Millisecond, time.Minute, math.MaxInt64} {
		source := connect.ReliabilitySettings{AffinityMaxTime: duration}
		before := source
		settings := reliabilitySettingsFromConnect(&source)
		if settings.AffinityMaxTimeMillis != duration.Milliseconds() {
			t.Fatalf("%v converted to %d ms", duration, settings.AffinityMaxTimeMillis)
		}
		if got := settings.toConnect().AffinityMaxTime; got != duration.Truncate(time.Millisecond) {
			t.Fatalf("%v round trip = %v", duration, got)
		}
		if source != before {
			t.Fatalf("%v mutated Connect input", duration)
		}
	}
}

// Generated bindings expose one signed 64-bit millisecond field, not a
// second boolean gate. JSON and gob keep exact values before conversion.
func TestSdkAffinityMaxTimeTypedWireSurface(t *testing.T) {
	fields := reflect.TypeOf(ReliabilitySettings{})
	field, ok := fields.FieldByName("AffinityMaxTimeMillis")
	if !ok || field.Type.Kind() != reflect.Int64 {
		t.Fatalf("affinity field is not int64: %+v", field)
	}
	if _, ok := fields.FieldByName("FreshFlowAffinity"); ok {
		t.Fatal("obsolete affinity boolean remains in the SDK surface")
	}
	for _, value := range []int64{math.MinInt64, -1, 0, 1, 60_000, math.MaxInt64} {
		source := ReliabilitySettings{AffinityMaxTimeMillis: value}
		encoded, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		var numeric int64
		if err := json.Unmarshal(fields["AffinityMaxTimeMillis"], &numeric); err != nil || numeric != value {
			t.Fatalf("%d: field is not an exact numeric millisecond value: %s, %v", value, encoded, err)
		}
		if _, ok := fields["FreshFlowAffinity"]; ok {
			t.Fatal("obsolete affinity boolean serialized")
		}
		var decoded ReliabilitySettings
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != source {
			t.Fatalf("%d: JSON round trip %+v, %v", value, decoded, err)
		}
		wired := gobRoundTrip(t, &DeviceRemoteReliabilitySettingsRpc{ReliabilitySettings: &source})
		if wired.ReliabilitySettings == nil || *wired.ReliabilitySettings != source {
			t.Fatalf("%d: gob round trip %+v", value, wired)
		}
		wired.ReliabilitySettings.AffinityMaxTimeMillis = 2
		if source.AffinityMaxTimeMillis != value {
			t.Fatalf("%d: gob result aliases source", value)
		}
	}
	for _, encoded := range []string{`{"FreshFlowAffinity":false}`, `{"FreshFlowAffinity":true}`} {
		var missing ReliabilitySettings
		if err := json.Unmarshal([]byte(encoded), &missing); err != nil || missing.AffinityMaxTimeMillis != 0 {
			t.Fatalf("legacy JSON invented a duration default: %+v, %v", missing, err)
		}
	}
	// A historical full settings object has common fields as well as the
	// removed gate. Gob ignores that unknown field rather than enabling reuse.
	legacy := struct {
		ReliabilitySettings *struct {
			UdpTeardownSignal bool
			FreshFlowAffinity bool
		}
	}{}
	legacy.ReliabilitySettings = &struct {
		UdpTeardownSignal bool
		FreshFlowAffinity bool
	}{UdpTeardownSignal: true, FreshFlowAffinity: true}
	for _, enabled := range []bool{false, true} {
		legacy.ReliabilitySettings.FreshFlowAffinity = enabled
		wiredLegacy := gobRoundTripAs[DeviceRemoteReliabilitySettingsRpc](t, legacy)
		if wiredLegacy.ReliabilitySettings == nil || !wiredLegacy.ReliabilitySettings.UdpTeardownSignal ||
			wiredLegacy.ReliabilitySettings.AffinityMaxTimeMillis != 0 || wiredLegacy.ReliabilitySettings.toConnect().AffinityMaxTime != 0 {
			t.Fatalf("legacy gob invented a duration default: %+v", wiredLegacy)
		}
	}
	zero := gobRoundTrip(t, &DeviceRemoteReliabilitySettingsRpc{ReliabilitySettings: &ReliabilitySettings{}})
	if zero.ReliabilitySettings == nil || zero.ReliabilitySettings.toConnect().AffinityMaxTime != 0 {
		t.Fatalf("explicit full zero settings enabled affinity: %+v", zero)
	}
	for _, encoded := range []string{
		`{"AffinityMaxTimeMillis":true}`,
		`{"AffinityMaxTimeMillis":"60000"}`,
		`{"AffinityMaxTimeMillis":1.5}`,
		`{"AffinityMaxTimeMillis":9223372036854775808}`,
	} {
		var decoded ReliabilitySettings
		if err := json.Unmarshal([]byte(encoded), &decoded); err == nil {
			t.Fatalf("non-int64 duration accepted: %s", encoded)
		}
	}
}

// No candidate can be opened by this configuration-only fixture. The inherited
// generator methods satisfy the ordinary constructor without network I/O.
type sdkAffinitySettingsGenerator struct {
	rpcLeakTestGenerator
}

func (self *sdkAffinitySettingsGenerator) NextDestinations(count int, excluded []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	return map[connect.MultiHopId]connect.DestinationStats{}, nil
}

func (self *sdkAffinitySettingsGenerator) FixedDestinationSize() (int, bool) {
	return 0, true
}

// Real DeviceLocal setters synchronously reach the actual Connect multi client.
// No polling or sleeps stand in for configuration propagation.
func TestSdkAffinityMaxTimeRuntimeOverrideAndReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := connect.DefaultMultiClientSettings()
	settings.Log = connect.NewNoopLogger()
	multi := connect.NewRemoteUserNatMultiClient(ctx, &sdkAffinitySettingsGenerator{}, nil, protocol.ProvideMode_Network, settings)
	defer func() {
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		if err := multi.CloseAndWait(joinCtx); err != nil {
			t.Errorf("multi client close: %v", err)
		}
	}()
	device := &DeviceLocal{remoteUserNatClient: multi}
	defaults := device.GetReliabilitySettings()
	if defaults == nil || defaults.AffinityMaxTimeMillis != 60_000 {
		t.Fatalf("effective default = %+v", defaults)
	}
	const maximumMillis = int64(math.MaxInt64) / int64(time.Millisecond)
	for _, test := range []struct{ input, want int64 }{
		{0, 0}, {-1, -1}, {1, 1}, {45_000, 45_000},
		{math.MinInt64, -maximumMillis}, {math.MaxInt64, maximumMillis},
	} {
		override := *defaults
		override.AffinityMaxTimeMillis = test.input
		device.SetReliabilitySettings(&override)
		override.AffinityMaxTimeMillis = 9
		got := device.GetReliabilitySettings()
		want := *defaults
		want.AffinityMaxTimeMillis = test.want
		if got == nil || *got != want || multi.ReliabilitySettings().AffinityMaxTime != time.Duration(test.want)*time.Millisecond {
			t.Fatalf("input %d: runtime settings = %+v", test.input, got)
		}
		if device.reliabilitySettings == nil || device.reliabilitySettings.AffinityMaxTime != time.Duration(test.want)*time.Millisecond {
			t.Fatalf("input %d: stored override not owned", test.input)
		}
		got.AffinityMaxTimeMillis = 8
		device.SetReliabilitySettings(nil)
		if current := device.GetReliabilitySettings(); current == nil || *current != want {
			t.Fatalf("input %d: getter alias or nil setter changed settings: %+v", test.input, current)
		}
		device.ResetReliabilitySettings()
		if device.reliabilitySettings != nil {
			t.Fatal("reset kept the stored override")
		}
		if restored := device.GetReliabilitySettings(); restored == nil || *restored != *defaults {
			t.Fatalf("reset did not restore Connect defaults: %+v", restored)
		}
	}
}

// While disconnected, an owned override is retained but is not reported as an
// effective configuration; reset clears the next-connect override as well.
func TestSdkAffinityMaxTimeOfflineOverrideOwnership(t *testing.T) {
	device := &DeviceLocal{}
	for _, value := range []int64{-1, 0, 45_000} {
		override := ReliabilitySettings{AffinityMaxTimeMillis: value}
		device.SetReliabilitySettings(&override)
		override.AffinityMaxTimeMillis = 9
		if device.GetReliabilitySettings() != nil {
			t.Fatal("disconnected getter invented an effective configuration")
		}
		if device.reliabilitySettings == nil || device.reliabilitySettings.AffinityMaxTime != time.Duration(value)*time.Millisecond {
			t.Fatalf("%d: disconnected override not owned", value)
		}
		device.SetReliabilitySettings(nil)
		if device.reliabilitySettings == nil || device.reliabilitySettings.AffinityMaxTime != time.Duration(value)*time.Millisecond {
			t.Fatalf("%d: nil setter cleared the override", value)
		}
		device.ResetReliabilitySettings()
		if device.reliabilitySettings != nil || device.GetReliabilitySettings() != nil {
			t.Fatal("disconnected reset retained settings")
		}
	}
}
