//go:build !sdk_mobile_bind

package sdk

import "github.com/urnetwork/sdk/internal/subprotocolrpc"

// These aliases preserve the existing Go/JavaScript companion-RPC source
// surface. The wire messages are implementation details and are deliberately
// absent while gobind enumerates the mobile API.
type DeviceSubprotocolRequest = subprotocolrpc.Request
type DeviceSubprotocolResponse = subprotocolrpc.Response
