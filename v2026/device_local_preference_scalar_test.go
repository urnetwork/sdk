// Complete scalar records cannot be confused with a valid prefix followed by
// damaged state. Tests cross actual required/optional Load and live consumers.
package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A required numeric policy must be completely observed before any preference
// or saved destination is adopted; a valid numeric prefix is not enough.
func TestDeviceLocalPreferenceIntegerPrefixCannotAdoptSavedConnection(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	for _, name := range []string{"provide-mode", "control-ip-family-policy"} {
		manager, fixture := testingPreferenceSpaceAt(t, t.TempDir())
		fixture.seedDistinctLogin(t)
		device := testingPreferenceDevice(t, fixture)
		target := testingSpecificPreferenceLocation()
		if err := fixture.localState.SetConnectLocation(target); err != nil {
			t.Fatal("could not seed saved connection")
		}
		if err := fixture.localState.SetCanRefer(true); err != nil {
			t.Fatal("could not seed another preference")
		}
		file, known := localPreferenceFile(name)
		if !known {
			t.Fatal("required integer is not cataloged")
		}
		path := filepath.Join(fixture.localState.localStorageDir, file)
		for _, invalid := range []string{"0trailing", "2 0", "0x1", "1.5", "2\n1", "1\x00", "1_000", "\xff", " \n\t"} {
			if err := os.WriteFile(path, []byte(invalid), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not seed malformed required integer")
			}
			result, err := device.Load()
			if result != nil || err == nil || err.Error() != "load "+name || device.GetCanRefer() ||
				device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
				t.Fatalf("numeric prefix was adopted instead of rejecting the complete record: %s", name)
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, []byte(invalid)) {
				t.Fatal("failed numeric observation changed stored evidence")
			}
			if saved, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(saved, target) {
				t.Fatal("failed numeric observation changed connection intent")
			}
		}
		testingJoinPreferenceDevice(t, device)
		manager.Close()
	}
}

// The string scalar has the same prefix hazard: a second token must not be
// discarded while its first token is adopted as the whole saved policy.
func TestDeviceLocalPreferenceNetworkExtraTokensCannotAdoptSavedConnection(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal("could not seed saved connection")
	}
	path := filepath.Join(fixture.localState.localStorageDir, ".provide_network_mode")
	for _, invalid := range []string{"wifi cellular", "wifi\ncellular", "all\ttrailing", "wifi\u2003cellular", " \n\t"} {
		if err := os.WriteFile(path, []byte(invalid), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed malformed network policy")
		}
		result, err := device.Load()
		if result != nil || err == nil || err.Error() != "load provide-network-mode" ||
			device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
			t.Fatal("network policy prefix was adopted instead of rejecting the complete record")
		}
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, []byte(invalid)) {
			t.Fatal("failed network policy observation changed stored evidence")
		}
		if saved, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(saved, target) {
			t.Fatal("failed network policy observation changed connection intent")
		}
	}
}

// Optional diagnostics retain their current value on failure without denying
// an otherwise valid saved destination or reporting the record as absent.
func TestDeviceLocalPreferenceOptionalIntegerPrefixKeepsLiveValueAndConnection(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetLogVerbosity(0)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal("could not seed saved connection")
	}
	path := filepath.Join(fixture.localState.localStorageDir, ".log_verbosity")
	invalid := []byte("1trailing")
	if err := os.WriteFile(path, invalid, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not seed malformed optional integer")
	}
	result, err := device.Load()
	if err != nil || result == nil || result.GetPreferenceError("log-verbosity") != "load log-verbosity" ||
		result.GetHasPreference("log-verbosity") || device.GetLogVerbosity() != 0 ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) == nil || device.GetAutoSave() {
		t.Fatal("optional numeric prefix changed a live setting or hid the failed observation")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, invalid) {
		t.Fatal("optional load rewrote stored evidence")
	}
}

// Existing complete decimal/string encodings stay compatible.
// Actual public writers still produce records that restore a real consumer.
func TestDeviceLocalPreferenceCompleteScalarRecordsRetainMeaning(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	for _, name := range []string{"provide-mode", "log-verbosity", "control-ip-family-policy"} {
		for _, token := range []struct {
			text  string
			value int
		}{
			{text: "0", value: 0}, {text: "+0", value: 0}, {text: "-0", value: 0},
			{text: "1", value: 1}, {text: "+1", value: 1}, {text: "0001", value: 1},
			{text: "-001", value: -1}, {text: " \t2 \n", value: 2},
		} {
			value, err := decodeLocalPreference(name, []byte(token.text))
			if err != nil || value != token.value {
				t.Fatalf("complete integer record changed meaning: %s", name)
			}
		}
	}
	for _, token := range []string{"wifi", "cellular", "all", "future-mode"} {
		value, err := decodeLocalPreference("provide-network-mode", []byte(" \t"+token+" \n"))
		if err != nil || value != token {
			t.Fatal("complete network token changed meaning or introduced enum validation")
		}
	}
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal("could not seed saved connection")
	}
	if err := fixture.localState.SetProvideMode(ProvideModeNone); err != nil {
		t.Fatal("could not write provide mode")
	}
	if err := fixture.localState.SetProvideNetworkMode(ProvideNetworkModeAll); err != nil {
		t.Fatal("could not write provide network mode")
	}
	if err := fixture.localState.SetControlIpFamilyPolicy(IpFamilyPolicyAuto); err != nil {
		t.Fatal("could not write control IP family policy")
	}
	if err := fixture.localState.SetLogVerbosity(0); err != nil {
		t.Fatal("could not write log verbosity")
	}
	device := testingPreferenceDevice(t, fixture)
	result, err := device.Load()
	if err != nil || result == nil || device.GetAutoSave() || testingPreferenceConsumer(device) == nil ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || device.GetProvideMode() != ProvideModeNone ||
		device.GetProvideNetworkMode() != ProvideNetworkModeAll || device.GetControlIpFamilyPolicy() != IpFamilyPolicyAuto || device.GetLogVerbosity() != 0 {
		t.Fatal("complete scalar records did not preserve existing live behavior and saved destination")
	}
	for _, name := range []string{"provide-mode", "provide-network-mode", "control-ip-family-policy", "log-verbosity"} {
		if !result.GetHasPreference(name) || result.GetPreferenceError(name) != "" {
			t.Fatal("complete scalar record was reported unavailable")
		}
	}
}

// Legacy getters retain their documented fallbacks and valid-value clamps.
// They must not manufacture a present integer policy from a damaged prefix.
func TestLocalStateScalarReadersRejectIncompleteRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := newLocalState(ctx, t.TempDir())
	for _, record := range []struct {
		file     string
		invalid  string
		fallback any
		valid    string
		want     any
		get      func() any
	}{
		{file: ".provide_mode", invalid: "1trailing", fallback: ProvideModeNone, valid: "1", want: ProvideMode(1), get: func() any { return state.GetProvideMode() }},
		{file: ".provide_network_mode", invalid: "all trailing", fallback: ProvideNetworkModeWiFi, valid: "all", want: ProvideNetworkModeAll, get: func() any { return state.GetProvideNetworkMode() }},
		{file: ".log_verbosity", invalid: "1trailing", fallback: LogVerbosityDefault, valid: "999", want: LogVerbosityTrace, get: func() any { return state.GetLogVerbosity() }},
		{file: controlIpFamilyPolicyFileName, invalid: "2trailing", fallback: IpFamilyPolicyAuto, valid: "999", want: IpFamilyPolicyAuto, get: func() any { return state.GetControlIpFamilyPolicy() }},
	} {
		path := filepath.Join(state.localStorageDir, record.file)
		if err := os.WriteFile(path, []byte(record.invalid), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed malformed legacy scalar")
		}
		if record.get() != record.fallback {
			t.Fatalf("legacy scalar getter accepted a damaged prefix: %s", record.file)
		}
		if record.file == ".log_verbosity" {
			if _, present := state.logVerbosityIfSet(); present {
				t.Fatal("damaged verbosity was reported as a chosen value")
			}
		}
		if record.file == controlIpFamilyPolicyFileName {
			if _, present := state.controlIpFamilyPolicyIfSet(); present {
				t.Fatal("damaged address-family policy was reported as a chosen value")
			}
		}
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, []byte(record.invalid)) {
			t.Fatal("legacy read rewrote stored evidence")
		}
		if err := os.WriteFile(path, []byte(record.valid), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed complete legacy scalar")
		}
		if record.get() != record.want {
			t.Fatalf("legacy scalar value or clamp changed: %s", record.file)
		}
	}
}

// Legacy fallback-only Boolean getters share the checked loader's token rule,
// while their distinct error defaults and accepted complete tokens remain.
func TestLocalStateBooleanReadersRejectIncompleteRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := newLocalState(ctx, t.TempDir())
	for _, record := range []struct {
		file     string
		invalid  string
		fallback bool
		get      func() bool
	}{
		{file: ".route_local-2", invalid: "false trailing", fallback: true, get: state.GetRouteLocal},
		{file: ".blocker_enabled", invalid: "true trailing", fallback: false, get: state.GetBlockerEnabled},
	} {
		path := filepath.Join(state.localStorageDir, record.file)
		if err := os.WriteFile(path, []byte(record.invalid), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed malformed legacy Boolean")
		}
		if record.get() != record.fallback {
			t.Fatalf("legacy Boolean getter accepted a damaged prefix: %s", record.file)
		}
		for _, token := range []struct {
			text  string
			value bool
		}{
			{text: "true", value: true}, {text: "false", value: false},
			{text: "1", value: true}, {text: "0", value: false},
			{text: "T", value: true}, {text: "F", value: false},
			{text: " TrUe ", value: true}, {text: " FaLsE ", value: false},
		} {
			if err := os.WriteFile(path, []byte(token.text), LocalStorageFilePermissions); err != nil {
				t.Fatal("could not seed complete legacy Boolean")
			}
			if record.get() != token.value {
				t.Fatal("complete legacy Boolean changed meaning")
			}
		}
	}
}

// The active space restores address-family policy before any DeviceLocal or
// Load exists. A malformed prefix must not overwrite the current process here.
func TestNetworkSpaceManagerMalformedScalarPreservesPreLoginPolicy(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directory := t.TempDir()
	key := *NewNetworkSpaceKey("scalar-bootstrap.test", "test")
	seed := newNetworkSpaceManagerWithContext(ctx, directory)
	seedState := newLocalState(ctx, seed.envStoragePath(&key))
	localDirectory := seedState.localStorageDir
	seedState.Close()
	seed.Close()
	path := filepath.Join(localDirectory, controlIpFamilyPolicyFileName)
	invalid := []byte("0trailing")
	if err := os.WriteFile(path, invalid, LocalStorageFilePermissions); err != nil {
		t.Fatal("could not seed malformed pre-login policy")
	}
	writeNetworkSpaceIndex(t, directory, []NetworkSpaceKey{key}, &key)
	SetControlIpFamilyPolicy(IpFamilyPolicyForce4)
	manager := newNetworkSpaceManagerWithContext(ctx, directory)
	t.Cleanup(manager.Close)
	active := manager.GetActiveNetworkSpace()
	if active == nil || active.GetHostName() != key.HostName {
		t.Fatal("cold manager did not restore the selected space")
	}
	if filepath.Join(active.GetAsyncLocalState().GetLocalState().localStorageDir, controlIpFamilyPolicyFileName) != path {
		t.Fatal("cold manager did not observe the seeded policy file")
	}
	if GetControlIpFamilyPolicy() != IpFamilyPolicyForce4 {
		t.Fatal("cold manager replaced pre-login policy with a damaged numeric prefix")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, invalid) {
		t.Fatal("cold manager changed the failed observation")
	}
	if err := active.GetAsyncLocalState().GetLocalState().SetControlIpFamilyPolicy(IpFamilyPolicyForce6); err != nil {
		t.Fatal("could not repair through the existing policy writer")
	}
	manager.Close()
	restored := newNetworkSpaceManagerWithContext(ctx, directory)
	t.Cleanup(restored.Close)
	if GetControlIpFamilyPolicy() != IpFamilyPolicyForce6 {
		t.Fatal("complete repaired policy did not restore before device construction")
	}
}

// Remote construction restores both globals and queues successful observations
// for first sync. Failed records must neither affect this process nor be sent
// to the extension; the actual dial is held and canceled without any socket.
func TestDeviceRemoteMalformedScalarsCannotRestoreOrQueueGlobals(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	for _, record := range []struct {
		file  string
		value string
	}{
		{file: ".log_verbosity", value: "1trailing"},
		{file: controlIpFamilyPolicyFileName, value: "2trailing"},
	} {
		if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, record.file), []byte(record.value), LocalStorageFilePermissions); err != nil {
			t.Fatal("could not seed malformed remote startup policy")
		}
	}
	if err := SetLogVerbosity(LogVerbosityDefault); err != nil {
		t.Fatal("could not set the existing process verbosity")
	}
	SetControlIpFamilyPolicy(IpFamilyPolicyForce4)
	dialer := &testingBlockedDeviceRpcDialer{entered: make(chan struct{}), release: make(chan struct{})}
	remote, err := newDeviceRemoteWithOverrides(fixture.networkSpace, fixture.initialJwt, fixture.instanceId,
		defaultDeviceRpcSettings(), connect.NewId(), dialer)
	if err != nil {
		t.Fatal("actual accepted remote construction failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := remote.CloseAndWait(ctx); err != nil {
			t.Error("remote startup fixture did not join")
		}
	})
	testingAwaitAuthBoundary(t, dialer.entered)
	if GetLogVerbosity() != LogVerbosityDefault || GetControlIpFamilyPolicy() != IpFamilyPolicyForce4 {
		t.Fatal("remote constructor applied damaged scalar prefixes to process globals")
	}
	remote.stateLock.Lock()
	level, policy := remote.state.LogVerbosity, remote.state.ControlIpFamilyPolicy
	remote.stateLock.Unlock()
	if level.IsSet || policy.IsSet {
		t.Fatal("remote constructor queued failed scalar observations for the extension")
	}
}
