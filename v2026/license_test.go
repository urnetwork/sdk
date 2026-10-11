package sdk

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Compare every field and text, including entry order, against the catalog
// previously embedded as YAML. The conversion must not omit or alter notices.
func TestLicenseRuntimeCatalogMatchesYml(t *testing.T) {
	data, err := os.ReadFile("license.yml")
	if err != nil {
		t.Fatal(err)
	}
	var expected licenseFile
	if err := yaml.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	actual, err := loadLicenseFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*actual, expected) {
		t.Fatal("runtime catalog differs from license.yml; regenerate and commit both catalogs")
	}
}

func TestLicenseRuntimeDoesNotImportYAML(t *testing.T) {
	for _, tags := range []string{"sdk_mobile_bind", "sdk_mobile_bind,ios_extension"} {
		t.Run(tags, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-tags="+tags, "-f", "{{.ImportPath}}", ".")
			cmd.Env = append(os.Environ(), "GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1", "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("list runtime dependencies: %v\n%s", err, out)
			}
			if slices.Contains(strings.Fields(string(out)), "gopkg.in/yaml.v3") {
				t.Fatal("Apple SDK still links a YAML parser for generated license data")
			}
		})
	}
}

var licenseApps = []string{
	LicenseAppAndroid,
	LicenseAppApple,
	LicenseAppWindows,
	LicenseAppLinux,
	LicenseAppWeb,
	LicenseAppExtension,
}

func TestLicenseCatalogParses(t *testing.T) {
	f, err := loadLicenseFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) == 0 {
		t.Fatal("runtime license catalog has no entries")
	}
	for _, entry := range f.Entries {
		if entry.Name == "" {
			t.Errorf("entry without a name: %+v", entry)
		}
		if strings.TrimSpace(f.Texts[entry.Text]) == "" {
			t.Errorf("%s %s: no license text (%q)", entry.Name, entry.Version, entry.Text)
		}
		if len(entry.Apps) == 0 {
			t.Errorf("%s %s: no apps", entry.Name, entry.Version)
		}
		for _, app := range entry.Apps {
			if !slices.Contains(licenseApps, app) {
				t.Errorf("%s %s: unknown app %q", entry.Name, entry.Version, app)
			}
		}
	}
}

// every app shows location data, so every app carries the GeoLite2 notice
// the MaxMind license requires, first in its list
func TestGetLicensesGeoLite2NoticeFirst(t *testing.T) {
	for _, app := range licenseApps {
		licenses := GetLicenses(app)
		if licenses.Len() == 0 {
			t.Fatalf("%s: no licenses", app)
		}
		first := licenses.Get(0)
		if first.Name != "GeoLite2 by MaxMind" {
			t.Errorf("%s: first entry is %q, want the GeoLite2 attribution", app, first.Name)
		}
		if !strings.Contains(first.Notice, "This product includes GeoLite2 data created by MaxMind") {
			t.Errorf("%s: GeoLite2 notice is %q", app, first.Notice)
		}
		if first.Text == "" {
			t.Errorf("%s: GeoLite2 has no text", app)
		}
	}
}

func TestGetLicensesFiltersByApp(t *testing.T) {
	all := GetLicenses("")
	for _, app := range licenseApps {
		licenses := GetLicenses(app)
		if licenses.Len() >= all.Len() {
			t.Errorf("%s: %d entries, not a subset of all %d", app, licenses.Len(), all.Len())
		}
	}
	// the wasm build does not link gomobile's bind packages
	for _, license := range GetLicenses(LicenseAppWeb).getAll() {
		if strings.HasPrefix(license.Name, "golang.org/x/mobile") {
			t.Errorf("web lists %s", license.Name)
		}
	}
	found := false
	for _, license := range GetLicenses(LicenseAppAndroid).getAll() {
		if license.Name == "golang.org/x/mobile" {
			found = true
		}
	}
	if !found {
		t.Error("android does not list golang.org/x/mobile")
	}
}

// license.yml must match the SDK's Go modules and licenses/extra.yml. The
// app repos' entries are checked only when that sibling is checked out, and
// each app's build runs its own check.
func TestLicenseYmlUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list for every SDK build target")
	}
	check := func(target string) {
		cmd := exec.Command("go", "run", "./licenses", "-check", target)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("-check %s: %v\n%s", target, err, out)
		}
	}
	check("sdk")

	siblings := map[string]string{
		"apple":     "apple",
		"web":       filepath.Join("mmm", "ur.io"),
		"extension": "extension",
	}
	for target, repo := range siblings {
		if _, err := os.Stat(filepath.Join("..", repo)); err != nil {
			t.Logf("%s is not checked out; not checking its entries", repo)
			continue
		}
		check(target)
	}
	// android resolves with gradle, which needs the android toolchain; its
	// build runs the check
}
