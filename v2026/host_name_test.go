package sdk

import (
	"slices"
	"testing"
)

func TestCollapseHostNames(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"base plus subdomains -> base absorbed into *.base (*.a matches a)",
			[]string{"example.com", "a.example.com", "b.example.com"},
			[]string{"*.example.com"},
		},
		{
			"base and one subdomain -> base absorbed (the itunes case)",
			[]string{"itunes.apple.com", "x.itunes.apple.com"},
			[]string{"*.itunes.apple.com"},
		},
		{
			"subdomains WITHOUT the base -> never invent *.base",
			[]string{"a.example.com", "b.example.com"},
			[]string{"a.example.com", "b.example.com"},
		},
		{
			"nested collapses to the most specific ancestor; bases absorbed into wildcards",
			[]string{"example.com", "a.example.com", "c.a.example.com"},
			[]string{"*.a.example.com", "*.example.com"},
		},
		{
			"no ancestor in set -> unchanged; never *.tld",
			[]string{"x.foo.com", "y.bar.com"},
			[]string{"x.foo.com", "y.bar.com"},
		},
		{
			"label-aligned only: xample.com is not an ancestor of a.example.com",
			[]string{"a.example.com", "xample.com"},
			[]string{"a.example.com", "xample.com"},
		},
		{
			"dedup, case-insensitive, trailing dot, base absorbed",
			[]string{"Example.com.", "example.com", "A.Example.com"},
			[]string{"*.example.com"},
		},
		{
			"multiple bases absorbed into their wildcards; a lone base with no subdomains stays",
			[]string{"foo.com", "a.foo.com", "bar.net", "b.bar.net", "lone.org"},
			[]string{"*.bar.net", "*.foo.com", "lone.org"},
		},
	}
	for _, c := range cases {
		got := CollapseHostNames(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\n  CollapseHostNames(%v)\n  = %v\n  want %v", c.name, c.in, got, c.want)
		}
	}
}

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
