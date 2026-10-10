//go:build !js

package sdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/v2026"
)

// Runs the packaged Node WASM against a real native DeviceLocal RPC session,
// whose subprotocol client is wired to a second real connect client in memory.
// No cloud account or external network is involved. Build sdk/js first.
func TestSubprotocolWasmCompanionRoundTrip(t *testing.T) {
	if os.Getenv("UR_SUBPROTOCOL_WASM_TEST") != "1" {
		t.Skip("set UR_SUBPROTOCOL_WASM_TEST=1 after make -C js build_wasm and npm --prefix js run build")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	pair := newSubprotocolTestPair(t)
	defer pair.close()
	space := Testing_NewNetworkSpaceWithUrls(pair.ctx, "http://127.0.0.1:1", "ws://127.0.0.1:1", connect.DefaultConnectSettings())
	defer space.close()
	settings := DefaultDeviceLocalSettings()
	settings.EnableRpc = false
	device, err := newDeviceLocalWithOverrides(space, "", "WASM test", "test", "1", NewId(), settings, pair.a.ClientId())
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	device.attachSubprotocolsToClient(pair.a)
	listener := NewHostedDeviceRpcListener(pair.ctx)
	defer listener.Close()
	device.StartHostedRpc(listener, NewId().String())
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = listener.ServeWs(ws)
	}))
	defer server.Close()
	text, _ := hex.DecodeString("55524d530101000200000000000000016869")
	ack, _ := hex.DecodeString("55524d53010200000000000000000001")
	inboundText := slices.Clone(text)
	inboundText[15] = 2
	inboundAck := slices.Clone(ack)
	inboundAck[15] = 2
	gotAck := make(chan struct{}, 1)
	var once sync.Once
	remove, err := pair.b.AddSubprotocolRawCallback(4096, func(source connect.TransferPath, _ connect.SubprotocolId, data []byte, _ connect.Peer) {
		if slices.Equal(data, text) {
			pair.b.SendSubprotocolBytes(4096, slices.Clone(ack), pair.a.ClientId(), func(error) {})
			pair.b.SendSubprotocolBytes(4096, slices.Clone(inboundText), pair.a.ClientId(), func(error) {})
		} else if slices.Equal(data, inboundAck) {
			once.Do(func() { gotAck <- struct{}{} })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	config := map[string]string{
		"apiUrl": "http://127.0.0.1:1", "platformUrl": "ws://127.0.0.1:1",
		"url": "ws" + server.URL[4:], "peer": pair.b.ClientId().String(),
		"clientId": pair.a.ClientId().String(), "instanceId": device.instanceId.String(),
		"byJwt": testingJwt(map[string]any{"client_id": pair.a.ClientId().String()}),
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, node, "js/test/subprotocol-companion.mjs", file).CombinedOutput()
	if err != nil {
		t.Fatalf("Node WASM companion exchange: %v\n%s", err, output)
	}
	select {
	case <-gotAck:
	default:
		t.Fatalf("native peer did not receive the JS ACK\n%s", output)
	}
	if len(device.subprotocols.enabledIds()) != 0 {
		t.Fatal("Node subscription survived close")
	}
	t.Log(string(output))
}
