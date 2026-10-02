package sdk

import (
	"context"
	"errors"
	"net"
	"net/rpc"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
)

// The real net/rpc stream accepts HTTP methods while one status method is
// parked. No production endpoint, device identity, or network credential is used.
type pickerBlockedStatusRpc struct {
	entered    chan struct{}
	release    chan struct{}
	requests   chan *DeviceRemoteHttpRequest
	status     *ContractStatus
	err        error
	statusCall func() (*ContractStatus, error)
}

func (s *pickerBlockedStatusRpc) GetContractStatus(_ RpcNoArg, result **DeviceRemoteContractStatus) error {
	if s.statusCall != nil {
		status, err := s.statusCall()
		*result = &DeviceRemoteContractStatus{ContractStatus: status}
		return err
	}
	close(s.entered)
	<-s.release
	*result = &DeviceRemoteContractStatus{ContractStatus: s.status}
	return s.err
}

func (s *pickerBlockedStatusRpc) HttpGetRaw(request *DeviceRemoteHttpRequest, _ RpcVoid) error {
	s.requests <- request
	return nil
}

func (s *pickerBlockedStatusRpc) HttpPostRaw(request *DeviceRemoteHttpRequest, _ RpcVoid) error {
	s.requests <- request
	return nil
}

func newPickerStatusRpc(t *testing.T) (*DeviceRemote, *pickerBlockedStatusRpc, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	clientConn, serverConn := net.Pipe()
	backend := &pickerBlockedStatusRpc{
		entered: make(chan struct{}), release: make(chan struct{}),
		requests: make(chan *DeviceRemoteHttpRequest, 2), status: &ContractStatus{Premium: true},
	}
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", backend); err != nil {
		t.Fatal(err)
	}
	go server.ServeConn(serverConn)
	service := &rpcClientWithTimeout{ctx: ctx, log: connect.NewNoopLogger(), timeout: time.Minute,
		closeClient: clientConn.Close, client: rpc.NewClient(clientConn)}
	settings := defaultDeviceRpcSettings()
	settings.RequireRemoteApi = true
	remote := &DeviceRemote{ctx: ctx, settings: settings, service: service, remoteConnected: true,
		httpResponseChannels:          map[connect.Id]chan *DeviceRemoteHttpResponse{},
		contractStatusChangeListeners: map[connect.Id]ContractStatusChangeListener{}}
	var once sync.Once
	release := func() { once.Do(func() { close(backend.release) }) }
	t.Cleanup(func() { release(); cancel(); service.Close(); serverConn.Close() })
	return remote, backend, release
}

func requirePickerStatusUnlocked(t *testing.T, remote *DeviceRemote) {
	t.Helper()
	// A mutex waiter is not durably blocked to synctest. Fail before Wait on
	// the regressed implementation instead of hanging the entire test bubble.
	if !remote.stateLock.TryLock() {
		t.Fatal("blocked status RPC holds the shared lock needed by initial GET, city POST, and their responses")
	}
	remote.stateLock.Unlock()
}

func TestDeviceRemotePickerHttpDoesNotWaitForContractStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote, backend, release := newPickerStatusRpc(t)
		statusResult := make(chan *ContractStatus, 1)
		go func() { statusResult <- remote.GetContractStatus() }()
		<-backend.entered
		requirePickerStatusUnlocked(t, remote)
		type result struct {
			method, body string
			err          error
		}
		completed := make(chan result, 2)
		go func() {
			body, err := remote.httpGetRaw(t.Context(), "https://picker.invalid/network/provider-locations", "")
			completed <- result{"GET", string(body), err}
		}()
		go func() {
			body, err := remote.httpPostRaw(t.Context(), "https://picker.invalid/network/find-provider-locations", []byte(`{"query":"Test City"}`), "")
			completed <- result{"POST", string(body), err}
		}()
		synctest.Wait()
		if len(backend.requests) != 2 {
			t.Fatalf("initial GET and city POST parked behind status RPC: reached=%d, want 2", len(backend.requests))
		}
		// Return the two real bridge responses in reverse order. The old status
		// call remains blocked throughout both request and response delivery.
		first, second := <-backend.requests, <-backend.requests
		for _, request := range []*DeviceRemoteHttpRequest{second, first} {
			method := "GET"
			if len(request.RequestBodyBytes) > 0 {
				method = "POST"
			}
			remote.httpResponse(newDeviceRemoteHttpResponseWithLimit(request.RequestId, []byte(method), nil, 1024))
		}
		for range 2 {
			got := <-completed
			if got.err != nil || got.body != got.method {
				t.Fatalf("response pairing: %+v", got)
			}
		}
		select {
		case <-statusResult:
			t.Fatal("status unexpectedly completed")
		default:
		}
		release()
		if got := <-statusResult; got == nil || !got.Premium {
			t.Fatalf("status result=%+v", got)
		}
		remote.stateLock.Lock()
		defer remote.stateLock.Unlock()
		if len(remote.httpResponseChannels) != 0 {
			t.Fatal("HTTP response registration leaked")
		}
	})
}

func TestDeviceRemoteContractStatusKeepsNewerNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote, backend, release := newPickerStatusRpc(t)
		result := make(chan *ContractStatus, 1)
		go func() { result <- remote.GetContractStatus() }()
		<-backend.entered
		requirePickerStatusUnlocked(t, remote)
		notified := make(chan struct{})
		latest := &ContractStatus{NoPermission: true}
		go func() { remote.contractStatusChanged(latest); close(notified) }()
		synctest.Wait()
		select {
		case <-notified:
		default:
			t.Fatal("status notification waited behind status RPC")
		}
		release()
		if got := <-result; got != latest {
			t.Fatalf("old RPC replaced newer notification: %+v", got)
		}
	})
}

func TestDeviceRemoteContractStatusOldGenerationCannotCloseReplacement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "late success", true: "late error"}[fail], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				remote, backend, release := newPickerStatusRpc(t)
				if fail {
					backend.err = errors.New("synthetic old status error")
				}
				result := make(chan *ContractStatus, 1)
				go func() { result <- remote.GetContractStatus() }()
				<-backend.entered
				requirePickerStatusUnlocked(t, remote)
				replaced := make(chan struct{})
				replacement := &rpcClientWithTimeout{}
				latest := &ContractStatus{InsufficientBalance: true}
				go func() {
					remote.stateLock.Lock()
					remote.service = replacement
					remote.lastKnownState.ContractStatus.Set(latest)
					remote.stateLock.Unlock()
					close(replaced)
				}()
				synctest.Wait()
				select {
				case <-replaced:
				default:
					t.Fatal("service replacement waited behind status RPC")
				}
				release()
				if got := <-result; got != latest {
					t.Fatalf("stale generation published status: %+v", got)
				}
				if remote.getService() != replacement || !remote.GetRemoteConnected() {
					t.Fatal("old status error closed replacement")
				}
			})
		})
	}
}

func TestDeviceRemoteContractStatusKeepsCompletedConcurrentRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		remote, backend, release := newPickerStatusRpc(t)
		var calls atomic.Int32
		backend.statusCall = func() (*ContractStatus, error) {
			if calls.Add(1) == 1 {
				close(backend.entered)
				<-backend.release
				return &ContractStatus{Premium: true}, nil
			}
			return &ContractStatus{NoPermission: true}, nil
		}
		first := make(chan *ContractStatus, 1)
		go func() { first <- remote.GetContractStatus() }()
		<-backend.entered
		requirePickerStatusUnlocked(t, remote)
		latest := remote.GetContractStatus()
		if latest == nil || !latest.NoPermission {
			t.Fatalf("concurrent read result=%+v", latest)
		}
		release()
		if got := <-first; got != latest {
			t.Fatalf("late RPC replaced completed concurrent read: %+v", got)
		}
	})
}

func TestDeviceRemoteContractStatusFailureRetainsCache(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "server_error", true: "rpc_timeout"}[timeout], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				remote, backend, release := newPickerStatusRpc(t)
				cached := &ContractStatus{InsufficientBalance: true}
				remote.lastKnownState.ContractStatus.Set(cached)
				if timeout {
					remote.service.timeout = 50 * time.Millisecond
				} else {
					backend.err = errors.New("synthetic current status error")
				}
				result := make(chan *ContractStatus, 1)
				go func() { result <- remote.GetContractStatus() }()
				<-backend.entered
				requirePickerStatusUnlocked(t, remote)
				if !timeout {
					release()
				}
				if got := <-result; got != cached {
					t.Fatalf("status failure discarded cache: %+v", got)
				}
				if remote.getService() != nil || remote.GetRemoteConnected() {
					t.Fatal("failed current service stayed connected")
				}
			})
		})
	}
}

func TestDeviceRemoteContractStatusWithoutNativeServiceUsesCache(t *testing.T) {
	remote := &DeviceRemote{settings: defaultDeviceRpcSettings()}
	if got := remote.GetContractStatus(); got != nil {
		t.Fatalf("empty status=%+v", got)
	}
	cached := &ContractStatus{Premium: true}
	remote.lastKnownState.ContractStatus.Set(cached)
	// Browser services cannot be synchronously called; they retain the same
	// cache-only behavior as an ordinary disconnected native remote.
	remote.browserService = &rpcClientWithTimeout{}
	if got := remote.GetContractStatus(); got != cached {
		t.Fatalf("disconnected/browser status=%+v", got)
	}
}
