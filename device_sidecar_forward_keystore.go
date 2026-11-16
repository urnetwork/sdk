// The VPN component's forwarding key store as apps implement it (DESIGN-DETAILS 4.2.2's key store; §18.6's Setup,
// §5.9's table; owner decision 197; SDK-M8-16): the bound types, on every leg the bindings build, beside the key sets
// kept in it (device_sidecar_forward_keys.go), which a browser never runs (D-BRIDGE-11).

package sdk

// What SidecarForwardKeyStore's PutSecret and DeleteSecret answer: SidecarKeyStore's numbers, one for one (§5.9's
// statuses), so one platform adapter answers both. Zero is Failed.
const (
	SidecarForwardKeyStoreFailed int32 = 0
	SidecarForwardKeyStoreOk     int32 = 1
	SidecarForwardKeyStoreLocked int32 = 4
)

// What a SidecarForwardSecret's Status says: SidecarSecret's numbers, one for one, zero Failed.
const (
	SidecarForwardSecretFailed  int32 = 0
	SidecarForwardSecretPresent int32 = 2
	SidecarForwardSecretAbsent  int32 = 3
	SidecarForwardSecretLocked  int32 = 4
)

// The VPN component's store of its forwarding keys, app-implemented and gobind-safe: each call answers once, with a
// status, and never throws. Called on Go threads, possibly several at once. Its items live in the component's own
// container or keychain, never an app group, and never beside the sidecar role's.
type SidecarForwardKeyStore interface {
	// Keeps the value under name, replacing any earlier one: SidecarForwardKeyStoreOk, Locked or Failed.
	PutSecret(name string, valueBase64 string) int32
	// The value under name, with its status. Never nil.
	GetSecret(name string) *SidecarForwardSecret
	// SidecarForwardKeyStoreOk whether it was deleted or absent, else Locked or Failed.
	DeleteSecret(name string) int32
}

// GetSecret's answer. json in the C ABI.
type SidecarForwardSecret struct {
	// SidecarForwardSecretPresent, Absent, Locked or Failed.
	Status int32 `json:"status"`
	// Standard base64 of the value, set only when Present.
	ValueBase64 string `json:"value_base64,omitempty"`
}

// Gives the device the VPN component's forwarding key store (sidecar §18.6's Setup, DESIGN-DETAILS 4.2.2): with
// ForwardStateDir set, the device keeps its forwarding roles' keys there and runs a forwarding role for each network a
// hand-off names. The store is the component's own, never the sidecar role's SidecarKeyStore. A gomobile host calls
// this on the settings it builds; a host on the c abi, whose settings cross as json, hands the store to
// NewDeviceLocalWithForwardKeyStore instead (QD-844), which sets it as this does.
func (self *DeviceLocalSettings) SetForwardKeyStore(keyStore SidecarForwardKeyStore) {
	self.forwardKeyStore = keyStore
}
