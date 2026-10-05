//go:build !linux && !darwin && !sdk_mobile_bind

package sdk

// The non-linux, non-darwin part of messageFragmentPartSizePlatformCopyRulings, empty because no
// copy of the part size is visible to such a build alone today. Its linux twin says why the split
// exists. Imported os.O_EXCL collisions are ruled by their exact expression in
// messageFragmentPartSizeRulings.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{}
