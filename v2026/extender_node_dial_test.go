//go:build !js

package sdk

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/gossip"
)

// Every space's node dials through the space's own connect settings
// (connect#227): the operator link's tcp, tls and websocket upgrade through
// the space's ConnectSettings.DialContext, and the extender carrier through the
// settings as a whole. A miner proxy slot's mesh link therefore leaves through
// its proxy, and an app's under its egress binding (R1) and DialControl.

// Root cause, first for a miner proxy slot and then for every app: the member
// node dialed the operator with libp2p's websocket transport, from the host,
// outside the slot's proxy and outside an app's egress binding and
// DialControl. Every space hands its node its own connect settings and no
// dialer of its own, whatever those settings route through.
func TestSpaceExtenderNodeTakesTheSpacesConnectSettings(t *testing.T) {
	key := NetworkSpaceKey{HostName: "custom", EnvName: "custom"}
	values := NetworkSpaceValues{ApiUrl: testUrlSpaceApiUrl}
	nodeSettings := func(clientStrategySettings *connect.ClientStrategySettings) *gossip.NodeSettings {
		return spaceExtenderNodeSettings(&key, &values, gossip.NodeRoleMember, nil, nil, nil, nil, clientStrategySettings, connect.NewNoopLogger())
	}

	proxied := connect.DefaultClientStrategySettings()
	proxied.ProxySettings = &connect.ProxySettings{Network: "tcp", Address: "192.0.2.70:1080"}
	dialed := make(chan string, 1)
	injected := connect.DefaultClientStrategySettings()
	injected.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
		dialed <- address
		return nil, errors.New("synthetic dialer refuses")
	}}
	ordinary := connect.DefaultClientStrategySettings()
	for _, clientStrategySettings := range []*connect.ClientStrategySettings{proxied, injected, ordinary} {
		settings := nodeSettings(clientStrategySettings)
		if settings.ConnectSettings != &clientStrategySettings.ConnectSettings {
			t.Fatal("a space's node dials through connect settings other than the space's")
		}
		if settings.DialContext != nil {
			t.Fatal("a space's node was given a dialer of its own beside the space's settings")
		}
	}

	// the node's route is the space's dial: a dial through it reaches the
	// space's dialer with the name unresolved
	if _, err := nodeSettings(injected).ConnectSettings.DialContext(context.Background(), "tcp", "gossip.space.example:443"); err == nil {
		t.Fatal("the synthetic dialer was not the one dialed")
	}
	if address := <-dialed; address != "gossip.space.example:443" {
		t.Fatalf("the node's dial reached the space's dialer as %q", address)
	}

	if settings := nodeSettings(nil); settings.ConnectSettings != nil || settings.DialContext != nil {
		t.Fatal("a node with no strategy settings got a dial of its own")
	}
}

// An app space's node, with no proxy, dials its operator link under the space's
// DialControl, which is where the egress binding (R1) runs too. Before, the
// link's socket was libp2p's own net.DialTCP, which no DialControl saw.
func TestSpaceExtenderNodeDialsTheOperatorUnderTheSpacesDialControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operator, gossipUrl := newTestOperatorNode(t, ctx)
	operatorAddrs, err := gossip.OperatorAddrsFromUrl(gossipUrl, operator.PeerId().String())
	if err != nil {
		t.Fatal(err)
	}
	operatorAddress := strings.TrimPrefix(gossipUrl, "ws://")

	controlledLock := sync.Mutex{}
	controlledAddresses := []string{}
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.DialControl = func(network string, address string, c syscall.RawConn) error {
		controlledLock.Lock()
		defer controlledLock.Unlock()
		controlledAddresses = append(controlledAddresses, address)
		return nil
	}
	directory := connect.NewExtenderDirectoryWithDefaults(ctx)
	t.Cleanup(directory.Close)
	key := NetworkSpaceKey{HostName: "custom", EnvName: "custom"}
	values := NetworkSpaceValues{ApiUrl: testUrlSpaceApiUrl}
	settings := spaceExtenderNodeSettings(&key, &values, gossip.NodeRoleMember, nil, nil, directory, nil, clientStrategySettings, connect.NewNoopLogger())
	settings.OperatorAddrs = operatorAddrs
	settings.StatusTimeout = 50 * time.Millisecond
	node, err := gossip.NewNode(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)

	deadline := time.After(60 * time.Second)
	for {
		status, update := node.StatusMonitor().Get()
		if status.OperatorConnected {
			break
		}
		select {
		case <-update:
		case <-deadline:
			t.Fatalf("the app space's node never reached its operator, status = %+v", node.Status())
		}
	}
	controlledLock.Lock()
	defer controlledLock.Unlock()
	if !slices.Contains(controlledAddresses, operatorAddress) {
		t.Fatalf("the operator link's socket was not the space's dialer's: its DialControl saw %v", controlledAddresses)
	}
}
