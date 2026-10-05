//go:build !linux && !darwin && !sdk_mobile_bind

package sdk

// The non-linux, non-darwin part of messageFragmentPartSizePlatformCopyRulings, empty because no
// copy of the part size is visible to such a build alone today. Its linux twin says why the split
// exists, and the darwin one holds the copy only darwin's flag values make.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{}
