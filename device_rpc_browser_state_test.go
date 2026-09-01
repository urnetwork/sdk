package sdk

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type testingBrowserStateBlockerListener struct {
	changed chan bool
}

// Extension-backed remotes use BrowserStateOnly so synchronous SDK getters do
// not deadlock the JavaScript event loop. Unlike the legacy browser remote,
// however, every API path must use that same private RPC connection and must
// never fall back to a request from the page. Exercise the Api hooks themselves
// so GET, ordinary POST, and streamed POST are all covered.
func TestBrowserStateOnlyRequiredRemoteApiUsesPrivateRpcService(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var requestsMu sync.Mutex
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, r.Method+":"+r.URL.Path+":"+string(body))
		requestsMu.Unlock()
		_, _ = w.Write([]byte("remote:" + r.URL.Path + ":" + string(body)))
	}))
	defer server.Close()

	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)
	settings.BrowserStateOnly = true
	settings.RequireRemoteApi = true
	deviceRemote := testing_newRpcDeviceRemote(
		t, deviceLocal, settings, deviceLocal.GetInstanceId(), DeviceRpcVersion,
	)
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(10 * time.Second) {
		t.Fatal("extension-style browser remote did not complete its initial sync")
	}

	deviceRemote.stateLock.Lock()
	ordinaryService := deviceRemote.service
	browserService := deviceRemote.browserService
	deviceRemote.stateLock.Unlock()
	if ordinaryService != nil || browserService == nil {
		t.Fatalf("browser service publication is wrong: ordinary=%v browser=%v", ordinaryService, browserService)
	}
	if got := deviceRemote.getHttpService(); got != browserService {
		t.Fatal("API tunnel did not select the private browser RPC service")
	}

	api := deviceRemote.GetApi()
	getBody, err := api.getHttpGetRaw()(ctx, server.URL+"/get", "")
	if err != nil || string(getBody) != "remote:/get:" {
		t.Fatalf("GET over extension RPC body=%q err=%v", getBody, err)
	}
	postBody, err := api.getHttpPostRaw()(ctx, server.URL+"/post", []byte("post-body"), "")
	if err != nil || string(postBody) != "remote:/post:post-body" {
		t.Fatalf("POST over extension RPC body=%q err=%v", postBody, err)
	}
	streamBody, err := api.getHttpPostStreamRaw()(ctx, server.URL+"/stream", strings.NewReader("stream-body"), "")
	if err != nil || string(streamBody) != "remote:/stream:stream-body" {
		t.Fatalf("stream POST over extension RPC body=%q err=%v", streamBody, err)
	}

	for _, want := range []string{"GET:/get:", "POST:/post:post-body", "POST:/stream:stream-body"} {
		requestsMu.Lock()
		snapshot := append([]string(nil), requests...)
		found := false
		for _, got := range snapshot {
			if got == want {
				found = true
				break
			}
		}
		requestsMu.Unlock()
		if !found {
			t.Fatalf("server did not receive %q through RPC; requests=%v", want, snapshot)
		}
	}
}

func (self *testingBrowserStateBlockerListener) BlockerEnabledChanged(enabled bool) {
	self.changed <- enabled
}

type testingBrowserRemoveProviderRpc struct {
	called    chan connect.Id
	release   chan struct{}
	completed chan struct{}
}

func (self *testingBrowserRemoveProviderRpc) RemoveConnectedProvider(clientId connect.Id, _ RpcVoid) error {
	self.called <- clientId
	<-self.release
	close(self.completed)
	return nil
}

// Browser callbacks cannot perform a synchronous round trip over a browser
// websocket: the response itself needs that callback's JavaScript event loop.
// The platform remote therefore serves reads from its sync snapshot and sends
// queued writes through a fresh lifecycle sync.
func TestBrowserStateOnlyRemoteCachesReadsAndResyncsWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deviceLocal, settings := testing_newRpcDeviceLocal(t, ctx)
	settings.BrowserStateOnly = true
	deviceRemote := testing_newRpcDeviceRemote(
		t,
		deviceLocal,
		settings,
		deviceLocal.GetInstanceId(),
		DeviceRpcVersion,
	)

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(10 * time.Second) {
		t.Fatal("browser-state remote did not complete its initial sync")
	}
	deviceRemote.stateLock.Lock()
	publishedService := deviceRemote.service
	remoteConnected := deviceRemote.remoteConnected
	deviceRemote.stateLock.Unlock()
	if publishedService != nil {
		t.Fatal("browser-state remote exposed a synchronous rpc service")
	}
	if !remoteConnected {
		t.Fatal("browser-state remote did not publish connected state")
	}
	if got, want := deviceRemote.GetBlockerEnabled(), deviceLocal.GetBlockerEnabled(); got != want {
		t.Fatalf("cached blocker state = %t, want %t", got, want)
	}

	listener := &testingBrowserStateBlockerListener{changed: make(chan bool, 1)}
	sub := deviceLocal.AddBlockerEnabledChangeListener(listener)
	defer sub.Close()
	want := !deviceLocal.GetBlockerEnabled()
	deviceRemote.SetBlockerEnabled(want)
	deviceRemote.Sync()
	select {
	case got := <-listener.changed:
		if got != want {
			t.Fatalf("resynced blocker state = %t, want %t", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("queued browser-state write did not reach the hosted device")
	}
}

// One-shot actions cannot be represented by the state snapshot. They still
// must use the private browser service, but must return before the rpc reply:
// that reply can only arrive after the JavaScript callback yields its event
// loop. A receiver barrier makes synchronous regression deterministic.
func TestBrowserStateOnlyRemoveConnectedProviderIsAsynchronous(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	receiver := &testingBrowserRemoveProviderRpc{
		called:    make(chan connect.Id, 1),
		release:   make(chan struct{}),
		completed: make(chan struct{}),
	}
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", receiver); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)

	settings := defaultDeviceRpcSettings()
	settings.DisableLogging = true
	settings.BrowserStateOnly = true
	service := &rpcClientWithTimeout{
		ctx:         t.Context(),
		log:         settings.logger(),
		timeout:     time.Second,
		closeClient: clientConn.Close,
		client:      rpc.NewClient(clientConn),
	}
	deviceRemote := &DeviceRemote{
		settings:        settings,
		browserService:  service,
		remoteConnected: true,
	}

	clientId := connect.NewId()
	returned := make(chan struct{})
	go func() {
		deviceRemote.RemoveConnectedProvider(newId(clientId))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("browser action waited for an rpc reply on the caller event loop")
	}
	select {
	case got := <-receiver.called:
		if got != clientId {
			t.Fatalf("removed client id = %s, want %s", got, clientId)
		}
	case <-time.After(time.Second):
		t.Fatal("browser action did not reach the private rpc service")
	}
	close(receiver.release)
	select {
	case <-receiver.completed:
	case <-time.After(time.Second):
		t.Fatal("browser action did not complete after the rpc barrier released")
	}
}
