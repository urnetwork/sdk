// SPDX-License-Identifier: MPL-2.0
package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestReleaseDownloadURLNamesTheNightlyTag(t *testing.T) {
	t.Setenv("SDK_PACKAGE_VERSION", "2026.11.22")
	t.Setenv("SDK_RELEASE_TAG", "")
	if got, want := releaseDownloadURL("a.gz"), "https://github.com/urnetwork/build/releases/download/v2026.11.22/a.gz"; got != want {
		t.Fatalf("default: %s, want %s", got, want)
	}
	// stable packages point at the nightly release that carries their assets
	t.Setenv("SDK_RELEASE_TAG", "v2026.11.22-1105365180")
	if got, want := releaseDownloadURL("a.gz"), "https://github.com/urnetwork/build/releases/download/v2026.11.22-1105365180/a.gz"; got != want {
		t.Fatalf("stable: %s, want %s", got, want)
	}
}

func TestStableVersionPattern(t *testing.T) {
	for v, ok := range map[string]bool{"2026.11.22": true, "2026.11.22-1105365180": false, "2026.11": false, "": false} {
		if stableVersionPattern.MatchString(v) != ok {
			t.Errorf("%q: want %v", v, ok)
		}
	}
	if got := stableBundleName("python", "2026.11.22-1"); got != "URnetworkSdk-stable-python-2026.11.22-1.tar.gz" {
		t.Fatal(got)
	}
}

func TestTarGzDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	textFile(filepath.Join(dir, "manifest.json"), "{}\n")
	textFile(filepath.Join(dir, "artifacts", "x.whl"), "wheel")
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := tarGzDir(dir, out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[h.Name] = string(b)
		}
	}
	sort.Strings(names)
	if len(names) != 3 || names[0] != "artifacts/" || files["artifacts/x.whl"] != "wheel" || files["manifest.json"] != "{}\n" {
		t.Fatalf("entries %v files %v", names, files)
	}
}
