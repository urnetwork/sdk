package sdk

import "crypto/fips140"

// GetFips140Enabled reports whether the Go cryptographic module was put in
// FIPS 140-3 mode at process startup. The iOS network extension rejects that
// configuration because the entropy source backs a 32 MiB scratch buffer with
// physical pages on first use, which is outside the extension memory budget.
func GetFips140Enabled() bool {
	return fips140.Enabled()
}
