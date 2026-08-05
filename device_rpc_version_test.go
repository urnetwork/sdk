package sdk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// testing_waitForSyncError polls DeviceRemote.GetSyncError until the local has
// rejected a sync, or the timeout expires (returning ""). The remote retries
// paced, so the rejection lands on the first attempt after Sync().
func testing_waitForSyncError(t *testing.T, deviceRemote *DeviceRemote, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if syncError := deviceRemote.GetSyncError(); syncError != "" {
			return syncError
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// testing_newRpcDeviceLocal stands up a DeviceLocal with the rpc listener on
// the per-process ephemeral address (see TestMain), plus the matching remote
// settings. Never the fixed production default (127.0.0.1:12025): a hardcoded
// port collides with a concurrent suite run or a locally running app, and the
// symptom is a sync timeout that reads as a delivery bug.
func testing_newRpcDeviceLocal(t *testing.T, ctx context.Context) (*DeviceLocal, *deviceRpcSettings) {
	t.Helper()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, byJwt, "", "", "", NewId(), testDeviceLocalSettingsRpc(), connect.NewId(),
	)
	connect.AssertEqual(t, err, nil)
	t.Cleanup(deviceLocal.Close)

	return deviceLocal, defaultDeviceRpcSettings()
}

// testing_newRpcDeviceRemote builds a DeviceRemote against deviceLocal's
// address, with rpcVersion on the wire and instanceId as the expected pairing.
func testing_newRpcDeviceRemote(
	t *testing.T,
	deviceLocal *DeviceLocal,
	settings *deviceRpcSettings,
	instanceId *Id,
	rpcVersion int,
) *DeviceRemote {
	t.Helper()

	remoteSettings := *settings
	remoteSettings.RpcVersion = rpcVersion

	deviceRemote, err := newDeviceRemoteWithOverrides(
		deviceLocal.networkSpace,
		deviceLocal.byJwt,
		instanceId,
		&remoteSettings,
		deviceLocal.clientId,
		testing_deviceRpcDialer(&remoteSettings),
	)
	connect.AssertEqual(t, err, nil)
	t.Cleanup(deviceRemote.Close)

	return deviceRemote
}

// TestDeviceRpcSyncRejectsVersionMismatch: DeviceRemote and DeviceLocal are
// separately deployed artifacts on the hosted/web path — the browser runs the
// remote out of a cached sdk.wasm while the local runs server side and
// redeploys continuously — and the rpc payload is gob, which fails QUIETLY
// across an incompatible struct change (a renamed field decodes as its zero
// value, so a feature dies with no error anywhere). A remote built against an
// incompatible DeviceRpcVersion must therefore be rejected outright, BEFORE
// any of its cached state is applied, rather than left to half-apply a state
// it misread.
func TestDeviceRpcSyncRejectsVersionMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)

	// the remote is built for the right device instance, but from a build with
	// an incompatible rpc wire version
	deviceRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, deviceLocal.GetInstanceId(), DeviceRpcVersion+1,
	)

	// seed a cached write while offline: a rejected sync must never apply it
	localRouteLocal := deviceLocal.GetRouteLocal()
	deviceRemote.SetRouteLocal(!localRouteLocal)

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(2*time.Second), false)
	connect.AssertEqual(t, deviceRemote.GetRemoteConnected(), false)

	// rejected BEFORE any remote state was applied
	connect.AssertEqual(t, deviceLocal.GetRouteLocal(), localRouteLocal)

	// the rejection is readable by the app, not only written to a log, and
	// names both versions
	syncError := testing_waitForSyncError(t, deviceRemote, 10*time.Second)
	if !strings.HasPrefix(syncError, "device rpc version mismatch:") {
		t.Fatalf("expected a version mismatch sync error, got %q", syncError)
	}
	if !strings.Contains(syncError, "remote is 2") || !strings.Contains(syncError, "local is 1") {
		t.Fatalf("expected the sync error to name both versions, got %q", syncError)
	}
}

// TestDeviceRpcSyncMatchingVersionSyncs is the other half: the production
// pairing (both halves at DeviceRpcVersion) syncs normally and applies the
// remote's state, so the guard cannot silently break the ordinary path.
func TestDeviceRpcSyncMatchingVersionSyncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)

	deviceRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, deviceLocal.GetInstanceId(), DeviceRpcVersion,
	)

	localRouteLocal := deviceLocal.GetRouteLocal()
	deviceRemote.SetRouteLocal(!localRouteLocal)

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(10*time.Second), true)

	// the state crossed and applied, and nothing was reported as rejected
	connect.AssertEqual(t, deviceLocal.GetRouteLocal(), !localRouteLocal)
	connect.AssertEqual(t, deviceRemote.GetSyncError(), "")
}

// TestDeviceRpcSyncVersionZeroBackCompat: a remote that predates the version
// field sends the gob zero value, which must SKIP the check rather than be
// rejected as "version 0 != version 1". Without this, shipping the guard would
// itself break every already-deployed remote — exactly the failure the guard
// exists to prevent. Mirrors the same zero-skips rule on InstanceId.
func TestDeviceRpcSyncVersionZeroBackCompat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)

	// an older remote: no RpcVersion on the wire
	deviceRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, deviceLocal.GetInstanceId(), 0,
	)

	localRouteLocal := deviceLocal.GetRouteLocal()
	deviceRemote.SetRouteLocal(!localRouteLocal)

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(10*time.Second), true)

	connect.AssertEqual(t, deviceLocal.GetRouteLocal(), !localRouteLocal)
	connect.AssertEqual(t, deviceRemote.GetSyncError(), "")
}

// TestDeviceRpcSyncVersionMismatchDistinctFromInstanceMismatch: the two
// pre-state rejections are different faults with different operator responses
// — an instance mismatch means the remote reached the wrong device on a reused
// address (retry/repair), a version mismatch means the two artifacts are from
// incompatible builds (upgrade one). The app must be able to tell them apart
// from the error alone, not by reading logs. Also pins the ORDER: a remote
// wrong on both reports the version, because a misread InstanceId cannot be
// trusted to diagnose anything.
func TestDeviceRpcSyncVersionMismatchDistinctFromInstanceMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)

	// right instance, incompatible version
	versionRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, deviceLocal.GetInstanceId(), DeviceRpcVersion+1,
	)
	versionRemote.Sync()
	connect.AssertEqual(t, versionRemote.waitForSync(2*time.Second), false)
	versionError := testing_waitForSyncError(t, versionRemote, 10*time.Second)

	// right version, wrong instance
	instanceRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, NewId(), DeviceRpcVersion,
	)
	instanceRemote.Sync()
	connect.AssertEqual(t, instanceRemote.waitForSync(2*time.Second), false)
	instanceError := testing_waitForSyncError(t, instanceRemote, 10*time.Second)

	if !strings.HasPrefix(versionError, "device rpc version mismatch:") {
		t.Fatalf("expected a version mismatch, got %q", versionError)
	}
	if !strings.HasPrefix(instanceError, "device instance mismatch:") {
		t.Fatalf("expected an instance mismatch, got %q", instanceError)
	}
	if versionError == instanceError {
		t.Fatalf("the two rejections are indistinguishable: %q", versionError)
	}

	// wrong on both: the version is checked first
	bothRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, NewId(), DeviceRpcVersion+1,
	)
	bothRemote.Sync()
	connect.AssertEqual(t, bothRemote.waitForSync(2*time.Second), false)
	bothError := testing_waitForSyncError(t, bothRemote, 10*time.Second)
	if !strings.HasPrefix(bothError, "device rpc version mismatch:") {
		t.Fatalf("expected the version to be checked before the instance, got %q", bothError)
	}
}
