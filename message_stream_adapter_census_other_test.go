//go:build !windows && !sdk_mobile_bind

package sdk

// THE NON-WINDOWS HALF OF THE PACKAGE-LEVEL VALUE CENSUS, and it is EMPTY on this tree rather than
// absent.
//
// The constraint is the exact complement of its twin's, so exactly one of the two is compiled on
// every platform and the census is never missing a half. What the flock exclusion file and the
// fail-closed one add, on the platforms that build them, is no package-level value at all: they
// declare a type and two functions each. The day one of them declares a value the enumeration
// cannot prove methodless, this gate fails ON THAT PLATFORM asking for an entry here -- which is
// the property these two files exist to have, since the Windows half cannot be compiled to say it.
var streamAdapterPlatformValueCensus = map[string]streamAdapterPackageVar{}

var streamAdapterPlatformNonSentinelRulings = map[string]string{}
