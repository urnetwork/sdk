package sdk

import (
	"context"
	"slices"
	"testing"
	"time"
)

// The kill switch and the split rules an app sets through its DeviceRemote
// survive the device the remote controls (device_remote_saved_settings.go).
//
// The windows service and the linux daemon build a new DeviceLocal for each
// tunnel session and after a restart, and that device restores nothing it was
// told over the rpc. These tests stand in for that device with the remote's
// first sync request, which is what a new device is given, and for the app's
// next launch with a second remote over the same local state. None of them
// needs a device or an rpc: the remotes have nothing listening on their address
// (newTestDeviceRemoteWithNoServiceInSpace), and the reverse rpc callback is
// run in place.

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
