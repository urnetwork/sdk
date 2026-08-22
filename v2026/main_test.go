package sdk

import (
	"fmt"
	"net"
	"os"
	"testing"
)

// TestMain points the default device rpc address at a per-process ephemeral
// port. Every rpc test — including the DeviceLocal default rpc manager, which
// reads the default settings internally — then shares a port unique to this
// test process, so a suite run never collides with an orphaned earlier run,
// another checkout's suite, or a locally running app on the fixed production
// default (127.0.0.1:12025). A genuinely shared port is separately rejected by
// the sync instance check, so a collision fails loud instead of pairing a
// remote with the wrong device.
func TestMain(m *testing.M) {
	deviceRpcDefaultAddress = testing_freeHostPort()
	os.Exit(m.Run())
}

// testing_freeHostPort reserves a free loopback port from the kernel (bind
// 127.0.0.1:0, read the assigned port, close) and returns "127.0.0.1:<port>".
// The kernel cycles ephemeral assignments, so successive calls return distinct
// ports and nothing rebinds a returned port for the lifetime of a test run.
func testing_freeHostPort() string {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	return fmt.Sprintf("127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
}
