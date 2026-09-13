//go:build !js

package sdk

import (
	"context"
	"sync"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
)

// extender_node_native.go — the member role's gossip node (EXTENDER.md D1, D5).
//
// A member app runs the mesh itself: it dials the operator and a sample of
// extenders, relays what it hears, and applies every accepted record to the
// space's directory. A feed app runs none of this and keeps its subscribe
// stream instead, which is what the js build always is.
//
// The node is built with the space, before anything is known about the
// operator: the operator's mesh identity arrives on the first hello (C7), so a
// small watch here hands it to the node as soon as the network client has it.
// Until then the node has no operator to dial and peers only with extenders its
// directory already names.
//
// The identity is the space's (B1): the persisted `.extender_key` seed, or
// what an embedder with no local state supplied through the device's key
// material. It is also the provider's extender identity, so a provider that
// activates keeps the mesh peer id its records name.
//
// The provider extender role replaces this node with a listening one in the
// extender role and restores it when the role stops (G2). A libp2p host can
// add a listen address but not drop one, so a changed set of activated
// addresses is a rebuild rather than an addition: otherwise a deactivated
// family would keep advertising an address this host no longer has.

// A process-wide switch for the member role's node, off in the sdk test suite
// exactly as the network client's is: a unit test that builds a production-host
// space must not stand up a libp2p host. The tests that need one turn it on.
var extenderNodeEnabled = true

// spaceExtenderNode owns the node of one network space and the watch that
// gives it the operator address.
type spaceExtenderNode struct {
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}

	node          *gossip.Node
	networkClient *connect.ExtenderNetworkClient
	gossipUrl     string
	log           connect.Logger
	// What the node was built with, retained so a settings change can rebuild
	// it in the role it is in (K6). An extender-role node that lost these
	// would come back as a member and silently stop listening.
	listener    *gossip.InProcessListener
	listenAddrs []ma.Multiaddr
}

// Builds the node of a member-role space, or nil when this space runs none:
// the feed role, a space with no directory, or a space whose extender network
// host is not a real dns name.
func newSpaceExtenderNode(
	ctx context.Context,
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	role string,
	directory *connect.ExtenderDirectory,
	networkClient *connect.ExtenderNetworkClient,
	identityKeySeed func() []byte,
	clientStrategySettings *connect.ClientStrategySettings,
	log connect.Logger,
) *spaceExtenderNode {
	if role != ExtenderRoleMember {
		return nil
	}
	return newSpaceExtenderNodeWithRole(
		ctx,
		key,
		values,
		gossip.NodeRoleMember,
		nil,
		nil,
		directory,
		networkClient,
		identityKeySeed,
		clientStrategySettings,
		log,
	)
}

// Builds the node of one space in one gossip role. The extender role carries
// the extender's in-process listener and the mesh address of every activated
// family, which is what makes it reachable (D2, G2); the member role carries
// neither and is outbound only.
func newSpaceExtenderNodeWithRole(
	ctx context.Context,
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	nodeRole string,
	listener *gossip.InProcessListener,
	listenAddrs []ma.Multiaddr,
	directory *connect.ExtenderDirectory,
	networkClient *connect.ExtenderNetworkClient,
	identityKeySeed func() []byte,
	clientStrategySettings *connect.ClientStrategySettings,
	log connect.Logger,
) *spaceExtenderNode {
	if !extenderNodeEnabled {
		return nil
	}
	if directory == nil || !extenderNetworkClientRuns(key, values) {
		return nil
	}
	// read only here, so a space that runs no node never creates an identity
	// it will not use
	var seed []byte
	if identityKeySeed != nil {
		seed = identityKeySeed()
	}

	settings := gossip.DefaultNodeSettings(nodeRole)
	settings.Log = log
	settings.NetworkHost = extenderNetworkHostName(key, values)
	settings.Directory = directory
	settings.IdentityKeySeed = seed
	settings.ExtenderListener = listener
	settings.ListenAddrs = listenAddrs
	if clientStrategySettings != nil {
		settings.ConnectSettings = &clientStrategySettings.ConnectSettings
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	node, err := gossip.NewNode(cancelCtx, settings)
	if err != nil {
		log.Infof("[extender]gossip node err = %s\n", err)
		cancel()
		return nil
	}
	self := &spaceExtenderNode{
		cancel:        cancel,
		done:          make(chan struct{}),
		node:          node,
		networkClient: networkClient,
		gossipUrl:     GossipUrl(key, values),
		log:           log,
		listener:      listener,
		listenAddrs:   listenAddrs,
	}
	go connect.HandleError(func() {
		defer close(self.done)
		self.watchOperator(cancelCtx)
	}, cancel)
	return self
}

// Hands the node the operator address as soon as hello carries the operator's
// identity, and again whenever it changes (C7, D3). Without a network client
// there is no hello, so there is nothing to watch.
func (self *spaceExtenderNode) watchOperator(ctx context.Context) {
	if self.networkClient == nil {
		<-ctx.Done()
		return
	}
	statusMonitor := self.networkClient.StatusMonitor()
	gossipPeerId := ""
	for {
		status, update := statusMonitor.Get()
		if status.GossipPeerId != gossipPeerId {
			gossipPeerId = status.GossipPeerId
			operatorAddrs, err := gossip.OperatorAddrsFromUrl(self.gossipUrl, gossipPeerId)
			if err != nil {
				self.log.Infof("[extender]operator address err = %s\n", err)
			}
			self.node.SetOperatorAddrs(operatorAddrs)
		}
		select {
		case <-ctx.Done():
			return
		case <-update:
		}
	}
}

// The mesh half of the status (F2). A space with no node reports no mesh.
func (self *spaceExtenderNode) gossipConnected() bool {
	if self == nil {
		return false
	}
	return 0 < self.node.Status().MeshPeerCount
}

func (self *spaceExtenderNode) gossipPeerCount() int {
	if self == nil {
		return 0
	}
	return self.node.Status().MeshPeerCount
}

// True while a peering round has dials in flight and no mesh peer is held yet,
// which is the member role's yellow dot (K4). A space with no node is never
// connecting: it is not trying.
func (self *spaceExtenderNode) gossipConnecting() bool {
	if self == nil {
		return false
	}
	return self.node.Status().Connecting
}

// A channel armed at the instant of the read, so the space's status watch is
// woken when the mesh changes. A space with no node waits on nil, which never
// fires.
func (self *spaceExtenderNode) statusUpdate() chan struct{} {
	if self == nil {
		return nil
	}
	_, update := self.node.StatusMonitor().Get()
	return update
}

// The gossip role of the node this space runs, empty when it runs none.
func (self *spaceExtenderNode) role() string {
	if self == nil {
		return ""
	}
	return self.node.Role()
}

// setExtenderNodeRole replaces this space's node with a listening one in the
// extender role, advertising one mesh address per activated family (G2, D2).
// The listener is stable for the life of the role, so only the node is rebuilt
// when the addresses change. Returns the node, nil when this space runs none:
// the js build, a space with nothing to join, or an app the user put in the
// feed role, whose extender then refuses the gossip service (A8).
func (self *NetworkSpace) setExtenderNodeRole(
	listener *gossip.InProcessListener,
	listenAddrs []ma.Multiaddr,
) *spaceExtenderNode {
	if extenderRole(extenderGossipMode(self.asyncLocalState)) != ExtenderRoleMember {
		self.swapExtenderNode(func() *spaceExtenderNode { return nil })
		return nil
	}
	values := self.valuesCopy()
	return self.swapExtenderNode(func() *spaceExtenderNode {
		return newSpaceExtenderNodeWithRole(
			self.ctx,
			&self.key,
			&values,
			gossip.NodeRoleExtender,
			listener,
			listenAddrs,
			self.extenderDirectory,
			self.getExtenderNetworkClient(),
			self.extenderIdentityKeySeed,
			self.clientStrategySettings,
			self.logger(),
		)
	})
}

// restoreExtenderNodeRole puts the member node back when the extender role
// stops (G2). A space whose app role is feed ends up with no node, which is
// what it had before the role started.
func (self *NetworkSpace) restoreExtenderNodeRole() {
	role := extenderRole(extenderGossipMode(self.asyncLocalState))
	values := self.valuesCopy()
	self.swapExtenderNode(func() *spaceExtenderNode {
		return newSpaceExtenderNode(
			self.ctx,
			&self.key,
			&values,
			role,
			self.extenderDirectory,
			self.getExtenderNetworkClient(),
			self.extenderIdentityKeySeed,
			self.clientStrategySettings,
			self.logger(),
		)
	})
}

// rebuildExtenderNode rebuilds this space's node on the values and the network
// client it now carries (K6). The role is preserved: a node the provider
// extender role installed keeps its in-process listener and its advertised
// addresses, so a settings change never takes an activated extender off the
// mesh. A space that runs no node gets one only if its role calls for one.
func (self *NetworkSpace) rebuildExtenderNode() {
	previous := self.getExtenderNode()
	if previous.role() == gossip.NodeRoleExtender {
		self.setExtenderNodeRole(previous.listener, previous.listenAddrs)
		return
	}
	self.restoreExtenderNodeRole()
}

// rebuildExtenderMemberNode rebuilds a member node on the identity the space
// now carries (B1). An embedder that supplies the extender identity through
// the device's key material arrives after the space is built, and the mesh
// peer id is derived from that identity, so a member node built on a generated
// one is replaced here rather than presenting an identity nothing else uses. A
// node in the extender role is left alone: its identity is the one the
// provider extender role activated with, which is already the space's.
func (self *NetworkSpace) rebuildExtenderMemberNode() {
	if self.getExtenderNode().role() != gossip.NodeRoleMember {
		return
	}
	self.restoreExtenderNodeRole()
}

// Swaps in a node built by `build`, closing the one it replaces first: both
// carry the same identity key, and two hosts on one key would present the same
// peer id to the mesh. A closed space installs nothing.
func (self *NetworkSpace) swapExtenderNode(
	build func() *spaceExtenderNode,
) *spaceExtenderNode {
	previous, closed := func() (*spaceExtenderNode, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.extenderNode
		self.extenderNode = nil
		return previous, self.closed
	}()
	previous.Close()
	if closed {
		self.extenderNodeMonitor.NotifyAll()
		return nil
	}

	node := build()
	installed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.extenderNode = node
		return true
	}()
	if !installed {
		node.Close()
		node = nil
	}
	self.extenderNodeMonitor.NotifyAll()
	return node
}

// Joins the watch and the node.
func (self *spaceExtenderNode) Close() {
	if self == nil {
		return
	}
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
		self.node.Close()
	})
}
