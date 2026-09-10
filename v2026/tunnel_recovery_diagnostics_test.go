package sdk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestTunnelRecoveryStageVocabularyAndBounds(t *testing.T) {
	restoreTestingLogDir(t)
	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatal("could not configure controlled diagnostic sink")
	}
	for _, stage := range []string{"auth-observation", "auth-reset", "key-load", "intent-load", "saved-load", "default-load", "destination", "destination-persist", "default-persist", "consumer", "rpc", "readiness", "settings", "wake", "path-recovery", "dns", "startup", "stop", "preferences-load", "auto-save", "destination-apply", "default-apply"} {
		line := RecordTunnelRecoveryStage(stage, "accepted", true, false, true, -1, -1)
		if !strings.Contains(line, "stage="+stage+" result=accepted") || !strings.HasSuffix(line, "providers=-1 generation=0") {
			t.Fatal("known stage or bounded negative values changed")
		}
	}
	if line := RecordTunnelRecoveryStage("consumer", "missing", true, false, true, -100, 0); !strings.Contains(line, "providers=-1 ") {
		t.Fatal("unavailable provider count became a measured zero")
	}
	for _, result := range []string{"started", "present", "missing", "accepted", "preserved", "completed", "superseded", "failed", "applied", "local", "establishing", "connected", "retry", "empty-window", "not-needed", "restored", "owned", "unowned", "user-disabled", "failure", "other", "skipped-unowned", "enabled", "disabled", "waiting", "timeout"} {
		line := RecordTunnelRecoveryStage("wake", result, true, true, true, 9_000_000, 9_000_000_000)
		if !strings.Contains(line, "result="+result+" ") || !strings.HasSuffix(line, "providers=1000000 generation=1000000000") {
			t.Fatal("known result or upper bounds changed")
		}
	}
	line := RecordTunnelRecoveryStage("private-marker\nBearer secret.jwt.value", "/private/path/error", true, false, false, 0, 12)
	if line != "[recovery] stage=unknown result=unknown intended=true consumer_present=false has_location=false providers=0 generation=12" {
		t.Fatal("unrecognized text leaked into the recovery line")
	}
}

// A real second process writes/flushes only the existing extension glog sink.
// It is inert during ordinary test discovery and takes no device/UI actions.
func TestTunnelRecoveryStageLogChild(t *testing.T) {
	root := os.Getenv("SDK_RECOVERY_LOG_ROOT")
	if root == "" {
		return
	}
	if err := SetLogDirForProcess(root, "extension"); err != nil {
		t.Fatal("could not configure extension diagnostic sink")
	}
	defer FlushGlog()
	RecordTunnelRecoveryStage("wake", "started", true, false, true, -1, 12)
	RecordTunnelRecoveryStage("readiness", "empty-window", true, true, true, 0, 12)
	RecordTunnelRecoveryStage("preferences-load", "skipped-unowned", false, false, false, -1, 12)
	RecordTunnelRecoveryStage("auto-save", "enabled", false, false, false, -1, 12)
	RecordTunnelRecoveryStage("auto-save", "disabled", false, false, false, -1, 12)
	RecordTunnelRecoveryStage("destination-apply", "failed", true, false, true, -1, 12)
	RecordTunnelRecoveryStage("default-apply", "applied", true, false, true, -1, 12)
	RecordTunnelRecoveryStage("private-marker\nBearer secret.jwt.value", "/private/path/error", true, false, false, 0, 12)
	testingRecoverySaveFailures(t)
	FlushGlog()
}

// The exported lines come from the real checked mutation, not a formatter
// fixture: first the file commit fails, then it commits before cancellation
// prevents live application. No external saver is installed in either case.
func testingRecoverySaveFailures(t *testing.T) {
	t.Helper()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.EnableRpc = false
	settings.ClientSettings.Log = connect.NewGlogLogger()
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} }
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, fixture.initialJwt,
		"recovery-log", "test", "0", fixture.instanceId, settings, connect.NewId())
	if err != nil {
		t.Fatal("actual diagnostic device construction failed")
	}
	t.Cleanup(func() { testingJoinPreferenceDevice(t, device) })
	device.SetUpgradeMuxSettings(nil)
	policyPath := filepath.Join(fixture.localState.localStorageDir, ".blocker_enabled")
	if err := os.WriteFile(policyPath, []byte("private-load-marker"), LocalStorageFilePermissions); err != nil {
		t.Fatal("could not seed required read failure")
	}
	if result, err := device.Load(); result != nil || err == nil || err.Error() != "load blocker-enabled" {
		t.Fatalf("required policy failure did not stop checked Load: result_present=%t error=%v", result != nil, err)
	}
	if err := os.Remove(policyPath); err != nil {
		t.Fatal("could not end controlled required failure")
	}
	for _, name := range []string{localDefaultLocationFileName, ".can_refer"} {
		if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, name), []byte("private-load-marker"), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed optional read failure")
		}
	}
	if result, err := device.Load(); err != nil || result == nil || result.GetDefaultError() == "" || result.GetPreferenceError("can-refer") == "" {
		t.Fatal("optional failure was confused with absence or required failure")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal("actual diagnostic device could not enable autosave")
	}
	target := testingSpecificPreferenceLocation()
	fixture.localState.testingBeforeLocationCommit = func(string) error {
		return errors.New("private-save-marker /private/path/error")
	}
	if err := device.SetConnectLocationChecked(target); err == nil || err.Error() != "save connect-location" {
		t.Fatal("actual storage failure did not return its fixed save stage")
	}
	failed := device.GetLastLocalStateSaveResult()
	if failed == nil || failed.GetSequence() != 1 || failed.GetSaved() || !failed.GetAutoSaveEnabled() ||
		failed.GetError() != "save connect-location" || testingPreferenceConsumer(device) != nil {
		t.Fatal("failed storage operation reported a durable or live destination")
	}
	if saved, err := fixture.localState.LoadConnectLocation(); err != nil || saved != nil {
		t.Fatal("failed first save left a committed destination")
	}
	fixture.localState.testingBeforeLocationCommit = func(string) error {
		device.cancel()
		return nil
	}
	if err := device.SetConnectLocationChecked(target); err == nil || err.Error() != "apply connect-location" {
		t.Fatal("committed but cancelled application did not report its fixed apply stage")
	}
	partial := device.GetLastLocalStateSaveResult()
	if partial == nil || partial.GetSequence() != 2 || !partial.GetSaved() || !partial.GetAutoSaveEnabled() ||
		partial.GetError() != "apply connect-location" || testingPreferenceConsumer(device) != nil || failed.GetSaved() {
		t.Fatal("partial operation lost its immutable commit versus application distinction")
	}
	if saved, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(saved, target) {
		t.Fatal("successful commit before cancellation was not actually durable")
	}
	testingJoinPreferenceDevice(t, device)
}

func TestTunnelRecoveryStageSurvivesSeparateProcessDiagnosticExport(t *testing.T) {
	restoreTestingLogDir(t)
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTunnelRecoveryStageLogChild$", "-test.count=1")
	command.Env = append(os.Environ(), "SDK_RECOVERY_LOG_ROOT="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("extension log writer did not complete: %v\n%s", err, output)
	}
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatal("could not configure app exporter")
	}
	for _, redact := range []bool{false, true} {
		opts := NewExportOptions()
		opts.Redact = redact
		path := filepath.Join(t.TempDir(), "recovery.zip")
		if _, err := ExportDiagnosticBundle(path, opts); err != nil {
			t.Fatal("existing diagnostic bundle export failed")
		}
		found := false
		foundMeasuredZero := false
		foundUnownedSkip := false
		foundAutoSave := false
		foundDisabled := false
		foundDestinationApply := false
		foundDefaultApply := false
		foundFailedSave := false
		foundSavedApplyFailure := false
		foundRequiredRead := false
		foundOptionalReads := false
		for _, name := range zipLogEntryNames(t, path) {
			if !strings.HasPrefix(name, "logs/extension/") {
				continue
			}
			body := readZipEntry(t, path, name)
			if strings.Contains(body, "[recovery] stage=wake result=started intended=true consumer_present=false has_location=true providers=-1 generation=12") {
				found = true
			}
			if strings.Contains(body, "[recovery] stage=readiness result=empty-window intended=true consumer_present=true has_location=true providers=0 generation=12") {
				foundMeasuredZero = true
			}
			foundUnownedSkip = foundUnownedSkip || strings.Contains(body, "[recovery] stage=preferences-load result=skipped-unowned intended=false consumer_present=false has_location=false providers=-1 generation=12")
			foundAutoSave = foundAutoSave || strings.Contains(body, "[recovery] stage=auto-save result=enabled intended=false consumer_present=false has_location=false providers=-1 generation=12")
			foundDisabled = foundDisabled || strings.Contains(body, "[recovery] stage=auto-save result=disabled intended=false consumer_present=false has_location=false providers=-1 generation=12")
			foundDestinationApply = foundDestinationApply || strings.Contains(body, "[recovery] stage=destination-apply result=failed intended=true consumer_present=false has_location=true providers=-1 generation=12")
			foundDefaultApply = foundDefaultApply || strings.Contains(body, "[recovery] stage=default-apply result=applied intended=true consumer_present=false has_location=true providers=-1 generation=12")
			foundFailedSave = foundFailedSave || strings.Contains(body, "[local-state] save sequence=1 preference=connect-location auto_save=true saved=false error=save connect-location")
			foundSavedApplyFailure = foundSavedApplyFailure || strings.Contains(body, "[local-state] save sequence=2 preference=connect-location auto_save=true saved=true error=apply connect-location")
			foundRequiredRead = foundRequiredRead || strings.Contains(body, "[local-state] load result=failed preference=blocker-enabled stage=read")
			foundOptionalReads = foundOptionalReads || strings.Contains(body, "[local-state] load result=completed optional_unavailable=default-location,can-refer")
			for _, secret := range []string{"private-marker", "private-save-marker", "private-load-marker", "Bearer secret.jwt.value", "/private/path/error"} {
				if strings.Contains(body, secret) {
					t.Fatal("diagnostic export retained untrusted recovery input")
				}
			}
		}
		if !found || !foundMeasuredZero || !foundUnownedSkip || !foundAutoSave || !foundDisabled ||
			!foundDestinationApply || !foundDefaultApply || !foundFailedSave || !foundSavedApplyFailure || !foundRequiredRead || !foundOptionalReads {
			t.Fatal("existing ZIP export lost the other process's actual recovery line")
		}
	}
}
