// SPDX-License-Identifier: MPL-2.0
package main

import (
	"crypto/sha512"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type cNativeAsset struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
	Archive  string `json:"archive"`
	SHA256   string `json:"sha256"`
	SHA512   string `json:"sha512"`
}
type cNativeIndex struct {
	Version string         `json:"version"`
	Assets  []cNativeAsset `json:"assets"`
}

func packageCNative() {
	out := packageOut("cgo")
	must(os.RemoveAll(out))
	artifacts := filepath.Join(out, "artifacts")
	mkdir(artifacts)
	native := loadNative()
	index := cNativeIndex{Version: packageVersion()}
	for _, lib := range native.Libraries {
		dir := filepath.Join(out, "runtime", lib.Platform)
		system, arch, ok := strings.Cut(lib.Platform, "-")
		require(ok && files[system] != "", "invalid C runtime platform")
		libraryDir := "lib"
		if system == "windows" {
			libraryDir = "bin"
		}
		copyFile(lib.Path, filepath.Join(dir, libraryDir, files[system]))
		for _, name := range []string{"urnetwork_sdk.h", "urnetwork_sdk.hpp", "urnetwork_sdk.def"} {
			copyFile(path("cgo/include", name), filepath.Join(dir, "include", name))
		}
		copyFile(path("LICENSE"), filepath.Join(dir, "share/urnetwork-sdk/copyright"))
		if system == "windows" {
			machine := map[string]string{"amd64": "i386:x86-64", "arm64": "arm64"}[arch]
			require(machine != "", "unsupported Windows machine")
			mkdir(filepath.Join(dir, "lib"))
			command(root, nil, "zig", "dlltool", "-m", machine, "-D", "URnetworkSdk.dll", "-d", path("cgo/include/urnetwork_sdk.def"), "-l", filepath.Join(dir, "lib/URnetworkSdk.lib"))
		}
		config := `get_filename_component(_ur_prefix "${CMAKE_CURRENT_LIST_DIR}/../../.." ABSOLUTE)
if(NOT TARGET urnetwork::sdk)
  add_library(urnetwork::sdk SHARED IMPORTED)
  set_target_properties(urnetwork::sdk PROPERTIES
    IMPORTED_LOCATION "${_ur_prefix}/` + libraryDir + "/" + files[system] + `"
    INTERFACE_INCLUDE_DIRECTORIES "${_ur_prefix}/include")
`
		if system == "windows" {
			config += `  set_target_properties(urnetwork::sdk PROPERTIES IMPORTED_IMPLIB "${_ur_prefix}/lib/URnetworkSdk.lib")
`
		}
		config += `  add_library(urnetwork::sdk-cpp INTERFACE IMPORTED)
  set_target_properties(urnetwork::sdk-cpp PROPERTIES INTERFACE_LINK_LIBRARIES urnetwork::sdk)
endif()
`
		textFile(filepath.Join(dir, "lib/cmake/urnetwork-sdk/urnetwork-sdk-config.cmake"), config)
		textFile(filepath.Join(dir, "lib/cmake/urnetwork-sdk/urnetwork-sdk-config-version.cmake"),
			fmt.Sprintf("set(PACKAGE_VERSION %q)\nif(PACKAGE_FIND_VERSION VERSION_EQUAL PACKAGE_VERSION)\n  set(PACKAGE_VERSION_EXACT TRUE)\n  set(PACKAGE_VERSION_COMPATIBLE TRUE)\nendif()\n", packageVersion()))
		archive := "URnetworkSdk-C-" + packageVersion() + "-" + lib.Platform + ".zip"
		p := filepath.Join(artifacts, archive)
		zipTree(dir, p)
		index.Assets = append(index.Assets, cNativeAsset{Platform: lib.Platform, URL: "https://github.com/urnetwork/build/releases/download/v" + packageVersion() + "/" + archive,
			Archive: archive, SHA256: hash(p), SHA512: fmt.Sprintf("%x", sha512.Sum512(read(p)))})
		if dst := os.Getenv("SDK_C_RELEASE_ASSETS"); dst != "" {
			copyFile(p, filepath.Join(dst, archive))
		}
	}
	indexDir := filepath.Join(out, "recipes")
	copyFile(path("packaging/conanfile.py"), filepath.Join(indexDir, "conan/conanfile.py"))
	jsonWrite(filepath.Join(indexDir, "conan/native.json"), index)
	jsonWrite(filepath.Join(indexDir, "vcpkg/native.json"), index)
	jsonWrite(filepath.Join(indexDir, "vcpkg/vcpkg.json"), map[string]any{"name": "urnetwork-sdk", "version-semver": packageVersion(),
		"description": "URnetwork user-space networking SDK with C and C++ interfaces", "homepage": "https://ur.io", "license": "MPL-2.0", "supports": "(osx | linux | windows) & (x64 | arm64)",
		"dependencies": []string{"nlohmann-json"}})
	copyFile(path("packaging/portfile.cmake"), filepath.Join(indexDir, "vcpkg/portfile.cmake"))
	zipTree(indexDir, filepath.Join(artifacts, "urnetwork-sdk-recipes.zip"))
	if hasCredential("conan") || os.Getenv("SDK_CHECK_CONAN") == "1" {
		buildConanCache(index, filepath.Join(indexDir, "conan"), artifacts)
	}
	public := publicNative(native)
	jsonWrite(filepath.Join(out, "manifest.json"), inventory{Version: packageVersion(), Native: &public, Artifacts: artifactsIn(artifacts)})
}
func unpackCRecipes(artifacts []string, directory string) cNativeIndex {
	for _, file := range artifacts {
		if filepath.Base(file) != "urnetwork-sdk-recipes.zip" {
			continue
		}
		for name, data := range zipFiles(file) {
			write(filepath.Join(directory, name), data)
		}
		var index cNativeIndex
		jsonRead(filepath.Join(directory, "conan/native.json"), &index)
		return index
	}
	panic("missing checked C package recipes")
}
func checkCNative() {
	out := packageOut("cgo")
	_, artifacts := verifiedFiles(out, packageVersion(), false)
	temp, cleanup := temporary("urnetwork-c-package-check-")
	defer cleanup()
	index := unpackCRecipes(artifacts, filepath.Join(temp, "recipes"))
	require(index.Version == packageVersion(), "stale C recipes")
	host := runtime.GOOS + "-" + runtime.GOARCH
	tested := false
	for _, asset := range index.Assets {
		archive := filepath.Join(out, "artifacts", asset.Archive)
		require(hash(archive) == asset.SHA256 && fmt.Sprintf("%x", sha512.Sum512(read(archive))) == asset.SHA512, "C runtime archive hash mismatch")
		if asset.Platform != host {
			continue
		}
		prefix := filepath.Join(temp, "sdk")
		for name, data := range zipFiles(archive) {
			write(filepath.Join(prefix, name), data)
		}
		textFile(filepath.Join(temp, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.24)
project(consumer LANGUAGES C)
find_package(urnetwork-sdk CONFIG REQUIRED)
add_executable(consumer main.c)
target_link_libraries(consumer PRIVATE urnetwork::sdk)
`)
		textFile(filepath.Join(temp, "main.c"), `#include <urnetwork_sdk.h>
#include <stdio.h>
int main(void) { if (urnet_abi_version()!=1) return 1; char* v=urnet_version(); puts(v); urnet_free_string(v); return 0; }
`)
		command(temp, nil, "cmake", "-S", ".", "-B", "build", "-DCMAKE_PREFIX_PATH="+prefix)
		command(temp, nil, "cmake", "--build", "build", "--config", "Release")
		executable := filepath.Join(temp, "build/consumer")
		if runtime.GOOS == "windows" {
			executable = filepath.Join(temp, "build/Release/consumer.exe")
			copyFile(filepath.Join(prefix, "bin/URnetworkSdk.dll"), filepath.Join(temp, "build/Release/URnetworkSdk.dll"))
		}
		command(temp, nil, executable)
		tested = true
	}
	require(tested, "no host runtime for C consumer check")
	for _, file := range artifacts {
		if filepath.Base(file) == "urnetwork-sdk-conan-cache.tgz" {
			conan := conanExecutable()
			e := map[string]string{"CONAN_HOME": filepath.Join(temp, "conan")}
			command(temp, e, conan, "cache", "restore", file)
			command(temp, e, conan, "cache", "check-integrity", "urnetwork-sdk/"+packageVersion()+"#*:*#*")
		}
	}
	markChecked(out)
}

func conanExecutable() string {
	python := buildPython()
	command(root, nil, python, "-m", "pip", "install", "conan>=2.12,<3")
	return filepath.Join(filepath.Dir(python), "conan")
}
func buildConanCache(index cNativeIndex, recipe, artifacts string) {
	temp, cleanup := temporary("urnetwork-conan-package-")
	defer cleanup()
	conan := conanExecutable()
	e := map[string]string{"CONAN_HOME": filepath.Join(temp, "cache"), "SDK_CONAN_ASSET_CACHE": artifacts}
	command(temp, e, conan, "profile", "detect", "--force")
	for _, asset := range index.Assets {
		system, arch, _ := strings.Cut(asset.Platform, "-")
		command(temp, e, conan, "create", recipe, "--version="+index.Version, "--build=missing", "-tf=",
			"-s", "os="+map[string]string{"darwin": "Macos", "linux": "Linux", "windows": "Windows"}[system],
			"-s", "arch="+map[string]string{"amd64": "x86_64", "arm64": "armv8"}[arch], "-cc", "core:non_interactive=True")
	}
	list := filepath.Join(artifacts, "urnetwork-sdk-conan-list.json")
	command(temp, e, conan, "list", "urnetwork-sdk/"+index.Version+"#*:*#*", "--format=json", "--out-file="+list)
	command(temp, e, conan, "cache", "save", "--list="+list, "--file="+filepath.Join(artifacts, "urnetwork-sdk-conan-cache.tgz"))
}
func publishConan(artifacts []string) {
	temp, cleanup := temporary("urnetwork-conan-publish-")
	defer cleanup()
	conan := conanExecutable()
	e := map[string]string{"CONAN_HOME": filepath.Join(temp, "cache")}
	archive, list := "", ""
	for _, file := range artifacts {
		switch filepath.Base(file) {
		case "urnetwork-sdk-conan-cache.tgz":
			archive = file
		case "urnetwork-sdk-conan-list.json":
			list = file
		}
	}
	require(archive != "" && list != "", "Conan publishing requires checked recipe and binary cache")
	remote := os.Getenv("SDK_CONAN_REMOTE_URL")
	require(strings.HasPrefix(remote, "https://"), "set SDK_CONAN_REMOTE_URL to the first-party HTTPS Conan remote")
	command(temp, e, conan, "cache", "restore", archive)
	command(temp, e, conan, "remote", "add", "urnetwork", remote)
	command(temp, e, conan, "upload", "--list="+list, "-r", "urnetwork", "--check", "--confirm", "-cc", "core:non_interactive=True")
}
func publishVcpkg(artifacts []string, version string) {
	temp, cleanup := temporary("urnetwork-vcpkg-publish-")
	defer cleanup()
	recipes := filepath.Join(temp, "recipes")
	unpackCRecipes(artifacts, recipes)
	repo := env("SDK_VCPKG_REPOSITORY", "urnetwork/vcpkg-registry")
	require(repositoryName.MatchString(repo), "SDK_VCPKG_REPOSITORY must be owner/repo")
	self, err := os.Executable()
	must(err)
	e := map[string]string{"GIT_ASKPASS": self, "SDK_GIT_ASKPASS": "1", "SDK_GIT_TOKEN_VARIABLE": "SDK_VCPKG_GIT_TOKEN", "GIT_TERMINAL_PROMPT": "0"}
	work := filepath.Join(temp, "repo")
	command(temp, e, "git", "clone", "--quiet", "https://github.com/"+repo+".git", work)
	versionsPath := filepath.Join(work, "versions/u-/urnetwork-sdk.json")
	versions := map[string][]map[string]any{"versions": {}}
	if exists(versionsPath) {
		jsonRead(versionsPath, &versions)
	}
	for _, v := range versions["versions"] {
		require(v["version-semver"] != version, "vcpkg version already exists")
	}
	port := filepath.Join(work, "ports/urnetwork-sdk")
	must(os.RemoveAll(port))
	copyTree(filepath.Join(recipes, "vcpkg"), port, nil)
	command(work, e, "git", "add", "--", "ports/urnetwork-sdk")
	command(work, e, "git", "-c", "user.name=URnetwork Release", "-c", "user.email=support@ur.io", "commit", "-m", "SDK port "+version)
	tree := strings.TrimSpace(string(output(work, e, "git", "rev-parse", "HEAD:ports/urnetwork-sdk")))
	versions["versions"] = append([]map[string]any{{"version-semver": version, "port-version": 0, "git-tree": tree}}, versions["versions"]...)
	jsonWrite(versionsPath, versions)
	baselinePath := filepath.Join(work, "versions/baseline.json")
	baselines := map[string]map[string]any{"default": {}}
	if exists(baselinePath) {
		jsonRead(baselinePath, &baselines)
	}
	if baselines["default"] == nil {
		baselines["default"] = map[string]any{}
	}
	baselines["default"]["urnetwork-sdk"] = map[string]any{"baseline": version, "port-version": 0}
	jsonWrite(baselinePath, baselines)
	command(work, e, "git", "add", "--", "versions/u-/urnetwork-sdk.json", "versions/baseline.json")
	command(work, e, "git", "-c", "user.name=URnetwork Release", "-c", "user.email=support@ur.io", "commit", "-m", "Index SDK "+version)
	// Fast-forward only; concurrent releases cause a push failure, not an overwrite.
	command(work, e, "git", "push", "origin", "HEAD")
}
