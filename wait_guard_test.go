package sdk

import (
	"testing"
	"time"
)

// How long a test's wait on an event is given before it fails. The wait is on
// the event itself, not on how soon it comes, so a loaded machine that
// delivers it late still passes; the guard only ends a wait that would never
// end, and the caller's failure names what it waited for. That is budget, or
// nine tenths of the time the test binary has left when that is sooner, so the
// failure is reported before the binary's own timeout panics.
func testingWaitGuard(t *testing.T, budget time.Duration) time.Duration {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if left := time.Until(deadline) * 9 / 10; left < budget {
			return left
		}
	}
	return budget
}
