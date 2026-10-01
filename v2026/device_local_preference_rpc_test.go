// Preference RPC tests exercise the real gob server and accepted local store.
// Barriers pause the actual file commit; no app, VPN or external API is used.
package sdk

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Serves production RPC over owned in-memory streams and joins its workers.
func testingPreferenceRpc(t *testing.T, device *DeviceLocal) (*DeviceLocalRpc, *rpc.Client) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	serverReverseConn, clientReverseConn := net.Pipe()
	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	server := newDeviceLocalRpc(context.Background(), serverConn, serverReverseConn, device, settings)
	client := rpc.NewClient(clientConn)
	t.Cleanup(func() {
		_ = client.Close()
		_ = clientReverseConn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.CloseAndWait(ctx); err != nil {
			t.Error("preference RPC server did not join")
		}
	})
	return server, client
}

// The timeout diagnoses a stuck owner; synchronization comes from RPC completion.
func testingPreferenceRpcCall(t *testing.T, client *rpc.Client, method string, request, response any) error {
	t.Helper()
	call := client.Go(method, request, response, make(chan *rpc.Call, 1))
	select {
	case completed := <-call.Done:
		return completed.Error
	case <-time.After(5 * time.Second):
		t.Fatal("preference RPC did not complete")
		return errors.New("unreachable")
	}
}

// Captures an immutable completed operation without adding persistence behavior.
type testingPreferenceRpcSaveListener func(*DeviceLocalSaveResult)

func (self testingPreferenceRpcSaveListener) LocalStateSaved(result *DeviceLocalSaveResult) {
	self(result)
}

// First Sync must not adopt a live destination before its own durable commit,
// even when no extension-side persistence listener has ever been registered.
func TestDeviceLocalPreferenceRpcFirstSyncWaitsForDurableDestination(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	_, client := testingPreferenceRpc(t, device)
	target := testingSpecificPreferenceLocation()
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.Location.Set(newDeviceRemoteConnectLocation(target))
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fixture.localState.testingBeforeLocationCommit = func(name string) error {
		if name != localConnectLocationFileName {
			return errors.New("unexpected preference file")
		}
		close(entered)
		<-release
		return nil
	}
	response := &DeviceRemoteSyncResponse{}
	call := client.Go("DeviceLocalRpc.Sync", request, response, make(chan *rpc.Call, 1))
	select {
	case <-entered:
	case completed := <-call.Done:
		t.Fatalf("first Sync returned before the controlled commit: %v", completed.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("first Sync never reached the actual location commit")
	}
	if device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
		t.Error("first Sync adopted its destination before committing it")
	}
	if _, err := os.Stat(filepath.Join(fixture.localState.localStorageDir, localConnectLocationFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("held first commit unexpectedly replaced the durable location")
	}
	select {
	case <-call.Done:
		t.Fatal("first Sync reported completion while its file commit was held")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case completed := <-call.Done:
		if completed.Error != nil {
			t.Fatal("first Sync did not finish after commit was released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Sync did not finish after its durable commit")
	}
	stored, err := fixture.localState.LoadConnectLocation()
	if err != nil || !connectLocationValuesEqual(stored, target) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) == nil {
		t.Fatal("first Sync did not persist and construct the exact selected destination")
	}
	if !response.State.Location.IsSet || response.State.Location.Value == nil ||
		!connectLocationValuesEqual(response.State.Location.Value.toConnectLocation(), target) {
		t.Fatal("Sync response did not describe the durably adopted location")
	}
}

// A wire-visible error cannot be converted to successful Sync or replace the
// healthy consumer. The original file remains the authoritative restart value.
func TestDeviceLocalPreferenceRpcSyncReturnsSaveFailure(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	original := testingSpecificPreferenceLocation()
	if err := device.SetConnectLocationChecked(original); err != nil {
		t.Fatal(err)
	}
	consumer := testingPreferenceConsumer(device)
	_, client := testingPreferenceRpc(t, device)
	fixture.localState.testingBeforeLocationCommit = func(string) error {
		return errors.New("private storage path and token must not escape")
	}
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.Location.Set(newDeviceRemoteConnectLocation(testingSpecificPreferenceLocation()))
	response := &DeviceRemoteSyncResponse{}
	err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, response)
	var rpcErr rpc.ServerError
	if !errors.As(err, &rpcErr) || err.Error() != "save connect-location" {
		t.Fatal("actual Sync did not return its sanitized persistence failure over RPC")
	}
	stored, err := fixture.localState.LoadConnectLocation()
	if err != nil || !connectLocationValuesEqual(stored, original) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), original) || testingPreferenceConsumer(device) != consumer {
		t.Fatal("failed Sync replaced a durable or live destination")
	}
	result := device.GetLastLocalStateSaveResult()
	if result == nil || result.GetSaved() || !result.GetAutoSaveEnabled() || result.GetError() != "save connect-location" {
		t.Fatal("failed Sync was recorded as durable success")
	}
}

// The direct wire setter must enforce the same storage boundary as first Sync.
func TestDeviceLocalPreferenceRpcSetterReturnsSaveFailure(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	_, client := testingPreferenceRpc(t, device)
	fixture.localState.testingBeforeLocationCommit = func(string) error { return errors.New("private injected write failure") }
	var reply any
	err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SetConnectLocation",
		newDeviceRemoteConnectLocation(testingSpecificPreferenceLocation()), &reply)
	if err == nil || err.Error() != "save connect-location" || strings.Contains(err.Error(), "private") {
		t.Fatal("wire setter discarded or exposed the actual save error")
	}
	if device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
		t.Fatal("failed wire setter constructed a consumer")
	}
}

// Successful Sync publishes after releasing both its service lock and the
// preference locks. TryLock makes the old locked-callback failure deterministic
// without intentionally wedging the suite; the green control then reenters Sync.
func TestDeviceLocalPreferenceRpcSyncSaveCallbackCanReenter(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	server, client := testingPreferenceRpc(t, device)
	var calls atomic.Int64
	var reentered atomic.Bool
	sub := device.AddLocalStateSaveListener(testingPreferenceRpcSaveListener(func(result *DeviceLocalSaveResult) {
		calls.Add(1)
		if result == nil || !result.GetSaved() || result.GetError() != "" {
			t.Error("successful Sync published the wrong operation result")
			return
		}
		for _, stateLock := range []*sync.Mutex{
			&server.stateLock, &device.preferenceMutationLock,
			&fixture.api.authMutationLock, &fixture.localState.authStateLock,
		} {
			if !stateLock.TryLock() {
				t.Error("Sync invoked a save observer while holding an owner/service lock")
				return
			}
			stateLock.Unlock()
		}
		if !device.GetAutoSave() || device.GetLastLocalStateSaveResult() == nil {
			t.Error("save observer could not read its device's persistence mode")
			return
		}
		var response *DeviceRemoteSyncResponse
		err := server.Sync(&DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}, &response)
		if err != nil || response == nil {
			t.Error("save observer could not reenter an empty Sync")
			return
		}
		reentered.Store(true)
	}))
	defer sub.Close()
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.Location.Set(newDeviceRemoteConnectLocation(testingSpecificPreferenceLocation()))
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, &DeviceRemoteSyncResponse{}); err != nil {
		t.Fatal("successful Sync did not complete")
	}
	if calls.Load() != 1 || !reentered.Load() {
		t.Fatal("successful Sync did not complete its reentrant save notification")
	}
}

// An error after one successful preference still publishes both immutable
// outcomes outside Sync's lock; returning early must not strand notifications.
func TestDeviceLocalPreferenceRpcPartialSyncPublishesBothSaveResults(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	server, client := testingPreferenceRpc(t, device)
	fixture.localState.testingBeforeLocationCommit = func(name string) error {
		if name == localDefaultLocationFileName {
			return errors.New("private default write failure")
		}
		return nil
	}
	results := make(chan *DeviceLocalSaveResult, 2)
	sub := device.AddLocalStateSaveListener(testingPreferenceRpcSaveListener(func(result *DeviceLocalSaveResult) {
		if !server.stateLock.TryLock() {
			t.Error("failed Sync invoked its save callback under the service lock")
		} else {
			server.stateLock.Unlock()
		}
		select {
		case results <- result:
		default:
			t.Error("Sync published an extra save result")
		}
	}))
	defer sub.Close()
	target := testingSpecificPreferenceLocation()
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.Location.Set(newDeviceRemoteConnectLocation(target))
	request.State.DefaultLocation.Set(newDeviceRemoteConnectLocation(testingSpecificPreferenceLocation()))
	err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, &DeviceRemoteSyncResponse{})
	if err == nil || err.Error() != "save default-location" {
		t.Fatal("partial Sync hid its failed optional preference mutation")
	}
	for index, preference := range []string{"connect-location", "default-location"} {
		select {
		case result := <-results:
			if result == nil || result.GetPreference() != preference || result.GetSequence() != int64(index+1) ||
				result.GetSaved() != (index == 0) || (result.GetError() == "") != (index == 0) {
				t.Error("partial Sync lost or mutated a completed operation result")
			}
		default:
			t.Error("partial Sync returned without publishing both completed operations")
		}
	}
	stored, err := fixture.localState.LoadConnectLocation()
	if err != nil || !connectLocationValuesEqual(stored, target) ||
		!connectLocationValuesEqual(device.GetConnectLocation(), target) || testingPreferenceConsumer(device) == nil {
		t.Fatal("default write failure undid the already accepted current destination")
	}
	if device.GetDefaultLocation() != nil {
		t.Fatal("failed default mutation was adopted despite its save failure")
	}
}
