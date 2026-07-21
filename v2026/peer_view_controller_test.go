package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

type testPeersListener struct {
	peers chan *NetworkPeerList
}

func (self *testPeersListener) PeersChanged(peers *NetworkPeerList) {
	select {
	case self.peers <- peers:
	default:
	}
}

// TestPeerViewControllerFilter asserts the peer view controller surfaces only
// the connected peers with provide enabled, keeps the filtered set current as
// peers start providing and disconnect, and fans changes out to its listeners.
func TestPeerViewControllerFilter(t *testing.T) {
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

	vc := device.OpenPeerViewController()
	defer device.CloseViewController(vc)

	listener := &testPeersListener{peers: make(chan *NetworkPeerList, 16)}
	sub := vc.AddPeersListener(listener)
	defer sub.Close()
	vc.Start()

	providerClient := device.providerClientSnapshot()
	connect.AssertNotEqual(t, providerClient, nil)

	// waits until the filtered set satisfies the condition, waking on the
	// listener fan-out
	waitForFilteredPeers := func(cond func(peers *NetworkPeerList) bool) {
		endTime := time.Now().Add(5 * time.Second)
		for {
			if cond(vc.GetPeers()) {
				return
			}
			if endTime.Before(time.Now()) {
				t.Fatalf("timeout waiting for filtered peers, have %d", vc.GetPeerCount())
			}
			select {
			case <-listener.peers:
			case <-time.After(100 * time.Millisecond):
			}
		}
	}

	// a providing peer and a connected-but-not-providing peer
	providePeerClientId := connect.NewId()
	idlePeerClientId := connect.NewId()
	testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
		{
			ClientId:     providePeerClientId.Bytes(),
			ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network},
			DeviceName:   "provider peer",
		},
		{
			ClientId:     idlePeerClientId.Bytes(),
			ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Stream},
		},
	})

	// only the provide-enabled peer is surfaced in the connectable list, but
	// the connected count includes BOTH online peers
	waitForFilteredPeers(func(peers *NetworkPeerList) bool { return peers.Len() == 1 })
	peers := vc.GetPeers()
	connect.AssertEqual(t, peers.Len(), 1)
	connect.AssertEqual(t, vc.GetPeerCount(), 1)
	connect.AssertEqual(t, vc.GetConnectedCount(), 2)
	peer := peers.Get(0)
	connect.AssertEqual(t, peer.ClientId.Cmp(newId(providePeerClientId)), 0)
	connect.AssertEqual(t, peer.DeviceName, "provider peer")
	connect.AssertEqual(t, peer.ProvideEnabled, true)

	// the idle peer starting to provide joins the filtered set
	testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
		{
			ClientId:     idlePeerClientId.Bytes(),
			ProvideModes: []protocol.ProvideMode{protocol.ProvideMode_Network, protocol.ProvideMode_Stream},
		},
	})
	waitForFilteredPeers(func(peers *NetworkPeerList) bool { return peers.Len() == 2 })
	connect.AssertEqual(t, vc.GetPeerCount(), 2)
	connect.AssertEqual(t, vc.GetConnectedCount(), 2)

	// a disconnect drops the peer from the filtered set
	disconnectTime := uint64(time.Now().UnixMilli())
	testing_injectNetworkPeersUpdate(providerClient, []*protocol.NetworkPeer{
		{
			ClientId:       providePeerClientId.Bytes(),
			DisconnectTime: &disconnectTime,
		},
	})
	waitForFilteredPeers(func(peers *NetworkPeerList) bool { return peers.Len() == 1 })
	connect.AssertEqual(t, vc.GetPeers().Get(0).ClientId.Cmp(newId(idlePeerClientId)), 0)
	// the disconnected peer leaves the connected count too
	connect.AssertEqual(t, vc.GetConnectedCount(), 1)
}
