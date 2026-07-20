package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type testOfflineProvideListener struct {
	modes   chan ProvideMode
	enabled chan bool
	keys    chan *ProvideSecretKeyList
}

func (self *testOfflineProvideListener) ProvideModeChanged(provideMode ProvideMode) {
	select {
	case self.modes <- provideMode:
	default:
	}
}

func (self *testOfflineProvideListener) ProvideChanged(provideEnabled bool) {
	select {
	case self.enabled <- provideEnabled:
	default:
	}
}

func (self *testOfflineProvideListener) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	select {
	case self.keys <- provideSecretKeyList:
	default:
	}
}

// TestDeviceRemoteOfflineProvideState covers the rpc-disconnected paths: with
// no service, the queued control mode must OWN the derived provide mode and
// enabled state (ProvideMode is a bit set — per-case, never ranges), setters
// must fire the local listeners with the derived values, and loading provide
// secret keys must announce them locally (the ui's has-network-key signal).
// Regression for the iOS symptoms where a Network control mode read as "not
// providing" until the rpc connected.
func TestDeviceRemoteOfflineProvideState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	// no DeviceLocal rpc server exists: the remote stays disconnected and
	// every mutation queues in the local sync state
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		NewId(),
		defaultDeviceRpcSettings(),
		connect.NewId(),
		testing_deviceRpcDialerDefault(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceRemote.Close()

	listener := &testOfflineProvideListener{
		modes:   make(chan ProvideMode, 16),
		enabled: make(chan bool, 16),
		keys:    make(chan *ProvideSecretKeyList, 16),
	}
	modeSub := deviceRemote.AddProvideModeChangeListener(listener)
	defer modeSub.Close()
	enabledSub := deviceRemote.AddProvideChangeListener(listener)
	defer enabledSub.Close()
	keysSub := deviceRemote.AddProvideSecretKeysListener(listener)
	defer keysSub.Close()

	expectMode := func(expected ProvideMode) {
		select {
		case provideMode := <-listener.modes:
			connect.AssertEqual(t, provideMode, expected)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for provide mode %d", expected)
		}
	}
	expectEnabled := func(expected bool) {
		select {
		case provideEnabled := <-listener.enabled:
			connect.AssertEqual(t, provideEnabled, expected)
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for provide enabled %t", expected)
		}
	}

	// a stale raw provide mode queued first (the app's persisted-mode replay
	// at init). The default control mode is manual, so the raw mode is the
	// truth here.
	deviceRemote.SetProvideMode(ProvideModePublic)
	expectMode(ProvideModePublic)
	expectEnabled(true)

	// the control mode OWNS the derived mode from here on — the stale raw
	// Public above must not leak through a Network control mode (the iOS
	// red-light-at-startup bug)
	deviceRemote.SetProvideControlMode(ProvideControlModeNetwork)
	expectMode(ProvideModeNetwork)
	expectEnabled(true)
	connect.AssertEqual(t, deviceRemote.GetProvideMode(), ProvideModeNetwork)
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), true)

	deviceRemote.SetProvideControlMode(ProvideControlModeAlways)
	expectMode(ProvideModePublic)
	expectEnabled(true)

	// auto while idle: network (peer-reachable), provider on
	deviceRemote.SetProvideControlMode(ProvideControlModeAuto)
	expectMode(ProvideModeNetwork)
	expectEnabled(true)

	deviceRemote.SetProvideControlMode(ProvideControlModeNever)
	expectMode(ProvideModeNone)
	expectEnabled(false)

	// loading keys while disconnected announces them locally — nothing else
	// will until the rpc connects
	provideSecretKeys := NewProvideSecretKeyList()
	provideSecretKeys.addAll(&ProvideSecretKey{
		ProvideMode:      ProvideModeNetwork,
		ProvideSecretKey: "test-network-key",
	})
	deviceRemote.LoadProvideSecretKeys(provideSecretKeys)
	select {
	case keys := <-listener.keys:
		connect.AssertEqual(t, keys.Len(), 1)
		connect.AssertEqual(t, keys.Get(0).ProvideMode, ProvideModeNetwork)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the loaded provide secret keys")
	}
}
