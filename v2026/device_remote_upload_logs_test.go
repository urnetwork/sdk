package sdk

// DeviceRemote.UploadLogs's callback. The upload runs in the device's process,
// which answers the request at once (DeviceLocalRpc.UploadLogs), so the remote
// used to drop the callback and its caller never heard how the upload ended.
// The device process now reports the result back over the rpc, and the
// callback gets it once: the server's answer, the error that kept the upload
// from the server, the end of the rpc that carried the request, or, from a
// device process that predates the report, that no result will come.

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// What one callback was given, in the order the callbacks ran.
type testingUploadLogsCallbackRecord struct {
	stateLock sync.Mutex
	results   []*UploadLogsResult
	errs      []error
	// one value per call, so a test can wait for each without a clock
	called chan struct{}
}

func newTestingUploadLogsCallbackRecord() *testingUploadLogsCallbackRecord {
	return &testingUploadLogsCallbackRecord{
		called: make(chan struct{}, 16),
	}
}

func (self *testingUploadLogsCallbackRecord) callback() UploadLogsCallback {
	return connect.NewApiCallback[*UploadLogsResult](func(result *UploadLogsResult, err error) {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.results = append(self.results, result)
			self.errs = append(self.errs, err)
		}()
		self.called <- struct{}{}
	})
}

// Waits for the next call. The bound only turns a callback that never runs
// into a failure instead of a hung test.
func (self *testingUploadLogsCallbackRecord) waitCall(t *testing.T, what string) {
	t.Helper()
	select {
	case <-self.called:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: the callback was never called", what)
	}
}

func (self *testingUploadLogsCallbackRecord) calls() ([]*UploadLogsResult, []error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*UploadLogsResult{}, self.results...), append([]error{}, self.errs...)
}

// The server's answer reaches the remote's callback, whatever it is: accepted,
// refused (the server's rate limit), or a post that failed on the way.
func TestDeviceRemoteUploadLogsCallbackGetsTheServersAnswer(t *testing.T) {
	restoreTestingLogDir(t)
	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	type uploadCase struct {
		name         string
		responseBody string
		postErr      error
		wantRefusal  string
		wantErr      string
	}
	cases := []uploadCase{
		{
			name:         "accepted",
			responseBody: "{}",
		},
		{
			name:         "refused",
			responseBody: `{"error":{"message":"Rate limited. One log upload per network per 5m0s."}}`,
			wantRefusal:  "Rate limited. One log upload per network per 5m0s.",
		},
		{
			name:    "post failed",
			postErr: errors.New("post to log.example failed"),
			wantErr: "post to log.example failed",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// one pair: each upload is answered as its case says
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	var currentCase atomic.Pointer[uploadCase]
	deviceLocal.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		io.Copy(io.Discard, body)
		c := currentCase.Load()
		if c.postErr != nil {
			return nil, c.postErr
		}
		return []byte(c.responseBody), nil
	})

	for _, c := range cases {
		currentCase.Store(&c)
		record := newTestingUploadLogsCallbackRecord()
		if err := deviceRemote.UploadLogs(NewId().String(), record.callback()); err != nil {
			t.Fatalf("%s: UploadLogs = %v", c.name, err)
		}
		record.waitCall(t, c.name)
		results, errs := record.calls()
		switch {
		case c.wantErr != "":
			if errs[0] == nil || !strings.Contains(errs[0].Error(), c.wantErr) || results[0] != nil {
				t.Errorf("%s: the callback got %+v, %v, want the post's error %q", c.name, results[0], errs[0], c.wantErr)
			}
		case c.wantRefusal != "":
			if errs[0] != nil || results[0] == nil || results[0].Error == nil || results[0].Error.Message != c.wantRefusal {
				t.Errorf("%s: the callback got %+v, %v, want the server's refusal %q", c.name, results[0], errs[0], c.wantRefusal)
			}
		default:
			if errs[0] != nil || results[0] == nil || results[0].Error != nil {
				t.Errorf("%s: the callback got %+v, %v, want the server's acceptance", c.name, results[0], errs[0])
			}
		}
	}
}

// An upload that never reaches its post (here its zip cannot be written) is
// reported too, with the device process's error. And a remote with no rpc
// returns the error itself and never calls the callback.
func TestDeviceRemoteUploadLogsCallbackGetsAnUploadThatNeverPosted(t *testing.T) {
	restoreTestingLogDir(t)
	logDir := t.TempDir()
	if err := SetLogDir(logDir); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	var posted atomic.Bool
	deviceLocal.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		posted.Store(true)
		return []byte("{}"), nil
	})
	// the zip is written into the log directory, which is gone
	if err := os.RemoveAll(logDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	record := newTestingUploadLogsCallbackRecord()
	if err := deviceRemote.UploadLogs(NewId().String(), record.callback()); err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	record.waitCall(t, "zip failed")
	results, errs := record.calls()
	if errs[0] == nil || results[0] != nil {
		t.Errorf("the callback got %+v, %v, want the error that kept the upload from its post", results[0], errs[0])
	}
	if posted.Load() {
		t.Error("an upload whose zip failed was posted")
	}

	unconnected := newTestDeviceRemoteWithNoService(t)
	unconnectedRecord := newTestingUploadLogsCallbackRecord()
	if err := unconnected.UploadLogs(NewId().String(), unconnectedRecord.callback()); err == nil {
		t.Fatal("UploadLogs = nil with no rpc service, want an error")
	}
	if results, _ := unconnectedRecord.calls(); len(results) != 0 {
		t.Errorf("the callback of an upload that returned an error was called %d times", len(results))
	}
}

// When the rpc generation that carried the request ends before the report,
// the callback is told so, once: the report cannot come back through a closed
// rpc. The device process still finishes the upload.
func TestDeviceRemoteUploadLogsCallbackEndsWithTheRpc(t *testing.T) {
	restoreTestingLogDir(t)
	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)

	// the upload waits before its zip until the rpc is gone
	enteredChannel := make(chan struct{})
	releaseChannel := make(chan struct{})
	deviceLocal.testingBeforeUploadLogs = func() {
		close(enteredChannel)
		<-releaseChannel
	}
	postedChannel := make(chan struct{}, 1)
	deviceLocal.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		io.Copy(io.Discard, body)
		postedChannel <- struct{}{}
		return []byte("{}"), nil
	})

	record := newTestingUploadLogsCallbackRecord()
	if err := deviceRemote.UploadLogs(NewId().String(), record.callback()); err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	select {
	case <-enteredChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the upload never started")
	}

	service := deviceRemote.getService()
	if service == nil {
		t.Fatal("device remote has no rpc service after sync")
	}
	deviceRemote.closeServiceInstance(service)
	record.waitCall(t, "rpc closed")
	results, errs := record.calls()
	if !errors.Is(errs[0], errUploadLogsRpcClosed) || results[0] != nil {
		t.Errorf("the callback got %+v, %v, want errUploadLogsRpcClosed", results[0], errs[0])
	}

	close(releaseChannel)
	select {
	case <-postedChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the device process did not finish the upload")
	}
	select {
	case <-record.called:
		results, errs := record.calls()
		t.Errorf("the callback was called again, with %+v, %v", results[len(results)-1], errs[len(errs)-1])
	default:
	}
}

// A device process that predates the report answers "can't find method". The
// remote then asks it the old way, which uploads all the same, keeps the rpc,
// and tells the callback that no result will come.
func TestDeviceRemoteUploadLogsCallbackFromAnOlderDeviceProcess(t *testing.T) {
	restoreTestingLogDir(t)
	if err := SetLogDir(t.TempDir()); err != nil {
		t.Fatalf("SetLogDir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deviceLocal, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)
	deviceRemote.testingUploadLogsWithResultMethodName = "DeviceLocalRpc.UploadLogsWithResultNotYetServed"
	postedChannel := make(chan struct{}, 1)
	deviceLocal.GetApi().setHttpPostStreamRaw(func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		io.Copy(io.Discard, body)
		postedChannel <- struct{}{}
		return []byte("{}"), nil
	})

	record := newTestingUploadLogsCallbackRecord()
	if err := deviceRemote.UploadLogs(NewId().String(), record.callback()); err != nil {
		t.Fatalf("UploadLogs = %v", err)
	}
	record.waitCall(t, "older device process")
	results, errs := record.calls()
	if !errors.Is(errs[0], errUploadLogsUnreported) || results[0] != nil {
		t.Errorf("the callback got %+v, %v, want errUploadLogsUnreported", results[0], errs[0])
	}
	select {
	case <-postedChannel:
	case <-time.After(30 * time.Second):
		t.Fatal("the older device process was not asked to upload")
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Error("asking an older device process for the report closed the rpc")
	}
}
