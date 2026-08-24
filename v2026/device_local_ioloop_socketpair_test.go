//go:build aix || android || darwin || dragonfly || freebsd || ios || linux || netbsd || openbsd || solaris

package sdk

import (
	"io"
	"os"
	"syscall"
	"testing"
)

// testingIoLoopSocketPair creates the Unix datagram pair used to model the
// platform TUN descriptor. Keep native-only syscalls out of the shared test
// file so JS and Windows release compile checks continue to cover the rest of
// the SDK tests.
func testingIoLoopSocketPair(t *testing.T) (int32, io.ReadWriteCloser) {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	closeBoth := func() {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	}
	for _, fd := range fds {
		if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 512<<10); err != nil {
			closeBoth()
			t.Fatalf("set socket send buffer: %v", err)
		}
		if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 512<<10); err != nil {
			closeBoth()
			t.Fatalf("set socket receive buffer: %v", err)
		}
		if err = syscall.SetNonblock(fd, true); err != nil {
			closeBoth()
			t.Fatalf("set socket nonblocking: %v", err)
		}
	}
	return int32(fds[0]), os.NewFile(uintptr(fds[1]), "test-tun")
}

func testingNewIoLoop(
	t *testing.T,
	device *DeviceLocal,
	fd int32,
) interface{ Close() } {
	t.Helper()
	return NewIoLoop(device, fd, nil)
}
