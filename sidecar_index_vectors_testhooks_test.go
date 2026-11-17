//go:build sidecar_testhooks && !ios_extension && !js

package sdk

// The index vectors in the binary the sdk builds (SEARCH-M10-41; D-TEST-15, FI-22): normalizer 1's texts and key sets,
// and the slots and shards decoded and encoded again, byte for byte, plaintext and hashes, through sidecar's seam, under
// the sdk's module graph and on whichever leg runs this (test-t4.sh runs it on darwin/arm64 and, under Rosetta,
// darwin/amd64; the gomobile legs by the cells).

import (
	"testing"

	"github.com/urnetwork/sidecar"
)

func TestSdkReproducesTheIndexVectors(t *testing.T) {
	if findings := sidecar.TestHookIndexVectorFindings(); len(findings) != 0 {
		t.Errorf("the sdk's build differs from testdata/index: %v", findings)
	}
	normVersion, casesVersion, unicodeVersion, textModuleVersion := sidecar.TestHookIndexLinkedTables()
	if normVersion != sdkNormalizerOneUnicodeVersion || casesVersion != sdkNormalizerOneUnicodeVersion ||
		unicodeVersion != sdkNormalizerOneUnicodeVersion || textModuleVersion != sdkNormalizerOneTextModuleVersion {
		t.Errorf("sidecar reads this build as %s %s %s %s", normVersion, casesVersion, unicodeVersion, textModuleVersion)
	}
}
