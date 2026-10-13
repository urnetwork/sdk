package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The device default of the provider extender setting and its order of
// precedence (EXTENDER.md F3, G1): the setting the space stores, else on a
// space with no local state the value set on the device, else the device
// default; the embedder's hard switch is outside the setting and is checked
// where the role runs (device_local_extender_default_test.go).

// A space with no local state, which is what a headless embedder builds.
func testingProvideExtenderUrlSpace(t *testing.T) *NetworkSpace {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	networkSpace := NewNetworkSpaceWithUrls(
		ctx,
		"https://api.space.example",
		"wss://connect.space.example",
		strategySettings,
	)
	t.Cleanup(networkSpace.Close)
	if networkSpace.asyncLocalState != nil {
		t.Fatal("the url-only space kept local state")
	}
	return networkSpace
}

// A quiet device on the space with the given device default.
func testingProvideExtenderDevice(
	t *testing.T,
	networkSpace *NetworkSpace,
	defaultProvideExtender bool,
) *DeviceLocal {
	t.Helper()
	settings := testExtenderStatusDeviceSettings()
	settings.DefaultProvideExtender = defaultProvideExtender
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	return deviceLocal
}

// The defaults turn both controls on: the role follows providing unless the
// user turns the setting off (G1, F3).
func TestDefaultDeviceLocalSettingsTurnTheProvideExtenderOn(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	if !settings.ProvideExtenderEnabled {
		t.Fatal("the embedder's switch is off by default")
	}
	if !settings.DefaultProvideExtender {
		t.Fatal("the device default of the setting is off by default")
	}
}

// The order the device reads the setting in, on a space with local state and
// on one without. Only an explicit true or false is a stored value, so a
// corrupt file defers to the device default.
func TestDeviceLocalProvideExtenderPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		localState bool
		// written as the space's `.provide_extender` before the device is
		// built; no file when empty
		storedText             string
		defaultProvideExtender bool
		// a SetProvideExtender call on the device, when set
		set                   bool
		setProvideExtender    bool
		expectProvideExtender bool
	}{
		{name: "stored space, nothing stored, default on", localState: true, defaultProvideExtender: true, expectProvideExtender: true},
		{name: "stored space, nothing stored, default off", localState: true, defaultProvideExtender: false, expectProvideExtender: false},
		{name: "stored on wins over default off", localState: true, storedText: "true", defaultProvideExtender: false, expectProvideExtender: true},
		{name: "stored off wins over default on", localState: true, storedText: "false", defaultProvideExtender: true, expectProvideExtender: false},
		{name: "corrupt file, default off", localState: true, storedText: "nonsense", defaultProvideExtender: false, expectProvideExtender: false},
		{name: "corrupt file, default on", localState: true, storedText: "nonsense", defaultProvideExtender: true, expectProvideExtender: true},
		{name: "stored space, set on over default off", localState: true, defaultProvideExtender: false, set: true, setProvideExtender: true, expectProvideExtender: true},
		{name: "no local state, default on", localState: false, defaultProvideExtender: true, expectProvideExtender: true},
		{name: "no local state, default off", localState: false, defaultProvideExtender: false, expectProvideExtender: false},
		{name: "no local state, set on over default off", localState: false, defaultProvideExtender: false, set: true, setProvideExtender: true, expectProvideExtender: true},
		{name: "no local state, set off over default on", localState: false, defaultProvideExtender: true, set: true, setProvideExtender: false, expectProvideExtender: false},
	}
	for _, c := range cases {
		var networkSpace *NetworkSpace
		if c.localState {
			_, networkSpace = testExtenderStatusSpace(t)
			if c.storedText != "" {
				localState := networkSpace.asyncLocalState.GetLocalState()
				if err := os.WriteFile(
					filepath.Join(localState.localStorageDir, provideExtenderFileName),
					[]byte(c.storedText),
					LocalStorageFilePermissions,
				); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			networkSpace = testingProvideExtenderUrlSpace(t)
		}
		deviceLocal := testingProvideExtenderDevice(t, networkSpace, c.defaultProvideExtender)
		if c.set {
			deviceLocal.SetProvideExtender(c.setProvideExtender)
		}
		if provideExtender := deviceLocal.GetProvideExtender(); provideExtender != c.expectProvideExtender {
			t.Errorf("%s: setting = %t, expected %t", c.name, provideExtender, c.expectProvideExtender)
		}
		// a value set on a space with local state is stored there, where it
		// outlives the device
		if c.set && c.localState {
			provideExtender, stored := networkSpace.asyncLocalState.GetLocalState().getStoredProvideExtender()
			if !stored || provideExtender != c.setProvideExtender {
				t.Errorf("%s: stored = %t, %t, expected the value set", c.name, provideExtender, stored)
			}
		}
	}
}

// On a space with no local state the value set is the device's own, for its
// life: another device on the space keeps its default, and nothing is written
// anywhere a later device would read.
func TestDeviceLocalProvideExtenderWithoutLocalStateIsTheDevicesOwn(t *testing.T) {
	networkSpace := testingProvideExtenderUrlSpace(t)
	deviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	deviceLocal.SetProvideExtender(false)
	if deviceLocal.GetProvideExtender() {
		t.Fatal("the setting did not take on a space with no local state")
	}

	otherDeviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	if !otherDeviceLocal.GetProvideExtender() {
		t.Fatal("another device on the space took the value set on the first")
	}

	deviceLocal.Close()
	laterDeviceLocal := testingProvideExtenderDevice(t, networkSpace, true)
	if !laterDeviceLocal.GetProvideExtender() {
		t.Fatal("a later device on the space took the value set on a closed one")
	}
}

// Only an explicit true or false is a stored setting. The space's own getter
// keeps reading on where nothing is stored, the sdk default (F3), while the
// device applies its own default there.
func TestLocalStateStoresTheProvideExtenderSettingOnlyWhenExplicit(t *testing.T) {
	cases := []struct {
		name string
		// the file's contents, or no file when nil
		fileBytes             []byte
		expectStored          bool
		expectProvideExtender bool
		expectGet             bool
	}{
		{name: "no file", fileBytes: nil, expectStored: false, expectProvideExtender: false, expectGet: true},
		{name: "true", fileBytes: []byte("true"), expectStored: true, expectProvideExtender: true, expectGet: true},
		{name: "false", fileBytes: []byte("false"), expectStored: true, expectProvideExtender: false, expectGet: false},
		{name: "false with whitespace", fileBytes: []byte(" false\n"), expectStored: true, expectProvideExtender: false, expectGet: false},
		{name: "corrupt", fileBytes: []byte("nonsense"), expectStored: false, expectProvideExtender: false, expectGet: true},
		{name: "empty", fileBytes: []byte{}, expectStored: false, expectProvideExtender: false, expectGet: true},
	}
	for _, c := range cases {
		localState := newLocalState(context.Background(), t.TempDir())
		t.Cleanup(localState.Close)
		if c.fileBytes != nil {
			if err := os.WriteFile(
				filepath.Join(localState.localStorageDir, provideExtenderFileName),
				c.fileBytes,
				LocalStorageFilePermissions,
			); err != nil {
				t.Fatal(err)
			}
		}
		provideExtender, stored := localState.getStoredProvideExtender()
		if stored != c.expectStored || provideExtender != c.expectProvideExtender {
			t.Errorf("%s: stored = %t, %t, expected %t, %t",
				c.name, provideExtender, stored, c.expectProvideExtender, c.expectStored)
		}
		if get := localState.GetProvideExtender(); get != c.expectGet {
			t.Errorf("%s: get = %t, expected %t", c.name, get, c.expectGet)
		}
	}

	// a write stores the value, in the cache and in the file
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	if err := localState.SetProvideExtender(false); err != nil {
		t.Fatal(err)
	}
	if provideExtender, stored := localState.getStoredProvideExtender(); !stored || provideExtender {
		t.Fatalf("stored = %t, %t after a write of off", provideExtender, stored)
	}
	fileBytes, err := os.ReadFile(filepath.Join(localState.localStorageDir, provideExtenderFileName))
	if err != nil || !bytes.Equal(fileBytes, []byte("false")) {
		t.Fatalf("file = %q, %v after a write of off", fileBytes, err)
	}
}

// The host-facing constructor carries both controls to the device, and every
// other host-facing constructor keeps both on (G1, F3).
func TestNewDeviceLocalWithProvideExtenderCarriesBothControls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// these devices never reach an api: the test is the construction
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	clientJwt := testingNetworkClientJwt(connect.NewId(), testingIdentityNetworkA)

	cases := []struct {
		provideExtenderEnabled bool
		defaultProvideExtender bool
	}{
		{provideExtenderEnabled: true, defaultProvideExtender: true},
		{provideExtenderEnabled: true, defaultProvideExtender: false},
		{provideExtenderEnabled: false, defaultProvideExtender: true},
		{provideExtenderEnabled: false, defaultProvideExtender: false},
	}
	for _, c := range cases {
		seed := testingIdentitySeed(7)
		deviceLocal, err := NewDeviceLocalWithProvideExtender(
			networkSpace,
			clientJwt,
			"",
			"",
			"",
			NewId(),
			false,
			NewDeviceLocalKeyMaterial(seed, nil, nil),
			c.provideExtenderEnabled,
			c.defaultProvideExtender,
		)
		if err != nil {
			t.Fatal(err)
		}
		if deviceLocal.settings.ProvideExtenderEnabled != c.provideExtenderEnabled ||
			deviceLocal.settings.DefaultProvideExtender != c.defaultProvideExtender {
			t.Errorf("%+v: settings = %t, %t", c,
				deviceLocal.settings.ProvideExtenderEnabled, deviceLocal.settings.DefaultProvideExtender)
		}
		// nothing is stored, so the setting is the device default
		if provideExtender := deviceLocal.GetProvideExtender(); provideExtender != c.defaultProvideExtender {
			t.Errorf("%+v: setting = %t, expected the device default", c, provideExtender)
		}
		// and the key material is the identity the device runs on
		if !bytes.Equal(deviceLocal.GetClientKeySeed(), seed) {
			t.Errorf("%+v: the device did not take the key material", c)
		}
		deviceLocal.Close()
	}

	newDevices := []func() (*DeviceLocal, error){
		func() (*DeviceLocal, error) {
			return NewDeviceLocalWithDefaults(networkSpace, clientJwt, "", "", "", NewId(), false)
		},
		func() (*DeviceLocal, error) {
			return NewDeviceLocalWithKeyMaterial(networkSpace, clientJwt, "", "", "", NewId(), false, nil)
		},
		func() (*DeviceLocal, error) {
			return NewDeviceLocalWithMemoryTarget(networkSpace, clientJwt, "", "", "", NewId(), false, nil, 0)
		},
	}
	for i, newDevice := range newDevices {
		deviceLocal, err := newDevice()
		if err != nil {
			t.Fatal(err)
		}
		if !deviceLocal.settings.ProvideExtenderEnabled || !deviceLocal.settings.DefaultProvideExtender {
			t.Errorf("constructor %d: settings = %t, %t, expected both on", i,
				deviceLocal.settings.ProvideExtenderEnabled, deviceLocal.settings.DefaultProvideExtender)
		}
		deviceLocal.Close()
	}
}

// A device remote reads the setting the device derives, its default included,
// and on a space with no local state a write it makes is what the device then
// holds (N2, F3).
func TestDeviceRemoteProvideExtenderFollowsTheDeviceDefault(t *testing.T) {
	networkSpace := testingProvideExtenderUrlSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		func(settings *DeviceLocalSettings) {
			settings.DefaultProvideExtender = false
		},
	)
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), false)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)

	deviceRemote.SetProvideExtender(true)
	connect.AssertEqual(t, deviceLocal.GetProvideExtender(), true)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), true)

	deviceLocal.SetProvideExtender(false)
	connect.AssertEqual(t, deviceRemote.GetProvideExtender(), false)
}
