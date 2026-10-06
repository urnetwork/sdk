// The fail-closed half. Its constraint is the exact complement of the windows file's and the
// flock file's, derived from the PRIMITIVE rather than from a build term that is an instance of
// one -- so the set below is what it is because neither syscall.CreateFile nor syscall.Flock is
// declared there.
//
// ITS CONSTITUENCY, named rather than left to be discovered: solaris, aix, js/wasm, wasip1/wasm
// and plan9. js/wasm is a target this MODULE already builds for, and the priced consequence is
// the same one sdk/message_stream_exclusion_other.go prices for the stream store: the durable
// state store refuses to open on that artifact, so a wasm build has no restart story until
// somebody supplies an exclusion for it. That is S2-20's constituency, not a new question.
//
// A PLATFORM THIS STORE CANNOT MAKE SAFE IS A PLATFORM IT REFUSES TO OPEN ON. A build tag that
// quietly compiled to a no-op returning a nil closer and a nil error would be the single-writer
// property deleted by a build constraint: every gate for it would still be green on the platforms
// that have the primitive, and the platform without one would ship two devices overwriting one
// device's MLS state with nothing to say so.

//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package urmessage

import (
	"fmt"
	"io"
	"runtime"
)

func acquireStateStoreExclusion(dir string) (io.Closer, error) {
	return nil, fmt.Errorf(
		"%w: %s has neither an exclusive CreateFile nor syscall.Flock, so this build cannot hold a single-writer exclusion on %s and will not open a state store there; two stores over one directory are two devices writing one device's mls state (S2-20)",
		ErrStateStoreLocked, runtime.GOOS, dir)
}

// Unreachable: the store cannot be opened on this platform, so nothing ever writes into a
// directory to flush. It is declared because the build needs it and it refuses rather than
// returning nil, so a platform that later grows an exclusion without growing this does not
// silently lose the flush.
func syncStateDir(dir string) error {
	return fmt.Errorf("%w: %s has no directory flush in this build", ErrStateStoreState, runtime.GOOS)
}
