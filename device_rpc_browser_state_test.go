package sdk

import (
	"context"
	"net"
	"net/rpc"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type testingBrowserStateBlockerListener struct {
	changed chan bool
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
