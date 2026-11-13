package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The linker drops the exported methods that nothing can reach, about 7 MiB of the
// iOS extension, until a reachable function calls reflect's Method or MethodByName
// with a name it can't read (APPLE_EXTENSION_MEMORY_BUDGET.md). Two such calls kept
// them all from the moment reflect grew a Methods iterator, each enough alone:
//
//   - net/rpc.suitableMethods, which walks a service's methods. The device rpc serves
//     from its own tables so that no mobile binding links net/rpc. net/rpc also
//     brought html/template and text/template, whose own lookup would do the same
//     if anything reached it.
//   - reflect.(*rtype).Methods and reflect.Value.Methods, whose closures make that
//     call. Protobuf before v1.36.12 asked reflect for a method named Methods when it
//     formats a service descriptor, and the linker keeps every method of that name.
//
// check_apple_size.sh holds the built binaries to the result, for any cause. This
// holds these two causes without a build.
func TestMobileBindingsLinkNothingThatKeepsEveryMethod(t *testing.T) {
	for _, view := range []struct {
		name string
		goos string
		tags string
	}{
		{"ios_app", "ios", "sdk_mobile_bind"},
		{"ios_extension", "ios", "sdk_mobile_bind,ios_extension"},
		{"macos_app_binding", "darwin", "sdk_mobile_bind,ios"},
		{"macos_extension_binding", "darwin", "sdk_mobile_bind,ios,ios_extension"},
		{"android", "android", "sdk_mobile_bind"},
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
			packages := strings.Fields(string(output))
			if !slices.Contains(packages, "github.com/urnetwork/sdk/v2026") {
				t.Fatalf("the list is not the SDK's:\n%s", output)
			}
			for _, forbidden := range []string{"net/rpc", "html/template", "text/template"} {
				if slices.Contains(packages, forbidden) {
					t.Errorf("the binding links %s", forbidden)
				}
			}
		})
	}
}

// gomobile builds under this module, so its choice of protobuf is the bindings'.
func TestMobileBindingProtobufDoesNotNameMethodsMethod(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Version}} {{.Dir}}", "google.golang.org/protobuf")
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("find protobuf: %v\n%s", err, output)
	}
	version, directory, ok := strings.Cut(strings.TrimSpace(string(output)), " ")
	if !ok {
		t.Fatalf("find protobuf: %s", output)
	}
	source, err := os.ReadFile(filepath.Join(directory, "internal", "descfmt", "stringer.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "ServiceDescriptor") {
		t.Fatalf("protobuf %s has moved the formatting of a service descriptor, so this reads nothing", version)
	}
	if strings.Contains(string(source), `MethodByName("Methods")`) {
		t.Fatalf("protobuf %s keeps reflect's Methods iterators, and with them every exported method of every type", version)
	}
}
