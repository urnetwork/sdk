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

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
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

func ioLoopCopyPacket(readBuffer []byte, byteCount int) []byte {
	return connect.MessagePoolCopy(readBuffer[:byteCount])
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

	// The packet slice is only borrowed by sendPacketsNoCopy for the duration
	// of the call. Keep one fixed batch for the lifetime of the TUN loop rather
	// than escaping a new [64][]byte to the heap on every read burst. Clear the
	// entries after ownership transfers so a pool buffer rejected by a full
	// free list is not kept alive by this long-lived goroutine.
	var packetStorage [64][]byte
	// A TUN read does not reveal its datagram length before Read. Stage into one
	// reusable buffer, then copy the exact packet into the matching message-pool
	// class. This adds one bounded memory copy, but lets the common 40--100 byte
	// TCP ACK retain 256 bytes instead of 2 KiB and avoids a Get/Return pair for
	// every nonblocking EAGAIN probe. The staging buffer escapes at most once for
	// the lifetime of the loop.
	var readBuffer [2048]byte
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		n, readErr := f.Read(readBuffer[:])
		if n <= 0 {
			if readErr != nil {
				return
			}
			continue
		}
		packetStorage[0] = ioLoopCopyPacket(readBuffer[:], n)
		packetCount := 1
		for packetCount < len(packetStorage) {
			nextByteCount, err := syscall.Read(self.fd, readBuffer[:])
			if 0 < nextByteCount {
				packetStorage[packetCount] = ioLoopCopyPacket(readBuffer[:], nextByteCount)
				packetCount += 1
				continue
			}
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			if err != nil {
				readErr = err
			}
			break
		}
		self.deviceLocal.sendPacketsNoCopy(packetStorage[:packetCount])
		clear(packetStorage[:packetCount])
		if readErr != nil {
			return
		}
	}
}

func (self *IoLoop) Close() {
	self.cancel()
}
