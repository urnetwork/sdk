//go:build js

package sdk

import (
	"context"

	"github.com/urnetwork/connect"
)

// extender_node_js.go — the js build has no gossip node (EXTENDER.md D5, H).
//
// go-libp2p is not part of the wasm binary, so this build is always the feed
// role and everything the space asks of a node answers as "no mesh". The type
// exists so the shared space code needs no build tag of its own.

// The js build never runs a node; the switch exists so the shared code reads
// the same on both builds.
var extenderNodeEnabled = false

type spaceExtenderNode struct{}

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
	return nil
}

// The js build runs no node, so there is none to rebuild on a changed
// identity, and none to restart on a changed setting.
func (self *NetworkSpace) rebuildExtenderMemberNode() {
}

func (self *NetworkSpace) rebuildExtenderNode() {
}

func (self *spaceExtenderNode) role() string {
	return ""
}

func (self *spaceExtenderNode) gossipConnected() bool {
	return false
}

func (self *spaceExtenderNode) gossipPeerCount() int {
	return 0
}

func (self *spaceExtenderNode) gossipConnecting() bool {
	return false
}

func (self *spaceExtenderNode) statusUpdate() chan struct{} {
	return nil
}

func (self *spaceExtenderNode) Close() {
}
