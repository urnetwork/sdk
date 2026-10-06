//go:build darwin && !sdk_mobile_bind

// The part-size gate's ruling that only a darwin build needs.
package sdk

// Darwin's os.O_EXCL collision is ruled by its imported value and exact
// expression in messageFragmentPartSizeRulings. A method-wide ruling here would
// also excuse unrelated fragment-size copies in the census writer.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{}
