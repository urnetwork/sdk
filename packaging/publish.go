// SPDX-License-Identifier: MPL-2.0
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var registryOrder = []string{"npm", "pypi", "nuget", "rubygems", "crates", "maven", "swift", "cocoapods", "conan", "vcpkg"}
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var registryCredentials = map[string][]string{
	"npm": {"NPM_TOKEN", "NODE_AUTH_TOKEN"}, "pypi": {"TWINE_PASSWORD", "PYPI_TOKEN"},
	"nuget": {"NUGET_API_KEY"}, "rubygems": {"GEM_HOST_API_KEY"}, "crates": {"CARGO_REGISTRY_TOKEN"},
	"maven": {"MAVEN_CENTRAL_USERNAME", "MAVEN_CENTRAL_PASSWORD", "SDK_GPG_KEY_ID"},
	"swift": {"SDK_SWIFT_GIT_TOKEN"}, "cocoapods": {"COCOAPODS_TRUNK_TOKEN"},
	"conan": {"CONAN_LOGIN_USERNAME", "CONAN_PASSWORD"}, "vcpkg": {"SDK_VCPKG_GIT_TOKEN"},
}
var registryLanguage = map[string]string{"npm": "js", "pypi": "python", "nuget": "csharp", "rubygems": "ruby", "crates": "rust", "maven": "java", "swift": "swift", "cocoapods": "swift", "conan": "cgo", "vcpkg": "cgo"}

func hasCredential(registry string) bool {
	names, ok := registryCredentials[registry]
	require(ok, "unknown package registry: %s", registry)
	if registry == "maven" || registry == "conan" {
		for _, name := range names {
			if strings.TrimSpace(os.Getenv(name)) == "" {
				return false
			}
		}
		return true
	}
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	if registry == "npm" {
		return npmCredentialConfig() != ""
	}
	return false
}

func npmCredentialConfig() string {
	home, e := os.UserHomeDir()
	must(e)
	for _, file := range []string{path("js/.npmrc"), env("NPM_CONFIG_USERCONFIG", env("npm_config_userconfig", filepath.Join(home, ".npmrc")))} {
		if !exists(file) {
			continue
		}
		for _, line := range strings.Split(string(read(file)), "\n") {
			line = os.Expand(strings.TrimSpace(line), os.Getenv)
			if regexp.MustCompile(`^(?://registry\.npmjs\.org/:)?(?:_authToken|_auth)\s*=\s*\S+`).MatchString(line) {
				return file
			}
		}
	}
	return ""
}

type registryClient struct{ client *http.Client }

func newRegistryClient() registryClient {
	return registryClient{&http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("authenticated upload unexpectedly redirected")
	}}}
}
func (c registryClient) request(endpoint, method string, data []byte, headers map[string]string) []byte {
	u, e := url.Parse(endpoint)
	must(e)
	require(u.Scheme == "https" && u.Host != "", "publication requires HTTPS")
	req, e := http.NewRequest(method, endpoint, bytes.NewReader(data))
	must(e)
	req.Header.Set("User-Agent", "urnetwork-sdk-release/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, e := c.client.Do(req)
	must(e)
	defer response.Body.Close()
	// Never echo untrusted error bodies; a server could reflect an auth header.
	require(response.StatusCode >= 200 && response.StatusCode < 300, "publication to %s returned HTTP %d", u.Host, response.StatusCode)
	b, e := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	must(e)
	return b
}
func multipartFile(field, p string) ([]byte, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filepath.Base(p)))
	header.Set("Content-Type", "application/octet-stream")
	part, e := w.CreatePart(header)
	must(e)
	f, e := os.Open(p)
	must(e)
	_, e = io.Copy(part, f)
	must(e)
	must(f.Close())
	must(w.Close())
	return b.Bytes(), w.FormDataContentType()
}
func publishNpm(artifacts []string) {
	temp, cleanup := temporary("urnetwork-npm-auth-")
	defer cleanup()
	e := map[string]string{}
	token := "NPM_TOKEN"
	if os.Getenv(token) == "" {
		token = "NODE_AUTH_TOKEN"
	}
	if os.Getenv(token) != "" {
		config := filepath.Join(temp, "npmrc")
		textFile(config, "//registry.npmjs.org/:_authToken=$"+"{"+token+"}\n")
		must(os.Chmod(config, 0600))
		e["NPM_CONFIG_USERCONFIG"] = config
	} else if config := npmCredentialConfig(); config != "" {
		// Publication runs in a temporary directory, so npm would otherwise
		// lose the project npmrc which enabled this publishing stage.
		e["NPM_CONFIG_USERCONFIG"] = config
	}
	for _, p := range artifacts {
		if strings.HasSuffix(p, ".tgz") {
			command(temp, e, "npm", "publish", p, "--ignore-scripts", "--access", "public", "--registry", "https://registry.npmjs.org", "--tag", env("SDK_PACKAGE_CHANNEL", "nightly"))
		}
	}
}
func publishPyPI(artifacts []string) {
	python := buildPython()
	command(root, nil, python, "-m", "pip", "install", "twine>=6,<7")
	e := map[string]string{"TWINE_USERNAME": "__token__", "TWINE_PASSWORD": env("TWINE_PASSWORD", os.Getenv("PYPI_TOKEN")), "TWINE_NON_INTERACTIVE": "1"}
	wheels := []string{}
	for _, p := range artifacts {
		if strings.HasSuffix(p, ".whl") {
			wheels = append(wheels, p)
		}
	}
	require(len(wheels) > 0, "PyPI requires wheels")
	// The developer sdist requires the source checkout; it is not a public install.
	command(root, e, python, append([]string{"-m", "twine", "check"}, wheels...)...)
	command(root, e, python, append([]string{"-m", "twine", "upload", "--non-interactive", "--repository-url", "https://upload.pypi.org/legacy/"}, wheels...)...)
}
func (c registryClient) publishNuget(artifacts []string) {
	found := false
	for _, p := range artifacts {
		if strings.HasSuffix(p, ".nupkg") {
			found = true
			b, contentType := multipartFile("package", p)
			c.request("https://www.nuget.org/api/v2/package", "PUT", b, map[string]string{"X-NuGet-ApiKey": os.Getenv("NUGET_API_KEY"), "Content-Type": contentType})
		}
	}
	require(found, "missing NuGet package")
}
func (c registryClient) publishRuby(artifacts []string) {
	found := false
	for _, p := range artifacts {
		if strings.HasSuffix(p, ".gem") {
			found = true
			c.request("https://rubygems.org/api/v1/gems", "POST", read(p), map[string]string{"Authorization": os.Getenv("GEM_HOST_API_KEY"), "Content-Type": "application/octet-stream"})
		}
	}
	require(found, "missing RubyGems package")
}

type cargoPackage struct {
	Name          string              `json:"name"`
	Version       string              `json:"version"`
	Authors       []string            `json:"authors"`
	Description   *string             `json:"description"`
	Documentation *string             `json:"documentation"`
	Homepage      *string             `json:"homepage"`
	Readme        *string             `json:"readme"`
	Keywords      []string            `json:"keywords"`
	Categories    []string            `json:"categories"`
	License       *string             `json:"license"`
	LicenseFile   *string             `json:"license_file"`
	Repository    *string             `json:"repository"`
	Links         *string             `json:"links"`
	RustVersion   *string             `json:"rust_version"`
	Features      map[string][]string `json:"features"`
	Dependencies  []struct {
		Name            string   `json:"name"`
		Req             string   `json:"req"`
		Features        []string `json:"features"`
		Optional        bool     `json:"optional"`
		DefaultFeatures bool     `json:"uses_default_features"`
		Target          *string  `json:"target"`
		Kind            *string  `json:"kind"`
		Registry        *string  `json:"registry"`
		Rename          *string  `json:"rename"`
		Path            *string  `json:"path"`
		Source          *string  `json:"source"`
	} `json:"dependencies"`
}

func (crate cargoPackage) publishMetadata(dir string) map[string]any {
	deps := []map[string]any{}
	for _, d := range crate.Dependencies {
		require(d.Path == nil && d.Source != nil && strings.HasPrefix(*d.Source, "registry+"), "crate has a non-registry dependency")
		kind := "normal"
		if d.Kind != nil {
			kind = *d.Kind
		}
		deps = append(deps, map[string]any{"name": d.Name, "version_req": d.Req, "features": d.Features, "optional": d.Optional, "default_features": d.DefaultFeatures, "target": d.Target, "kind": kind, "registry": d.Registry, "explicit_name_in_toml": d.Rename})
	}
	meta := map[string]any{"name": crate.Name, "vers": crate.Version, "deps": deps, "features": crate.Features, "authors": crate.Authors,
		"description": crate.Description, "documentation": crate.Documentation, "homepage": crate.Homepage, "keywords": crate.Keywords, "categories": crate.Categories,
		"license": crate.License, "license_file": crate.LicenseFile, "repository": crate.Repository, "links": crate.Links, "rust_version": crate.RustVersion, "badges": map[string]any{}}
	if crate.Readme != nil {
		require(filepath.IsLocal(*crate.Readme), "invalid crate readme")
		meta["readme_file"] = *crate.Readme
		meta["readme"] = string(read(filepath.Join(dir, *crate.Readme)))
	}
	return meta
}
func crateMetadata(p string) map[string]any {
	temp, cleanup := temporary("urnetwork-crate-metadata-")
	defer cleanup()
	dir := ""
	for name, b := range tarFiles(p) {
		write(filepath.Join(temp, name), b)
		if strings.Count(name, "/") == 1 && strings.HasSuffix(name, "/Cargo.toml") {
			dir = filepath.Dir(filepath.Join(temp, name))
		}
	}
	require(dir != "", "crate manifest is missing")
	var metadata struct {
		Packages []cargoPackage `json:"packages"`
	}
	must(json.Unmarshal(output(dir, nil, "cargo", "metadata", "--format-version", "1", "--no-deps", "--offline"), &metadata))
	require(len(metadata.Packages) == 1, "unexpected crate metadata")
	return metadata.Packages[0].publishMetadata(dir)
}
func (c registryClient) publishCrates(artifacts []string) {
	found := false
	for _, p := range artifacts {
		if strings.HasSuffix(p, ".crate") {
			found = true
			data := read(p)
			metadata, e := json.Marshal(crateMetadata(p))
			must(e)
			payload := make([]byte, 0, len(data)+len(metadata)+8)
			payload = binary.LittleEndian.AppendUint32(payload, uint32(len(metadata)))
			payload = append(payload, metadata...)
			payload = binary.LittleEndian.AppendUint32(payload, uint32(len(data)))
			payload = append(payload, data...)
			b := c.request("https://crates.io/api/v1/crates/new", "PUT", payload, map[string]string{"Authorization": os.Getenv("CARGO_REGISTRY_TOKEN"), "Content-Type": "application/octet-stream", "Accept": "application/json"})
			var result struct {
				Errors []any `json:"errors"`
			}
			must(json.Unmarshal(b, &result))
			require(len(result.Errors) == 0, "crates.io rejected the package")
		}
	}
	require(found, "missing Rust crate")
}
func mavenBundle(artifacts []string, version, dir string) string {
	temp, cleanup := temporary("urnetwork-central-")
	defer cleanup()
	found := false
	for _, p := range artifacts {
		ext := filepath.Ext(p)
		if ext != ".jar" && ext != ".aar" && ext != ".pom" {
			continue
		}
		found = true
		artifact := "urnetwork-sdk"
		if strings.HasPrefix(filepath.Base(p), "urnetwork-sdk-android-") {
			artifact = "urnetwork-sdk-android"
		}
		target := filepath.Join(temp, "io/ur", artifact, version, filepath.Base(p))
		copyFile(p, target)
		args := []string{"--batch", "--yes", "--armor", "--detach-sign", "--local-user", os.Getenv("SDK_GPG_KEY_ID"), "--output", target + ".asc"}
		if password, ok := os.LookupEnv("SDK_GPG_PASSPHRASE"); ok {
			args = append(args, "--pinentry-mode", "loopback", "--passphrase-fd", "0", target)
			cmd := exec.Command("gpg", args...)
			cmd.Stdin = strings.NewReader(password + "\n")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			must(cmd.Run())
		} else {
			command(root, nil, "gpg", append(args, target)...)
		}
		// Central requires checksums and detached signatures. Include stronger
		// hashes alongside the mandated legacy MD5/SHA1 metadata.
		for _, file := range []string{target, target + ".asc"} {
			b := read(file)
			textFile(file+".md5", fmt.Sprintf("%x", md5.Sum(b)))
			textFile(file+".sha1", fmt.Sprintf("%x", sha1.Sum(b)))
			textFile(file+".sha256", fmt.Sprintf("%x", sha256.Sum256(b)))
			textFile(file+".sha512", fmt.Sprintf("%x", sha512.Sum512(b)))
		}
	}
	require(found, "missing Maven artifacts")
	bundle := filepath.Join(dir, "central-bundle.zip")
	zipTree(temp, bundle)
	return bundle
}
func (c registryClient) publishMaven(artifacts []string, version, dir string) {
	bundle := mavenBundle(artifacts, version, dir)
	b, contentType := multipartFile("bundle", bundle)
	token := base64.StdEncoding.EncodeToString([]byte(os.Getenv("MAVEN_CENTRAL_USERNAME") + ":" + os.Getenv("MAVEN_CENTRAL_PASSWORD")))
	headers := map[string]string{"Authorization": "Bearer " + token, "Content-Type": contentType}
	deployment := strings.TrimSpace(string(c.request("https://central.sonatype.com/api/v1/publisher/upload?publishingType=AUTOMATIC", "POST", b, headers)))
	require(regexp.MustCompile(`^[A-Za-z0-9-]+$`).MatchString(deployment), "invalid Central deployment ID")
	jsonWrite(filepath.Join(dir, "central-deployment.json"), map[string]string{"id": deployment, "bundle_sha256": hash(bundle)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	for {
		var status struct {
			State string `json:"deploymentState"`
		}
		must(json.Unmarshal(c.request("https://central.sonatype.com/api/v1/publisher/status?id="+url.QueryEscape(deployment), "POST", nil, map[string]string{"Authorization": "Bearer " + token}), &status))
		if status.State == "PUBLISHED" {
			return
		}
		require(status.State != "FAILED", "Maven Central deployment failed validation: %s", deployment)
		fmt.Println("Maven Central deployment", deployment+":", status.State)
		select {
		case <-ctx.Done():
			panic(fmt.Errorf("Maven Central deployment still pending: %s", deployment))
		case <-time.After(10 * time.Second):
		}
	}
}
func publishSwift(artifacts []string, version string) {
	repo := env("SDK_SWIFT_REPOSITORY", "urnetwork/sdk-swift")
	require(regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo), "SDK_SWIFT_REPOSITORY must be owner/repo")
	temp, cleanup := temporary("urnetwork-swift-publish-")
	defer cleanup()
	self, e := os.Executable()
	must(e)
	environment := map[string]string{"GIT_ASKPASS": self, "SDK_GIT_ASKPASS": "1", "GIT_TERMINAL_PROMPT": "0"}
	work := filepath.Join(temp, "repo")
	command(temp, environment, "git", "clone", "--quiet", "--depth=1", "https://github.com/"+repo+".git", work)
	cmd := exec.Command("git", "ls-remote", "--exit-code", "--tags", "origin", "refs/tags/"+version)
	cmd.Dir = work
	cmd.Env = withEnv(environment)
	err := cmd.Run()
	exit, ok := err.(*exec.ExitError)
	require(ok && exit.ExitCode() == 2, "Swift tag already exists or remote could not be checked")
	names := []string{}
	for _, p := range artifacts {
		copyFile(p, filepath.Join(work, filepath.Base(p)))
		names = append(names, filepath.Base(p))
	}
	command(work, environment, "git", append([]string{"add", "--"}, names...)...)
	command(work, environment, "git", "-c", "user.name=URnetwork Release", "-c", "user.email=support@ur.io", "commit", "--allow-empty", "-m", "SDK "+version)
	command(work, environment, "git", "tag", version)
	command(work, environment, "git", "push", "origin", "refs/tags/"+version)
}
func publishRegistry(registry, dir, version string) {
	if !hasCredential(registry) {
		fmt.Printf("SKIP %s: no publishing credentials (%s)\n", registry, strings.Join(registryCredentials[registry], ", "))
		return
	}
	if dir == "" {
		dir = path(registryLanguage[registry], "dist")
		if registry == "npm" {
			dir = path("js/release")
		}
	}
	_, artifacts := verifiedFiles(dir, version, true)
	client := newRegistryClient()
	switch registry {
	case "npm":
		publishNpm(artifacts)
	case "pypi":
		publishPyPI(artifacts)
	case "nuget":
		client.publishNuget(artifacts)
	case "rubygems":
		client.publishRuby(artifacts)
	case "crates":
		client.publishCrates(artifacts)
	case "maven":
		client.publishMaven(artifacts, version, dir)
	case "swift":
		publishSwift(artifacts, version)
	case "conan":
		publishConan(artifacts)
	case "vcpkg":
		publishVcpkg(artifacts, version)
	case "cocoapods":
		found := false
		for _, p := range artifacts {
			if strings.HasSuffix(p, ".podspec") {
				found = true
				command(root, nil, "pod", "trunk", "push", p, "--allow-warnings")
			}
		}
		require(found, "missing CocoaPods spec")
	}
	jsonWrite(filepath.Join(dir, "published-"+registry+".json"), map[string]any{"registry": registry, "version": version, "manifest_sha256": hash(filepath.Join(dir, "manifest.json"))})
	fmt.Println("Published", registry, version)
}
