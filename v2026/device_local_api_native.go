//go:build !sdk_mobile_bind

package sdk

import "context"

// A hosted device's private API dispatches through the embedding platform.
// Implementations authenticate each request and return ordinary API JSON; no
// error falls back to HTTP. The device joins API work before releasing it.
// This Go-only authority is excluded from the mobile binding view.
type LocalDeviceApi interface {
	Get(context.Context, string, string) ([]byte, error)
	Post(context.Context, string, []byte, string) ([]byte, error)
}

// Keep the native named type and settings field identity unchanged.
type localDeviceApi = LocalDeviceApi
