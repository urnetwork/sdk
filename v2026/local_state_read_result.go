// Checked optional reads need a nonnil success envelope at native boundaries.
// Swift treats a nullable Objective-C result plus NSError as nil-on-failure;
// an error-free getter preserves successful absence without suppressing errors.
package sdk

// Captures one checked destination read, not a lease over its storage owner.
// The existing Load methods retain their Go and native compatibility contract.
type LocalStateLocationReadResult struct {
	location *ConnectLocation
}

// A nil value means the checked record was absent. Returned values are copies.
func (self *LocalStateLocationReadResult) GetLocation() *ConnectLocation {
	return cloneConnectLocation(self.location)
}

// Carries the checked loader's optional value without changing its error.
func newLocalStateLocationReadResult(location *ConnectLocation, err error) (*LocalStateLocationReadResult, error) {
	if err != nil {
		return nil, err
	}
	return &LocalStateLocationReadResult{location: location}, nil
}

// Successful absence is a nonnil result with no location, not a thrown error.
func (self *LocalState) ReadConnectLocation() (*LocalStateLocationReadResult, error) {
	return newLocalStateLocationReadResult(self.LoadConnectLocation())
}

// Preserves the separately saved default's checked absence/error distinction.
func (self *LocalState) ReadDefaultLocation() (*LocalStateLocationReadResult, error) {
	return newLocalStateLocationReadResult(self.LoadDefaultLocation())
}

// Uses the captured auth pair's existing ownership check on the same read.
func (self *LocalAuthStateSnapshot) ReadConnectLocation() (*LocalStateLocationReadResult, error) {
	return newLocalStateLocationReadResult(self.LoadConnectLocation())
}

// Supersession and storage failure remain errors, never a missing default.
func (self *LocalAuthStateSnapshot) ReadDefaultLocation() (*LocalStateLocationReadResult, error) {
	return newLocalStateLocationReadResult(self.LoadDefaultLocation())
}

// Captures optional identity material without creating or repairing its file.
type LocalStateKeyMaterialReadResult struct {
	keyMaterial *DeviceLocalKeyMaterial
}

// A successful missing or legacy-empty record has no key material.
func (self *LocalStateKeyMaterialReadResult) GetKeyMaterial() *DeviceLocalKeyMaterial {
	if self.keyMaterial == nil {
		return nil
	}
	return NewDeviceLocalKeyMaterial(
		self.keyMaterial.GetClientKeySeed(),
		self.keyMaterial.GetProvideTlsCertificatePem(),
		self.keyMaterial.GetProvideTlsPrivateKeyPem(),
	)
}

// Native callers distinguish an absent identity from every checked failure.
func (self *LocalState) ReadDeviceLocalKeyMaterial() (*LocalStateKeyMaterialReadResult, error) {
	material, err := self.LoadDeviceLocalKeyMaterial()
	if err != nil {
		return nil, err
	}
	return &LocalStateKeyMaterialReadResult{keyMaterial: material}, nil
}
