package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The kill switch, split rules, dns settings and blocker an app sets through
// its DeviceRemote survive the device the remote controls
// (device_remote_saved_settings.go).
//
// The windows service and the linux daemon build a new DeviceLocal for each
// tunnel session and after a restart, and that device restores nothing it was
// told over the rpc. These tests stand in for that device with the remote's
// first sync request, which is what a new device is given, and for the app's
// next launch with a second remote over the same local state. None of them
// needs a device or an rpc: the remotes have nothing listening on their address
// (newTestDeviceRemoteWithNoServiceInSpace), and the reverse rpc callback is
// run in place. The one device test drives DeviceLocalRpc.Sync over a pipe.
//
// The dns settings carry a personal id: the tests compare them without
// printing them.

// The app process's space: its local state is the app's own.
func newSavedSettingsTestSpace(t *testing.T) (*NetworkSpace, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	return networkSpace, byJwt
}

func newSavedSettingsTestSplitRule(host string, local bool) *BlockActionOverride {
	hosts := NewStringList()
	hosts.Add(host)
	return &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         hosts,
		RouteOverride: &RouteOverride{Local: local},
	}
}

func newSavedSettingsTestSplitRules(overrides ...*BlockActionOverride) *BlockActionOverrideList {
	list := NewBlockActionOverrideList()
	for _, override := range overrides {
		list.Add(override)
	}
	return list
}

// The ids and hosts of the rules, in order, with whether each is excluded from
// the tunnel (route local).
func savedSettingsTestRuleKeys(overrides *BlockActionOverrideList) []string {
	if overrides == nil {
		return nil
	}
	keys := []string{}
	for _, override := range overrides.getAll() {
		key := override.OverrideId.String()
		for _, host := range stringsFromStringList(override.Hosts) {
			key += " " + host
		}
		if override.RouteOverride != nil && override.RouteOverride.Local {
			key += " local"
		}
		keys = append(keys, key)
	}
	return keys
}

func savedSettingsTestRuleRpcKeys(overridesRpc []*BlockActionOverrideRpc) []string {
	return savedSettingsTestRuleKeys(toBlockActionOverrideList(overridesRpc))
}

type savedSettingsTestCommit chan bool

func (self savedSettingsTestCommit) Complete(success bool) {
	self <- success
}

// Waits for the saves queued on the app's local state so far: its jobs run one
// at a time, in the order they were queued.
func waitForSavedSettingsTestSaves(t *testing.T, networkSpace *NetworkSpace) {
	t.Helper()
	done := make(savedSettingsTestCommit, 1)
	networkSpace.GetAsyncLocalState().serialAsync(func() error {
		return nil
	}, done)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the app's local state never ran the queued saves")
	}
}

// The device's report of its split rules, through the reverse rpc entry point.
func reportSavedSettingsTestSplitRules(t *testing.T, deviceRemote *DeviceRemote, overrides *BlockActionOverrideList) {
	t.Helper()
	deviceRemoteRpc := &DeviceRemoteRpc{
		ctx:          context.Background(),
		deviceRemote: deviceRemote,
		callbacks:    make(chan func(), 1),
	}
	err := deviceRemoteRpc.BlockActionOverridesChanged(&DeviceRemoteBlockActionOverrides{
		BlockActionOverrides: newBlockActionOverridesRpc(overrides),
	}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	callback := <-deviceRemoteRpc.callbacks
	callback()
}

// support inbox 8227: split rules came back empty after a restart. The new
// device the remote synced with was given no rules, though the app had saved
// them.
func TestDeviceRemoteQueuesTheSavedSplitRulesForItsDevice(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	saved := newSavedSettingsTestSplitRules(
		newSavedSettingsTestSplitRule("bank.example", true),
		newSavedSettingsTestSplitRule("**.stream.example", false),
	)
	if err := networkSpace.GetAsyncLocalState().GetLocalState().SetBlockActionOverrides(saved); err != nil {
		t.Fatalf("save: %v", err)
	}
	want := savedSettingsTestRuleKeys(saved)

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	// the app shows the rules while the tunnel is down
	if got := savedSettingsTestRuleKeys(deviceRemote.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("rules with the tunnel down: %v, want %v", got, want)
	}
	// and the device gets them at the first sync
	syncRequest := deviceRemote.takeSyncRequest()
	if !syncRequest.State.BlockActionOverrides.IsSet {
		t.Fatal("the first sync gives the new device no split rules, so they are gone after a restart")
	}
	if got := savedSettingsTestRuleRpcKeys(syncRequest.State.BlockActionOverrides.Value); !slices.Equal(got, want) {
		t.Fatalf("the first sync gives the device %v, want %v", got, want)
	}
}

// support inbox 8200, for the device: the kill switch came back off after a
// restart. The new device the remote synced with started with route local on,
// though the app had saved it off.
func TestDeviceRemoteQueuesTheSavedKillSwitchForItsDevice(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	// the kill switch on
	if err := networkSpace.GetAsyncLocalState().GetLocalState().SetRouteLocal(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	if deviceRemote.GetRouteLocal() {
		t.Fatal("route local is on with the tunnel down: the kill switch reads off")
	}
	syncRequest := deviceRemote.takeSyncRequest()
	if !syncRequest.State.RouteLocal.IsSet || syncRequest.State.RouteLocal.Value {
		t.Fatalf("the first sync gives the device route local %+v, want off: the kill switch is off on the new device", syncRequest.State.RouteLocal)
	}
}

// Nothing saved is no instruction: the device keeps its own kill switch and
// rules until the app sets them.
func TestDeviceRemoteWithNothingSavedQueuesNoKillSwitchOrSplitRules(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	if !deviceRemote.GetRouteLocal() {
		t.Fatal("route local is off with nothing saved, want the default")
	}
	syncRequest := deviceRemote.takeSyncRequest()
	if syncRequest.State.RouteLocal.IsSet {
		t.Fatal("route local was queued with nothing saved")
	}
	if syncRequest.State.BlockActionOverrides.IsSet {
		t.Fatal("split rules were queued with nothing saved, which would clear the device's")
	}
}

// support inbox 8227: a split rule added through the remote was given only to
// the device, which did not keep it, and was gone after a restart.
func TestDeviceRemoteSavesTheSplitRulesItEditsForTheNextDevice(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	first := newSavedSettingsTestSplitRule("bank.example", true)
	if err := localState.SetBlockActionOverrides(newSavedSettingsTestSplitRules(first)); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	second := newSavedSettingsTestSplitRule("mail.example", false)
	deviceRemote.AddBlockActionOverride(second)
	waitForSavedSettingsTestSaves(t, networkSpace)

	want := savedSettingsTestRuleKeys(newSavedSettingsTestSplitRules(first, second))
	if got := savedSettingsTestRuleKeys(localState.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("saved rules after the add: %v, want %v", got, want)
	}

	// the next launch, or the next tunnel session, gives its device both
	nextDeviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	syncRequest := nextDeviceRemote.takeSyncRequest()
	if got := savedSettingsTestRuleRpcKeys(syncRequest.State.BlockActionOverrides.Value); !syncRequest.State.BlockActionOverrides.IsSet || !slices.Equal(got, want) {
		t.Fatalf("the next device is given %v, want %v", got, want)
	}

	// an edit of one rule, as the apps make it: the whole list
	edited := newSavedSettingsTestSplitRule("mail.example", true)
	edited.OverrideId = second.OverrideId
	nextDeviceRemote.SetBlockActionOverrides(newSavedSettingsTestSplitRules(first, edited))
	waitForSavedSettingsTestSaves(t, networkSpace)
	want = savedSettingsTestRuleKeys(newSavedSettingsTestSplitRules(first, edited))
	if got := savedSettingsTestRuleKeys(localState.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("saved rules after the edit: %v, want %v", got, want)
	}
}

// The device's own list is the whole list: a remote that never had one saved
// keeps the one its device reports, so the next launch shows and queues it.
func TestDeviceRemoteSavesTheSplitRulesItsDeviceReports(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	reported := newSavedSettingsTestSplitRules(newSavedSettingsTestSplitRule("bank.example", true))
	reportSavedSettingsTestSplitRules(t, deviceRemote, reported)
	waitForSavedSettingsTestSaves(t, networkSpace)

	want := savedSettingsTestRuleKeys(reported)
	if got := savedSettingsTestRuleKeys(localState.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("saved rules after the device's report: %v, want %v", got, want)
	}
}

// Removing the last rule saves an empty list, never nothing: nothing would
// leave the next device with the rules the user removed.
func TestDeviceRemoteSavesTheLastSplitRuleRemovedAsAnEmptyList(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	only := newSavedSettingsTestSplitRule("bank.example", true)
	if err := localState.SetBlockActionOverrides(newSavedSettingsTestSplitRules(only)); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.RemoveBlockActionOverride(only.OverrideId)
	waitForSavedSettingsTestSaves(t, networkSpace)

	saved := localState.GetBlockActionOverrides()
	if saved == nil || saved.Len() != 0 {
		t.Fatalf("saved rules after removing the last one: %v, want an empty list", savedSettingsTestRuleKeys(saved))
	}
	syncRequest := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt).takeSyncRequest()
	if !syncRequest.State.BlockActionOverrides.IsSet || len(syncRequest.State.BlockActionOverrides.Value) != 0 {
		t.Fatal("the next device is not told the rules were removed")
	}
}

// An edit of a list the remote never saw is not the whole list. Saving it would
// replace the device's rules with it at the next remote's first sync.
func TestDeviceRemoteDoesNotSaveAnEditOfSplitRulesItNeverSaw(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.AddBlockActionOverride(newSavedSettingsTestSplitRule("bank.example", true))
	waitForSavedSettingsTestSaves(t, networkSpace)

	if saved := localState.GetBlockActionOverrides(); saved != nil {
		t.Fatalf("saved %v from a list the remote never saw", savedSettingsTestRuleKeys(saved))
	}
}

// A device's list that arrives while an edit is queued predates the edit, which
// the next sync applies over it.
func TestDeviceRemoteDoesNotSaveADeviceReportThatPredatesAQueuedEdit(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	first := newSavedSettingsTestSplitRule("bank.example", true)
	if err := localState.SetBlockActionOverrides(newSavedSettingsTestSplitRules(first)); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	second := newSavedSettingsTestSplitRule("mail.example", false)
	deviceRemote.AddBlockActionOverride(second)
	reportSavedSettingsTestSplitRules(t, deviceRemote, newSavedSettingsTestSplitRules(first))
	waitForSavedSettingsTestSaves(t, networkSpace)

	want := savedSettingsTestRuleKeys(newSavedSettingsTestSplitRules(first, second))
	if got := savedSettingsTestRuleKeys(localState.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("saved rules: %v, want %v with the queued add", got, want)
	}
}

// Signing out wipes the account's local settings, split rules included
// (LocalState.Logout). A save after it must not bring them back for the next
// account.
func TestDeviceRemoteDropsASplitRuleSaveAfterSignOut(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	if err := localState.SetBlockActionOverrides(newSavedSettingsTestSplitRules(newSavedSettingsTestSplitRule("bank.example", true))); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	if err := localState.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	deviceRemote.AddBlockActionOverride(newSavedSettingsTestSplitRule("mail.example", false))
	waitForSavedSettingsTestSaves(t, networkSpace)

	if saved := localState.GetBlockActionOverrides(); saved != nil {
		t.Fatalf("saved %v after the sign-out", savedSettingsTestRuleKeys(saved))
	}
}

// A hosted device's rules are its own, not the app's: a remote to one neither
// queues the app's saved settings nor saves its rules over them.
func TestHostedDeviceRemoteNeitherQueuesNorSavesTheAppSettings(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	saved := newSavedSettingsTestSplitRules(newSavedSettingsTestSplitRule("bank.example", true))
	if err := localState.SetBlockActionOverrides(saved); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := localState.SetRouteLocal(false); err != nil {
		t.Fatalf("save: %v", err)
	}

	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	settings.DisableHostedIncompatible = true
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		NewId().toConnectId(),
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	t.Cleanup(deviceRemote.Close)

	syncRequest := deviceRemote.takeSyncRequest()
	if syncRequest.State.RouteLocal.IsSet || syncRequest.State.BlockActionOverrides.IsSet {
		t.Fatal("the app's settings were queued for a hosted device")
	}
	reportSavedSettingsTestSplitRules(t, deviceRemote, newSavedSettingsTestSplitRules())
	waitForSavedSettingsTestSaves(t, networkSpace)
	want := savedSettingsTestRuleKeys(saved)
	if got := savedSettingsTestRuleKeys(localState.GetBlockActionOverrides()); !slices.Equal(got, want) {
		t.Fatalf("the hosted device's rules replaced the app's: %v, want %v", got, want)
	}
}

// The personal id the dns tests save. The synthetic personal preset takes six
// hex digits (testingDnsPresetCatalog), which no id in a log line has as a
// whole word.
const savedSettingsTestPersonalId = "c0ffee"

// A personal adblock preset with its id, and Fast DNS on connect on.
func newSavedSettingsTestPresetDnsSettings(t *testing.T) *DnsResolverSettings {
	t.Helper()
	settings := testingPresetSettings(t, "gamma-personal", savedSettingsTestPersonalId)
	settings.EnableFallback = true
	return settings
}

// Custom servers, with Fast DNS on connect off.
func newSavedSettingsTestCustomDnsSettings() *DnsResolverSettings {
	settings := DnsResolverSettingsAsCustom(GetDefaultDnsResolverSettings())
	settings.RemoteDohUrlsIpv4 = testing_stringList("https://192.0.2.80/dns-query")
	settings.RemoteDohUrlsIpv6 = testing_stringList("https://[2001:db8::80]/dns-query")
	settings.EnableFallback = false
	return settings
}

// Whether two dns settings are the same, compared without printing them.
func sameSavedSettingsTestDnsSettings(a *DnsResolverSettings, b *DnsResolverSettings) bool {
	return reflect.DeepEqual(testingDnsResolverSettingsValue(a), testingDnsResolverSettingsValue(b))
}

// The device's report of its dns settings, through the reverse rpc entry
// point.
func reportSavedSettingsTestDnsSettings(t *testing.T, deviceRemote *DeviceRemote, settings *DnsResolverSettings) {
	t.Helper()
	deviceRemoteRpc := &DeviceRemoteRpc{
		ctx:          context.Background(),
		deviceRemote: deviceRemote,
		callbacks:    make(chan func(), 1),
	}
	err := deviceRemoteRpc.DnsResolverSettingsChanged(&DeviceRemoteDnsResolverSettings{
		DnsResolverSettings: newDnsResolverSettingsRpc(settings),
	}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	callback := <-deviceRemoteRpc.callbacks
	callback()
}

// The device's report of its blocker, through the reverse rpc entry point.
func reportSavedSettingsTestBlocker(t *testing.T, deviceRemote *DeviceRemote, blockerEnabled bool) {
	t.Helper()
	deviceRemoteRpc := &DeviceRemoteRpc{
		ctx:          context.Background(),
		deviceRemote: deviceRemote,
		callbacks:    make(chan func(), 1),
	}
	if err := deviceRemoteRpc.BlockerEnabledChanged(blockerEnabled, nil); err != nil {
		t.Fatalf("report: %v", err)
	}
	callback := <-deviceRemoteRpc.callbacks
	callback()
}

// Whether the app's local state holds a saved blocker record at all.
func savedSettingsTestBlockerSaved(localState *LocalState) bool {
	_, err := os.Stat(filepath.Join(localState.localStorageDir, ".blocker_enabled"))
	return err == nil
}

// support inbox 8227, for the dns settings: a personal adblock preset, its id,
// custom servers and Fast DNS on connect came back as the defaults after a
// restart. The new device the remote synced with was given none of the
// settings the app had saved.
func TestDeviceRemoteQueuesTheSavedDnsSettingsForItsDevice(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	for _, c := range []struct {
		name     string
		settings func() *DnsResolverSettings
	}{
		{name: "personal preset with fast dns on connect", settings: func() *DnsResolverSettings { return newSavedSettingsTestPresetDnsSettings(t) }},
		{name: "custom servers without fast dns on connect", settings: newSavedSettingsTestCustomDnsSettings},
	} {
		networkSpace, byJwt := newSavedSettingsTestSpace(t)
		localState := networkSpace.GetAsyncLocalState().GetLocalState()
		if err := localState.SetDnsResolverSettings(c.settings()); err != nil {
			t.Fatalf("%s: save: %v", c.name, err)
		}
		saved := localState.GetDnsResolverSettings()

		deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

		if !sameSavedSettingsTestDnsSettings(deviceRemote.GetDnsResolverSettings(), saved) {
			t.Errorf("%s: the dns settings with the tunnel down are not the saved ones", c.name)
			continue
		}
		syncRequest := deviceRemote.takeSyncRequest()
		if !syncRequest.State.DnsResolverSettings.IsSet ||
			!sameSavedSettingsTestDnsSettings(syncRequest.State.DnsResolverSettings.Value.toDnsResolverSettings(), saved) {
			t.Errorf("%s: the first sync does not give the new device the saved dns settings", c.name)
		}
	}
}

// The ad blocker came back off after a restart: the new device the remote
// synced with was not given the blocker the app had saved.
func TestDeviceRemoteQueuesTheSavedBlockerForItsDevice(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	if err := networkSpace.GetAsyncLocalState().GetLocalState().SetBlockerEnabled(true); err != nil {
		t.Fatalf("save: %v", err)
	}

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	if !deviceRemote.GetBlockerEnabled() {
		t.Fatal("the blocker is off with the tunnel down")
	}
	syncRequest := deviceRemote.takeSyncRequest()
	if !syncRequest.State.BlockerEnabled.IsSet || !syncRequest.State.BlockerEnabled.Value {
		t.Fatalf("the first sync gives the device blocker %+v, want on", syncRequest.State.BlockerEnabled)
	}
}

// Nothing saved is no instruction: the device keeps its own dns settings and
// blocker until the app sets them.
func TestDeviceRemoteWithNothingSavedQueuesNoDnsSettingsOrBlocker(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)

	syncRequest := deviceRemote.takeSyncRequest()
	if syncRequest.State.DnsResolverSettings.IsSet {
		t.Fatal("dns settings were queued with nothing saved")
	}
	if syncRequest.State.BlockerEnabled.IsSet {
		t.Fatal("the blocker was queued with nothing saved")
	}
}

// The dns settings the app set through the remote were given only to the
// device, which did not keep them, and were gone after a restart.
func TestDeviceRemoteSavesTheDnsSettingsTheAppSetsForTheNextDevice(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.SetDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t))
	waitForSavedSettingsTestSaves(t, networkSpace)

	saved := localState.GetDnsResolverSettings()
	if saved == nil || !sameSavedSettingsTestDnsSettings(saved, deviceRemote.GetDnsResolverSettings()) {
		t.Fatal("the dns settings the app set were not saved")
	}
	// the next launch, or the next tunnel session, gives its device them
	syncRequest := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt).takeSyncRequest()
	if !syncRequest.State.DnsResolverSettings.IsSet ||
		!sameSavedSettingsTestDnsSettings(syncRequest.State.DnsResolverSettings.Value.toDnsResolverSettings(), saved) {
		t.Fatal("the next device is not given the dns settings the app set")
	}
}

// The device's own dns settings are the whole value: a remote that never had
// any saved keeps the ones its device reports.
func TestDeviceRemoteSavesTheDnsSettingsItsDeviceReports(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	reported := ownDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t))
	reportSavedSettingsTestDnsSettings(t, deviceRemote, reported)
	waitForSavedSettingsTestSaves(t, networkSpace)

	if !sameSavedSettingsTestDnsSettings(localState.GetDnsResolverSettings(), reported) {
		t.Fatal("the dns settings the device reported were not saved")
	}
}

// Dns settings the device reports while the app's are queued predate them:
// the next sync applies the queued ones over them.
func TestDeviceRemoteDoesNotSaveADnsReportThatPredatesAQueuedWrite(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.SetDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t))
	queued := deviceRemote.GetDnsResolverSettings()
	reportSavedSettingsTestDnsSettings(t, deviceRemote, ownDnsResolverSettings(newSavedSettingsTestCustomDnsSettings()))
	waitForSavedSettingsTestSaves(t, networkSpace)

	if !sameSavedSettingsTestDnsSettings(localState.GetDnsResolverSettings(), queued) {
		t.Fatal("the saved dns settings are not the queued ones")
	}
}

// The blocker the app set through the remote was given only to the device,
// which did not keep it, and was off after a restart.
func TestDeviceRemoteSavesTheBlockerTheAppSetsForTheNextDevice(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.SetBlockerEnabled(true)
	waitForSavedSettingsTestSaves(t, networkSpace)

	if !savedSettingsTestBlockerSaved(localState) || !localState.GetBlockerEnabled() {
		t.Fatal("the blocker the app turned on was not saved")
	}
	syncRequest := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt).takeSyncRequest()
	if !syncRequest.State.BlockerEnabled.IsSet || !syncRequest.State.BlockerEnabled.Value {
		t.Fatal("the next device is not given the blocker the app turned on")
	}

	// turned off is saved too, so the next device turns it off
	deviceRemote.SetBlockerEnabled(false)
	waitForSavedSettingsTestSaves(t, networkSpace)
	if !savedSettingsTestBlockerSaved(localState) || localState.GetBlockerEnabled() {
		t.Fatal("the blocker the app turned off was not saved")
	}
}

// The device's own blocker setting is the whole value: a remote keeps the one
// its device reports.
func TestDeviceRemoteSavesTheBlockerItsDeviceReports(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	reportSavedSettingsTestBlocker(t, deviceRemote, true)
	waitForSavedSettingsTestSaves(t, networkSpace)

	if !savedSettingsTestBlockerSaved(localState) || !localState.GetBlockerEnabled() {
		t.Fatal("the blocker the device reported was not saved")
	}
}

// A blocker the device reports while the app's is queued predates it.
func TestDeviceRemoteDoesNotSaveABlockerReportThatPredatesAQueuedWrite(t *testing.T) {
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	deviceRemote.SetBlockerEnabled(true)
	reportSavedSettingsTestBlocker(t, deviceRemote, false)
	waitForSavedSettingsTestSaves(t, networkSpace)

	if !savedSettingsTestBlockerSaved(localState) || !localState.GetBlockerEnabled() {
		t.Fatal("the blocker the device reported before the queued write replaced it")
	}
}

// Signing out wipes the account's local settings (LocalState.Logout). A save
// after it must not bring its dns settings or blocker back for the next
// account.
func TestDeviceRemoteDropsDnsAndBlockerSavesAfterSignOut(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, networkSpace, byJwt)
	if err := localState.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	deviceRemote.SetDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t))
	deviceRemote.SetBlockerEnabled(true)
	waitForSavedSettingsTestSaves(t, networkSpace)

	if localState.GetDnsResolverSettings() != nil {
		t.Fatal("dns settings were saved after the sign-out")
	}
	if savedSettingsTestBlockerSaved(localState) {
		t.Fatal("the blocker was saved after the sign-out")
	}
}

// A hosted device's dns settings and blocker are its own, not the app's.
func TestHostedDeviceRemoteNeitherQueuesNorSavesTheDnsSettingsOrBlocker(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	if err := localState.SetDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t)); err != nil {
		t.Fatalf("save: %v", err)
	}
	saved := localState.GetDnsResolverSettings()

	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	settings.DisableHostedIncompatible = true
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		NewId().toConnectId(),
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	t.Cleanup(deviceRemote.Close)

	syncRequest := deviceRemote.takeSyncRequest()
	if syncRequest.State.DnsResolverSettings.IsSet || syncRequest.State.BlockerEnabled.IsSet {
		t.Fatal("the app's dns settings or blocker were queued for a hosted device")
	}
	reportSavedSettingsTestDnsSettings(t, deviceRemote, ownDnsResolverSettings(newSavedSettingsTestCustomDnsSettings()))
	reportSavedSettingsTestBlocker(t, deviceRemote, true)
	deviceRemote.SetBlockerEnabled(true)
	waitForSavedSettingsTestSaves(t, networkSpace)
	if !sameSavedSettingsTestDnsSettings(localState.GetDnsResolverSettings(), saved) {
		t.Fatal("the hosted device's dns settings replaced the app's")
	}
	if savedSettingsTestBlockerSaved(localState) {
		t.Fatal("the hosted device's blocker was saved as the app's")
	}
}

// Records every line, those at any verbosity included.
type testingSavedSettingsLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testingSavedSettingsLogger) record(format string, args ...any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *testingSavedSettingsLogger) allLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *testingSavedSettingsLogger) Info(args ...any) {
	self.record("%s", fmt.Sprint(args...))
}

func (self *testingSavedSettingsLogger) Infof(format string, args ...any) {
	self.record(format, args...)
}

func (self *testingSavedSettingsLogger) Warningf(format string, args ...any) {
	self.record(format, args...)
}

func (self *testingSavedSettingsLogger) Errorf(format string, args ...any) {
	self.record(format, args...)
}

func (self *testingSavedSettingsLogger) V(int32) connect.Verbose {
	return testingSavedSettingsVerbose{logger: self}
}

type testingSavedSettingsVerbose struct {
	logger *testingSavedSettingsLogger
}

func (self testingSavedSettingsVerbose) Enabled() bool { return true }

func (self testingSavedSettingsVerbose) Info(args ...any) {
	self.logger.record("%s", fmt.Sprint(args...))
}

func (self testingSavedSettingsVerbose) Infof(format string, args ...any) {
	self.logger.record(format, args...)
}

// A failed save logs which setting failed and nothing of its value: the dns
// settings carry a personal id, which the failure's own error text names here.
func TestDeviceRemoteNeverLogsTheDnsPersonalId(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	networkSpace, byJwt := newSavedSettingsTestSpace(t)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	// the commit fails with an error that names the id
	localState.testingBeforePreferenceCommit = func(name string) error {
		return errors.New("refused commit for " + savedSettingsTestPersonalId)
	}
	logger := &testingSavedSettingsLogger{}
	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	settings.ClientSettings.Log = logger
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		settings,
		NewId().toConnectId(),
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatalf("device remote: %v", err)
	}
	t.Cleanup(deviceRemote.Close)

	deviceRemote.SetDnsResolverSettings(newSavedSettingsTestPresetDnsSettings(t))
	waitForSavedSettingsTestSaves(t, networkSpace)

	personalId := regexp.MustCompile(`\b` + savedSettingsTestPersonalId + `\b`)
	failureLogged := false
	for _, line := range logger.allLines() {
		if personalId.MatchString(line) || strings.Contains(line, "refused commit") || strings.Contains(line, "id.gamma.example") {
			t.Fatal("a log line carries the personal id")
		}
		if strings.Contains(line, "save dns settings failed") {
			failureLogged = true
		}
	}
	if !failureLogged {
		t.Fatal("the failed dns settings save was not logged")
	}
}

// The resolver settings a device has installed, read under its lock.
func testingSavedSettingsInstalledMuxSettings(device *DeviceLocal) *connect.UpgradeMuxSettings {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return device.upgradeMuxSettings
}

// A sync that carries the device's own dns settings leaves its resolvers
// alone. Every remote now queues the app's saved settings, also on an attach
// to a running device, and applying them again installed them anew, which
// restarts a live device's resolvers.
func TestDeviceLocalRpcSyncLeavesTheDevicesOwnDnsSettingsAlone(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	_, client := testingPreferenceRpc(t, device)

	var void RpcVoid
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SetDnsResolverSettings",
		&DeviceRemoteDnsResolverSettings{DnsResolverSettings: newDnsResolverSettingsRpc(newSavedSettingsTestPresetDnsSettings(t))}, &void); err != nil {
		t.Fatal(err)
	}
	installed := testingSavedSettingsInstalledMuxSettings(device)
	saveSequence := device.GetLastLocalStateSaveResult().GetSequence()

	sync := func(settings *DnsResolverSettings) {
		t.Helper()
		request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
		// queued as a remote queues them (DeviceRemote.SetDnsResolverSettings)
		request.State.DnsResolverSettings.Set(newDnsResolverSettingsRpc(dnsResolverSettingsAsEdited(settings)))
		response := &DeviceRemoteSyncResponse{}
		if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, response); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if response.Error != "" {
			t.Fatalf("sync: %s", response.Error)
		}
	}

	// the app's copy of what the device has, as a new remote queues it
	sync(device.GetDnsResolverSettings())
	if testingSavedSettingsInstalledMuxSettings(device) != installed {
		t.Fatal("the sync installed the device's own dns settings again, restarting its resolvers")
	}
	if device.GetLastLocalStateSaveResult().GetSequence() != saveSequence {
		t.Fatal("the sync saved the device's own dns settings again")
	}

	// other settings still apply
	custom := newSavedSettingsTestCustomDnsSettings()
	sync(custom)
	if testingSavedSettingsInstalledMuxSettings(device) == installed ||
		!sameSavedSettingsTestDnsSettings(device.GetDnsResolverSettings(), ownDnsResolverSettings(dnsResolverSettingsAsEdited(custom))) {
		t.Fatal("the sync did not apply dns settings that differ from the device's")
	}
}

// A sync leaves dns settings that are the device's own alone, so a field the
// comparison does not read would leave a real change unapplied. Each field
// changed alone makes the settings differ.
func TestSameDnsResolverSettingsReadsEveryField(t *testing.T) {
	base := func() *DnsResolverSettings {
		return &DnsResolverSettings{
			PresetId:              "gamma-personal",
			PresetPersonalId:      savedSettingsTestPersonalId,
			EnableRemoteDoh:       true,
			EnableLocalDoh:        false,
			EnableRemoteDns:       true,
			EnableLocalDns:        false,
			EnableFallback:        true,
			DnsUpgradeMaskAddress: "192.0.2.53",
			RemoteDohUrlsIpv4:     testing_stringList("https://192.0.2.1/dns-query"),
			RemoteDohUrlsIpv6:     testing_stringList("https://[2001:db8::1]/dns-query"),
			LocalDohUrlsIpv4:      testing_stringList("https://192.0.2.2/dns-query"),
			LocalDohUrlsIpv6:      testing_stringList("https://[2001:db8::2]/dns-query"),
			RemoteDnsIpv4:         testing_stringList("192.0.2.3"),
			RemoteDnsIpv6:         testing_stringList("2001:db8::3"),
			LocalDnsIpv4:          testing_stringList("192.0.2.4"),
			LocalDnsIpv6:          testing_stringList("2001:db8::4"),
		}
	}
	if !sameDnsResolverSettings(base(), base()) {
		t.Fatal("equal settings read as different")
	}
	settingsType := reflect.TypeOf(DnsResolverSettings{})
	for i := 0; i < settingsType.NumField(); i += 1 {
		changed := base()
		field := reflect.ValueOf(changed).Elem().Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(field.String() + "0")
		case reflect.Bool:
			field.SetBool(!field.Bool())
		case reflect.Pointer:
			field.Set(reflect.ValueOf(testing_stringList("192.0.2.99")))
		default:
			t.Fatalf("%s: a field kind this test does not change", settingsType.Field(i).Name)
		}
		if sameDnsResolverSettings(base(), changed) {
			t.Errorf("%s: a change reads as the same settings", settingsType.Field(i).Name)
		}
	}

	// a missing list and an empty one alike
	missing := base()
	missing.LocalDnsIpv6 = nil
	empty := base()
	empty.LocalDnsIpv6 = NewStringList()
	if !sameDnsResolverSettings(missing, empty) {
		t.Fatal("a missing list and an empty one read as different")
	}
	if sameDnsResolverSettings(base(), nil) || !sameDnsResolverSettings(nil, nil) {
		t.Fatal("no settings compare wrong")
	}
}
