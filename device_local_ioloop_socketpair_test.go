//go:build aix || android || darwin || dragonfly || freebsd || ios || linux || netbsd || openbsd || solaris

package sdk

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/urnetwork/connect"
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

func TestIoLoopCopyPacketUsesExactPacketRootClass(t *testing.T) {
	var readBuffer [2048]byte
	for _, testCase := range []struct {
		name          string
		packetBytes   int
		wantRootBytes connect.ByteCount
	}{
		{name: "tcp ack", packetBytes: 60, wantRootBytes: 256},
		{name: "full payload", packetBytes: 1100, wantRootBytes: 2048},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			packet := ioLoopCopyPacket(readBuffer[:], testCase.packetBytes)
			defer connect.MessagePoolReturn(packet)
			if got := connect.MessagePoolPacketRootByteCount(packet); got != testCase.wantRootBytes {
				t.Fatalf("packet root bytes = %d, want %d", got, testCase.wantRootBytes)
			}
		})
	}
}

// BenchmarkIoLoopPacketOwnership isolates the cost of the exact-size staging
// copy used after a TUN read. direct-full-root models the former ownership
// handoff after the kernel had filled a 2-KiB root; staged-exact includes the
// new copy into either the 256-byte or 2-KiB class. The syscall itself is
// deliberately outside this microbenchmark and is identical in both designs.
func BenchmarkIoLoopPacketOwnership(b *testing.B) {
	for _, size := range []int{60, 1100} {
		b.Run(fmt.Sprintf("direct-full-root/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				packet := connect.MessagePoolGet(2048)
				packet[0] = 1
				connect.MessagePoolReturn(packet[:size])
			}
		})
		b.Run(fmt.Sprintf("staged-exact/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			var readBuffer [2048]byte
			readBuffer[0] = 1
			for b.Loop() {
				packet := connect.MessagePoolCopy(readBuffer[:size])
				connect.MessagePoolReturn(packet)
			}
		})
	}
}
