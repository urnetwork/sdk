package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The android binding binds the sdk and the hotspot proxy together, as the
// Makefile's build_android does. The hotspot package's whole api must bind:
// the broad omission policy accepts any identifier that contains "Proxy", so
// this test holds the package to no omission at all. Its java class names
// must also differ from the package facade's by more than case, since a
// case-insensitive disk keeps one of two such files.
func TestHotspotProxyBindsForAndroidWithoutOmissions(t *testing.T) {
	outputDirectory := t.TempDir()
	command := exec.Command(
		"go", "run", "golang.org/x/mobile/cmd/gobind",
		"-lang=java",
		"-tags=sdk_mobile_bind",
		"-outdir="+outputDirectory,
		"-javapkg=com.bringyour",
		"github.com/urnetwork/sdk/v2026",
		"github.com/urnetwork/sdk/v2026/hotspot",
	)
	command.Dir = "../.."
	command.Env = append(
		os.Environ(),
		"GOEXPERIMENT=greenteagc",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOWORK=off",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate java bindings: %v\n%s", err, output)
	}
	if err := validateMobileExports(outputDirectory); err != nil {
		t.Fatalf("validate generated Java: %v", err)
	}

	hotspotDirectory := filepath.Join(outputDirectory, "java", "com", "bringyour", "hotspot")
	entries, err := os.ReadDir(hotspotDirectory)
	if err != nil {
		t.Fatal(err)
	}
	foldedNames := map[string]string{}
	classNames := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if folded, ok := foldedNames[strings.ToLower(name)]; ok {
			t.Errorf("java files %s and %s differ only in case", folded, name)
		}
		foldedNames[strings.ToLower(name)] = name
		classNames[strings.TrimSuffix(name, ".java")] = true

		contents, err := os.ReadFile(filepath.Join(hotspotDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(contents), "\n") {
			if strings.Contains(line, "// skipped ") {
				t.Errorf("%s omits part of the hotspot api: %s", name, strings.TrimSpace(line))
			}
		}
	}
	for _, className := range []string{
		"Hotspot",
		"HotspotProxy",
		"HotspotProxyStatus",
		"HotspotProxyStatusChangeListener",
		"HotspotProxyEndpoint",
		"HotspotProxyEndpointList",
		"HotspotInterface",
		"HotspotInterfaceList",
	} {
		if !classNames[className] {
			t.Errorf("the android binding has no %s class: %v", className, entries)
		}
	}

	proxySource, err := os.ReadFile(filepath.Join(hotspotDirectory, "HotspotProxy.java"))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{
		"public native void setEnabled(boolean enabled);",
		"public native boolean getEnabled();",
		"public native void setInterfaces(HotspotInterfaceList interfaces);",
		"public native void regenerateCredentials();",
		"public native HotspotProxyStatus getStatus();",
		"public native com.bringyour.sdk.Sub addStatusChangeListener(HotspotProxyStatusChangeListener listener);",
		"public native void close();",
	} {
		if !strings.Contains(string(proxySource), method) {
			t.Errorf("HotspotProxy.java has no %q", method)
		}
	}
}
