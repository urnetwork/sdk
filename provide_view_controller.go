package sdk

import (
	"context"
)

var pvcLog = logFn("provide_view_controller")

type ProvideViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	device Device
}

func newProvideViewController(ctx context.Context, device Device) *ProvideViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ProvideViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	// vc.drawController = vc
	return vc
}

func (self *ProvideViewController) Start() {
	// FIXME
}

func (self *ProvideViewController) Stop() {
	// FIXME
}

// func (self *ProvideViewController) draw(g gl.Context) {
// 	// pvcLog("draw")

// 	g.ClearColor(self.bgRed, self.bgGreen, self.bgBlue, 1.0)
// 	g.Clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT)
// }

// func (self *ProvideViewController) drawLoopOpen() {
// 	self.frameRate = 24
// }

// func (self *ProvideViewController) drawLoopClose() {
// }

func (self *ProvideViewController) Close() {
	pvcLog("close")

	self.cancel()
}
