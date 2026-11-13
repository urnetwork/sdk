//go:build !ios_extension

package sdk

// extender_seed_view_controller.go — the seed import of the account screen's
// Extenders section (connect EXTENDER-UPDATE2.md §2.3), beside the K7 share
// import in extender_view_controller.go and separate from it.
//
// The import applies to the device's space: the directory that carries the
// app's own dials, and the seed store, which both processes of an ios install
// share. The packet tunnel extension reads that store at its next pass, so a
// bundle imported in the app reaches the tunnel too.

// ImportSeed imports pasted `ur-seed:1:` text (§2.3).
func (self *ExtenderViewController) ImportSeed(text string) *ExtenderSeedImportResult {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	return networkSpace.ImportExtenderSeed(text)
}

// ImportSeedData imports the contents of a chosen seed file, the text form or
// the raw envelope a mirror serves.
func (self *ExtenderViewController) ImportSeedData(data []byte) *ExtenderSeedImportResult {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	return networkSpace.ImportExtenderSeedData(data)
}

// ImportSeedFile imports the seed file at `path`, in either form.
func (self *ExtenderViewController) ImportSeedFile(path string) *ExtenderSeedImportResult {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	return networkSpace.ImportExtenderSeedFile(path)
}

// The seed state of the device's space, for the section's diagnostics. Never
// nil.
func (self *ExtenderViewController) GetSeedStatus() *ExtenderSeedStatus {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return &ExtenderSeedStatus{}
	}
	return networkSpace.GetExtenderSeedStatus()
}
