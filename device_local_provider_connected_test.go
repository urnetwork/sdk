package sdk

// device_local_provider_connected_test.go -- the changes of GetProviderConnected
// (device_local_provider_connected.go), on fake transport generations.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Drops the fake's route, waking a wait on its connect change.
func (self *fakeMigratablePlatformTransport) disconnect() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if !self.connected {
		return
	}
	self.connected = false
	close(self.notify)
	self.notify = make(chan struct{})
}

// The watch reports the current generation's connect changes, and moves to
// the generation a migration installs: the replaced generation never changes
// again once it is closed, so a watch left on it would miss every later change
// of the provider.
func TestDeviceLocalWatchProviderConnectedFollowsGenerations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth := &connect.ClientAuth{
		ByJwt:      "test",
		InstanceId: connect.NewId(),
		AppVersion: "0.0.0",
	}
	oldTransport := newFakeMigratablePlatformTransport(auth, false)
	replacement := newFakeMigratablePlatformTransport(auth, false)
	provider := &deviceLocalProvider{
		ctx:                       ctx,
		auth:                      auth,
		platformTransport:         oldTransport,
		platformTransportSettings: connect.DefaultPlatformTransportSettings(),
		platformTransportMonitor:  connect.NewMonitor(),
		migrateConnectTimeout:     time.Hour,
		migrateMaxScheduleDelay:   time.Hour,
		newPlatformTransport: func(
			*connect.ClientAuth,
			connect.TransportMode,
			*connect.PlatformTransportSettings,
		) migratablePlatformTransport {
			return replacement
		},
	}
	device := &DeviceLocal{
		ctx:                              ctx,
		provider:                         provider,
		providerConnectedChangeCallbacks: connect.NewCallbackList[func(providerConnected bool)](),
	}
	connectedChanges := make(chan bool, 16)
	sub := device.addProviderConnectedChangeCallback(func(providerConnected bool) {
		connectedChanges <- providerConnected
	})
	defer sub.Close()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		device.watchProviderConnected(provider)
	}()
	defer func() {
		cancel()
		<-watchDone
	}()
	nextChange := func(want bool, message string) {
		t.Helper()
		select {
		case got := <-connectedChanges:
			if got != want {
				t.Fatalf("the watch reported connected %t, want %t", got, want)
			}
		case <-time.After(30 * time.Second):
			t.Fatal(message)
		}
	}

	oldTransport.connect()
	nextChange(true, "the watch missed the generation connecting")
	oldTransport.disconnect()
	nextChange(false, "the watch missed the generation disconnecting")
	oldTransport.connect()
	nextChange(true, "the watch missed the generation connecting again")

	// make before break: the replacement is installed once it connects, and
	// the old generation is closed without another change
	migrated := make(chan struct{})
	go func() {
		defer close(migrated)
		provider.migratePlatformTransport(time.Now())
	}()
	<-replacement.waitStarted
	replacement.connect()
	select {
	case <-migrated:
	case <-time.After(30 * time.Second):
		t.Fatal("the migration never installed the connected replacement")
	}
	provider.stateLock.Lock()
	current := provider.platformTransport
	provider.stateLock.Unlock()
	if current != replacement {
		t.Fatal("the migration did not install the replacement")
	}

	replacement.disconnect()
	nextChange(false, "the watch stayed on the replaced generation")
	replacement.connect()
	nextChange(true, "the watch missed the installed generation connecting")
}
