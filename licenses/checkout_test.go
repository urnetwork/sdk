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
	oldSDK, oldRoot, oldMMM, oldExtra, oldOffline := sdkDir, rootDir, mmmDir, extra, offline
	t.Cleanup(func() {
		sdkDir, rootDir, mmmDir, extra, offline = oldSDK, oldRoot, oldMMM, oldExtra, oldOffline
	})
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
			sdk := filepath.Join(root, "build", "sdk")
			write := func(path, contents string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(sdk, "go.mod"), "module github.com/urnetwork/sdk\n\ngo 1.26.7\n")
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
		})
	}
}
