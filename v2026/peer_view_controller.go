package sdk

import (
	"context"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

// PeersListener receives the current set of connected network peers that have provide
// enabled, whenever it changes.
type PeersListener interface {
	PeersChanged(peers *NetworkPeerList)
}

// PeerViewController surfaces ONLY the connected network peers that have provide enabled --
// i.e. peers worth connecting to. It exists so every platform shares one definition of that
// filter instead of each app re-filtering device.GetNetworkPeers() itself. It is backed by
// the device's network-peers change stream plus one initial snapshot (no polling); a device
// without a provider has no network peers and the list stays empty.
type PeerViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device

	stateLock sync.Mutex
	peers     *NetworkPeerList

	peersListeners  *connect.CallbackList[PeersListener]
	networkPeersSub Sub
}

func newPeerViewController(ctx context.Context, device Device) *PeerViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &PeerViewController{
		ctx:            cancelCtx,
		cancel:         cancel,
		device:         device,
		peers:          NewNetworkPeerList(),
		peersListeners: connect.NewCallbackList[PeersListener](),
	}
	return vc
}

func (self *PeerViewController) Start() {
	self.networkPeersSub = self.device.AddNetworkPeersChangeListener(&peerViewControllerNetworkPeersListener{self})
	self.update()
}

func (self *PeerViewController) Stop() {}

func (self *PeerViewController) Close() {
	deviceLog(self.device).Info("[pvc]close")

	self.cancel()
	if self.networkPeersSub != nil {
		self.networkPeersSub.Close()
	}
}

// update recomputes the filtered peer set (connected AND provide enabled) and fans it out.
func (self *PeerViewController) update() {
	peers := NewNetworkPeerList()
	if networkPeers := self.device.GetNetworkPeers(); networkPeers != nil && networkPeers.Connected != nil {
		connected := networkPeers.Connected
		for i := 0; i < connected.Len(); i += 1 {
			peer := connected.Get(i)
			// surface only peers that are connected AND providing to the network
			if peer != nil && peer.ProvideEnabled {
				peers.Add(peer)
			}
		}
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.peers = peers
	}()

	self.peersChanged(peers)
}

// GetPeers returns the current connected, provide-enabled peers.
func (self *PeerViewController) GetPeers() *NetworkPeerList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.peers
}

// GetPeerCount is the number of connected, provide-enabled peers (for a drawer count label).
func (self *PeerViewController) GetPeerCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.peers.Len()
}

func (self *PeerViewController) AddPeersListener(listener PeersListener) Sub {
	callbackId := self.peersListeners.Add(listener)
	return newSub(func() {
		self.peersListeners.Remove(callbackId)
	})
}

func (self *PeerViewController) peersChanged(peers *NetworkPeerList) {
	for _, listener := range self.peersListeners.Get() {
		connect.HandleError(func() {
			listener.PeersChanged(peers)
		})
	}
}

// peerViewControllerNetworkPeersListener adapts the device's network-peers change stream.
type peerViewControllerNetworkPeersListener struct {
	vc *PeerViewController
}

func (self *peerViewControllerNetworkPeersListener) NetworkPeersChanged(networkPeers *NetworkPeers) {
	self.vc.update()
}
