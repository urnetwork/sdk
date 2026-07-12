package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
)

type testing_blockerEnabledChangeListener struct {
	stateLock      sync.Mutex
	count          int
	blockerEnabled bool
}

func (self *testing_blockerEnabledChangeListener) BlockerEnabledChanged(blockerEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.count += 1
	self.blockerEnabled = blockerEnabled
}

func (self *testing_blockerEnabledChangeListener) get() (int, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count, self.blockerEnabled
}

// TestDeviceLocalBlockerEnabled: the device toggle drives the shared blocker
// and emits the change listener; the default is off.
func TestDeviceLocalBlockerEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, _ := testing_newBlockDevice(ctx, t, false)
	defer deviceLocal.Close()

	assert.Equal(t, false, deviceLocal.GetBlockerEnabled())

	listener := &testing_blockerEnabledChangeListener{}
	sub := deviceLocal.AddBlockerEnabledChangeListener(listener)
	defer sub.Close()

	deviceLocal.SetBlockerEnabled(true)
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())
	count, value := listener.get()
	assert.Equal(t, 1, count)
	assert.Equal(t, true, value)

	// setting the same value again does not re-emit
	deviceLocal.SetBlockerEnabled(true)
	count, _ = listener.get()
	assert.Equal(t, 1, count)

	deviceLocal.SetBlockerEnabled(false)
	assert.Equal(t, false, deviceLocal.GetBlockerEnabled())
	count, value = listener.get()
	assert.Equal(t, 2, count)
	assert.Equal(t, false, value)

	// the toggle survives destination changes: the mux and multi client are
	// torn down and rebuilt, and the stable blocker (with its enabled state)
	// is re-wired into the fresh instances
	deviceLocal.SetBlockerEnabled(true)
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			ClientId:        NewId(),
			LocationId:      NewId(),
			LocationGroupId: NewId(),
			BestAvailable:   true,
		},
	}
	deviceLocal.SetDestination(location, NewProviderSpecList())
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())
	deviceLocal.Shuffle()
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())
	deviceLocal.RemoveDestination()
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())
}

// TestDeviceLocalBlockerToggleChurn: rapid toggling (with listeners attached
// and a live destination) leaks no goroutines and no pool buffers.
func TestDeviceLocalBlockerToggleChurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, _ := testing_newBlockDevice(ctx, t, false)
	defer deviceLocal.Close()

	baseStacks := captureGoroutineStacks()
	basePool := poolOutstanding()

	listener := &testing_blockerEnabledChangeListener{}
	sub := deviceLocal.AddBlockerEnabledChangeListener(listener)

	for i := 0; i < 1000; i += 1 {
		deviceLocal.SetBlockerEnabled(i%2 == 0)
	}
	sub.Close()

	count, _ := listener.get()
	assert.Equal(t, 1000, count)

	finalStacks := captureGoroutineStacks()
	reportGoroutineLeaks(t, baseStacks, finalStacks, 2)
	reportPoolLeaks(t, basePool, 0)
}

// TestDeviceRemoteBlockerEnabled: the toggle round-trips over the rpc in both
// directions — remote set → local applies; local set → reverse-notify fires
// the remote listener and updates the remote getter.
func TestDeviceRemoteBlockerEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)

	assert.Equal(t, false, deviceRemote.GetBlockerEnabled())
	assert.Equal(t, false, deviceLocal.GetBlockerEnabled())

	listener := &testing_blockerEnabledChangeListener{}
	sub := deviceRemote.AddBlockerEnabledChangeListener(listener)
	defer sub.Close()

	// remote set applies to the local device
	deviceRemote.SetBlockerEnabled(true)
	assert.Equal(t, true, deviceRemote.GetBlockerEnabled())
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())

	// the local change reverse-notifies the remote listener
	waitFor := func(cond func() bool) bool {
		end := time.Now().Add(5 * time.Second)
		for time.Now().Before(end) {
			if cond() {
				return true
			}
			select {
			case <-time.After(20 * time.Millisecond):
			}
		}
		return cond()
	}
	if !waitFor(func() bool {
		count, value := listener.get()
		return 0 < count && value
	}) {
		t.Fatal("remote listener did not observe the enable")
	}

	// local set flows back to the remote
	deviceLocal.SetBlockerEnabled(false)
	if !waitFor(func() bool {
		_, value := listener.get()
		return !value
	}) {
		t.Fatal("remote listener did not observe the disable")
	}
	assert.Equal(t, false, deviceRemote.GetBlockerEnabled())
}

// TestDeviceRemoteBlockerEnabledOfflineSync: a set on a disconnected remote
// is cached (with a local event) and applied to the device on sync.
func TestDeviceRemoteBlockerEnabledOfflineSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	// the remote comes up first, with no local device to dial: sets are
	// cached offline
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, byJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()

	listener := &testing_blockerEnabledChangeListener{}
	sub := deviceRemote.AddBlockerEnabledChangeListener(listener)
	defer sub.Close()

	assert.Equal(t, false, deviceRemote.GetBlockerEnabled())
	deviceRemote.SetBlockerEnabled(true)
	assert.Equal(t, true, deviceRemote.GetBlockerEnabled())
	count, value := listener.get()
	assert.Equal(t, 1, count)
	assert.Equal(t, true, value)

	// the local device comes up and the remote syncs: the pending write is
	// applied to the device
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, byJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	assert.Equal(t, false, deviceLocal.GetBlockerEnabled())

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(5 * time.Second) {
		t.Fatal("device remote did not sync")
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) && !deviceLocal.GetBlockerEnabled() {
		select {
		case <-time.After(20 * time.Millisecond):
		}
	}
	assert.Equal(t, true, deviceLocal.GetBlockerEnabled())
	assert.Equal(t, true, deviceRemote.GetBlockerEnabled())
}

// TestLocalStateBlockerEnabled: the app-facing persistence pair round-trips
// and defaults to off.
func TestLocalStateBlockerEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	localState := networkSpace.GetAsyncLocalState().GetLocalState()

	assert.Equal(t, false, localState.GetBlockerEnabled())
	assert.Equal(t, nil, localState.SetBlockerEnabled(true))
	assert.Equal(t, true, localState.GetBlockerEnabled())
	assert.Equal(t, nil, localState.SetBlockerEnabled(false))
	assert.Equal(t, false, localState.GetBlockerEnabled())
}
