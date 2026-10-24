// Generate the real C ABI into temporary output and prove the Go-only pooled
// batch has no callable C surface. Portable packed packet sending remains.
package main

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An actual declaration check rejects stale export data before generation;
// the exact coverage reason rejects accidental unsupported-signature omission.
func TestPrivateNativePacketBatchIsExcludedFromCAbi(t *testing.T) {
	g := testingPreferenceGenerator(t)
	deviceType := testingPreferenceType(t, g, "DeviceLocal")
	if types.NewMethodSet(types.NewPointer(deviceType.Type())).Lookup(nil, "SendNativePacketsNoCopy") == nil {
		t.Fatal("real native Go batch declaration is missing from generator input")
	}
	outputDirectory := t.TempDir()
	t.Chdir(outputDirectory)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{}
	for _, name := range []string{
		"exports_gen.go", "exports_gen_unix.go", "callbacks.h", "callbacks.c",
		filepath.Join("include", "urnetwork_sdk.h"),
		filepath.Join("include", "urnetwork_sdk.def"),
		filepath.Join("include", "urnetwork_sdk.hpp"),
	} {
		contents, err := os.ReadFile(filepath.Join(outputDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		artifacts[name] = string(contents)
		for _, forbidden := range []string{"SendNativePacketsNoCopy", "sendNativePacketsNoCopy", "urnet_device_local_send_native_packets_no_copy"} {
			if strings.Contains(string(contents), forbidden) {
				t.Errorf("NATIVE_POOLED_BATCH_EXPOSED_TO_C: artifact=%s symbol=%s", name, forbidden)
			}
		}
	}
	coverage, err := os.ReadFile(filepath.Join(outputDirectory, "coverage_report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(coverage), "DeviceLocal.SendNativePacketsNoCopy: skipped: pool ownership does not cross the abi; use urnet_device_local_send_packet_batch\n") {
		t.Error("native Go batch lacks the explicit pool-ownership exclusion reason")
	}
	for _, expected := range []struct{ artifact, symbol string }{
		{artifact: "exports_gen.go", symbol: "//export urnet_device_local_send_packet_batch\n"},
		{artifact: filepath.Join("include", "urnetwork_sdk.h"), symbol: "urnet_device_local_send_packet_batch("},
		{artifact: filepath.Join("include", "urnetwork_sdk.def"), symbol: "\n\turnet_device_local_send_packet_batch\n"},
	} {
		if !strings.Contains(artifacts[expected.artifact], expected.symbol) {
			t.Errorf("portable packed batch export changed: artifact=%s", expected.artifact)
		}
	}
}
