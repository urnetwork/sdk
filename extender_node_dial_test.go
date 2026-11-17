//go:build !js

package sdk

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
)

// The node of a space that dials through a proxy or a dialer of its own, a
// miner proxy slot, dials through it too, the operator link included
// (connect#227, gossip NodeSettings.DialContext). Every other space's node
// dials as it always has.

// Root cause: the member node dialed the operator with libp2p's websocket
// transport from the host, so every miner slot's mesh link left from the
// host's own address, outside the proxy the slot was configured with. A space
// with a proxy hands its node the space's own dial.
func TestSpaceExtenderNodeDialsThroughTheSpacesProxy(t *testing.T) {
	key := NetworkSpaceKey{HostName: "custom", EnvName: "custom"}
	values := NetworkSpaceValues{ApiUrl: testUrlSpaceApiUrl}
	nodeSettings := func(clientStrategySettings *connect.ClientStrategySettings) *gossip.NodeSettings {
		return spaceExtenderNodeSettings(&key, &values, gossip.NodeRoleMember, nil, nil, nil, nil, clientStrategySettings, connect.NewNoopLogger())
	}

	proxied := connect.DefaultClientStrategySettings()
	proxied.ProxySettings = &connect.ProxySettings{Network: "tcp", Address: "192.0.2.70:1080"}
	if nodeSettings(proxied).DialContext == nil {
		t.Fatal("the node of a space with a proxy dials from the host")
	}

	// the space's own dial is what the node gets: a dial through it reaches
	// the space's dialer with the name unresolved
	dialed := make(chan string, 1)
	injected := connect.DefaultClientStrategySettings()
	injected.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
		dialed <- address
		return nil, errors.New("synthetic dialer refuses")
	}}
	settings := nodeSettings(injected)
	if settings.DialContext == nil {
		t.Fatal("the node of a space with a dialer of its own dials from the host")
	}
	if _, err := settings.DialContext(context.Background(), "tcp", "gossip.space.example:443"); err == nil {
		t.Fatal("the synthetic dialer was not the one dialed")
	}
	if address := <-dialed; address != "gossip.space.example:443" {
		t.Fatalf("the node's dial reached the space's dialer as %q", address)
	}

	if nodeSettings(connect.DefaultClientStrategySettings()).DialContext != nil {
		t.Fatal("the node of an ordinary space no longer dials as it always has")
	}
	if nodeSettings(nil).DialContext != nil {
		t.Fatal("a node with no strategy settings got a dialer")
	}
}
