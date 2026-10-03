//go:build !linux

package sdk

// The non-linux half of messageFragmentPartSizePlatformCopyRulings, EMPTY because no copy of the
// part size is visible to a non-linux build alone today. Its linux twin says why the split exists.
var messageFragmentPartSizePlatformCopyRulings = map[string]string{}
