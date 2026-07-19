package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// TestDeviceRemoteOfflineToConnectedSync covers the offline -> connected
// handoff: state queued and listeners registered while the rpc is
// DISCONNECTED must sync to the DeviceLocal when the service appears, with
// the control mode OWNING the effective provide mode (regression for the
// sync-order bug where the stale raw provide mode was applied after the
// control mode and silently overrode its mapping on every rpc connect), the
// queued provide secret keys landing on the local, and the offline-registered
// listeners receiving the reverse-push replay and live changes.
func TestDeviceRemoteOfflineToConnectedSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	// the remote starts with NO service listening: everything below queues
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
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
	keysSub := deviceRemote.AddProvideSecretKeysListener(listener)
	defer keysSub.Close()

	peersListener := &testNetworkPeersListener{
		networkPeers: make(chan *NetworkPeers, 16),
	}
	peersSub := deviceRemote.AddNetworkPeersChangeListener(peersListener)
	defer peersSub.Close()

	// the app's init sequence: the persisted raw mode first (stale relative
	// to the control mode), then the control mode, then the persisted keys
	deviceRemote.SetProvideMode(ProvideModeNone)
	deviceRemote.SetProvideControlMode(ProvideControlModeNetwork)
	provideSecretKeys := NewProvideSecretKeyList()
	provideSecretKeys.addAll(&ProvideSecretKey{
		ProvideMode:      ProvideModeNetwork,
		ProvideSecretKey: "test-network-key",
	})
	deviceRemote.LoadProvideSecretKeys(provideSecretKeys)

	// drain the offline events so the assertions below observe only the
	// post-connect state
	drain := func() {
		for {
			select {
			case <-listener.modes:
			case <-listener.enabled:
			case <-listener.keys:
			default:
				return
			}
		}
	}
	drain()

	// the service appears: the remote's reconnect loop syncs the queued state
	settings := testDeviceLocalSettingsRpc()
	settings.DisableLogging = true
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", instanceId, settings, clientId)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	// the control mode owns the effective provide mode on the local: the
	// stale raw None queued before it must not override the Network mapping
	waitFor := func(description string, cond func() bool) {
		endTime := time.Now().Add(30 * time.Second)
		for !cond() {
			if endTime.Before(time.Now()) {
				t.Fatalf("timeout waiting for %s", description)
			}
			select {
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	waitFor("local provide mode network", func() bool {
		return deviceLocal.GetProvideMode() == ProvideModeNetwork
	})
	connect.AssertEqual(t, deviceLocal.GetProvideControlMode(), ProvideControlModeNetwork)
	connect.AssertEqual(t, deviceLocal.GetProvideEnabled(), true)

	// the queued keys landed on the local
	waitFor("local provide secret keys", func() bool {
		keys := deviceLocal.GetProvideSecretKeys()
		if keys == nil {
			return false
		}
		for i := 0; i < keys.Len(); i += 1 {
			if key := keys.Get(i); key != nil && key.ProvideMode == ProvideModeNetwork && key.ProvideSecretKey == "test-network-key" {
				return true
			}
		}
		return false
	})

	// the remote reads live values once connected
	waitFor("remote provide mode network", func() bool {
		return deviceRemote.GetProvideMode() == ProvideModeNetwork
	})
	connect.AssertEqual(t, deviceRemote.GetProvideEnabled(), true)

	// the offline-registered network peers listener is live after the sync:
	// a peer injected at the local pushes through the reverse channel
	peerClientId := connect.NewId()
	waitFor("peer push to the offline-registered listener", func() bool {
		providerClient := deviceLocal.providerClientSnapshot()
		if providerClient == nil {
			return false
		}
		testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
			{
				ClientId:     peerClientId.Bytes(),
				ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network},
				DeviceName:   "peer device",
			},
		})
		select {
		case networkPeers := <-peersListener.networkPeers:
			return networkPeers.Connected.Len() == 1
		case <-time.After(2 * time.Second):
			return false
		}
	})
}
