package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type testNetworkPeersListener struct {
	networkPeers chan *NetworkPeers
}

func (self *testNetworkPeersListener) NetworkPeersChanged(networkPeers *NetworkPeers) {
	select {
	case self.networkPeers <- networkPeers:
	default:
	}
}

// testing_injectNetworkPeersUpdate delivers a peers update to the provider
// client's peer manager, as the platform would over the control channel
func testing_injectNetworkPeersUpdate(providerClient *connect.Client, peers []*protocol.NetworkPeer) {
	updateFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.NetworkPeersUpdate{
		Peers: peers,
	})
	providerClient.PeerManager().Receive(
		connect.SourceId(connect.ControlId),
		[]*protocol.Frame{updateFrame},
		connect.Peer{ProvideMode: protocol.ProvideMode_Network},
	)
}

// TestDeviceLocalNetworkPeersUpdates injects peer updates into the provider
// client's peer manager, as the platform would over the control channel, and
// asserts the device surfaces them through GetNetworkPeers and fires the
// network peers change listener on the epoch.
func TestDeviceLocalNetworkPeersUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.NetworkPeersEpoch = 100 * time.Millisecond

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	connect.AssertEqual(t, err, nil)
	defer device.Close()

	listener := &testNetworkPeersListener{
		networkPeers: make(chan *NetworkPeers, 16),
	}
	sub := device.AddNetworkPeersChangeListener(listener)
	defer sub.Close()

	providerClient := device.providerClientSnapshot()
	connect.AssertNotEqual(t, providerClient, nil)

	// inject a peer update as the platform would over the control channel
	peerClientId := connect.NewId()
	updateFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.NetworkPeersUpdate{
		Peers: []*protocol.NetworkPeer{
			{
				ClientId:     peerClientId.Bytes(),
				ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network, protocol.ProvideMode_Stream},
				Principal:    "svc-a",
				Roles:        []string{"role1", "role2"},
				DeviceName:   "device a",
				DeviceSpec:   "spec a",
			},
		},
	})
	providerClient.PeerManager().Receive(
		connect.SourceId(connect.ControlId),
		[]*protocol.Frame{updateFrame},
		connect.Peer{ProvideMode: protocol.ProvideMode_Network},
	)

	// the listener fires on the epoch
	select {
	case networkPeers := <-listener.networkPeers:
		connect.AssertEqual(t, networkPeers.Connected.Len(), 1)
		connect.AssertEqual(t, networkPeers.DisconnectedCount, 0)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for network peers change")
	}

	networkPeers := device.GetNetworkPeers()
	connect.AssertNotEqual(t, networkPeers, nil)
	connect.AssertEqual(t, networkPeers.Connected.Len(), 1)
	peer := networkPeers.Connected.Get(0)
	connect.AssertEqual(t, peer.ClientId.Cmp(newId(peerClientId)), 0)
	connect.AssertEqual(t, peer.ProvideEnabled, true)
	connect.AssertEqual(t, peer.Principal, "svc-a")
	connect.AssertEqual(t, peer.Roles.Len(), 2)
	connect.AssertEqual(t, peer.DeviceName, "device a")
	connect.AssertEqual(t, peer.DeviceSpec, "spec a")

	// a disconnect marker moves the peer to the disconnected count
	disconnectTime := uint64(time.Now().UnixMilli())
	disconnectFrame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.NetworkPeersUpdate{
		Peers: []*protocol.NetworkPeer{
			{
				ClientId:       peerClientId.Bytes(),
				DisconnectTime: &disconnectTime,
			},
		},
	})
	providerClient.PeerManager().Receive(
		connect.SourceId(connect.ControlId),
		[]*protocol.Frame{disconnectFrame},
		connect.Peer{ProvideMode: protocol.ProvideMode_Network},
	)

	select {
	case networkPeers := <-listener.networkPeers:
		connect.AssertEqual(t, networkPeers.Connected.Len(), 0)
		connect.AssertEqual(t, networkPeers.DisconnectedCount, 1)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for network peers disconnect change")
	}
}

// TestDeviceLocalNetworkPeersEpochCoalesce asserts that a burst of peer
// updates within one epoch emits exactly one network peers change, carrying
// the complete accumulated state.
func TestDeviceLocalNetworkPeersEpochCoalesce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.NetworkPeersEpoch = 500 * time.Millisecond

	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	connect.AssertEqual(t, err, nil)
	defer device.Close()

	listener := &testNetworkPeersListener{
		networkPeers: make(chan *NetworkPeers, 16),
	}
	sub := device.AddNetworkPeersChangeListener(listener)
	defer sub.Close()

	providerClient := device.providerClientSnapshot()
	connect.AssertNotEqual(t, providerClient, nil)

	// a burst of separate updates well within one epoch
	burstCount := 10
	for range burstCount {
		testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
			{
				ClientId:     connect.NewId().Bytes(),
				ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network},
			},
		})
	}

	// exactly one coalesced emit with the complete state. The timeout is
	// generous: under parallel test load the epoch goroutine can be starved
	// well past the epoch, but the coalescing (one emit per epoch) is what is
	// under test, not the latency.
	select {
	case networkPeers := <-listener.networkPeers:
		connect.AssertEqual(t, networkPeers.Connected.Len(), burstCount)
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for network peers change")
	}
	select {
	case <-listener.networkPeers:
		t.Fatal("the burst must coalesce to one emit per epoch")
	case <-time.After(2 * settings.NetworkPeersEpoch):
	}
}

// TestDeviceRemoteNetworkPeers asserts the rpc surface: the remote device
// reads the peers over rpc and its change listener fires through the reverse
// channel when the local peer state changes.
func TestDeviceRemoteNetworkPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	settings.EnableRpc = true
	settings.NetworkPeersEpoch = 100 * time.Millisecond

	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", instanceId, settings, clientId)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

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

	listener := &testNetworkPeersListener{
		networkPeers: make(chan *NetworkPeers, 16),
	}
	sub := deviceRemote.AddNetworkPeersChangeListener(listener)
	defer sub.Close()

	providerClient := deviceLocal.providerClientSnapshot()
	connect.AssertNotEqual(t, providerClient, nil)

	peerClientId := connect.NewId()
	testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
		{
			ClientId:     peerClientId.Bytes(),
			ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network, protocol.ProvideMode_Stream},
			Principal:    "svc-a",
			Roles:        []string{"role1", "role2"},
			DeviceName:   "device a",
		},
	})

	// the remote listener fires through the reverse channel
	select {
	case networkPeers := <-listener.networkPeers:
		connect.AssertEqual(t, networkPeers.Connected.Len(), 1)
		connect.AssertEqual(t, networkPeers.DisconnectedCount, 0)
		peer := networkPeers.Connected.Get(0)
		connect.AssertEqual(t, peer.ClientId.Cmp(newId(peerClientId)), 0)
		connect.AssertEqual(t, peer.ProvideEnabled, true)
		connect.AssertEqual(t, peer.Principal, "svc-a")
		connect.AssertEqual(t, peer.Roles.Len(), 2)
		connect.AssertEqual(t, peer.DeviceName, "device a")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for remote network peers change")
	}

	// the remote reads the peers over rpc
	networkPeers := deviceRemote.GetNetworkPeers()
	connect.AssertNotEqual(t, networkPeers, nil)
	connect.AssertEqual(t, networkPeers.Connected.Len(), 1)
	connect.AssertEqual(t, networkPeers.Connected.Get(0).Principal, "svc-a")

	// a disconnect marker propagates too
	disconnectTime := uint64(time.Now().UnixMilli())
	testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
		{
			ClientId:       peerClientId.Bytes(),
			DisconnectTime: &disconnectTime,
		},
	})

	select {
	case networkPeers := <-listener.networkPeers:
		connect.AssertEqual(t, networkPeers.Connected.Len(), 0)
		connect.AssertEqual(t, networkPeers.DisconnectedCount, 1)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for remote network peers disconnect change")
	}
}
