package sdk

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/urnetwork/connect"
)

// Created only at the configured raw client-control request boundary, before
// any JSON decoding. Local credential/custody operations cannot create it.
type ClientControlUnavailableError struct{ cause error }

func (self *ClientControlUnavailableError) Error() string {
	return "client control request is unavailable"
}
func (self *ClientControlUnavailableError) Unwrap() error { return self.cause }

func transientClientControlRequestError(err error) bool {
	if err == nil || err == context.Canceled {
		return false
	}
	if err == context.DeadlineExceeded || err == io.EOF || err == io.ErrUnexpectedEOF || err == net.ErrClosed || err == syscall.ECONNRESET || err == syscall.ECONNREFUSED || err == syscall.ECONNABORTED || err == syscall.EPIPE || err == syscall.ETIMEDOUT {
		return true
	}
	switch cause := err.(type) {
	case *ClientControlResponseError, *os.PathError, *os.LinkError:
		return false
	case *connect.HttpStatusError:
		return cause.StatusCode == http.StatusRequestTimeout || cause.StatusCode == http.StatusTooEarly || cause.StatusCode == http.StatusTooManyRequests || 500 <= cause.StatusCode && cause.StatusCode <= 599
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !transientClientControlRequestError(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return transientClientControlRequestError(wrapped.Unwrap())
	}
	if network, ok := err.(net.Error); ok {
		return network.Timeout() || network.Temporary()
	}
	return false
}

// A capability response is complete only when every leaf says the same
// unsupported status. A timeout or hard sibling retains the original tree.
func clientControlOnlyUnsupportedStatus(err error) (int, bool) {
	if status, ok := err.(*connect.HttpStatusError); ok {
		return status.StatusCode, status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusMethodNotAllowed || status.StatusCode == http.StatusNotImplemented
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return 0, false
		}
		expected := 0
		for _, cause := range causes {
			status, valid := clientControlOnlyUnsupportedStatus(cause)
			if !valid || expected != 0 && status != expected {
				return 0, false
			}
			expected = status
		}
		return expected, true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return clientControlOnlyUnsupportedStatus(wrapped.Unwrap())
	}
	return 0, false
}
