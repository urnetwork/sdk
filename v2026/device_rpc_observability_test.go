package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

type testingDeviceRpcAttemptLogger struct {
	mu    sync.Mutex
	lines []string
}

func (self *testingDeviceRpcAttemptLogger) append(format string, args ...any) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *testingDeviceRpcAttemptLogger) Info(args ...any) {
	self.append("%s", fmt.Sprint(args...))
}

func (self *testingDeviceRpcAttemptLogger) Infof(format string, args ...any) {
	self.append(format, args...)
}

func (self *testingDeviceRpcAttemptLogger) Warningf(format string, args ...any) {
	self.append(format, args...)
}

func (self *testingDeviceRpcAttemptLogger) Errorf(format string, args ...any) {
	self.append(format, args...)
}

func (self *testingDeviceRpcAttemptLogger) V(int32) connect.Verbose {
	return testingDeviceRpcAttemptVerbose{}
}

type testingDeviceRpcAttemptVerbose struct{}

func (testingDeviceRpcAttemptVerbose) Enabled() bool        { return false }
func (testingDeviceRpcAttemptVerbose) Info(...any)          {}
func (testingDeviceRpcAttemptVerbose) Infof(string, ...any) {}

type testingDeviceRpcTimeoutError struct{}

func (testingDeviceRpcTimeoutError) Error() string   { return "private timeout detail" }
func (testingDeviceRpcTimeoutError) Timeout() bool   { return true }
func (testingDeviceRpcTimeoutError) Temporary() bool { return true }

func TestDeviceRpcAttemptErrorResultIsBounded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "ok", err: nil, want: "ok"},
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "eof", err: io.EOF, want: "eof"},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: "eof"},
		{name: "closed network", err: net.ErrClosed, want: "closed"},
		{name: "closed pipe", err: io.ErrClosedPipe, want: "closed"},
		{name: "rpc error", err: rpc.ServerError("private server response"), want: "rpc-error"},
		{name: "network timeout", err: testingDeviceRpcTimeoutError{}, want: "timeout"},
		{name: "other", err: errors.New("signedProxyId=private endpoint=private"), want: "transport-error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deviceRpcAttemptErrorResult(test.err); got != test.want {
				t.Fatalf("result = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeviceRpcSyncRejectionResultIsBounded(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "device rpc version mismatch: remote is 3, local is 2", want: "version-rejected"},
		{value: "device instance mismatch: remote expects private, local is private", want: "instance-rejected"},
		{value: "private server rejection", want: "rejected"},
	}
	for _, test := range tests {
		if got := deviceRpcSyncRejectionResult(test.value); got != test.want {
			t.Fatalf("result = %q, want %q", got, test.want)
		}
	}
}

func TestDeviceRpcAttemptMarkerOmitsRawError(t *testing.T) {
	const privateDetail = "signedProxyId=private endpoint=wss://private.example"
	logger := &testingDeviceRpcAttemptLogger{}
	logDeviceRpcAttempt(logger, "sync", deviceRpcAttemptErrorResult(errors.New(privateDetail)))

	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.lines) != 1 {
		t.Fatalf("markers = %d, want 1", len(logger.lines))
	}
	if got, want := logger.lines[0], "[drpc-attempt] endpoint=remote stage=sync result=transport-error"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
	if strings.Contains(logger.lines[0], privateDetail) || strings.Contains(logger.lines[0], "private.example") {
		t.Fatalf("marker retained private error detail: %q", logger.lines[0])
	}
}

var _ net.Error = testingDeviceRpcTimeoutError{}
var _ connect.Logger = (*testingDeviceRpcAttemptLogger)(nil)
