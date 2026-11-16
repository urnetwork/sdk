//go:build !ios_extension && !js && !ios && !android

package sdk

// Forwarding's setup for a remote-mode app, through a fake ur daemon (SDK-M8-07, sidecar §18.6's setup row): phones run
// no daemon and their builds link none of its types, so this case runs on desktops alone, beside
// sidecar_forward_setup_test.go's, which every app leg builds.

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A remote-mode app's setup runs through its daemon: the daemon's witness says what is due, bridge.forwarding.setup
// takes the component's public keys and answers the hand-off, which the app relays to its component, confirmed only
// by the person's confirmation.
func TestDesktopPathThroughControl(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	keys := &bridge.ForwardingKeys{KeySetId: connect.NewId().Bytes(), ForwardPublicKey: make([]byte, 32)}
	handOff := &bridge.ForwardingHandOff{KeySetId: keys.GetKeySetId(), ForwardClientJwt: "synthetic-forward-jwt"}
	handOffBytes, _ := proto.Marshal(handOff)
	pinBytes, _ := proto.Marshal(&bridge.AuthPin{Version: 4})
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecar.DaemonMethodForwardingWitness:
			return map[string]any{"pin": pinBytes, "witnessed": true, "setup_due": "new_client_generation"}, nil, true
		case sidecar.DaemonMethodForwardingSetup:
			params := &sidecarControlForwardingSetupParams{}
			if err := json.Unmarshal(request.Params, params); err != nil {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "params"}, true
			}
			sent := &bridge.ForwardingKeys{}
			if err := proto.Unmarshal(params.Keys, sent); err != nil || !proto.Equal(sent, keys) {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "keys"}, true
			}
			return map[string]any{"hand_off": handOffBytes}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, backend, _ := openTestRemoteSidecar(t, socketPath)
	backend.stateLock.Lock()
	networkId := daemon.networkId
	backend.networkId = newId([16]byte(networkId))
	backend.stateLock.Unlock()
	channel := &testingForwardingChannel{keys: keys}
	forwarding := remote.forwardingState()
	forwarding.stateLock.Lock()
	forwarding.device = channel
	forwarding.stateLock.Unlock()
	if err := remote.runForwardingSetup(false); err != nil {
		t.Fatal(err)
	}
	if channel.handOffCount() != 1 || channel.handOffs[0].GetForwardClientJwt() != "synthetic-forward-jwt" {
		t.Fatal("the daemon's hand-off did not reach the component")
	}
	for _, request := range daemon.seen() {
		if request.Method == sidecar.DaemonMethodForwardingSetup && request.UrConfirmed {
			t.Error("a setup the person did not confirm went confirmed")
		}
	}
}
