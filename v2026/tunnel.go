package sdk

import (
	"context"
)


type Tunnel struct {
	ctx context.Context
	cancel context.CancelFunc
}

func NewTunnel() *Tunnel {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tunnel{
		ctx: ctx,
		cancel: cancel,
	}
}

func (self *Tunnel) GetDone() bool {
	select {
	case <- self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *Tunnel) Cancel() {
	self.cancel()
}

func (self *Tunnel) Close() {
	self.cancel()
}
