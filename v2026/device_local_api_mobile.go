//go:build sdk_mobile_bind

package sdk

import "context"

// Keep local dispatch available to Go without exposing a foreign proxy.
// Gobind accepts named interfaces even with private names, so use an anonymous
// interface alias: the Go-only settings field is then an explicit omission.
type localDeviceApi = interface {
	Get(context.Context, string, string) ([]byte, error)
	Post(context.Context, string, []byte, string) ([]byte, error)
}
