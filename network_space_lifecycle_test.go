// Network-space lifecycle tests pin synchronous ownership release at the SDK
// boundary rather than relying on a canceled-context watcher to win a race.
package sdk

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type networkSpaceBlockingCloseConn struct {
	net.Conn
	closeOnce    sync.Once
	closeEntered chan struct{}
	closeRelease chan struct{}
	closeError   error
}

type networkSpaceCommitCallback func(bool)

func (self networkSpaceCommitCallback) Complete(success bool) {
	self(success)
}

// Holds socket release at an exact barrier so the owner cannot confuse a
// cancellation request with completed strategy cleanup.
func (self *networkSpaceBlockingCloseConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closeEntered)
		<-self.closeRelease
		self.closeError = self.Conn.Close()
	})
	return self.closeError
}

// A network space owns its client strategy. Closing the space must not return
// until the strategy has synchronously released its pooled connection.
func TestNetworkSpaceCloseJoinsClientStrategyRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(closeRelease)
		})
	}

	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &networkSpaceBlockingCloseConn{
				Conn:         connection,
				closeEntered: closeEntered,
				closeRelease: closeRelease,
			}, nil
		},
	}
	networkSpace := NewNetworkSpaceWithUrls(
		context.Background(),
		server.URL,
		"ws://unused.invalid",
		settings,
	)
	t.Cleanup(func() {
		release()
		networkSpace.close()
	})
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		server.URL,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := networkSpace.clientStrategy.HttpParallel(request); err != nil {
		t.Fatal(err)
	}

	closeResult := make(chan struct{})
	go func() {
		networkSpace.close()
		close(closeResult)
	}()
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-closeEntered:
	}
	select {
	case <-closeResult:
		t.Fatal("network space returned before its strategy released the connection")
	default:
	}
	release()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-closeResult:
	}
}

// Replacing a space publishes the new pointer under the manager lock, then
// closes the old transport outside it. A slow socket release must not block
// unrelated readers of the manager state.
func TestNetworkSpaceManagerReplacementDoesNotHoldStateLockDuringClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(closeRelease)
		})
	}

	settings := connect.DefaultClientStrategySettings()
	settings.EnableResilient = false
	settings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &networkSpaceBlockingCloseConn{
				Conn:         connection,
				closeEntered: closeEntered,
				closeRelease: closeRelease,
			}, nil
		},
	}
	previous := NewNetworkSpaceWithUrls(
		context.Background(),
		server.URL,
		"ws://unused.invalid",
		settings,
	)
	key := NetworkSpaceKey{HostName: "replacement.test", EnvName: "test"}
	previous.key = key
	previous.values.ApiUrl = server.URL
	previous.values.PlatformUrl = "ws://unused.invalid"
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := previous.clientStrategy.HttpParallel(request); err != nil {
		t.Fatal(err)
	}

	manager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(func() {
		release()
		manager.Close()
	})
	manager.stateLock.Lock()
	manager.networkSpaces[key] = previous
	manager.stateLock.Unlock()

	replacementResult := make(chan *NetworkSpace, 1)
	go func() {
		replacementResult <- manager.updateNetworkSpace(&key, func(values *NetworkSpaceValues) {
			values.ApiUrl = server.URL
			values.PlatformUrl = "ws://unused.invalid"
		})
	}()
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-closeEntered:
	}

	readResult := make(chan *NetworkSpace, 1)
	go func() {
		readResult <- manager.GetNetworkSpace(&key)
	}()
	select {
	case <-testCtx.Done():
		t.Fatal("manager state read waited for old transport close")
	case replacement := <-readResult:
		if replacement == nil || replacement == previous {
			t.Fatalf("manager published replacement = %p, previous = %p", replacement, previous)
		}
	}

	release()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-replacementResult:
	}
}

// A stale pointer cannot remove a newer generation with the same key. The
// pointer identity is the generation boundary as well as the cleanup owner.
func TestNetworkSpaceManagerStaleRemovePreservesReplacement(t *testing.T) {
	manager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(manager.Close)
	key := NewNetworkSpaceKey("stale-remove.test", "test")
	previous := manager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	replacement := manager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if previous == nil || replacement == nil || previous == replacement {
		t.Fatalf("network-space generations previous=%p replacement=%p", previous, replacement)
	}
	if manager.RemoveNetworkSpace(previous) {
		t.Fatal("stale generation removed its replacement")
	}
	if current := manager.GetNetworkSpace(key); current != replacement {
		t.Fatalf("current network space = %p, want replacement %p", current, replacement)
	}
	if !manager.RemoveNetworkSpace(replacement) {
		t.Fatal("current generation was not removed")
	}
}

// A stale pointer is not a valid active-generation handle merely because its
// key matches a live replacement. Selecting it would publish a closed API and
// strategy to every active-space consumer.
func TestNetworkSpaceManagerStaleActiveSelectionPreservesReplacement(t *testing.T) {
	manager := NewNetworkSpaceManagerNoStorage()
	t.Cleanup(manager.Close)
	key := NewNetworkSpaceKey("stale-active.test", "test")
	previous := manager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	manager.SetActiveNetworkSpace(previous)
	replacement := manager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if previous == nil || replacement == nil || previous == replacement {
		t.Fatalf("network-space generations previous=%p replacement=%p", previous, replacement)
	}
	if active := manager.GetActiveNetworkSpace(); active != replacement {
		t.Fatalf("replacement did not inherit active state: got %p want %p", active, replacement)
	}

	manager.SetActiveNetworkSpace(previous)
	if active := manager.GetActiveNetworkSpace(); active != replacement {
		t.Fatalf("stale generation replaced active space: got %p want %p", active, replacement)
	}
}

// Closing a storage-backed space joins the local-state job already admitted
// before close and rejects a later job with exactly one failed completion.
func TestNetworkSpaceCloseJoinsAsyncLocalStateAndRejectsLateJob(t *testing.T) {
	space := newNetworkSpace(
		context.Background(),
		NetworkSpaceKey{HostName: "local-state.test", EnvName: "test"},
		NetworkSpaceValues{},
		t.TempDir(),
	)
	jobEntered := make(chan struct{})
	jobRelease := make(chan struct{})
	jobCompleted := make(chan bool, 1)
	space.asyncLocalState.serialAsync(func() error {
		close(jobEntered)
		<-jobRelease
		return nil
	}, networkSpaceCommitCallback(func(success bool) {
		jobCompleted <- success
	}))

	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-jobEntered:
	}
	closeResult := make(chan struct{})
	go func() {
		space.close()
		close(closeResult)
	}()
	select {
	case <-closeResult:
		t.Fatal("network space returned before its local-state job completed")
	default:
	}
	close(jobRelease)
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case success := <-jobCompleted:
		if !success {
			t.Fatal("admitted local-state job was rejected")
		}
	}
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-closeResult:
	}

	lateCompleted := make(chan bool, 1)
	lateRan := make(chan struct{}, 1)
	space.asyncLocalState.serialAsync(func() error {
		lateRan <- struct{}{}
		return nil
	}, networkSpaceCommitCallback(func(success bool) {
		lateCompleted <- success
	}))
	if success := <-lateCompleted; success {
		t.Fatal("late local-state job reported success")
	}
	select {
	case <-lateRan:
		t.Fatal("late local-state job ran")
	default:
	}
	space.close()
}

// Closing while an update callback is in flight must reject and synchronously
// close the late generation instead of installing an unowned canceled space.
func TestNetworkSpaceManagerCloseRejectsRacingUpdate(t *testing.T) {
	manager := NewNetworkSpaceManagerNoStorage()
	callbackEntered := make(chan struct{})
	callbackRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(callbackRelease)
		})
	}
	t.Cleanup(func() {
		release()
		manager.Close()
	})

	updateResult := make(chan *NetworkSpace, 1)
	go func() {
		updateResult <- manager.updateNetworkSpace(
			NewNetworkSpaceKey("close-race.test", "test"),
			func(values *NetworkSpaceValues) {
				close(callbackEntered)
				<-callbackRelease
			},
		)
	}()
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()
	select {
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	case <-callbackEntered:
	}
	manager.Close()
	release()
	if result := <-updateResult; result != nil {
		t.Fatalf("closed manager installed late network space %p", result)
	}
	if spaces := manager.GetNetworkSpaces(); spaces.Len() != 0 {
		t.Fatalf("closed manager retained %d network spaces", spaces.Len())
	}
	manager.Close()
}
