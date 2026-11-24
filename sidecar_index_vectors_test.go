//go:build !ios_extension && !js

package sdk

// The search index's normalizer under the sdk's module graph (SEARCH-M10-41; §4.7, §11.14's tables row, D-TEST-15,
// FI-22): the apps link the golang.org/x/text sdk's go.mod selects, not the one sidecar's does, and normalizer 1 is
// x/text v0.44.0 linked unreplaced with Unicode 17.0.0's tables. A bump that rides another module's (x/text rides
// x/net's) fails here, in the binary the sdk builds, until a new normalizer is assigned, or until normalizer 1 is
// re-pinned on tables that are byte for byte its own, as QD-1385 did from v0.43.0 to v0.44.0; sidecar's check/ holds
// the go.mod files, and this the selection the build made. sidecar_index_vectors_testhooks_test.go reproduces the
// vectors themselves in this binary.

import (
	"runtime/debug"
	"testing"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Normalizer 1's pins (§11.14's tables row): facts of the format, not tunables.
const (
	sdkNormalizerOneUnicodeVersion    = "17.0.0"
	sdkNormalizerOneTextModulePath    = "golang.org/x/text"
	sdkNormalizerOneTextModuleVersion = "v0.44.0"
)

// What a build's info and tables say against normalizer 1's, a finding each difference.
func sdkNormalizerOneFindings(info *debug.BuildInfo, normVersion string, casesVersion string, unicodeVersion string) []string {
	findings := []string{}
	for _, version := range []struct{ name, value string }{
		{name: "norm.Version", value: normVersion},
		{name: "cases.UnicodeVersion", value: casesVersion},
		{name: "unicode.Version", value: unicodeVersion},
	} {
		if version.value != sdkNormalizerOneUnicodeVersion {
			findings = append(findings, version.name+" is "+version.value+", and normalizer 1's is "+sdkNormalizerOneUnicodeVersion)
		}
	}
	if info == nil {
		return append(findings, "no build info names the x/text this binary links")
	}
	found := false
	for _, module := range info.Deps {
		if module.Path != sdkNormalizerOneTextModulePath {
			continue
		}
		found = true
		if module.Version != sdkNormalizerOneTextModuleVersion {
			findings = append(findings, "the sdk selects "+module.Path+" "+module.Version+", and normalizer 1's is "+sdkNormalizerOneTextModuleVersion)
		}
		if module.Replace != nil {
			findings = append(findings, "the sdk replaces "+module.Path+" with "+module.Replace.Path)
		}
	}
	if !found {
		findings = append(findings, "the build info lists no "+sdkNormalizerOneTextModulePath)
	}
	return findings
}

// This binary, built under the sdk's module graph, links normalizer 1's x/text and tables.
func TestSdkLinksNormalizerOne(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("no build info")
	}
	if findings := sdkNormalizerOneFindings(info, norm.Version, cases.UnicodeVersion, unicode.Version); len(findings) != 0 {
		t.Errorf("%v", findings)
	}
}

// A build selecting another x/text, replacing it, or on other tables fails: a bump cannot pass unseen.
func TestSdkLinksNormalizerOneFailsOnAnotherTextModule(t *testing.T) {
	other := &debug.BuildInfo{Deps: []*debug.Module{{Path: sdkNormalizerOneTextModulePath, Version: "v0.45.0"}}}
	if findings := sdkNormalizerOneFindings(other, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion); len(findings) != 1 {
		t.Errorf("x/text v0.45.0: %v", findings)
	}
	replaced := &debug.BuildInfo{Deps: []*debug.Module{{
		Path: sdkNormalizerOneTextModulePath, Version: sdkNormalizerOneTextModuleVersion,
		Replace: &debug.Module{Path: "../text"},
	}}}
	if findings := sdkNormalizerOneFindings(replaced, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion); len(findings) != 1 {
		t.Errorf("a replaced x/text: %v", findings)
	}
	current := &debug.BuildInfo{Deps: []*debug.Module{{Path: sdkNormalizerOneTextModulePath, Version: sdkNormalizerOneTextModuleVersion}}}
	if findings := sdkNormalizerOneFindings(current, "18.0.0", sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion); len(findings) != 1 {
		t.Errorf("Unicode 18.0.0's norm tables: %v", findings)
	}
	if findings := sdkNormalizerOneFindings(current, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion, sdkNormalizerOneUnicodeVersion); len(findings) != 0 {
		t.Errorf("normalizer 1 itself: %v", findings)
	}
}
