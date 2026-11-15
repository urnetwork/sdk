//go:build !js

package sdk

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/gossip"
)

// The member role's gossip node in the sdk (EXTENDER.md D1, D5, F2).
//
// The node is a real libp2p host, so the suite keeps it off by default and
// these tests turn it on for their own spaces. Everything they wait on is a
// monitor, not a clock.

// One space under a fresh storage path.
func newTestExtenderSpace(t *testing.T, storagePath string) (*NetworkSpaceManager, *NetworkSpace) {
	t.Helper()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {},
	)
	return networkSpaceManager, networkSpace
}

// The member role runs a node and the feed role does not, and the identity key
// is written once and reused (D5, B1).
func TestExtenderMemberRoleRunsTheNode(t *testing.T) {
	testEnableExtenderNode(t)
	storagePath, err := os.MkdirTemp("", "test_extender_node")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager, networkSpace := newTestExtenderSpace(t, storagePath)
	if networkSpace.extenderNode == nil {
		t.Fatal("the member role did not run a node")
	}
	if role := networkSpace.GetExtenderStatus().Role; role != ExtenderRoleMember {
		t.Fatalf("role = %s, expected member", role)
	}
	keyPath := filepath.Join(
		storagePath,
		"network_spaces",
		"space.example",
		"main",
		".by",
		extenderKeyFileName,
	)
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(keyBytes), "seed_hex") {
		t.Fatalf("the stored identity = %s", keyBytes)
	}
	peerId := networkSpace.extenderNode.node.PeerId().String()
	networkSpaceManager.Close()

	// the same install keeps the same identity, which is what a record for
	// this key will name in phase 6
	restoredManager, restoredSpace := newTestExtenderSpace(t, storagePath)
	if restoredSpace.extenderNode == nil {
		t.Fatal("the restored member role did not run a node")
	}
	restoredKeyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredKeyBytes) != string(keyBytes) {
		t.Fatalf("the identity key was rewritten: %s then %s", keyBytes, restoredKeyBytes)
	}
	if restoredPeerId := restoredSpace.extenderNode.node.PeerId().String(); restoredPeerId != peerId {
		t.Fatalf("peer id = %s, expected the stored identity %s", restoredPeerId, peerId)
	}

	// the feed role runs no node at all
	restoredSpace.SetExtenderGossipMode(ExtenderGossipModeFeed)
	deadline := time.Now().Add(10 * time.Second)
	for restoredSpace.GetExtenderGossipMode() != ExtenderGossipModeFeed {
		if deadline.Before(time.Now()) {
			t.Fatal("the gossip mode was never persisted")
		}
		time.Sleep(time.Millisecond)
	}
	restoredManager.Close()

	feedManager, feedSpace := newTestExtenderSpace(t, storagePath)
	t.Cleanup(feedManager.Close)
	if feedSpace.extenderNode != nil {
		t.Fatal("the feed role ran a node")
	}
	if role := feedSpace.GetExtenderStatus().Role; role != ExtenderRoleFeed {
		t.Fatalf("role = %s, expected feed", role)
	}
}

// The subscribe stream follows the role: the feed role holds it open, the
// member role takes the one-shot sample and hears the rest from the mesh (D5).
func TestExtenderSubscribeFollowsTheRole(t *testing.T) {
	key := NewNetworkSpaceKey("space.example", "main")
	values := NetworkSpaceValues{}
	cases := []struct {
		role   string
		expect bool
	}{
		{role: ExtenderRoleFeed, expect: true},
		{role: ExtenderRoleMember, expect: false},
	}
	for _, c := range cases {
		settings := spaceExtenderNetworkClientSettings(
			key,
			&values,
			c.role,
			"https://api.space.example",
			nil,
		)
		if settings.Subscribe != c.expect {
			t.Errorf("%s: subscribe = %v, expected %v", c.role, settings.Subscribe, c.expect)
		}
		if settings.ExtenderDnsName != "extender.space.example" {
			t.Errorf("%s: extender dns name = %q", c.role, settings.ExtenderDnsName)
		}
	}
}

// The status carries the node's mesh counts, which is what an app renders for
// the mesh (F2).
func TestExtenderStatusCarriesTheNodeCounts(t *testing.T) {
	testEnableExtenderNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	storagePath, err := os.MkdirTemp("", "test_extender_node_status")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	operatorNode, operatorGossipUrl := newTestOperatorNode(t, ctx)
	operatorAddrs, err := gossip.OperatorAddrsFromUrl(operatorGossipUrl, operatorNode.PeerId().String())
	if err != nil {
		t.Fatal(err)
	}

	networkSpaceManager, networkSpace := newTestExtenderSpace(t, storagePath)
	t.Cleanup(networkSpaceManager.Close)
	if networkSpace.extenderNode == nil {
		t.Fatal("the member role did not run a node")
	}

	// hello carries the operator address in production (C7); here it is handed
	// to the node directly, so this test pins the status rather than the hello
	// path, which has its own test below
	networkSpace.extenderNode.node.SetOperatorAddrs(operatorAddrs)

	deadline := time.Now().Add(60 * time.Second)
	for {
		// subscribe immediately before the read, so a change in between is
		// carried by the channel rather than lost
		_, update := networkSpace.extenderNode.node.StatusMonitor().Get()
		status := networkSpace.GetExtenderStatus()
		if status.GossipConnected && 1 <= status.GossipPeerCount {
			break
		}
		select {
		case <-update:
		case <-time.After(time.Until(deadline)):
			t.Fatalf(
				"the node never joined the operator's mesh, status = %+v",
				networkSpace.GetExtenderStatus(),
			)
		}
	}
}

// The operator address the network client learned from hello is what the node
// dials (C7, D3).
func TestExtenderNodeDialsTheOperatorFromHello(t *testing.T) {
	testEnableExtenderNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operatorNode, operatorGossipUrl := newTestOperatorNode(t, ctx)

	directory := connect.NewExtenderDirectoryWithDefaults(ctx)
	t.Cleanup(directory.Close)

	// a network client whose hello carries the operator identity and nothing
	// else, so nothing here resolves or dials outside the test
	networkClientSettings := connect.DefaultExtenderNetworkClientSettings()
	networkClientSettings.ExtenderDnsName = ""
	networkClientSettings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
		return &connect.ExtenderHelloResult{
			GossipPeerId: operatorNode.PeerId().String(),
		}, nil
	}
	networkClient := connect.NewExtenderNetworkClient(ctx, nil, directory, networkClientSettings)
	t.Cleanup(networkClient.Close)

	values := NetworkSpaceValues{GossipUrl: operatorGossipUrl}
	extenderNode := newSpaceExtenderNode(
		ctx,
		NewNetworkSpaceKey("space.example", "main"),
		&values,
		ExtenderRoleMember,
		directory,
		networkClient,
		nil,
		nil,
		nil,
	)
	if extenderNode == nil {
		t.Fatal("the member role did not run a node")
	}
	t.Cleanup(extenderNode.Close)

	deadline := time.Now().Add(60 * time.Second)
	for {
		update := extenderNode.statusUpdate()
		if extenderNode.gossipConnected() {
			break
		}
		select {
		case <-update:
		case <-time.After(time.Until(deadline)):
			t.Fatal("the node never dialed the operator that hello named")
		}
	}
}

// One operator node listening with the websocket transport, which is the shape
// the server runs in phase 5b, and the gossip url a space reaches it at.
func newTestOperatorNode(t *testing.T, ctx context.Context) (*gossip.Node, string) {
	t.Helper()
	operatorAddress := testing_freeHostPort()
	operatorPort, err := strconv.Atoi(operatorAddress[strings.LastIndexByte(operatorAddress, ':')+1:])
	if err != nil {
		t.Fatal(err)
	}
	listenAddrs, err := gossip.WebsocketListenAddrs("127.0.0.1", operatorPort, false)
	if err != nil {
		t.Fatal(err)
	}
	directory := connect.NewExtenderDirectoryWithDefaults(ctx)
	t.Cleanup(directory.Close)

	settings := gossip.DefaultNodeSettings(gossip.NodeRoleMember)
	settings.NetworkHost = "space.example"
	settings.Directory = directory
	settings.ListenAddrs = listenAddrs
	settings.StatusTimeout = 50 * time.Millisecond
	node, err := gossip.NewNode(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)
	return node, "ws://127.0.0.1:" + strconv.Itoa(operatorPort)
}
