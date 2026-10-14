package main

import (
	"testing"
	"unsafe"
)

// A callback's string result is allocated with urnet_copy_string and freed by
// cAdapterFlowOwnerLookup with urnet_free_string: one allocator, this
// library's, on both ends (windows/urnetwork#17).
func TestCopyStringIsOwnedByTheLibrary(t *testing.T) {
	const value = `C:\Program Files\App\app.exe`
	source := cString(value)
	defer cStringFree(source)

	copied := urnet_copy_string(source)
	if copied == nil {
		t.Fatal("urnet_copy_string returned NULL for a string")
	}
	if unsafe.Pointer(copied) == unsafe.Pointer(source) {
		t.Fatal("urnet_copy_string returned the caller's buffer instead of a library copy")
	}
	if got := goString(copied); got != value {
		t.Fatalf("urnet_copy_string = %q, want %q", got, value)
	}
	urnet_free_string(copied)

	empty := cString("")
	defer cStringFree(empty)
	copiedEmpty := urnet_copy_string(empty)
	if copiedEmpty == nil || goString(copiedEmpty) != "" {
		t.Fatal("urnet_copy_string lost an empty string")
	}
	urnet_free_string(copiedEmpty)

	if urnet_copy_string(nil) != nil {
		t.Fatal("urnet_copy_string(NULL) is not NULL")
	}
}
