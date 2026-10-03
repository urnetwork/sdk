//go:build windows && !sdk_mobile_bind

package sdk

// THIS PLATFORM'S HALF OF THE PACKAGE-LEVEL VALUE CENSUS.
//
// It exists because a census entry NAMES the value it censuses, and a value declared in a
// build-constrained file can only be named by source carrying a constraint that build satisfies.
// The portable half, streamAdapterPackageValueCensus, covers the production files that carry no
// constraint; this file covers what //go:build windows adds, and its non-windows twin covers what
// the flock and fail-closed exclusion files add on their own platforms.
//
// WHAT THE CONST HALF OF THE ENUMERATION FOUND HERE, and it had been invisible: the two entries
// below are syscall.Errno constants, and syscall.Errno HAS an Error method, so package sdk
// declares two package-level ERROR VALUES on Windows that the sentinel class never saw. They are
// not the store's sentinels -- acquireStreamStoreExclusion compares a CreateFile result against
// them with == and turns a match into ErrStreamStoreLocked, and neither value is ever wrapped,
// passed or returned -- so they are ruled in the second table rather than in the adapter's
// mapping, and TestTheStoreSentinelClassIsTotalOverTheAdaptersMapping MEASURES that excuse rather
// than taking it: it fails the day production hands either of them to a call or returns one.
var streamAdapterPlatformValueCensus = map[string]streamAdapterPackageVar{
	"windowsLockViolation":    streamAdapterPackageConstOf(windowsLockViolation),
	"windowsSharingViolation": streamAdapterPackageConstOf(windowsSharingViolation),
}

var streamAdapterPlatformNonSentinelRulings = map[string]string{
	"windowsLockViolation":    "ERROR_LOCK_VIOLATION, one of the two Windows error numbers that mean \"somebody else holds the guard entry\". It is a syscall.Errno, so it IS an error value, and it is compared with == against a CreateFile result and never put into an error chain: the refusal a caller sees is ErrStreamStoreLocked, which carries its own ruling",
	"windowsSharingViolation": "ERROR_SHARING_VIOLATION, the other. Same seat, same reason: it is read as a code and never raised as an error",
}
