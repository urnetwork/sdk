// The fail-closed half. Its constraint is the exact complement of the windows file's and the
// flock file's, derived from the primitive rather than from a build term that is an instance of
// one -- so the set below is what it is because neither syscall.CreateFile nor syscall.Flock is
// declared there, and not because `unix` happens to exclude it.
//
// ITS CONSTITUENCY, named rather than left to be discovered: solaris, aix, js/wasm, wasip1/wasm
// and plan9. js/wasm is a target THIS MODULE ALREADY BUILDS FOR -- device_rpc_platform_js.go
// carries //go:build js and device_rpc_platform_native.go carries //go:build !js -- so the priced
// consequence is concrete: the message stream store refuses to open on the wasm artifact, and
// NewGroupSession refuses a nil reserver, so that artifact has no messaging until somebody
// supplies an exclusion for it. Whether it should is S2-20.
//
// A PLATFORM THIS STORE CANNOT MAKE SAFE IS A PLATFORM IT REFUSES TO OPEN ON. A build tag that
// quietly compiled to a no-op returning a nil closer and a nil error would be the single-writer
// property deleted by a build constraint: every gate for it would still be green on the platforms
// that have the primitive, and the platform without one would ship two stores allocating the same
// index with nothing to say so.

//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !sdk_mobile_bind

package sdk

import (
	"fmt"
	"io"
	"runtime"
)

func acquireStreamStoreExclusion(dir string) (io.Closer, error) {
	return nil, fmt.Errorf(
		"%w: %s has neither an exclusive CreateFile nor syscall.Flock, so this build cannot hold a single-writer exclusion on %s and will not open a store there; two stores over one directory each read the same persisted high water and each allocate the same next index, which spec A section 5.6 calls a total break of both AEADs for that record (S2-20)",
		ErrStreamStoreLocked,
		runtime.GOOS,
		dir,
	)
}
