//go:build !ios_extension

package sdk

// Whether this build is an Apple network extension's on macOS (deviceForwarderHostClass): not on this leg.
const deviceForwardAppleExtension = false
