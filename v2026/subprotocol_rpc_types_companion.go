//go:build !sdk_mobile_bind

package sdk

// These aliases preserve the existing Go/JavaScript companion-RPC source
// surface. The wire messages are implementation details and are deliberately
// absent while gobind enumerates the mobile API.
type DeviceSubprotocolRequest = subprotocolRpcRequest
type DeviceSubprotocolResponse = subprotocolRpcResponse
