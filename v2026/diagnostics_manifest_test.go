package sdk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The manifest must be valid json with the fields the bundle's reader relies
// on, and must never be empty -- an export with no manifest is an export that
// cannot be dated or attributed to a build.
func TestDiagnosticManifestJsonShape(t *testing.T) {
	manifestJson := buildDiagnosticManifestJson(diagnosticManifestInput{
		SdkVersion:      "0.0.0-test",
		ClientId:        "11111111-1111-1111-1111-111111111111",
		InstanceId:      "22222222-2222-2222-2222-222222222222",
		NetworkSpace:    "main",
		ConnectEnabled:  true,
		ProvideEnabled:  false,
		DeviceAvailable: true,
	})

	var decoded map[string]any
	if err := json.Unmarshal([]byte(manifestJson), &decoded); err != nil {
		t.Fatalf("manifest is not valid json: %v\n%s", err, manifestJson)
	}

	for _, key := range []string{"sdk_version", "client_id", "instance_id", "network_space", "connect_enabled", "device_available"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("manifest missing %q; has %v", key, decoded)
		}
	}
	if decoded["sdk_version"] != "0.0.0-test" {
		t.Errorf("sdk_version = %v, want 0.0.0-test", decoded["sdk_version"])
	}
}

// When the rpc is down the manifest must still be produced, marked so the
// reader knows the device-side fields are absent rather than false.
func TestDiagnosticManifestJsonWhenDeviceUnavailable(t *testing.T) {
	manifestJson := buildDiagnosticManifestJson(diagnosticManifestInput{
		SdkVersion:      "0.0.0-test",
		DeviceAvailable: false,
	})

	var decoded map[string]any
	if err := json.Unmarshal([]byte(manifestJson), &decoded); err != nil {
		t.Fatalf("manifest is not valid json: %v", err)
	}
	if decoded["device_available"] != false {
		t.Fatalf("device_available = %v, want false", decoded["device_available"])
	}
}

// The manifest's log fields describe the process that BUILT the manifest, and
// must say so. On ios that process is the network extension, reached over the
// rpc, while the archive around the manifest is assembled in the app from a
// different container -- so a bare "log_root" key was read as the root the
// bundle's files came from when it was nothing of the kind.
func TestDiagnosticManifestNamesItsOwnProcessLogRoot(t *testing.T) {
	restoreTestingLogDir(t)
	restoreTestingLogVerbosity(t)

	root := t.TempDir()
	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	if err := SetLogVerbosity(LogVerbosityTrace); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}

	var decoded map[string]any
	manifestJson := buildDiagnosticManifestJson(diagnosticManifestInput{
		SdkVersion:      "0.0.0-test",
		DeviceAvailable: true,
	})
	if err := json.Unmarshal([]byte(manifestJson), &decoded); err != nil {
		t.Fatalf("manifest is not valid json: %v\n%s", err, manifestJson)
	}

	// the ambiguous key is gone rather than kept alongside its replacement: a
	// reader who finds both has no way to know which one to follow
	if _, ok := decoded["log_root"]; ok {
		t.Errorf("manifest still carries the unqualified log_root key: %s", manifestJson)
	}
	if decoded["manifest_log_root"] != root {
		t.Errorf("manifest_log_root = %v, want %q", decoded["manifest_log_root"], root)
	}
	// the directory, not just the root, so the process that produced the
	// manifest is named unambiguously
	if decoded["manifest_log_dir"] != filepath.Join(root, "extension") {
		t.Errorf("manifest_log_dir = %v, want %q", decoded["manifest_log_dir"], filepath.Join(root, "extension"))
	}
	// what the reader can expect to find: at the default level none of the
	// V(1) contract or transport lines were written at all
	if decoded["manifest_log_verbosity"] != float64(LogVerbosityTrace) {
		t.Errorf("manifest_log_verbosity = %v, want %d", decoded["manifest_log_verbosity"], LogVerbosityTrace)
	}
}

// TestExportDiagnosticBundleManifestNamesTheRootEachSourceWasReadFrom is the
// defect a real exported bundle showed: the manifest named
//
//	/var/mobile/Containers/Data/PluginKitPlugin/835B.../Library/Caches/Logs
//
// -- the extension's root, because DiagnosticManifestJson executes there over
// the rpc -- while the archive held the app's logs, read in the app process
// from
//
//	/var/mobile/Containers/Data/Application/C495.../Library/Caches/Logs/app
//
// A support engineer following the manifest opens the wrong container, and it
// misleads worst exactly when the two processes have diverged.
func TestExportDiagnosticBundleManifestNamesTheRootEachSourceWasReadFrom(t *testing.T) {
	restoreTestingLogDir(t)
	restoreTestingLogVerbosity(t)

	// the exporting process (the app) reads from its own root
	exportRoot := t.TempDir()
	appDir := filepath.Join(exportRoot, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	if err := SetLogDirForProcess(exportRoot, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}
	if err := SetLogVerbosity(LogVerbosityVerbose); err != nil {
		t.Fatalf("SetLogVerbosity: %v", err)
	}

	// the manifest body arrives from the device process, which is in another
	// container entirely
	deviceRoot := "/var/mobile/Containers/Data/PluginKitPlugin/835B/Library/Caches/Logs"
	destPath := filepath.Join(t.TempDir(), "bundle.zip")
	opts := NewExportOptions()
	opts.IncludeManifest = true
	opts.SetManifestJson(`{"sdk_version":"0.0.0-test","device_available":true,"manifest_log_root":"` + deviceRoot + `"}`)
	opts.MissingSourceReason("extension", "app group container unavailable")

	if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}

	var manifest map[string]any
	raw := readZipEntry(t, destPath, "manifest.json")
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("manifest.json is not valid json: %v\n%s", err, raw)
	}

	// the device's own root survives, still labelled as the manifest's
	if manifest["manifest_log_root"] != deviceRoot {
		t.Errorf("manifest_log_root = %v, want the device's %q", manifest["manifest_log_root"], deviceRoot)
	}
	// and the exporting process names its own, separately
	if manifest["export_log_root"] != exportRoot {
		t.Errorf("export_log_root = %v, want %q", manifest["export_log_root"], exportRoot)
	}
	if manifest["export_log_verbosity"] != float64(LogVerbosityVerbose) {
		t.Errorf("export_log_verbosity = %v, want %d", manifest["export_log_verbosity"], LogVerbosityVerbose)
	}

	sources, ok := manifest["sources"].([]any)
	if !ok {
		t.Fatalf("manifest.json has no sources list; has %v", manifest)
	}
	seen := map[string]map[string]any{}
	for _, entry := range sources {
		source, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("sources entry is not an object: %v", entry)
		}
		seen[fmt.Sprintf("%v", source["source"])] = source
	}

	app, ok := seen["app"]
	if !ok {
		t.Fatalf("sources missing the app source; has %v", seen)
	}
	// the whole point: the path under this entry is the one the archived files
	// were actually read from, in the process that read them
	if app["log_root"] != exportRoot {
		t.Errorf("app source log_root = %v, want the root it was read from, %q", app["log_root"], exportRoot)
	}
	if app["log_root"] == deviceRoot {
		t.Errorf("app source log_root names the manifest process's root, which holds none of these files")
	}

	// a source no enumeration reached carries no root rather than borrowing
	// one it was never read from
	extension, ok := seen["extension"]
	if !ok {
		t.Fatalf("sources missing the declared-unavailable extension source; has %v", seen)
	}
	if root, ok := extension["log_root"]; ok {
		t.Errorf("unread extension source claims log_root = %v", root)
	}
}
