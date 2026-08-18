package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

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
