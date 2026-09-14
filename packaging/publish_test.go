// SPDX-License-Identifier: MPL-2.0
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func panicMessage(f func()) (message string) {
	defer func() {
		if value := recover(); value != nil {
			message = fmt.Sprint(value)
		}
	}()
	f()
	return
}
func isolateCredentials(t *testing.T) {
	t.Helper()
	for _, names := range registryCredentials {
		for _, name := range names {
			t.Setenv(name, "")
		}
	}
	t.Setenv("NPM_CONFIG_USERCONFIG", filepath.Join(t.TempDir(), "empty.npmrc"))
	old := root
	root = t.TempDir()
	t.Cleanup(func() { root = old })
}
func fixturePackage(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "artifacts", name)
	textFile(p, "the checked artifact")
	jsonWrite(filepath.Join(dir, "manifest.json"), inventory{Version: "2026.9.14-123", Artifacts: artifactsIn(filepath.Dir(p))})
	markChecked(dir)
	return dir, p
}
func TestEveryRegistrySkipsWithoutCredentialsBeforeReadingArtifacts(t *testing.T) {
	isolateCredentials(t)
	for _, registry := range registryOrder {
		t.Run(registry, func(t *testing.T) {
			if hasCredential(registry) {
				t.Fatal("unexpected publishing credential")
			}
			if failure := panicMessage(func() { publishRegistry(registry, "/missing-artifacts", "2026.9.14-123") }); failure != "" {
				t.Fatal(failure)
			}
		})
	}
}
func TestRegistryCredentialsIndependentAndMavenRequiresAll(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("PYPI_TOKEN", "test-only-token")
	if !hasCredential("pypi") || hasCredential("nuget") {
		t.Fatal("registry credentials were mixed")
	}
	t.Setenv("MAVEN_CENTRAL_USERNAME", "user")
	t.Setenv("MAVEN_CENTRAL_PASSWORD", "password")
	if hasCredential("maven") {
		t.Fatal("Maven enabled without signing key")
	}
	t.Setenv("SDK_GPG_KEY_ID", "test-key")
	if !hasCredential("maven") {
		t.Fatal("complete Maven credentials ignored")
	}
}
func TestNpmLoginAndEnvironmentExpansion(t *testing.T) {
	isolateCredentials(t)
	file := os.Getenv("NPM_CONFIG_USERCONFIG")
	textFile(file, "//registry.npmjs.org/:_authToken=$"+"{NPM_TOKEN}\n")
	if hasCredential("npm") {
		t.Fatal("unset token placeholder treated as a credential")
	}
	textFile(file, "//registry.npmjs.org/:_authToken=test-only-token\n")
	if !hasCredential("npm") {
		t.Fatal("existing npm login ignored")
	}
	textFile(file, "//unrelated.example/:_authToken=test-only-token\n")
	if hasCredential("npm") {
		t.Fatal("credential for unrelated registry enabled npmjs publication")
	}
}

func TestNpmBuildAndPublishUseSourceVersionWithoutReleaseOverride(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("SDK_PACKAGE_VERSION", "")
	t.Setenv("EXTERNAL_WARP_VERSION", "")
	textFile(path("js/package.json"), `{"version":"0.0.1-beta.7"}`)
	if npmPackageVersion() != "0.0.1-beta.7" {
		t.Fatal("npm did not retain the source package version")
	}
	t.Setenv("EXTERNAL_WARP_VERSION", "2026.9.14-123")
	if npmPackageVersion() != "2026.9.14-123" {
		t.Fatal("external release version ignored")
	}
	t.Setenv("SDK_PACKAGE_VERSION", "2026.9.14-124")
	if npmPackageVersion() != "2026.9.14-124" {
		t.Fatal("explicit SDK package version ignored")
	}
}
func TestPublicationRejectsChangedArtifactsManifestsAndVersions(t *testing.T) {
	dir, p := fixturePackage(t, "example.whl")
	_, files := verifiedFiles(dir, "2026.9.14-123", true)
	if len(files) != 1 {
		t.Fatal(files)
	}
	textFile(p, "tampered package")
	if panicMessage(func() { verifiedFiles(dir, "2026.9.14-123", true) }) == "" {
		t.Fatal("modified package was accepted")
	}
	textFile(p, "the checked artifact")
	if panicMessage(func() { verifiedFiles(dir, "2026.9.14-124", true) }) == "" {
		t.Fatal("wrong release was accepted")
	}
	textFile(filepath.Join(dir, "manifest.json"), string(read(filepath.Join(dir, "manifest.json")))+" ")
	if panicMessage(func() { verifiedFiles(dir, "2026.9.14-123", true) }) == "" {
		t.Fatal("changed manifest was accepted")
	}
}
func TestAuthenticatedHTTPRejectsErrorsAndRedirectsWithoutPrintingSecrets(t *testing.T) {
	for _, status := range []int{401, 500, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == 302 {
					w.Header().Set("Location", "https://example.invalid")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, r.Header.Get("Authorization"))
			}))
			defer server.Close()
			client := newRegistryClient()
			client.client.Transport = server.Client().Transport
			failure := panicMessage(func() {
				client.request(server.URL, "POST", nil, map[string]string{"Authorization": "do-not-print-this-test-secret"})
			})
			if failure == "" || strings.Contains(failure, "do-not-print") {
				t.Fatalf("unsafe error: %q", failure)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestNugetAndRubyUploadExactCheckedBytes(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("NUGET_API_KEY", "nuget-test")
	t.Setenv("GEM_HOST_API_KEY", "ruby-test")
	dir, p := fixturePackage(t, "sdk.nupkg")
	_, artifacts := verifiedFiles(dir, "2026.9.14-123", true)
	requests := 0
	c := registryClient{&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		var body []byte
		if r.URL.Host == "www.nuget.org" {
			if r.Method != "PUT" || r.Header.Get("X-NuGet-ApiKey") != "nuget-test" {
				t.Fatal("incorrect NuGet authentication/method")
			}
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			reader := multipart.NewReader(r.Body, params["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			if part.FormName() != "package" || part.FileName() != "sdk.nupkg" {
				t.Fatal("invalid NuGet multipart")
			}
			body, _ = io.ReadAll(part)
		} else {
			if r.URL.Host != "rubygems.org" || r.Method != "POST" || r.Header.Get("Authorization") != "ruby-test" {
				t.Fatal("incorrect RubyGems request")
			}
			body, _ = io.ReadAll(r.Body)
		}
		if !bytes.Equal(body, read(p)) {
			t.Fatal("upload differs from checked artifact")
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	c.publishNuget(artifacts)
	gem := filepath.Join(dir, "sdk.gem")
	copyFile(p, gem)
	c.publishRuby([]string{gem})
	if requests != 2 {
		t.Fatal(requests)
	}
}
func TestRegistryWireMetadataIncludesBuildDependenciesAndRenames(t *testing.T) {
	dir := t.TempDir()
	textFile(filepath.Join(dir, "README.md"), "SDK")
	var crate cargoPackage
	must(json.Unmarshal([]byte(`{"name":"urnetwork-sdk","version":"1.2.3","readme":"README.md","features":{},"dependencies":[{"name":"ureq","req":"^3","source":"registry+https://github.com/rust-lang/crates.io-index","features":[],"uses_default_features":true,"kind":"build","rename":"http"}]}`), &crate))
	meta := crate.publishMetadata(dir)
	deps := meta["deps"].([]map[string]any)
	if deps[0]["kind"] != "build" || *deps[0]["explicit_name_in_toml"].(*string) != "http" {
		t.Fatal(deps)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	payload := binary.LittleEndian.AppendUint32(nil, uint32(len(b)))
	payload = append(payload, b...)
	if int(binary.LittleEndian.Uint32(payload)) != len(b) {
		t.Fatal("invalid registry length prefix")
	}
}
func TestWheelFloorsAndVersionMapping(t *testing.T) {
	tests := []struct {
		entry library
		want  string
	}{
		{library{Platform: "darwin-arm64", MinimumOS: "13.5"}, "macosx_14_0_arm64"},
		{library{Platform: "darwin-amd64", MinimumOS: "13.0"}, "macosx_13_0_x86_64"},
		{library{Platform: "linux-amd64", MinimumOS: "glibc-2.35"}, "manylinux_2_35_x86_64"},
		{library{Platform: "windows-arm64", MinimumOS: "windows-10"}, "win_arm64"},
	}
	for _, test := range tests {
		if got := wheelPlatform(test.entry); got != test.want {
			t.Fatalf("got %s, want %s", got, test.want)
		}
	}
	if got := languageVersion("python", "2026.9.14-1046068620"); got != "2026.9.14.dev1046068620" {
		t.Fatal(got)
	}
	if got := languageVersion("python", "0.0.1-dev.0"); got != "0.0.1.dev0" {
		t.Fatal(got)
	}
	if got := languageVersion("ruby", "2026.9.14-12"); got != "2026.9.14.pre.12" {
		t.Fatal(got)
	}
	if panicMessage(func() { wheelPlatform(library{Platform: "linux-arm64", MinimumOS: "musl"}) }) == "" {
		t.Fatal("false glibc claim")
	}
}
