//go:build !(aix || android || darwin || dragonfly || freebsd || ios || linux || netbsd || openbsd || solaris)

package sdk

import (
	"io"
	"testing"
)

// testingIoLoopSocketPair skips only the native-fd IoLoop fixture where the
// target has no Unix datagram socketpair. Other tests in
// device_local_backpressure_test.go remain compiled for that target.
func testingIoLoopSocketPair(t *testing.T) (int32, io.ReadWriteCloser) {
	t.Helper()
	t.Skip("native IoLoop socketpair fixture is unavailable on this target")
	return -1, nil
}

func testingNewIoLoop(
	t *testing.T,
	_ *DeviceLocal,
	_ int32,
) interface{ Close() } {
	t.Helper()
	t.Skip("native IoLoop implementation is unavailable on this target")
	return nil
}
