//go:build js && wasm && sidecar_http && sidecar_testhooks

// The search index's vectors on the page Worker's leg (SDK-M10-06's js/wasm test leg; D-TEST-15, FI-22, §22.9's Index
// determinism row): normalizer 1's texts and key sets, and the slots and shards decoded and encoded again, byte for
// byte, plaintext and hashes, through sidecar's seam, in the wasm binary built under the page Worker's own module
// graph, as the Worker links x/text. test/sidecarwasm_budget.test.ts runs it under Node, alone in its process, since
// on js/wasm the vector tests together pass wasm's 4 GiB (SEARCH-M10-41's finding).

package main

import (
	"testing"

	"github.com/urnetwork/sidecar"
)

func TestPageWorkerReproducesTheIndexVectors(t *testing.T) {
	if findings := sidecar.TestHookIndexVectorFindings(); len(findings) != 0 {
		t.Errorf("the page Worker's build differs from testdata/index: %v", findings)
	}
	// normalizer 1's pins (§11.14's tables row): facts of the format
	normVersion, casesVersion, unicodeVersion, textModuleVersion := sidecar.TestHookIndexLinkedTables()
	if normVersion != "17.0.0" || casesVersion != "17.0.0" || unicodeVersion != "17.0.0" || textModuleVersion != "v0.43.0" {
		t.Errorf("sidecar reads the page Worker's build as %s %s %s %s", normVersion, casesVersion, unicodeVersion, textModuleVersion)
	}
}
