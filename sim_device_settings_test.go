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
