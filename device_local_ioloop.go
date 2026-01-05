package sdk

import (
	"context"
	"os"
	"syscall"

	// "net"
	"sync"

	"github.com/urnetwork/glog"

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
	ctx, cancel := context.WithCancel(deviceLocal.ctx)

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

	f := os.NewFile(uintptr(self.fd), "urnetwork")
	defer f.Close()

	defer connect.HandleError(func() {
		if self.doneCallback != nil {
			self.doneCallback.IoLoopDone()
		}
	})

	defer self.cancel()

	err := syscall.SetNonblock(self.fd, true)
	if err != nil {
		glog.Infof("[io]WARNING: could not set non-blocking = %s\n", err)
	}

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
	callbackId := self.deviceLocal.receiveCallbacks.Add(receive)
	defer self.deviceLocal.receiveCallbacks.Remove(callbackId)

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		packet := MessagePoolGet(2048)
		n, err := f.Read(packet)
		// glog.Infof("[io]READ PACKET %d (%s)\n", n, err)
		if 0 < n {
			success := self.deviceLocal.sendPacket(packet[:n])
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
