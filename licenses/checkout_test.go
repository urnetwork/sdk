package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebLicenseCheckSelectsMMMCheckout(t *testing.T) {
	oldSDK, oldRepo, oldRoot, oldMMM, oldExtra, oldOffline := sdkDir, sdkRepoDir, rootDir, mmmDir, extra, offline
	t.Cleanup(func() {
		sdkDir, sdkRepoDir, rootDir, mmmDir, extra, offline = oldSDK, oldRepo, oldRoot, oldMMM, oldExtra, oldOffline
	})
	for _, moduleSuffix := range []string{"", "v2026"} {
		t.Run("module="+moduleSuffix, func(t *testing.T) {
			for _, test := range []struct {
				name                       string
				explicit, relative, absent bool
				version, wantError         string
			}{
				{name: "default sibling", version: "1.0.0"},
				{name: "external checkout", explicit: true, version: "1.0.0"},
				{name: "relative external checkout", explicit: true, relative: true, version: "1.0.0"},
				{name: "external drift remains fatal", explicit: true, version: "2.0.0", wantError: "missing npm-web fixture-web 2.0.0"},
				{name: "missing explicit checkout cannot use sibling", explicit: true, absent: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					root := t.TempDir()
					sdk := filepath.Join(root, "build", "sdk", moduleSuffix)
					write := func(path, contents string) {
						t.Helper()
						if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					module := "module github.com/urnetwork/sdk"
					if moduleSuffix != "" {
						module += "/" + moduleSuffix
					}
					write(filepath.Join(sdk, "go.mod"), module+"\n\ngo 1.26.7\n")
					write(filepath.Join(sdk, "licenses", "extra.yml"), "{}\n")
					write(filepath.Join(sdk, "license.yml"), `entries:
  - name: fixture-web
    version: 1.0.0
    kind: software
    origin: npm-web
    apps: [web]
    spdx: MIT
    text: mit
  - name: fixture-extension
    version: 1.0.0
    kind: software
    origin: npm-extension
    apps: [extension]
    spdx: MIT
    text: mit
texts:
  mit: "MIT License\nPermission is hereby granted"
`)
					lock := func(name, version string) string {
						return fmt.Sprintf(`{"packages":{"":{"dependencies":{%q:%q}},"node_modules/%s":{"version":%q,"license":"MIT"}}}`, name, version, name, version)
					}
					writeSite := func(checkout, version string) {
						write(filepath.Join(checkout, "ur.io", "react", "package-lock.json"), lock("fixture-web", version))
						write(filepath.Join(checkout, "ur.io", "astro", "package-lock.json"), `{"packages":{"":{}}}`)
					}
					// A matching sibling is a decoy for explicit overrides: it must never
					// hide missing inputs or dependency drift in the selected checkout.
					writeSite(filepath.Join(root, "build", "mmm"), "1.0.0")
					write(filepath.Join(root, "build", "extension", "package-lock.json"), lock("fixture-extension", "1.0.0"))
					checkout := ""
					if test.explicit {
						checkout = filepath.Join(root, "mmm")
						if !test.absent {
							writeSite(checkout, test.version)
						}
						if test.relative {
							var err error
							checkout, err = filepath.Rel(sdk, checkout)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					t.Chdir(sdk)
					err := run("web", "", checkout)
					if test.absent {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("missing explicit checkout error = %v, want os.ErrNotExist", err)
						}
					} else if test.wantError == "" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
						t.Fatalf("web check error = %v, want %q", err, test.wantError)
					}
					// The site override must not redirect the other app repositories.
					if err := run("extension", "", checkout); err != nil {
						t.Fatalf("site override changed the extension checkout: %v", err)
					}
					write(filepath.Join(root, "build", "extension", "package-lock.json"), lock("fixture-extension", "2.0.0"))
					if err := run("extension", "", checkout); err == nil || !strings.Contains(err.Error(), "missing npm-extension fixture-extension 2.0.0") {
						t.Fatalf("extension dependency drift must still fail: %v", err)
					}
				})
			}
		})
	}
}

func TestFindSdkDirs(t *testing.T) {
	for _, test := range []struct {
		name, module, suffix, subdir string
		invalid                      bool
	}{
		{name: "main", module: "module github.com/urnetwork/sdk\n"},
		{name: "main from licenses", module: "module github.com/urnetwork/sdk\n", subdir: "licenses"},
		{name: "release", module: "module github.com/urnetwork/sdk/v2026\n", suffix: "v2026"},
		{name: "future release from licenses", module: "module github.com/urnetwork/sdk/v2027\n", suffix: "v2027", subdir: "licenses"},
		{name: "versioned before fork", module: "module github.com/urnetwork/sdk/v2026\n"},
		{name: "unrelated module", module: "module example.com/sdk\n", invalid: true},
		{name: "nested build module is not root", module: "module github.com/urnetwork/sdk/js\n", invalid: true},
		{name: "version prefix alone is not root", module: "module github.com/urnetwork/sdk/v2026/js\n", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repoDir := filepath.Join(t.TempDir(), "sdk")
			moduleDir := filepath.Join(repoDir, test.suffix)
			cwd := filepath.Join(moduleDir, test.subdir)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte(test.module), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			gotModule, gotRepo, err := findSdkDirs()
			if test.invalid {
				if err == nil {
					t.Fatal("unrelated module accepted as SDK root")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotModule != moduleDir || gotRepo != repoDir {
				t.Fatalf("got module %s, checkout %s; want %s, %s", gotModule, gotRepo, moduleDir, repoDir)
			}
		})
	}
}

func TestGoTargetsKeepBuildModulesAtCheckoutRoot(t *testing.T) {
	oldSDK, oldRepo := sdkDir, sdkRepoDir
	t.Cleanup(func() { sdkDir, sdkRepoDir = oldSDK, oldRepo })
	for _, suffix := range []string{"", "v2026"} {
		sdkRepoDir = filepath.Join(t.TempDir(), "sdk")
		sdkDir = filepath.Join(sdkRepoDir, suffix)
		for _, target := range goTargets {
			want := filepath.Join(sdkRepoDir, target.dir)
			if target.dir == "." {
				want = sdkDir
			}
			if got := target.moduleDir(); got != want {
				t.Errorf("module %q target %q directory = %s, want %s", suffix, target.dir, got, want)
			}
		}
	}
}
