// the fd loop assumes posix fd semantics
// on windows the app-side tunnel pumps packets via SendPacket/AddReceivePacketCallback instead

//go:build !windows

package sdk

import (
	"context"
	"os"
	"syscall"

	// "net"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// implements a file descriptor send/receive loop
// this avoids transferring byte buffers between go and native code
// on android, byte buffers are copied between go and native code which leads to unnecessary performance overhead

type IoLoopDoneCallback interface {
	IoLoopDone()
}

type IoLoop struct {
	ctx          context.Context
	cancel       context.CancelFunc
	deviceLocal  *DeviceLocal
	fd           int
	doneCallback IoLoopDoneCallback
}

// the fd must be:
// - opened in non blocking mode
// - detached so that it can be closed the the ioloop
func NewIoLoop(deviceLocal *DeviceLocal, fd int32, doneCallback IoLoopDoneCallback) *IoLoop {
	ctx, cancel := context.WithCancel(deviceLocal.Ctx())

	ioLoop := &IoLoop{
		ctx:          ctx,
		cancel:       cancel,
		deviceLocal:  deviceLocal,
		fd:           int(fd),
		doneCallback: doneCallback,
	}
	go connect.HandleError(ioLoop.run, cancel)
	return ioLoop
}

func (self *IoLoop) run() {
	defer self.cancel()

	// set non-blocking BEFORE os.NewFile: os.NewFile only registers the fd with the
	// runtime poller when it is already non-blocking, and the poller registration is
	// what lets Close unblock a pending Read (see below)
	err := syscall.SetNonblock(self.fd, true)
	if err != nil {
		self.deviceLocal.log.Infof("[io]WARNING: could not set non-blocking = %s\n", err)
	}

	f := os.NewFile(uintptr(self.fd), "urnetwork")

	// unblock a Read parked on a quiet link when the loop (or its device) closes:
	// without this the read loop below sits in f.Read until the next packet happens
	// to arrive, leaking the goroutine and holding the detached fd across every
	// tunnel stop/start on an idle interface
	go connect.HandleError(func() {
		defer f.Close()
		select {
		case <-self.ctx.Done():
		}
	})

	defer connect.HandleError(func() {
		if self.doneCallback != nil {
			self.doneCallback.IoLoopDone()
		}
	})

	var writeMutex sync.Mutex

	receivePackets := func(
		source connect.TransferPath,
		provideMode protocol.ProvideMode,
		ipPath *connect.IpPath,
		packets [][]byte,
	) {
		// Every packet is borrowed for this call only. The synchronous TUN write
		// is the device-side flow-control exception documented in CODESTYLE.md.
		writeMutex.Lock()
		defer writeMutex.Unlock()
		for _, packet := range packets {
			if _, err := f.Write(packet); err != nil {
				self.cancel()
				return
			}
		}
	}

	unsub := self.deviceLocal.AddReceivePacketsCallback(receivePackets)
	defer unsub()

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		var packetStorage [64][]byte
		packet := MessagePoolGet(2048)
		n, readErr := f.Read(packet)
		if n <= 0 {
			MessagePoolReturn(packet)
			if readErr != nil {
				return
			}
			continue
		}
		packetStorage[0] = packet[:n]
		packetCount := 1
		for packetCount < len(packetStorage) {
			nextPacket := MessagePoolGet(2048)
			nextByteCount, err := syscall.Read(self.fd, nextPacket)
			if 0 < nextByteCount {
				packetStorage[packetCount] = nextPacket[:nextByteCount]
				packetCount += 1
				continue
			}
			MessagePoolReturn(nextPacket)
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			if err != nil {
				readErr = err
			}
			break
		}
		self.deviceLocal.sendPacketsNoCopy(packetStorage[:packetCount])
		if readErr != nil {
			return
		}
	}
}

func (self *IoLoop) Close() {
	self.cancel()
}
