//go:build darwin && !sdk_mobile_bind

// The part-size gate's ruling that only a darwin build needs.
package sdk

// The darwin part of messageFragmentPartSizePlatformCopyRulings, for a copy of the part size only a
// darwin build makes: darwin's open(2) flag values differ from linux's, so a flag can land on 2048
// here and nowhere else. Its linux twin says why the split exists.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{
	"memory_owner_census.go DeviceLocal.WriteMemoryOwnerCensus": "os.O_EXCL -- an open(2) flag bit, " +
		"0x800 on darwin (0x80 on linux and windows). The memory owner census creates its file with it. " +
		"A flag, not a byte count of any kind",
}
