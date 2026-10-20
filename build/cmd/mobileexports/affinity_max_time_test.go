package main

import (
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every mobile view exports the same basic signed integer, with no alias or
// obsolete boolean. The three views use the existing real package loader.
func TestMobileAffinityMaxTimeViewsUseInt64(t *testing.T) {
	for _, view := range []struct{ goos, tags string }{
		{"android", "sdk_mobile_bind"},
		{"ios", "sdk_mobile_bind"},
		{"ios", "sdk_mobile_bind,ios_extension"},
	} {
		pkg := testingLoadMobileView(t, view.goos, view.tags)
		object, ok := pkg.Types.Scope().Lookup("ReliabilitySettings").(*types.TypeName)
		if !ok {
			t.Fatalf("%+v: missing ReliabilitySettings", view)
		}
		named, ok := types.Unalias(object.Type()).(*types.Named)
		if !ok {
			t.Fatalf("%+v: ReliabilitySettings is not named", view)
		}
		model, ok := named.Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("%+v: ReliabilitySettings is not a struct", view)
		}
		found := 0
		for i := 0; i < model.NumFields(); i++ {
			field := model.Field(i)
			if field.Name() == "FreshFlowAffinity" {
				t.Fatalf("%+v: obsolete affinity boolean exported", view)
			}
			if field.Name() == "AffinityMaxTimeMillis" {
				found++
				basic, ok := field.Type().(*types.Basic)
				if !ok || basic.Kind() != types.Int64 || !field.Exported() {
					t.Fatalf("%+v: affinity field is not a plain exported int64: %v", view, field)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%+v: affinity field count = %d", view, found)
		}
	}
}

// Generate only in test-owned directories through the already pinned gobind;
// no handwritten Java/Objective-C header can satisfy this typed boundary.
func TestMobileAffinityMaxTimeBindingsUseSignedMillis(t *testing.T) {
	for _, language := range []string{"java", "objc"} {
		outputDirectory := t.TempDir()
		args := []string{
			"run", "golang.org/x/mobile/cmd/gobind",
			"-lang=" + language, "-tags=sdk_mobile_bind", "-outdir=" + outputDirectory,
		}
		if language == "java" {
			args = append(args, "-javapkg=com.bringyour")
		}
		args = append(args, "github.com/urnetwork/sdk")
		command := exec.Command("go", args...)
		command.Dir = "../.."
		command.Env = append(os.Environ(), "GOEXPERIMENT=greenteagc", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate %s: %v\n%s", language, err, output)
		}
		var path string
		var required []string
		if language == "java" {
			path = filepath.Join(outputDirectory, "java", "com", "bringyour", "sdk", "ReliabilitySettings.java")
			required = []string{
				"public final native long getAffinityMaxTimeMillis();",
				"public final native void setAffinityMaxTimeMillis(long v);",
			}
		} else {
			path = filepath.Join(outputDirectory, "src", "gobind", "Sdk.objc.h")
			required = []string{"@property (nonatomic) int64_t affinityMaxTimeMillis;"}
		}
		contents, err := os.ReadFile(path)
		testingBuildNoError(t, err)
		text := string(contents)
		for _, declaration := range required {
			if strings.Count(text, declaration) != 1 {
				t.Fatalf("%s: missing or duplicate signed millisecond declaration %q", language, declaration)
			}
		}
		if strings.Contains(text, "FreshFlowAffinity") || strings.Contains(text, "freshFlowAffinity") {
			t.Fatalf("%s: obsolete affinity boolean remains", language)
		}
	}
}
