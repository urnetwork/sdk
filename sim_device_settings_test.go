package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestSimClientGeneratorSettingsPinsLocalWebSocket(t *testing.T) {
	settings := newSimClientGeneratorSettings()
	if settings.PlatformTransportMode != connect.TransportModeH1 {
		t.Fatalf("simulator platform mode = %q, want %q", settings.PlatformTransportMode, connect.TransportModeH1)
	}

	settings.PlatformTransportMode = connect.TransportModeH3
	if next := newSimClientGeneratorSettings(); next.PlatformTransportMode != connect.TransportModeH1 {
		t.Fatalf("new simulator platform mode = %q after prior mutation, want %q", next.PlatformTransportMode, connect.TransportModeH1)
	}
}

func TestSimClientGeneratorSettingsIsolatePlatformTransportBudget(t *testing.T) {
	first := newSimClientGeneratorSettings()
	second := newSimClientGeneratorSettings()
	if first.PlatformTransportSettingsGenerator == nil || second.PlatformTransportSettingsGenerator == nil {
		t.Fatal("simulator platform settings generator is nil")
	}

	firstWindow := first.PlatformTransportSettingsGenerator()
	firstNextWindow := first.PlatformTransportSettingsGenerator()
	secondWindow := second.PlatformTransportSettingsGenerator()
	if firstWindow.PlatformTransportBudget == nil || secondWindow.PlatformTransportBudget == nil {
		t.Fatal("simulator platform transport budget is nil")
	}
	if firstWindow.PlatformTransportBudget != firstNextWindow.PlatformTransportBudget {
		t.Fatal("windows from one simulated device do not share a transport budget")
	}
	if firstWindow.PlatformTransportBudget == secondWindow.PlatformTransportBudget {
		t.Fatal("independent simulated devices share a transport budget")
	}

	defaultStats := connect.DefaultPlatformTransportBudget().Stats()
	firstStats := firstWindow.PlatformTransportBudget.Stats()
	if firstStats.TotalByteCount != defaultStats.TotalByteCount ||
		firstStats.MaxTransportCount != defaultStats.MaxTransportCount {
		t.Fatalf(
			"simulator budget = (%d bytes, %d transports), want (%d bytes, %d transports)",
			firstStats.TotalByteCount,
			firstStats.MaxTransportCount,
			defaultStats.TotalByteCount,
			defaultStats.MaxTransportCount,
		)
	}
}

func TestSimProviderSettingsIsolatePlatformTransportBudget(t *testing.T) {
	first := newSimProviderPlatformTransportSettings(connect.NewNoopLogger())
	second := newSimProviderPlatformTransportSettings(connect.NewNoopLogger())
	if first.PlatformTransportBudget == nil || second.PlatformTransportBudget == nil {
		t.Fatal("simulator provider platform transport budget is nil")
	}
	if first.PlatformTransportBudget == second.PlatformTransportBudget {
		t.Fatal("independent simulated providers share a transport budget")
	}

	defaultStats := connect.DefaultPlatformTransportBudget().Stats()
	firstStats := first.PlatformTransportBudget.Stats()
	if firstStats.TotalByteCount != defaultStats.TotalByteCount ||
		firstStats.MaxTransportCount != defaultStats.MaxTransportCount {
		t.Fatalf(
			"simulator provider budget = (%d bytes, %d transports), want (%d bytes, %d transports)",
			firstStats.TotalByteCount,
			firstStats.MaxTransportCount,
			defaultStats.TotalByteCount,
			defaultStats.MaxTransportCount,
		)
	}
}

func TestCloneSimMultiClientSettingsOwnsWindowMap(t *testing.T) {
	source := connect.DefaultMultiClientSettings()
	cloned := cloneSimMultiClientSettings(source)
	quality := cloned.WindowSizes[connect.WindowTypeQuality]
	quality.WindowSizeMin = 9
	cloned.WindowSizes[connect.WindowTypeQuality] = quality

	if source.WindowSizes[connect.WindowTypeQuality].WindowSizeMin == 9 {
		t.Fatal("cloned simulator settings alias the caller's window map")
	}
	if cloned.SequenceBufferSize != source.SequenceBufferSize {
		t.Fatal("cloned simulator settings lost scalar fields")
	}
}
