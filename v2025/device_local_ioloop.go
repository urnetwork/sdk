package sdk

import (
	"context"
	"syscall"

	// "github.com/golang/glog"

	"github.com/urnetwork/connect/v2025"
	"github.com/urnetwork/connect/v2025/protocol"
)

// implements a file descriptor send/receive loop
// this avoids transferring byte buffers between go and native code
// on android, byte buffers are copied between go and native code which leads to unnecessary performance overhead

type IoLoop struct {
	ctx         context.Context
	cancel      context.CancelFunc
	deviceLocal *DeviceLocal
	// f *os.File
	fd int
}

func NewIoLoop(deviceLocal *DeviceLocal, fd int32) *IoLoop {
	ctx, cancel := context.WithCancel(deviceLocal.ctx)

	// f := os.NewFile(uintptr(fd), "tun")

	ioLoop := &IoLoop{
		ctx:         ctx,
		cancel:      cancel,
		deviceLocal: deviceLocal,
		fd:          int(fd),
	}
	return ioLoop
}

func (self *IoLoop) Run() {
	defer self.cancel()

	// defer self.f.Close()

	receive := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		// _, err := self.f.Write(packet)
		_, err := syscall.Write(self.fd, packet)
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
		// n, err := self.f.Read(p)
		n, err := syscall.Read(self.fd, packet)
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
