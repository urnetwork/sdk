//go:build !ios_extension && !js && !ios && !android

package sdk

// The presence vectors the cells carry are the sidecar module's (sidecar_presence_vectors_test.go): a copy that
// drifted from testdata/presence/vectors.json would test the sdk against a format the root no longer writes. Runs on a
// desktop, where the module's source is.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPresenceVectorsAreTheSidecarModules(t *testing.T) {
	output, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/urnetwork/sidecar").Output()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(output)), "testdata", "presence", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, testPresenceVectorsJson) {
		t.Error("testdata/presence/vectors.json is not the sidecar module's: copy it again")
	}
}
