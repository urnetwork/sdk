// SPDX-License-Identifier: MPL-2.0
package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var desktopRegistries = []string{"pypi", "nuget", "rubygems", "maven", "crates", "conan", "vcpkg"}

type releasePlan struct {
	Version    string          `json:"version"`
	StartedAt  time.Time       `json:"started_at"`
	Registries map[string]bool `json:"registries"`
}

func releaseNative() string {
	var state releasePlan
	jsonRead(path("packaging/release/plan.json"), &state)
	require(state.Version == packageVersion(), "stale SDK release plan")
	command(path("cgo"), nil, "make", "build_darwin")
	// The Windows VM retrieves a ZIP; loose host DLLs may be left over from
	// another release. Require a fresh archive and extract only its DLLs.
	stageWindowsRuntime(path("cgo/build/URnetworkSdkWindows.zip"), path("cgo/build"), state.StartedAt)
	m := nativeManifest{ABI: 1, Version: env("WARP_VERSION", packageVersion()),
		SourceRevision: strings.TrimSpace(string(output(root, nil, "git", "rev-parse", "HEAD")))}
	for _, system := range []string{"darwin", "linux", "windows"} {
		arches := []string{"amd64", "arm64"}
		if system == "windows" {
			arches = strings.Split(env("WINDOWS_BUILD_ARCHITECTURES", "amd64,arm64"), ",")
		}
		for _, arch := range arches {
			require(arch == "amd64" || arch == "arm64", "invalid native architecture")
			p := path("cgo/build", system, arch, files[system])
			stat, e := os.Stat(p)
			must(e)
			require(stat.Size() > 0 && !stat.ModTime().Before(state.StartedAt), "missing or stale release native library: %s", p)
			minimum := map[string]string{"darwin": "13.5", "linux": "glibc-2.35", "windows": "windows-10"}[system]
			if system == "darwin" {
				minimum = minimumOS(system, p)
			}
			m.Libraries = append(m.Libraries, library{Platform: system + "-" + arch, Path: p, SHA256: hash(p), MinimumOS: minimum})
		}
	}
	p := path("packaging/release/native.json")
	jsonWrite(p, m)
	return p
}

func stageWindowsRuntime(archive, destination string, started time.Time) {
	stat, err := os.Stat(archive)
	must(err)
	require(!stat.ModTime().Before(started), "stale Windows SDK ZIP")
	entries := zipFiles(archive)
	for _, arch := range strings.Split(env("WINDOWS_BUILD_ARCHITECTURES", "amd64,arm64"), ",") {
		require(arch == "amd64" || arch == "arm64", "invalid Windows SDK architecture")
		name := "windows/" + arch + "/URnetworkSdk.dll"
		data, ok := entries[name]
		require(ok && len(data) > 0, "Windows SDK ZIP is missing %s", name)
		write(filepath.Join(destination, name), data)
		// write intentionally preserves timestamps when bytes match. The fresh
		// archive is the provenance check in that case.
		must(os.Chtimes(filepath.Join(destination, name), stat.ModTime(), stat.ModTime()))
	}
}
func verifyPublicURL(assetURL, expected string) {
	require(strings.HasPrefix(assetURL, "https://"), "public SDK asset requires HTTPS")
	client := http.Client{Timeout: 5 * time.Minute}
	response, e := client.Get(assetURL)
	must(e)
	defer response.Body.Close()
	require(response.StatusCode == http.StatusOK, "public SDK asset returned HTTP %d", response.StatusCode)
	h := sha256.New()
	_, e = io.Copy(h, response.Body)
	must(e)
	require(fmt.Sprintf("%x", h.Sum(nil)) == expected, "public SDK asset checksum mismatch")
}
func releaseStage(phase string) {
	switch phase {
	case "plan":
		out := path("packaging/release")
		mkdir(out)
		// Old assets must never be uploaded by a later nullglob loop.
		must(os.RemoveAll(filepath.Join(out, "assets")))
		state := releasePlan{Version: packageVersion(), StartedAt: time.Now(), Registries: map[string]bool{}}
		for _, r := range registryOrder {
			enabled := hasCredential(r)
			state.Registries[r] = enabled
			status := "skipped (no publishing credentials)"
			if enabled {
				status = "enabled"
			}
			fmt.Println("SDK", r+":", status)
		}
		jsonWrite(filepath.Join(out, "plan.json"), state)
	case "desktop":
		enabled := []string{}
		for _, r := range desktopRegistries {
			if hasCredential(r) {
				enabled = append(enabled, r)
			}
		}
		if len(enabled) == 0 {
			fmt.Println("SKIP native SDK packages: no registry publishing credentials")
			return
		}
		native := releaseNative()
		environment := map[string]string{"SDK_PACKAGE_VERSION": packageVersion(), "SDK_NATIVE_MANIFEST": native, "SDK_RUST_RELEASE_ASSETS": path("packaging/release/assets"), "SDK_C_RELEASE_ASSETS": path("packaging/release/assets")}
		// All selected package checks finish before the first native upload.
		built := map[string]bool{}
		for _, r := range enabled {
			dir := path(registryLanguage[r])
			if built[dir] {
				continue
			}
			built[dir] = true
			command(dir, environment, "make", "package")
			if r == "maven" {
				command(dir, environment, "make", "package-android")
			}
			command(dir, environment, "make", "check-package")
		}
		for _, r := range enabled {
			if r != "crates" && r != "conan" && r != "vcpkg" {
				publishRegistry(r, "", packageVersion())
			}
		}
	case "mobile":
		if hasCredential("swift") || hasCredential("cocoapods") {
			command(path("swift"), map[string]string{"SDK_PACKAGE_VERSION": packageVersion()}, "make", "package", "check-package")
		} else {
			fmt.Println("SKIP SwiftPM, Carthage and CocoaPods: no publishing credentials")
		}
	case "check-swift":
		checkSwift()
	case "public":
		if hasCredential("conan") || hasCredential("vcpkg") {
			_, artifacts := verifiedFiles(path("cgo/dist"), packageVersion(), true)
			temp, cleanup := temporary("urnetwork-c-release-")
			defer cleanup()
			index := unpackCRecipes(artifacts, temp)
			for _, asset := range index.Assets {
				verifyPublicURL(asset.URL, asset.SHA256)
			}
			for _, registry := range []string{"conan", "vcpkg"} {
				publishRegistry(registry, "", packageVersion())
			}
		}
		if hasCredential("crates") {
			var m nativeManifest
			jsonRead(path("rust/dist/project/native/manifest.json"), &m)
			for _, e := range m.Libraries {
				verifyPublicURL(e.URL, e.ArchiveSHA256)
			}
			publishRegistry("crates", "", packageVersion())
		}
		if hasCredential("swift") || hasCredential("cocoapods") {
			var inv inventory
			jsonRead(path("swift/dist/manifest.json"), &inv)
			require(inv.XCFramework != nil, "missing Swift release artifact")
			verifyPublicURL(inv.XCFramework.URL, inv.XCFramework.SHA256)
			for _, r := range []string{"swift", "cocoapods"} {
				publishRegistry(r, "", packageVersion())
			}
		}
	default:
		panic("unknown SDK release phase")
	}
}
