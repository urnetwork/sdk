//go:build !js

package sdk

import (
	"context"
	"sync"

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
// The identity is the persisted `.extender_key` seed (B1). Phase 6 makes the
// same key the provider's extender identity, so a provider that activates keeps
// the mesh peer id its records will name.

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
}

// Builds the node of a member-role space, or nil when this space runs none:
// the feed role, a space with no directory, a space whose host is not a real
// dns name, or a space with no storage to keep an identity in.
func newSpaceExtenderNode(
	ctx context.Context,
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	role string,
	directory *connect.ExtenderDirectory,
	networkClient *connect.ExtenderNetworkClient,
	asyncLocalState *AsyncLocalState,
	clientStrategySettings *connect.ClientStrategySettings,
	log connect.Logger,
) *spaceExtenderNode {
	if !extenderNodeEnabled || role != ExtenderRoleMember {
		return nil
	}
	if directory == nil || !extenderNetworkClientRuns(key, values) {
		return nil
	}
	var identityKeySeed []byte
	if asyncLocalState != nil {
		seed, err := asyncLocalState.GetLocalState().GetOrCreateExtenderKeySeed()
		if err != nil {
			// an install that cannot persist an identity still joins the mesh,
			// with an ephemeral one
			log.Infof("[extender]identity key err = %s\n", err)
		} else {
			identityKeySeed = seed
		}
	}

	settings := gossip.DefaultNodeSettings(gossip.NodeRoleMember)
	settings.Log = log
	settings.NetworkHost = spaceHostName(key, values)
	settings.Directory = directory
	settings.IdentityKeySeed = identityKeySeed
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
