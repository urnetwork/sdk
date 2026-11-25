// SPDX-License-Identifier: MPL-2.0
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Stable SDK packages. A nightly publishes its registry packages (when the
// builder has credentials) under the nightly's prerelease version. A stable
// release needs the same packages under a release version, which plain
// `pip install`, `gem install`, `dotnet add package` and `cargo add` select.
//
// `release stable` builds and checks every native registry package again with
// SDK_STABLE_VERSION (the app's release version, e.g. 2026.11.22), in a copy of
// this tree so the nightly's own dist directories are untouched, and writes
// each language's checked dist directory as one release asset:
//
//	URnetworkSdk-stable-<language>-<nightly version>.tar.gz
//
// Nothing is published here. factury/release downloads the asset, verifies it
// against GitHub's digest and runs `publish <registry> <dir>` on it when the
// nightly is released. Binary assets the packages download (Rust runtimes, C
// archives) are written next to the bundles so they are uploaded to this
// nightly's release, and every package URL names the nightly's tag
// (SDK_RELEASE_TAG). public.json in each bundle lists those URLs and their
// sha256, for the publisher to check before an upload that references them.
//
// One language failing does not stop the others; the exit status is 1 when
// any failed, and the stage's index asset records which.

// stableLanguages are the package owners that a stable release publishes.
var stableLanguages = []string{"python", "csharp", "ruby", "java", "rust", "cgo", "swift"}

var stableVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func stableBundleName(language, nightly string) string {
	return "URnetworkSdk-stable-" + language + "-" + nightly + ".tar.gz"
}

// releaseTag is the GitHub release the package URLs point at.
func releaseTag() string {
	return env("SDK_RELEASE_TAG", "v"+packageVersion())
}

func releaseDownloadURL(name string) string {
	return "https://github.com/urnetwork/build/releases/download/" + releaseTag() + "/" + name
}

type stableIndex struct {
	StableVersion string            `json:"stable_version"`
	Nightly       string            `json:"nightly"`
	Languages     map[string]string `json:"languages"`
}

func releaseStable() {
	stable := os.Getenv("SDK_STABLE_VERSION")
	require(stableVersionPattern.MatchString(stable), "SDK_STABLE_VERSION must be a release version x.y.z")
	nightly := packageVersion()
	require(nightly != stable, "SDK_STABLE_VERSION must differ from the nightly version")
	tag := env("SDK_RELEASE_TAG", "v"+nightly)
	assets := path("packaging/release/assets")
	mkdir(assets)
	native := path("packaging/release/native.json")
	if !exists(native) {
		native = releaseNative()
	}
	temp, cleanup := temporary("urnetwork-sdk-stable-")
	defer cleanup()
	tree := filepath.Join(temp, "sdk")
	copyTree(root, tree, map[string]bool{".git": true, "dist": true, "node_modules": true})

	xcName := "URnetworkSdk-" + nightly + ".xcframework.zip"
	environment := map[string]string{
		"SDK_PACKAGE_VERSION":     stable,
		"SDK_RELEASE_TAG":         tag,
		"SDK_NATIVE_MANIFEST":     native,
		"SDK_RUST_RELEASE_ASSETS": assets,
		"SDK_C_RELEASE_ASSETS":    assets,
		"SDK_XCFRAMEWORK_ZIP":     path("build/apple/URnetworkSdk.xcframework.zip"),
		"SDK_XCFRAMEWORK_URL":     "https://github.com/urnetwork/build/releases/download/" + tag + "/" + xcName,
		"SDK_ANDROID_AAR":         path("build/android/URnetworkSdk.aar"),
		"SDK_ANDROID_SOURCES":     path("build/android/URnetworkSdk-sources.jar"),
	}
	index := stableIndex{StableVersion: stable, Nightly: nightly, Languages: map[string]string{}}
	failed := 0
	for _, language := range stableLanguages {
		before := dirFiles(assets)
		err := stableLanguage(filepath.Join(tree, language), language, environment)
		if err == nil {
			// The binary assets this language added, plus the XCFramework the
			// Swift package names, must be public before its packages publish.
			var public []remoteAsset
			for name := range dirFiles(assets) {
				if !before[name] {
					public = append(public, remoteAsset{URL: "https://github.com/urnetwork/build/releases/download/" + tag + "/" + name, SHA256: hash(filepath.Join(assets, name))})
				}
			}
			if language == "swift" {
				public = append(public, remoteAsset{URL: environment["SDK_XCFRAMEWORK_URL"], SHA256: hash(environment["SDK_XCFRAMEWORK_ZIP"])})
			}
			sort.Slice(public, func(i, j int) bool { return public[i].URL < public[j].URL })
			dist := filepath.Join(tree, language, "dist")
			jsonWrite(filepath.Join(dist, "public.json"), map[string]any{"version": stable, "assets": public})
			err = tarGzDir(dist, filepath.Join(assets, stableBundleName(language, nightly)))
		}
		if err != nil {
			failed++
			index.Languages[language] = "failed: " + err.Error()
			fmt.Printf("SDK stable %s %s: FAILED: %v\n", language, stable, err)
			continue
		}
		index.Languages[language] = "ok"
		fmt.Printf("SDK stable %s %s: staged %s\n", language, stable, stableBundleName(language, nightly))
	}
	jsonWrite(filepath.Join(assets, "URnetworkSdk-stable-"+nightly+".json"), index)
	require(failed == 0, "%d stable SDK package languages failed", failed)
}

// stableLanguage builds and checks one language's packages.
func stableLanguage(dir, language string, environment map[string]string) error {
	steps := [][]string{{"package"}, {"check-package"}}
	if language == "java" {
		steps = [][]string{{"package"}, {"package-android"}, {"check-package"}}
	}
	for _, step := range steps {
		cmd := exec.Command("make", step...)
		cmd.Dir = dir
		cmd.Env = withEnv(environment)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("make %s: %w", strings.Join(step, " "), err)
		}
	}
	if !exists(filepath.Join(dir, "dist", "checked.json")) {
		return fmt.Errorf("no checked package in %s/dist", language)
	}
	return nil
}

func dirFiles(dir string) map[string]bool {
	out := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type().IsRegular() {
			out[e.Name()] = true
		}
	}
	return out
}

// tarGzDir writes dir's regular files and directories, relative to dir.
func tarGzDir(dir, out string) error {
	f, err := os.Create(out + ".part")
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unexpected file type in package: %s", rel)
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	for _, c := range []io.Closer{tw, gz, f} {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(out + ".part")
		return err
	}
	return os.Rename(out+".part", out)
}
