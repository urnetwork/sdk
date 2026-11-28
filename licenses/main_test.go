package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPolicyExpressionAllowed(t *testing.T) {
	cases := []struct {
		expression   string
		linuxDynamic bool
		allowed      bool
	}{
		{"MIT", false, true},
		{"Apache-2.0 AND MIT", false, true},
		{"(Apache-2.0 AND MIT)", false, true},
		{"Apache-2.0 OR MIT", false, true},
		{"MIT OR GPL-3.0-only", false, true},
		{"MIT AND GPL-3.0-only", false, false},
		{"GPL-2.0-only", false, false},
		{"GPL-2.0+", false, false},
		{"AGPL-3.0-or-later", false, false},
		{"SSPL-1.0", false, false},
		{"CC-BY-NC-4.0", false, false},
		{"CC-BY-SA-4.0", false, false},
		{"GPL-2.0-only WITH Classpath-exception-2.0", false, false},
		{"LGPL-2.1-or-later", false, false},
		{"LGPL-2.1-or-later", true, true},
		{"LGPL-2.1-only OR MPL-1.1", false, true},
		{"EPL-1.0", false, false},
		{"LicenseRef-Android-SDK", false, true},
		{"LicenseRef-Something-Unreviewed", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		allowed, bad := policyExpressionAllowed(c.expression, c.linuxDynamic)
		if allowed != c.allowed {
			t.Errorf("%q (linux dynamic %v): allowed %v, want %v (bad %v)", c.expression, c.linuxDynamic, allowed, c.allowed, bad)
		}
	}
}

func TestCheckPolicy(t *testing.T) {
	text := map[*entry]string{}
	textOf := func(e *entry) string { return text[e] }
	add := func(e *entry, t string) *entry {
		text[e] = t
		return e
	}

	ok := []*entry{
		add(&entry{Name: "a", Origin: "go", Apps: []string{"android"}, Spdx: "MIT"}, "MIT License\n\nPermission is hereby granted"),
		// MPL and LGPL texts mention the GPL; that is not a GPL license
		add(&entry{Name: "b", Origin: "go", Apps: []string{"android"}, Spdx: "MPL-2.0"}, "Mozilla Public License Version 2.0\n... GNU General Public License ..."),
		add(&entry{Name: "c", Origin: "extra", Apps: []string{"linux"}, Spdx: "LGPL-2.1-or-later"}, "GNU LESSER GENERAL PUBLIC LICENSE\nVersion 2.1"),
	}
	if err := checkPolicy(ok, textOf); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []*entry{
		// a permissive package.json field over a GPL text
		add(&entry{Name: "gpl-text", Origin: "npm-web", Apps: []string{"web"}, Spdx: "MIT"}, "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007"),
		add(&entry{Name: "agpl", Origin: "npm-web", Apps: []string{"web"}, Spdx: "AGPL-3.0-only"}, "GNU AFFERO GENERAL PUBLIC LICENSE"),
		// LGPL linked statically into a mobile app
		add(&entry{Name: "lgpl-static", Origin: "go", Apps: []string{"android"}, Spdx: "LGPL-3.0-only"}, "GNU LESSER GENERAL PUBLIC LICENSE"),
		// unidentified
		add(&entry{Name: "unknown", Origin: "npm-web", Apps: []string{"web"}}, "Some license"),
		// a GPL notice with no license title
		add(&entry{Name: "gpl-notice", Origin: "go", Apps: []string{"linux"}, Spdx: "MIT"}, "Copyright 2020 X\n\nThis program is free software: you can redistribute it and/or modify it under the terms of the GNU General Public License as published by the Free Software Foundation"),
	} {
		err := checkPolicy([]*entry{bad}, textOf)
		if err == nil || !strings.Contains(err.Error(), bad.Name) {
			t.Errorf("%s: want a policy failure, got %v", bad.Name, err)
		}
	}
}

// The catalog's own compressed copy is named license_data.json.gz, which the license file pattern
// takes for a license. Reading it put 81 KB of gzip into the sdk's own entry as its license text,
// on the first regeneration after the file was added.
func TestReadLicenseFilesSkipsTheCatalogItself(t *testing.T) {
	directory := t.TempDir()
	const licenseText = "the license text"
	for name, content := range map[string]string{
		"LICENSE":              licenseText,
		"license.go":           "package code",
		"license.yml":          "entries: []",
		"license_data.json":    "{}",
		"license_data.json.gz": "\x1f\x8b\x08 compressed",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	text, err := readLicenseFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	if text != licenseText {
		t.Fatalf("read %q, want only %q", text, licenseText)
	}
}

// A module go.mod replaces with a directory is read from a checkout, whose
// NOTICE can change while the module's version stays the same: -check fails on
// that, naming the module, until the catalog is regenerated. The SDK's own
// checkout counts as one. The texts of a module from the module cache, whose
// files go.sum pins, and of the Go standard library are not compared (QD-1455).
func TestLicenseCheckComparesReplacedModuleTexts(t *testing.T) {
	oldTargets, oldSdk, oldRepo, oldRoot, oldMmm, oldExtra, oldOffline := goTargets, sdkDir, sdkRepoDir, rootDir, mmmDir, extra, offline
	t.Cleanup(func() {
		goTargets, sdkDir, sdkRepoDir, rootDir, mmmDir, extra, offline = oldTargets, oldSdk, oldRepo, oldRoot, oldMmm, oldExtra, oldOffline
	})
	goTargets = []goTarget{{dir: ".", env: []string{"CGO_ENABLED=0"}, packages: []string{"."}, apps: []string{"linux"}}}

	root := t.TempDir()
	write := func(path string, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// fixture.example/pinned comes from a module proxy on disk into the test's
	// own module cache, checked against go.sum as any module is. go.sum's h1
	// hash is dirhash's: the SHA-256 of one line per file, in name order, of the
	// file's SHA-256 and its name.
	hash1 := func(nameContents map[string]string) string {
		summary := sha256.New()
		for _, name := range slices.Sorted(maps.Keys(nameContents)) {
			fmt.Fprintf(summary, "%x  %s\n", sha256.Sum256([]byte(nameContents[name])), name)
		}
		return "h1:" + base64.StdEncoding.EncodeToString(summary.Sum(nil))
	}
	pinnedGoMod := "module fixture.example/pinned\n\ngo 1.26\n"
	pinnedLicense := "MIT License\n\nCopyright (c) 2026 Pinned Fixture Authors\n\nPermission is hereby granted, free of charge, to any person obtaining a copy of the pinned fixture.\n"
	pinnedZipNameContents := map[string]string{
		"fixture.example/pinned@v1.0.0/go.mod":    pinnedGoMod,
		"fixture.example/pinned@v1.0.0/pinned.go": "package pinned\n",
		"fixture.example/pinned@v1.0.0/LICENSE":   pinnedLicense,
	}
	var pinnedZip bytes.Buffer
	zipWriter := zip.NewWriter(&pinnedZip)
	for _, name := range slices.Sorted(maps.Keys(pinnedZipNameContents)) {
		fileWriter, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fileWriter, pinnedZipNameContents[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	proxyDir := filepath.Join(root, "proxy")
	versionDir := filepath.Join(proxyDir, "fixture.example", "pinned", "@v")
	write(filepath.Join(versionDir, "list"), "v1.0.0\n")
	write(filepath.Join(versionDir, "v1.0.0.info"), `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	write(filepath.Join(versionDir, "v1.0.0.mod"), pinnedGoMod)
	write(filepath.Join(versionDir, "v1.0.0.zip"), pinnedZip.String())

	sdk := filepath.Join(root, "sdk")
	write(filepath.Join(sdk, "go.mod"), "module github.com/urnetwork/sdk\n\ngo 1.26\n\nrequire (\n\tfixture.example/pinned v1.0.0\n\tfixture.example/sibling v1.2.0\n)\n\nreplace fixture.example/sibling => ../sibling\n")
	write(filepath.Join(sdk, "go.sum"), "fixture.example/pinned v1.0.0 "+hash1(pinnedZipNameContents)+"\nfixture.example/pinned v1.0.0/go.mod "+hash1(map[string]string{"go.mod": pinnedGoMod})+"\n")
	// every build links the standard library, which gives its entry the apps
	write(filepath.Join(sdk, "sdk.go"), "package sdk\n\nimport (\n\t_ \"errors\"\n\n\t_ \"fixture.example/pinned\"\n\t_ \"fixture.example/sibling\"\n)\n")
	write(filepath.Join(sdk, "LICENSE"), "Mozilla Public License Version 2.0\n\nThe sdk fixture's license.\n")
	write(filepath.Join(sdk, "licenses", "extra.yml"), "{}\n")
	sibling := filepath.Join(root, "sibling")
	write(filepath.Join(sibling, "go.mod"), "module fixture.example/sibling\n\ngo 1.26\n")
	write(filepath.Join(sibling, "sibling.go"), "package sibling\n")
	write(filepath.Join(sibling, "LICENSE"), "MIT License\n\nCopyright (c) 2026 Sibling Fixture Authors\n\nPermission is hereby granted, free of charge, to any person obtaining a copy of the sibling fixture.\n")
	notice := "The sibling fixture includes a vendored file under the MIT license above.\n"
	write(filepath.Join(sibling, "NOTICE"), notice)

	// go list reaches only the proxy on disk, and with -mod=readonly a module
	// go.sum does not hash is an error rather than a checksum database lookup
	t.Setenv("GOENV", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOFLAGS", "-mod=readonly -modcacherw")
	t.Setenv("GOMODCACHE", filepath.Join(root, "modcache"))
	t.Setenv("GOPROXY", (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxyDir)}).String())
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	t.Chdir(sdk)

	regenerate := func() {
		t.Helper()
		if err := run("", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	check := func() error {
		return run("sdk", "", "")
	}
	regenerate()

	// positive controls: which entries are read from a checkout, and a fresh
	// catalog passes
	entries, err := collectGo()
	if err != nil {
		t.Fatal(err)
	}
	nameLocalDirs := map[string]string{}
	for _, e := range entries {
		nameLocalDirs[e.Name] = e.localDir
	}
	wantNameLocalDirs := map[string]string{
		"github.com/urnetwork/sdk": sdk,
		"fixture.example/sibling":  sibling,
		"fixture.example/pinned":   "",
		"Go standard library":      "",
	}
	if !maps.Equal(nameLocalDirs, wantNameLocalDirs) {
		t.Fatalf("go entries' checkouts = %v, want %v", nameLocalDirs, wantNameLocalDirs)
	}
	if err := check(); err != nil {
		t.Fatalf("a fresh catalog fails -check: %v", err)
	}

	// the sibling's NOTICE changes, its version v1.2.0 does not
	changedNotice := notice + "It also includes a second vendored file, whose license asks for this line.\n"
	write(filepath.Join(sibling, "NOTICE"), changedNotice)
	err = check()
	if err == nil || !strings.Contains(err.Error(), "go fixture.example/sibling v1.2.0: license text changed in "+sibling) {
		t.Fatalf("a replaced module's changed NOTICE passed -check: %v", err)
	}
	for _, other := range []string{"fixture.example/pinned", "github.com/urnetwork/sdk", "Go standard library"} {
		if strings.Contains(err.Error(), other) {
			t.Fatalf("-check named %s, whose text did not change: %v", other, err)
		}
	}
	regenerate()
	if err := check(); err != nil {
		t.Fatalf("the regenerated catalog fails -check: %v", err)
	}
	// texts compare as normalizeText writes them: line endings and trailing
	// blanks are not a change
	write(filepath.Join(sibling, "NOTICE"), strings.ReplaceAll(changedNotice, "\n", "  \r\n"))
	if err := check(); err != nil {
		t.Fatalf("a NOTICE that differs only in line endings fails -check: %v", err)
	}

	// the SDK's own checkout is read like a sibling's
	write(filepath.Join(sdk, "NOTICE"), "The sdk fixture includes a vendored file.\n")
	err = check()
	if err == nil || !strings.Contains(err.Error(), "go github.com/urnetwork/sdk: license text changed in "+sdk) {
		t.Fatalf("the SDK's own changed NOTICE passed -check: %v", err)
	}
	if err := os.Remove(filepath.Join(sdk, "NOTICE")); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("the SDK's restored NOTICE fails -check: %v", err)
	}

	// the cached module's and the standard library's stored texts differ from
	// their files, and -check reads neither
	catalogPath := filepath.Join(sdk, "license.yml")
	catalog, err := readLicenseFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range catalog.Entries {
		e.text = catalog.storedText(e)
		if e.Name == "fixture.example/pinned" || e.Name == "Go standard library" {
			e.text += "\nA line the module's own files do not have.\n"
		}
	}
	if err := catalog.write(catalogPath); err != nil {
		t.Fatal(err)
	}
	catalog, err = readLicenseFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range catalog.Entries {
		if e.Name == "fixture.example/pinned" && catalog.storedText(e) == normalizeText(pinnedLicense) {
			t.Fatal("the catalog still holds the cached module's own text")
		}
	}
	if err := check(); err != nil {
		t.Fatalf("-check compared a text go.sum pins: %v", err)
	}
}
