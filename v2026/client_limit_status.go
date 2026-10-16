package sdk

// client_limit_status.go -- the client limit status of the device's platform
// connection, for apps to show while the platform holds the device off.
//
// The platform closes a client over its network's concurrent client limit
// with the client limit close (connect/transport_client_limit.go): a device
// that declared provide intent and did not qualify as a public provider while
// no normal client slot is free, or an ordinary client over the limit. The
// device then holds every platform transport of its client for at least 15
// minutes instead of reconnecting, and reports ClientLimitStatusExceeded with
// the time the hold ends. The status returns to ClientLimitStatusNone when the
// hold ends; a new close sets it again. The platform tells the client nothing
// else about provider qualification.
//
// A device declares provide intent by itself while its provide mode includes
// public (see deviceLocalProvider.setProvideMode); an app has nothing to set.

import (
	"github.com/urnetwork/connect/v2026"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// no client limit condition
	ClientLimitStatusNone = ""
	// the platform closed the device for its network's client limit, and the
	// device holds off reconnecting until RetryTime
	ClientLimitStatusExceeded = "client_limit_exceeded"
)

// ClientLimitStatus is one readout of the device's client limit hold. Plain
// string and int64 fields so gomobile and the cgo/js bindings carry it as is.
type ClientLimitStatus struct {
	// ClientLimitStatusNone or ClientLimitStatusExceeded.
	Status string
	// When the hold ends and the device reconnects, in unix epoch
	// milliseconds. 0 while Status is ClientLimitStatusNone.
	RetryTime int64
}

// ClientLimitStatusChangeListener receives every change of the client limit
// status (see Device.GetClientLimitStatus).
type ClientLimitStatusChangeListener interface {
	ClientLimitStatusChanged(status *ClientLimitStatus)
}

// newClientLimitStatus maps a connect hold readout to the app form.
func newClientLimitStatus(status connect.ClientLimitStatus) *ClientLimitStatus {
	if !status.Exceeded {
		return noneClientLimitStatus()
	}
	return &ClientLimitStatus{
		Status:    ClientLimitStatusExceeded,
		RetryTime: status.RetryTime.UnixMilli(),
	}
}

// noneClientLimitStatus is the readout with no hold, and of a device with no
// platform connection to hold.
func noneClientLimitStatus() *ClientLimitStatus {
	return &ClientLimitStatus{
		Status: ClientLimitStatusNone,
	}
}

func cloneClientLimitStatus(status *ClientLimitStatus) *ClientLimitStatus {
	if status == nil {
		return nil
	}
	copied := *status
	return &copied
}
