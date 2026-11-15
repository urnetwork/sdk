//go:build ios_extension

package sdk

// Whether this build is an Apple network extension's on macOS (deviceForwarderHostClass): the ios_extension leg builds
// both Apple extensions, so the macOS one takes the Apple class's bounds as the iOS one does.
const deviceForwardAppleExtension = true
