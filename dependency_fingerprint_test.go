package sdk

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

// Each consuming module owns its dependency records, including host generators
// for browser artifacts and the C ABI's alternate loopback modfile. Keep the
// TLS and DTLS fingerprint dependencies pinned to connect with both checksums.
func TestSdkModulesKeepConnectFingerprintDependencies(t *testing.T) {
	readLines := func(path string) map[string]bool {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := map[string]bool{}
		scanner := bufio.NewScanner(bytes.NewReader(content))
		for scanner.Scan() {
			line, _, _ := strings.Cut(scanner.Text(), "//")
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "require" {
				fields = fields[1:]
			}
			if len(fields) > 0 {
				lines[strings.Join(fields, " ")] = true
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return lines
	}

	dependencies := []string{
		"github.com/refraction-networking/utls",
		"github.com/theodorsm/covert-dtls",
		"src.agwa.name/tlshacks",
	}
	connectRequirements := readLines("../connect/go.mod")
	connectSums := readLines("../connect/go.sum")
	versions := map[string]string{}
	checksums := map[string][]string{}
	for _, dependency := range dependencies {
		for line := range connectRequirements {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == dependency {
				versions[dependency] = fields[1]
			}
		}
		version := versions[dependency]
		if version == "" {
			t.Fatalf("connect/go.mod does not pin fingerprint dependency %s", dependency)
		}
		for _, suffix := range []string{"", "/go.mod"} {
			prefix := dependency + " " + version + suffix + " "
			expected := ""
			for line := range connectSums {
				if strings.HasPrefix(line, prefix+"h1:") {
					expected = line
				}
			}
			if expected == "" {
				t.Fatalf("connect/go.sum is missing %schecksum", prefix)
			}
			checksums[dependency] = append(checksums[dependency], expected)
		}
	}

	for _, modulePath := range []string{
		"go.mod",
		"build/go.mod",
		"cgo/go.mod",
		"cgo/loopback.go.mod",
		"js/go.mod",
		"cp3b/go.mod",
		"livepeer/go.mod",
		"liveprobe/go.mod",
	} {
		requirements := readLines(modulePath)
		sumPath := strings.TrimSuffix(modulePath, ".mod") + ".sum"
		sums := readLines(sumPath)
		for _, dependency := range dependencies {
			version := versions[dependency]
			if !requirements[dependency+" "+version] {
				t.Errorf("%s must require %s %s to match connect", modulePath, dependency, version)
			}
			for _, expected := range checksums[dependency] {
				if !sums[expected] {
					t.Errorf("%s must record connect's checksum %q", sumPath, expected)
				}
			}
		}
	}
}
