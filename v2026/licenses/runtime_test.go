package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRuntimeLicenseCatalogPreservesAllFieldsAndTexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "license.yml")
	catalog := &licenseFile{
		Sources: []source{{Origin: "extra", Repo: "sdk", Commit: "source-metadata-only"}},
		Entries: []*entry{{
			Name: "First attribution", Version: "1.2.3", Kind: "data", Origin: "extra",
			Apps: []string{"apple", "android"}, Url: "https://example.test/?a=1&b=2",
			Spdx: "MIT", Copyright: "Copyright © 2026 <owner>",
			Notice: "A verbatim notice.\nA second line.", text: "MIT License\n\nPermission is hereby granted.\n",
		}, {
			Name: "Second attribution", Kind: "data", Origin: "extra", Apps: []string{"apple"},
			Spdx: "MIT", text: "MIT License\n\nPermission is hereby granted.\n",
		}},
	}
	if err := catalog.write(path); err != nil {
		t.Fatal(err)
	}
	want, err := readLicenseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(filepath.Dir(path), "license_data.json")
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	var got licenseFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want.Sources = nil // provenance is not part of the runtime API
	if !reflect.DeepEqual(&got, want) {
		t.Fatalf("runtime catalog differs from YAML:\ngot %#v\nwant %#v", got, want)
	}
	if bytes.Contains(data, []byte("source-metadata-only")) || len(got.Texts) != 1 {
		t.Fatal("runtime catalog included source metadata or lost text deduplication")
	}
	if err := want.checkRuntimeFile(runtimePath); err != nil {
		t.Fatal(err)
	}
	if err := want.writeRuntimeFile(runtimePath); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(runtimePath)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("runtime output is not deterministic: %v", err)
	}
	want.Entries[0].Notice = "changed notice"
	if err := want.checkRuntimeFile(runtimePath); err == nil {
		t.Fatal("runtime check accepted an outdated notice")
	}
	if err := want.checkRuntimeFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("runtime check accepted a missing catalog")
	}
}
