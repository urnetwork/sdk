package sdk

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLogInventoryFindsEveryProcessAndSkipsSymlinks pins two things that the
// naive implementation gets wrong: logs live under one subdirectory per
// process, and glog puts a <program>.<SEVERITY> SYMLINK beside every real file
// (glog/glog_file.go:124-140), which would otherwise be counted twice.
func TestLogInventoryFindsEveryProcessAndSkipsSymlinks(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()

	appDir := filepath.Join(root, "app")
	extensionDir := filepath.Join(root, "extension")
	for _, dir := range []string{appDir, extensionDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}

	realName := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	writeTestingLogFile(t, appDir, realName)
	writeTestingLogFile(t, extensionDir, "urnetwork.host.user.log.ERROR.20260830-101112.4243")

	// the symlink glog maintains next to the real file
	if err := os.Symlink(filepath.Join(appDir, realName), filepath.Join(appDir, "urnetwork.INFO")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// A symlink whose name ALSO matches the ".log."+SEVERITY filename filter,
	// unlike glog's real "urnetwork.INFO" symlink above. This is what makes
	// the type check (entry.Type()&os.ModeSymlink) load-bearing: the filename
	// filter alone would let this one through, so if the type check were
	// dropped or broken, it would be double-counted as a real log file.
	spoofedName := "urnetwork.host.user.log.INFO.20260830-101112.9999"
	if err := os.Symlink(filepath.Join(appDir, realName), filepath.Join(appDir, spoofedName)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	inventory := LogInventory()

	bySource := map[string]*LogFileInfo{}
	for i := 0; i < inventory.Len(); i += 1 {
		info := inventory.Get(i)
		if info.Name == "urnetwork.INFO" || info.Name == spoofedName {
			t.Fatalf("inventory included a symlink (%s); it must list real files only", info.Name)
		}
		bySource[info.Source] = info
	}

	app, ok := bySource["app"]
	if !ok {
		t.Fatal("inventory missing the app source")
	}
	if app.Severity != "INFO" {
		t.Fatalf("app severity = %q, want INFO", app.Severity)
	}
	if app.ByteCount <= 0 {
		t.Fatalf("app ByteCount = %d, want > 0", app.ByteCount)
	}

	extension, ok := bySource["extension"]
	if !ok {
		t.Fatal("inventory missing the extension source -- it only scanned this process's directory")
	}
	if extension.Severity != "ERROR" {
		t.Fatalf("extension severity = %q, want ERROR", extension.Severity)
	}
}

// TestExportDiagnosticBundleWritesEverySelectedSource covers the zip layout and
// that a source which cannot be read is REPORTED, never fatal -- an ios build
// whose provisioning profile lacks the app group must still export its own
// logs.
func TestExportDiagnosticBundleWritesEverySelectedSource(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	// SetLogDirForProcess's own SetLogDir call writes one incidental
	// bookkeeping entry ("New glog initialized") into appDir before this test
	// ever calls ExportDiagnosticBundle (see log_export_test.go's identical
	// note; SetLogDir/clearOldLogs are out of this task's scope to change).
	// LogInventory correctly counts that real file alongside the fixture one,
	// so the expected count is taken from the inventory itself rather than
	// hardcoded, keeping the assertion honest about what is actually on disk.
	wantFileCount := LogInventory().Len()

	destPath := filepath.Join(t.TempDir(), "bundle.zip")

	opts := NewExportOptions()
	opts.IncludeManifest = true
	opts.MissingSourceReason("extension", "app group container unavailable")

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil", err)
	}
	if result.FileCount != wantFileCount {
		t.Fatalf("FileCount = %d, want %d", result.FileCount, wantFileCount)
	}
	if result.MissingSources.Len() != 1 {
		t.Fatalf("MissingSources.Len() = %d, want 1", result.MissingSources.Len())
	}

	reader, err := zip.OpenReader(destPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()

	names := map[string]bool{}
	for _, f := range reader.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"README.txt",
		"manifest.json",
		"logs/app/urnetwork.host.user.log.INFO.20260830-101112.4242",
	} {
		if !names[want] {
			t.Errorf("bundle missing %q; has %v", want, names)
		}
	}
}

// A redacted export must not carry the raw value anywhere in the archive.
func TestExportDiagnosticBundleRedactsWhenAsked(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	name := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	if err := os.WriteFile(filepath.Join(appDir, name),
		[]byte("I0830 10:11:12.131415 1 x.go:1] peer 203.0.113.7:443\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "redacted.zip")
	opts := NewExportOptions()
	opts.Redact = true

	if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}

	reader, err := zip.OpenReader(destPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %q: %v", f.Name, err)
		}
		if strings.Contains(string(content), "203.0.113.7") {
			t.Fatalf("entry %q in a redacted bundle still contains the raw address", f.Name)
		}
	}
}

// TestExportDiagnosticBundleRedactsIPv6AndLeavesARawExportVerbatim covers the
// two modes end to end on the address shapes a Go network log actually prints:
// a net.Dial error and a %+v of a struct, where the address has a ':' or a '{'
// hard up against it. Those went out of a REDACTED bundle in the clear once,
// and the unit table did not catch it because it only ever put an address
// between spaces -- so the check is repeated here on a real archive.
//
// The raw half is the control: with Redact off no transform is installed at
// all, so the log entry must come back byte for byte, redaction fix or not.
func TestExportDiagnosticBundleRedactsIPv6AndLeavesARawExportVerbatim(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	name := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	body := strings.Join([]string{
		"I0830 10:11:12.131415 1 c.go:1] dial tcp 2001:db8::1:443: connect: connection refused",
		"I0830 10:11:12.131416 1 c.go:2] client {Ip:2001:db8::1 Port:443}",
		"I0830 10:11:12.131417 1 c.go:3] peer fe80::1: timeout",
		"I0830 10:11:12.131418 1 c.go:4] route src:2001:db8:1:2:3:4:5:6",
		"I0830 10:11:12.131419 1 c.go:5] peer 203.0.113.7:443 retry [10] of [42]",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(appDir, name), []byte(body), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	leaks := []string{"2001:db8::1", "fe80::1", "2001:db8:1:2:3:4:5:6", "203.0.113.7"}

	redactedPath := filepath.Join(t.TempDir(), "redacted.zip")
	redacted := NewExportOptions()
	redacted.Redact = true
	if _, err := ExportDiagnosticBundle(redactedPath, redacted); err != nil {
		t.Fatalf("ExportDiagnosticBundle(redacted) = %v", err)
	}
	reader, err := zip.OpenReader(redactedPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %q: %v", f.Name, err)
		}
		for _, leak := range leaks {
			if strings.Contains(string(content), leak) {
				t.Errorf("entry %q in a REDACTED bundle still contains %q:\n%s", f.Name, leak, content)
			}
		}
	}
	// the structure that must survive redaction
	entry := readZipEntry(t, redactedPath, "logs/app/"+name)
	for _, want := range []string{"I0830 10:11:12.131415", "c.go:1]", "retry [10] of [42]", "Port:443}"} {
		if !strings.Contains(entry, want) {
			t.Errorf("redacted log entry lost %q:\n%s", want, entry)
		}
	}

	rawPath := filepath.Join(t.TempDir(), "raw.zip")
	raw := NewExportOptions()
	raw.Redact = false
	if _, err := ExportDiagnosticBundle(rawPath, raw); err != nil {
		t.Fatalf("ExportDiagnosticBundle(raw) = %v", err)
	}
	if got := readZipEntry(t, rawPath, "logs/app/"+name); got != body {
		t.Errorf("a raw export rewrote the log:\n got %q\nwant %q", got, body)
	}
}

// TestExportDiagnosticBundlePreservesLogFileModTimes pins that a log entry's
// zip header carries the source file's real Modified time (and, via
// zip.FileInfoHeader, its mode bits), not a header built from scratch. A
// zip.FileHeader assembled without an fs.FileInfo has no Modified set, which
// the zip format resolves to 1979-11-30 -- its DOS-era zero-value sentinel --
// silently changing the bytes ExportDiagnosticBundle/UploadLogs ship and
// discarding the per-rotation timestamps that make a diagnostic bundle
// readable.
func TestExportDiagnosticBundlePreservesLogFileModTimes(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	name := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	writeTestingLogFile(t, appDir, name)

	// A distinctive mtime, clear of both zip's 1979 sentinel and "now", so a
	// header built from time.Now() (as a synthetic entry gets) instead of
	// the file's real fs.FileInfo cannot pass this test by accident.
	wantModTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	logPath := filepath.Join(appDir, name)
	if err := os.Chtimes(logPath, wantModTime, wantModTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "modtime.zip")
	if _, err := ExportDiagnosticBundle(destPath, NewExportOptions()); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}

	reader, err := zip.OpenReader(destPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()

	entryName := "logs/app/" + name
	var entry *zip.File
	for _, f := range reader.File {
		if f.Name == entryName {
			entry = f
			break
		}
	}
	if entry == nil {
		t.Fatalf("bundle missing %q", entryName)
	}

	if entry.Modified.Year() <= 1980 {
		t.Fatalf("entry %q Modified = %v, looks like the zip 1979 zero-value sentinel, not the source file's real mtime", entryName, entry.Modified)
	}

	// The zip format's DOS date/time fields store 2-second granularity.
	diff := entry.Modified.UTC().Sub(wantModTime)
	if diff < 0 {
		diff = -diff
	}
	if 2*time.Second < diff {
		t.Fatalf("entry %q Modified = %v, want ~%v (the source file's mtime, within 2s)", entryName, entry.Modified, wantModTime)
	}
}

// TestExportDiagnosticBundleManifestFallbackUsesDeviceAvailableKey pins the
// key name in the fallback manifest written when IncludeManifest is set but
// no platform ever calls SetManifestJson -- the case for an Android export
// started while disconnected, where deviceManager.device is null. The
// fallback must use the same "device_available" key that
// buildDiagnosticManifestJson uses everywhere else, not a hand-written
// "available" key that a manifest.json reader would never look for.
func TestExportDiagnosticBundleManifestFallbackUsesDeviceAvailableKey(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "no-manifest-call.zip")

	opts := NewExportOptions()
	opts.IncludeManifest = true
	// deliberately no opts.SetManifestJson(...) call

	if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil", err)
	}

	reader, err := zip.OpenReader(destPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()

	var manifest *zip.File
	for _, f := range reader.File {
		if f.Name == "manifest.json" {
			manifest = f
			break
		}
	}
	if manifest == nil {
		t.Fatalf("bundle missing manifest.json")
	}

	rc, err := manifest.Open()
	if err != nil {
		t.Fatalf("open manifest.json: %v", err)
	}
	content, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatalf("manifest.json is not valid json: %v\n%s", err, content)
	}

	available, ok := decoded["device_available"]
	if !ok {
		t.Fatalf("manifest.json missing %q; has %v", "device_available", decoded)
	}
	if available != false {
		t.Fatalf("device_available = %v, want false", available)
	}
}

// TestExportDiagnosticBundleAcceptsZeroValueOptions pins that an ExportOptions
// that never went through NewExportOptions still exports. The c abi builds
// exactly that: urnet_export_diagnostic_bundle json-unmarshals into a bare
// &sdk.ExportOptions{}, so every unexported list, and SelectedNames when the
// json omits it, arrives nil. Reading Len() off one of those nil embedded
// lists is a nil dereference, which the c abi turned into a silent NULL return
// with no error set (cgoGuard recovers it) and which a gomobile seq bridge
// would turn into an app crash.
func TestExportDiagnosticBundleAcceptsZeroValueOptions(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "zero-value-options.zip")

	// exactly what the c abi hands the exporter, and what a zero-value
	// constructor in a language binding would hand it
	opts := &ExportOptions{IncludeManifest: true, IncludePlatformLogs: true}

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil", err)
	}
	if result.FileCount <= 0 {
		t.Fatalf("FileCount = %d, want > 0", result.FileCount)
	}

	// the setters must survive a zero value too -- they write into the same
	// lists
	opts.MissingSourceReason("extension", "app group container unavailable")
	opts.AddPlatformLog("logcat.txt", "platform line\n")
	if opts.missingNames.Len() != 1 || opts.platformNames.Len() != 1 {
		t.Fatalf("setters on a zero-value ExportOptions did not record: missing=%d platform=%d",
			opts.missingNames.Len(), opts.platformNames.Len())
	}
}

// TestExportDiagnosticBundleRedactsTheReadmeNotIncludedBlock pins the one
// entry that used to be written with a nil transform. README.txt quotes the
// os.Open error of every file that could not be read, and that error string
// carries the file's absolute path -- on ios, a path containing the app group
// container uuid, which manifest.json masks. A redacted bundle that leaks in
// its README the identifier it masks in its manifest is worse than no
// redaction, because the README claims uuid-shaped ids are replaced.
//
// The unreadable file here is real (mode 0000), so this exercises the actual
// leak channel, not just the platform-declared MissingSourceReason text.
func TestExportDiagnosticBundleRedactsTheReadmeNotIncludedBlock(t *testing.T) {
	restoreTestingLogDir(t)

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 does not deny access")
	}
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// the uuid rides into the error string via the file's own path
	secretId := "11111111-2222-3333-4444-555555555555"
	unreadable := "urnetwork." + secretId + ".log.INFO.20260830-101112.4242"
	writeTestingLogFile(t, appDir, unreadable)
	unreadablePath := filepath.Join(appDir, unreadable)
	if err := os.Chmod(unreadablePath, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(unreadablePath, 0600) })

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "readme-redaction.zip")
	opts := NewExportOptions()
	opts.Redact = true
	// the platform-declared channel into the same block
	opts.MissingSourceReason("extension", "container 66666666-7777-8888-9999-aaaaaaaaaaaa unavailable")

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil", err)
	}
	if result.MissingSources.Len() < 2 {
		t.Fatalf("MissingSources.Len() = %d, want the unreadable file and the declared source", result.MissingSources.Len())
	}

	readme := readZipEntry(t, destPath, "README.txt")
	if !strings.Contains(readme, "NOT INCLUDED") {
		t.Fatalf("README.txt has no NOT INCLUDED block, so this test proves nothing:\n%s", readme)
	}
	for _, leaked := range []string{secretId, "66666666-7777-8888-9999-aaaaaaaaaaaa"} {
		if strings.Contains(readme, leaked) {
			t.Fatalf("README.txt in a redacted bundle still contains %q:\n%s", leaked, readme)
		}
	}
}

func readZipEntry(t *testing.T, zipPath string, name string) string {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()
	for _, f := range reader.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", name, err)
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		return string(content)
	}
	t.Fatalf("bundle %q missing entry %q", zipPath, name)
	return ""
}

// TestExportDiagnosticBundleReportsAnUnreadableSourceDirectory pins the
// degradation promise for the case the inventory used to swallow: os.ReadDir
// failing on a per-process directory (a sandbox or permissions change, an
// App Group container present but its Logs subdirectory unreadable). That
// error used to be discarded, so a whole process's logs went missing with an
// empty NOT INCLUDED block and an ExportResult claiming zero missing sources.
func TestExportDiagnosticBundleReportsAnUnreadableSourceDirectory(t *testing.T) {
	restoreTestingLogDir(t)

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 does not deny access")
	}
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	extensionDir := filepath.Join(root, "extension")
	for _, dir := range []string{appDir, extensionDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	writeTestingLogFile(t, extensionDir, "urnetwork.host.user.log.INFO.20260830-101112.4243")

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	// the extension's directory becomes unreadable after it was written
	if err := os.Chmod(extensionDir, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(extensionDir, 0700) })

	destPath := filepath.Join(t.TempDir(), "unreadable-source.zip")
	result, err := ExportDiagnosticBundle(destPath, NewExportOptions())
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil -- an unreadable source degrades, never fails", err)
	}

	reported := false
	for i := 0; i < result.MissingSources.Len(); i += 1 {
		if strings.HasPrefix(result.MissingSources.Get(i), "extension: ") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("MissingSources does not mention the unreadable extension directory; has %v",
			stringListValues(result.MissingSources))
	}

	readme := readZipEntry(t, destPath, "README.txt")
	if !strings.Contains(readme, "extension: ") {
		t.Fatalf("README.txt does not name the unreadable source:\n%s", readme)
	}
}

func stringListValues(list *StringList) []string {
	values := []string{}
	for i := 0; i < list.Len(); i += 1 {
		values = append(values, list.Get(i))
	}
	return values
}

// TestExportDiagnosticBundleDoesNotAbortOnAnUnreadableEntry pins that a
// failure while COPYING one log file costs that file, not the export.
//
// The concrete trigger used here is the one that is reachable without an
// injected i/o error: on the redacted path zipWriteEntry scans lines with a
// 4 MiB cap, so a single longer line -- a corrupt or non-newline-terminated
// file, and glog files run to 16 MB -- returns bufio.ErrTooLong. That used to
// abort ExportDiagnosticBundle outright, discarding every other process's
// logs and leaving a truncated zip on disk at a path the platform had already
// been told to share.
func TestExportDiagnosticBundleDoesNotAbortOnAnUnreadableEntry(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	good := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	writeTestingLogFile(t, appDir, good)

	// one line past the redaction scanner's cap, no trailing newline
	oversize := "urnetwork.host.user.log.WARNING.20260830-101112.4243"
	if err := os.WriteFile(filepath.Join(appDir, oversize),
		[]byte(strings.Repeat("x", 5*1024*1024)), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "oversize-line.zip")
	opts := NewExportOptions()
	opts.Redact = true

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil -- one unreadable entry must not fail the export", err)
	}
	if result.FileCount < 1 {
		t.Fatalf("FileCount = %d, want the readable files to still be exported", result.FileCount)
	}

	reported := false
	for i := 0; i < result.MissingSources.Len(); i += 1 {
		if strings.HasPrefix(result.MissingSources.Get(i), oversize+": ") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("MissingSources does not name the entry that could not be copied; has %v",
			stringListValues(result.MissingSources))
	}

	// the archive must still be a well-formed zip carrying the other files
	if got := readZipEntry(t, destPath, "logs/app/"+good); !strings.Contains(got, "x.go:1") {
		t.Fatalf("readable log entry is missing or empty: %q", got)
	}
}

// TestExportDiagnosticBundleManifestCarriesModeAndSourceAvailability pins the
// two export-metadata fields the bundle had nowhere else to record: the
// mode, and the per-source availability list.
//
// Without them nothing machine-readable in the bundle says whether it was
// redacted -- the mode survived only as English prose in README.txt -- so a
// support engineer or any tooling reading manifest.json could not tell a
// redacted bundle from a raw one, nor an unreachable source from an empty one.
func TestExportDiagnosticBundleManifestCarriesModeAndSourceAvailability(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestingLogFile(t, appDir, "urnetwork.host.user.log.INFO.20260830-101112.4242")
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	for _, redact := range []bool{true, false} {
		destPath := filepath.Join(t.TempDir(), "manifest-mode.zip")
		opts := NewExportOptions()
		opts.Redact = redact
		opts.IncludeManifest = true
		// the platform-supplied half must survive the merge
		opts.SetManifestJson(`{"sdk_version":"0.0.0-test","device_available":true}`)
		opts.MissingSourceReason("extension", "app group container unavailable")

		if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
			t.Fatalf("ExportDiagnosticBundle = %v", err)
		}

		var manifest map[string]any
		raw := readZipEntry(t, destPath, "manifest.json")
		if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
			t.Fatalf("manifest.json is not valid json: %v\n%s", err, raw)
		}

		wantMode := "raw"
		if redact {
			wantMode = "redacted"
		}
		if manifest["export_mode"] != wantMode {
			t.Fatalf("export_mode = %v, want %q\n%s", manifest["export_mode"], wantMode, raw)
		}
		if manifest["sdk_version"] != "0.0.0-test" {
			t.Fatalf("the platform-supplied manifest body did not survive the merge: %s", raw)
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
		if app["available"] != true {
			t.Fatalf("app source available = %v, want true", app["available"])
		}
		if app["file_count"].(float64) < 1 {
			t.Fatalf("app source file_count = %v, want at least 1", app["file_count"])
		}

		extension, ok := seen["extension"]
		if !ok {
			t.Fatalf("sources missing the declared-unavailable extension source; has %v", seen)
		}
		if extension["available"] != false {
			t.Fatalf("extension source available = %v, want false", extension["available"])
		}
		if extension["reason"] == nil {
			t.Fatalf("extension source carries no reason: %v", extension)
		}
	}
}

// A manifest body the platform could not produce as a json object must not
// cost the bundle its export metadata, and must be reported rather than
// written through as-is.
func TestExportDiagnosticBundleReportsAnUnparseableSuppliedManifest(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "bad-manifest.zip")
	opts := NewExportOptions()
	opts.IncludeManifest = true
	opts.SetManifestJson("this is not json")

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v, want nil", err)
	}

	reported := false
	for i := 0; i < result.MissingSources.Len(); i += 1 {
		if strings.HasPrefix(result.MissingSources.Get(i), "manifest: ") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("MissingSources does not report the unusable manifest; has %v", stringListValues(result.MissingSources))
	}

	var manifest map[string]any
	raw := readZipEntry(t, destPath, "manifest.json")
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("manifest.json is not valid json: %v\n%s", err, raw)
	}
	if manifest["export_mode"] != "raw" {
		t.Fatalf("export_mode = %v, want raw even on the fallback manifest", manifest["export_mode"])
	}
}

// TestExportDiagnosticBundleExportsOnlyTheSelectedNames is the only guard on a
// dangerous documented semantic: SelectedNames empty means EVERY file, so the
// difference between "export these two files" and "export everything, raw" is
// one unchecked list. It is one of the three shipped export modes and had no
// test on either side of the bind; a consumer has already fallen into it
// (Android wired "Export selected" straight to the selection with no empty
// guard).
//
// The source-qualified form is pinned alongside the bare one: Name alone is
// not guaranteed unique across per-process directories, and the qualified form
// is the entry's zip path.
func TestExportDiagnosticBundleExportsOnlyTheSelectedNames(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	extensionDir := filepath.Join(root, "extension")
	for _, dir := range []string{appDir, extensionDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	wanted := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	unwanted := "urnetwork.host.user.log.ERROR.20260830-101112.4243"
	shared := "urnetwork.host.user.log.WARNING.20260830-101112.4244"
	writeTestingLogFile(t, appDir, wanted)
	writeTestingLogFile(t, appDir, unwanted)
	// the same file name under two sources, which Name alone cannot separate
	writeTestingLogFile(t, appDir, shared)
	writeTestingLogFile(t, extensionDir, shared)

	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	// one bare name, one source-qualified name
	destPath := filepath.Join(t.TempDir(), "selected.zip")
	opts := NewExportOptions()
	opts.SelectedNames.Add(wanted)
	opts.SelectedNames.Add("extension/" + shared)

	result, err := ExportDiagnosticBundle(destPath, opts)
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}
	if result.FileCount != 2 {
		t.Fatalf("FileCount = %d, want 2 -- the selection was not applied", result.FileCount)
	}

	logEntries := zipLogEntryNames(t, destPath)
	want := []string{"logs/app/" + wanted, "logs/extension/" + shared}
	if len(logEntries) != len(want) {
		t.Fatalf("bundle log entries = %v, want %v", logEntries, want)
	}
	for _, name := range want {
		if !slices.Contains(logEntries, name) {
			t.Fatalf("bundle log entries = %v, missing %q", logEntries, name)
		}
	}

	// and the semantic the picker depends on: empty means every file
	allPath := filepath.Join(t.TempDir(), "all.zip")
	all, err := ExportDiagnosticBundle(allPath, NewExportOptions())
	if err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}
	if all.FileCount <= result.FileCount {
		t.Fatalf("an empty selection exported %d files, a two-name selection %d; empty must mean every file",
			all.FileCount, result.FileCount)
	}
}

func zipLogEntryNames(t *testing.T, zipPath string) []string {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer reader.Close()
	names := []string{}
	for _, f := range reader.File {
		if strings.HasPrefix(f.Name, "logs/") {
			names = append(names, f.Name)
		}
	}
	return names
}

// TestExportDiagnosticBundleRedactsPlatformLogs covers the Android-only
// platform log path, which had no test: the logcat dump is caller-supplied
// text, it is written under platform/, and on the redacted path it must be
// filtered like any log file -- logcat carries the same addresses and ids the
// glog files do.
func TestExportDiagnosticBundleRedactsPlatformLogs(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "platform.zip")
	opts := NewExportOptions()
	opts.Redact = true
	opts.IncludePlatformLogs = true
	opts.AddPlatformLog("logcat.txt", "08-30 10:11:12.131 1 2 I ur: connected to 203.0.113.7:443\n")

	if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}

	content := readZipEntry(t, destPath, "platform/logcat.txt")
	if strings.Contains(content, "203.0.113.7") {
		t.Fatalf("platform/logcat.txt in a redacted bundle still contains the raw address: %q", content)
	}
	if !strings.Contains(content, "connected to ") {
		t.Fatalf("platform/logcat.txt lost its structure: %q", content)
	}

	// and the README advertises the directory only because it is there
	if readme := readZipEntry(t, destPath, "README.txt"); !strings.Contains(readme, "platform/") {
		t.Fatalf("README.txt does not mention the platform directory this bundle has:\n%s", readme)
	}
}

// TestExportDiagnosticBundleRedactsByteSliceAddresses carries the byte-slice
// leak through the whole export path, not just redactLine.
//
// Both lines here are the real shapes a device writes for one destination:
// ip_remote_multi_client.go:15474 prints an Ip4Path with %v, whose [4]byte
// fields fmt renders as a list of decimal bytes, and :5864 prints the same
// address as a dotted quad. A REDACTED bundle exported from a real iPhone
// masked the second and shipped the first in the clear.
//
// The whole entry is asserted, so a partial mask fails here too, and the two
// tokens are compared, because masking one destination to two tokens leaves a
// reader unable to tell that the two lines are about one flow.
func TestExportDiagnosticBundleRedactsByteSliceAddresses(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	name := "urnetwork.host.user.log.INFO.20260830-101112.4242"
	body := "I0831 22:47:58.387826 51714 ip_remote_multi_client.go:15474] [multi]max source count 3 = {tcp [0 0 0 0] 0 [17 23 18 34] 443 }\n" +
		"I0831 22:47:58.387830 51714 ip_remote_multi_client.go:5864] [multi]drop packet ipv4 p6 -> 17.23.18.34:443\n"
	if err := os.WriteFile(filepath.Join(appDir, name), []byte(body), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "redacted.zip")
	opts := NewExportOptions()
	opts.Redact = true
	if _, err := ExportDiagnosticBundle(destPath, opts); err != nil {
		t.Fatalf("ExportDiagnosticBundle = %v", err)
	}

	entry := readZipEntry(t, destPath, "logs/app/"+name)

	tokens := redactTokenPattern.FindAllString(entry, -1)
	if len(tokens) != 2 {
		t.Fatalf("want one address token on each of the two lines, got %v:\n%s", tokens, entry)
	}
	if tokens[0] != tokens[1] {
		t.Errorf("the byte-slice and dotted-quad renderings of one destination became %q and %q; one address must read as one token:\n%s",
			tokens[0], tokens[1], entry)
	}

	want := "I0831 22:47:58.387826 51714 ip_remote_multi_client.go:15474] [multi]max source count 3 = {tcp [0 0 0 0] 0 <addr> 443 }\n" +
		"I0831 22:47:58.387830 51714 ip_remote_multi_client.go:5864] [multi]drop packet ipv4 p6 -> <addr>\n"
	if got := normalizeRedactionTokens(entry); got != want {
		t.Errorf("redacted entry\n got %q\nwant %q", got, want)
	}
}
