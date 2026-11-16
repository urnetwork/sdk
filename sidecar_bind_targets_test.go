//go:build !ios && !android && !js

package sdk

// The sdk package compiles for every target build/Makefile binds, as gomobile builds it (R-SDK-5, §18.10). A file
// of the sidecar role that names a root symbol tagged !ios && !android, the daemon's JSON-RPC codes among them,
// builds on this host and fails only the phones' bind: sidecar_search_remote.go did, on android and ios alike, and
// nothing before the bind itself compiled the package there, since the boundary tests read `go list`, which does not
// type-check.

import (
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// One gomobile build of the package.
type testBindBuild struct {
	goos   string
	goarch string
	tags   string
}

// The package compiles for every bound target, its cgo aside: a compile with no cgo needs no NDK or Xcode, and the
// failures this guards are Go's. One build per distinct GOOS, GOARCH and tags among each gomobile bind's targets.
func TestSdkCompilesForEveryBoundTarget(t *testing.T) {
	makefile, err := os.ReadFile("build/Makefile")
	if err != nil {
		t.Fatal(err)
	}
	// the GOOS and tags gomobile builds a platform's slice with: cmd/gomobile/env.go's platformOS and platformTags
	// at the golang.org/x/mobile build/go.mod pins, where the macOS slices build with GOOS=ios and the macos tag
	bindPlatform := func(platform string) (goos string, tags []string, ok bool) {
		switch platform {
		case "android":
			return "android", []string{"android"}, true
		case "ios", "iossimulator":
			return "ios", []string{"ios"}, true
		case "macos":
			return "ios", []string{"macos"}, true
		}
		return "", nil, false
	}
	// a bind's flags run from `gomobile bind` to the package it binds; a recipe continues every line, so two binds
	// can share one
	const boundPackage = `"github.com/urnetwork/sdk"`
	targetPattern := regexp.MustCompile(`-target\s+(\S+)`)
	tagsPattern := regexp.MustCompile(`-tags\s+(\S+)`)
	builds := []testBindBuild{}
	for _, bind := range strings.Split(string(makefile), "gomobile bind")[1:] {
		flags, _, found := strings.Cut(bind, boundPackage)
		if !found {
			t.Fatalf("a bind of no %s: %q", boundPackage, bind)
		}
		target := targetPattern.FindStringSubmatch(flags)
		if target == nil {
			t.Fatalf("a bind with no -target: %q", flags)
		}
		bindTags := []string{}
		if tags := tagsPattern.FindStringSubmatch(flags); tags != nil {
			bindTags = strings.Split(tags[1], ",")
		}
		for _, platformArch := range strings.Split(target[1], ",") {
			platform, arch, found := strings.Cut(platformArch, "/")
			goos, platformTags, ok := bindPlatform(platform)
			if !found || !ok {
				t.Fatalf("a bind target this test cannot map: %q", platformArch)
			}
			tags := slices.Concat(bindTags, platformTags)
			slices.Sort(tags)
			build := testBindBuild{goos: goos, goarch: arch, tags: strings.Join(slices.Compact(tags), ",")}
			if !slices.Contains(builds, build) {
				builds = append(builds, build)
			}
		}
	}
	// the Android and Apple binds and the network extension's, each with its platforms
	for _, want := range []testBindBuild{
		{goos: "android", goarch: "arm64", tags: "android,sdk_mobile_bind"},
		{goos: "ios", goarch: "arm64", tags: "ios,sdk_mobile_bind"},
		{goos: "ios", goarch: "arm64", tags: "ios,ios_extension,sdk_mobile_bind"},
	} {
		if !slices.Contains(builds, want) {
			t.Fatalf("build/Makefile binds no %+v: %+v", want, builds)
		}
	}
	var wait sync.WaitGroup
	failures := make([]string, len(builds))
	for i, build := range builds {
		wait.Add(1)
		go func() {
			defer wait.Done()
			command := exec.Command("go", "build", "-o", os.DevNull, "-tags", build.tags, ".")
			command.Env = append(os.Environ(), "GOOS="+build.goos, "GOARCH="+build.goarch, "CGO_ENABLED=0")
			if output, err := command.CombinedOutput(); err != nil {
				failures[i] = build.goos + "/" + build.goarch + " " + build.tags + ": " + err.Error() + "\n" + string(output)
			}
		}()
	}
	wait.Wait()
	for _, failure := range failures {
		if failure != "" {
			t.Error(failure)
		}
	}
	t.Logf("compiled for %d bound builds: %+v", len(builds), builds)
}
