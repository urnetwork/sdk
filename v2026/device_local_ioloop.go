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

	receive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		// note `packet` is only valid for the lifecycle of this call
		writeMutex.Lock()
		defer writeMutex.Unlock()

		_, err := f.Write(packet)
		if err != nil {
			self.cancel()
		}
	}

	unsub := self.deviceLocal.AddReceivePacketCallback(receive)
	defer unsub()

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		packet := MessagePoolGet(2048)
		n, err := f.Read(packet)
		// self.log.Infof("[io]READ PACKET %d (%s)\n", n, err)
		if 0 < n {
			success := self.deviceLocal.SendPacketNoCopy(packet, int32(n))
			if !success {
				MessagePoolReturn(packet)
			}
		} else {
			MessagePoolReturn(packet)
		}
		if err != nil {
			return
		}
	}
}

func (self *IoLoop) Close() {
	self.cancel()
}
