// Holds the model routes' mirror (api_compute_models.go; CSRV-M11-01) to its
// mobile policy: the forms under settings and the durations only they read
// are Go-only by name, and nothing beside them is.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The model routes' mirror's Go-only members pass the gate by name, each in
// the record gobind writes for it, and the forms apps call, the settings'
// byte bounds and names beside the accepted ones do not.
func TestMobileComputeModelGoOnlyOmissionsAreExplicit(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"\t// skipped method Api.ComputeModelChunkSyncWithSettings with unsupported parameter or return types",
		"\t// skipped method Api.ComputeModelIndexSyncWithSettings with unsupported parameter or return types",
		"\t// skipped field ComputeModelApiSettings.IndexTimeout with unsupported type: time.Duration",
		"\t// skipped field ComputeModelApiSettings.ChunkTimeout with unsupported type: time.Duration",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "ComputeModelApiSettings.java"), []byte(source+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMobileExports(root); err != nil {
		t.Fatalf("the model routes' Go-only members were not accepted: %v", err)
	}
	for _, identifier := range []string{
		"Api.ComputeModelIndex",
		"Api.ComputeModelChunk",
		"Api.ComputeModelIndexSync",
		"Api.ComputeModelChunkSync",
		"DefaultComputeModelApiSettings",
		"ComputeModelApiSettings.IndexMaxByteCount",
		"ComputeModelApiSettings.ChunkMaxByteCount",
		"Api.ComputeModelIndexSyncWithSettingsAndMore",
		"ComputeModelApiSettings.IndexTimeoutMillis",
		"ComputeModelApiSettingsExtra.ChunkTimeout",
	} {
		if allowedMobileOmission(identifier) {
			t.Errorf("an omission of %s is accepted", identifier)
		}
	}
}
