package main

import (
	"os"
	"path/filepath"
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
