//go:build !race

package sdk

// raceEnabled reports whether the test binary was built with -race. See the
// -race build variant in race_on_test.go.
const raceEnabled = false
