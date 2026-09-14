//go:build sdk_mobile_bind

package sdk

// The explicit gomobile-only view keeps Device's portable OpenSocket method
// without native signatures that gobind omits from foreign proxy types.
// Concrete Devices still implement their native dial methods unchanged.
type deviceSocketDialer interface{}
