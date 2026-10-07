package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Gomobile roots every exported SDK function, even diagnostic entry points
// that the host never calls. A transitive import (for example, a logging fatal
// fallback) can also retain all of pprof's registered profile writers. Guard
// the whole shipped iOS dependency graph, while retaining Android and native
// profiling. Gomobile's macOS slices also set the ios build tag.
func TestMobileHeapProfileDependencies(t *testing.T) {
	for _, view := range []struct {
		name        string
		goos        string
		tags        string
		wantProfile bool
	}{
		{"ios_app", "ios", "sdk_mobile_bind", false},
		{"ios_extension", "ios", "sdk_mobile_bind,ios_extension", false},
		{"macos_app_binding", "darwin", "sdk_mobile_bind,ios", false},
		{"macos_extension_binding", "darwin", "sdk_mobile_bind,ios,ios_extension", false},
		{"android", "android", "sdk_mobile_bind", true},
		{"native", "darwin", "", true},
	} {
		t.Run(view.name, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), "go", "list", "-deps", "-tags="+view.tags, "-f", "{{.ImportPath}}", ".")
			command.Dir = ".."
			command.Env = append(os.Environ(),
				"GOOS="+view.goos, "GOARCH=arm64", "CGO_ENABLED=1",
				"GOEXPERIMENT=greenteagc", "GOWORK=off",
			)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("list SDK dependencies: %v\n%s", err, output)
			}
			if got := slices.Contains(strings.Fields(string(output)), "runtime/pprof"); got != view.wantProfile {
				t.Fatalf("SDK imports runtime/pprof = %v, want %v", got, view.wantProfile)
			}
		})
	}
}
