// Literal arm expectations stay test-only; no production measurement API.
package sdk

// Literal expectations are independent of the production split function.
type privateProviderFixedTotalExpectedRow struct {
	target  ByteCount
	send    ByteCount
	receive ByteCount
}

// Exact values include the original20MiB one-byte residue.
func privateProviderFixedTotalExpected() (string, []privateProviderFixedTotalExpectedRow) {
	return "baseline", []privateProviderFixedTotalExpectedRow{
		{target: 20 * 1024 * 1024, send: 898779, receive: 1198372},
		{target: 32 * 1024 * 1024, send: 1438047, receive: 1917396},
		{target: 64 * 1024 * 1024, send: 2876094, receive: 3834792},
	}
}
