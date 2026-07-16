package sdk

import (
	"testing"
)

func TestHostBaseName(t *testing.T) {
	cases := map[string]string{
		"example.com":         "example.com",
		"www.example.com":     "example.com",
		"cdn.a.example.com":   "example.com",
		"example.co.uk":       "example.co.uk",
		"www.example.co.uk":   "example.co.uk",
		"cdn.a.example.co.uk": "example.co.uk",
		"co.uk":               "co.uk",
		"uk":                  "uk",
		"example.com.au":      "example.com.au",
		"a.b.example.gob.mx":  "example.gob.mx",
		"a.example.co.jp":     "example.co.jp",
		// case-insensitive suffix match, original case preserved
		"www.Example.CO.UK": "Example.CO.UK",
		// trailing dot
		"www.example.com.": "example.com",
		"localhost":        "localhost",
	}
	for host, expected := range cases {
		if baseName := HostBaseName(host); baseName != expected {
			t.Fatalf("HostBaseName(%q) = %q, expected %q", host, baseName, expected)
		}
	}
}
