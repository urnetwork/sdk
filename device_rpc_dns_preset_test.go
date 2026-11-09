package sdk

// The dns selection, status and blocked count across the device rpc (connect
// ADBLOCK3RD E2, E3, C5, I3): the mirrors carry every field, a refused
// selection is refused by the remote and skipped by a sync without failing
// it, and a remote reads the local device's selection, status and count.

import (
	"context"
	"reflect"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect"
)

// The settings mirror carries every field through the gob wire, the
// selection included.
func TestRpcMirrorDnsResolverSettingsComplete(t *testing.T) {
	seed := 0
	settings := &DnsResolverSettings{}
	fillNonZero(t, reflect.ValueOf(settings), &seed)
	back := gobRoundTrip(t, newDnsResolverSettingsRpc(settings)).toDnsResolverSettings()
	connect.AssertEqual(t, testingDnsResolverSettingsValue(back), testingDnsResolverSettingsValue(settings))
	if back.PresetId == "" || back.PresetPersonalId == "" {
		t.Fatal("the mirror dropped the selection")
	}
}

// The status crosses as it stands.
func TestRpcMirrorDnsResolverStatusComplete(t *testing.T) {
	seed := 0
	status := &DnsResolverStatus{}
	fillNonZero(t, reflect.ValueOf(status), &seed)
	connect.AssertEqual(t, gobRoundTrip(t, status), status)
}

// The device rpc sets and reads the selection, the status and the count; a
// refused selection is answered with the fixed refusal, the rpc keeps serving
// and the previous settings stay; a sync that carries one completes, skips it
// and applies the rest.
func TestDeviceLocalRpcDnsPreset(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testing_loadedBlockDevice(t, fixture.networkSpace)
	_, counter := testingDnsResolverFakeMux(device)
	_, client := testingPreferenceRpc(t, device)

	selected := testingPresetSettings(t, "gamma-personal", "abc123")
	var void RpcVoid
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SetDnsResolverSettings",
		&DeviceRemoteDnsResolverSettings{DnsResolverSettings: newDnsResolverSettingsRpc(selected)}, &void); err != nil {
		t.Fatal(err)
	}
	var current *DeviceRemoteDnsResolverSettings
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetDnsResolverSettings", RpcNoArg(0), &current); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, testingDnsResolverSettingsValue(current.DnsResolverSettings.toDnsResolverSettings()), testingDnsResolverSettingsValue(selected))

	counter.set(17)
	var status *DnsResolverStatus
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetDnsResolverStatus", RpcNoArg(0), &status); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, []any{status.PresetId, status.State, status.BlockedCount}, []any{"gamma-personal", DnsResolverStateUnknown, int64(17)})
	var count int64
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetDnsBlockedCount", RpcNoArg(0), &count); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, count, int64(17))

	refused := &DnsResolverSettings{PresetId: "gamma-personal", PresetPersonalId: "not-an-id"}
	err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SetDnsResolverSettings",
		&DeviceRemoteDnsResolverSettings{DnsResolverSettings: newDnsResolverSettingsRpc(refused)}, &void)
	if err == nil || err.Error() != "invalid dns-resolver-settings" {
		t.Fatalf("the rpc did not report the refusal: %v", err)
	}
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetDnsResolverSettings", RpcNoArg(0), &current); err != nil {
		t.Fatalf("the rpc stopped serving after the refusal: %v", err)
	}
	connect.AssertEqual(t, current.DnsResolverSettings.PresetPersonalId, "abc123")

	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.DnsResolverSettings.Set(newDnsResolverSettingsRpc(refused))
	// applied after the dns settings, so it shows the rest of the sync still runs
	request.State.BlockerEnabled.Set(true)
	syncResponse := &DeviceRemoteSyncResponse{}
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, syncResponse); err != nil {
		t.Fatalf("a sync carrying a refused selection failed: %v", err)
	}
	if syncResponse.Error != "" || !device.GetBlockerEnabled() {
		t.Fatalf("the sync stopped at the refused selection: %q", syncResponse.Error)
	}
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetPersonalId, "abc123")
}

// A selection the device would refuse stays out of the remote's sync queue
// (E2).
func TestDeviceRemoteRefusesInvalidDnsSelection(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	device := &DeviceRemote{settings: defaultDeviceRpcSettings(), log: connect.NewNoopLogger()}
	for _, refused := range []*DnsResolverSettings{
		{PresetId: "no-such-preset"},
		{PresetId: "gamma-personal"},
		{PresetId: "gamma-personal", PresetPersonalId: "abc/12"},
	} {
		device.SetDnsResolverSettings(refused)
		if device.state.DnsResolverSettings.IsSet {
			t.Fatalf("%+v: the remote queued the selection for its next sync", refused)
		}
	}
	// a valid one is queued as the device applies it: edited default lists
	// as custom
	edited := GetDefaultDnsResolverSettings()
	edited.RemoteDohUrlsIpv4 = testing_stringList("https://192.0.2.80/dns-query")
	device.SetDnsResolverSettings(edited)
	if !device.state.DnsResolverSettings.IsSet || device.state.DnsResolverSettings.Value.PresetId != "" {
		t.Fatalf("the remote queued %+v", device.state.DnsResolverSettings.Value)
	}
}

// A remote sets and reads the local device's selection, reads its status and
// count, and hears its status changes; with the device gone it reads the last
// status and no count (C5, I3).
func TestDeviceRemoteDnsPresetOverTheRpc(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	upgradeMux, counter := testingDnsResolverFakeMux(deviceLocal)
	deviceLocal.testingDnsResolverProbe = func(ctx context.Context, upgradeMux *connect.UpgradeMux, name string) []connect.DnsResolverProbeResult {
		return []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"0.0.0.0"})}
	}
	listener := newTestingDnsResolverStatusListener()
	sub := deviceRemote.AddDnsResolverStatusChangeListener(listener)
	defer sub.Close()

	selected := testingPresetSettings(t, "alpha-ads", "")
	deviceRemote.SetDnsResolverSettings(selected)
	connect.AssertEqual(t, deviceLocal.GetDnsResolverSettings().PresetId, "alpha-ads")
	connect.AssertEqual(t, testingDnsResolverSettingsValue(deviceRemote.GetDnsResolverSettings()), testingDnsResolverSettingsValue(selected))
	listener.next(t, "of the selection over the rpc", func(status *DnsResolverStatus) bool { return status.PresetId == "alpha-ads" })

	deviceLocal.dnsResolverHealthChanged(upgradeMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthOk})
	confirmed := listener.next(t, "confirmed over the rpc", func(status *DnsResolverStatus) bool {
		return status.State == DnsResolverStateOk && status.Verification == DnsResolverVerificationConfirmed
	})
	connect.AssertEqual(t, confirmed.PresetId, "alpha-ads")
	counter.set(9)
	connect.AssertEqual(t, deviceRemote.GetDnsBlockedCount(), int64(9))
	remoteStatus := deviceRemote.GetDnsResolverStatus()
	connect.AssertEqual(t, []any{remoteStatus.State, remoteStatus.Verification, remoteStatus.BlockedCount},
		[]any{DnsResolverStateOk, DnsResolverVerificationConfirmed, int64(9)})

	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	connect.AssertEqual(t, deviceRemote.GetDnsBlockedCount(), int64(0))
	cached := deviceRemote.GetDnsResolverStatus()
	connect.AssertEqual(t, []any{cached.State, cached.Verification}, []any{DnsResolverStateOk, DnsResolverVerificationConfirmed})
}

// A remote that never reached a device reads an unknown status and no count.
func TestDeviceRemoteDnsResolverStatusWithoutService(t *testing.T) {
	device := &DeviceRemote{}
	connect.AssertEqual(t, device.GetDnsResolverStatus(), unknownDnsResolverStatus())
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(0))
}
