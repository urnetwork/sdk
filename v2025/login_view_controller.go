package sdk

import (
	"context"
	"time"

	"github.com/golang/glog"
)

const defaultNetworkCheckTimeout = 5 * time.Second

type LoginViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	api *Api

	// networkCheck *networkCheck
}

func NewLoginViewController(api *Api) *LoginViewController {
	return newLoginViewControllerWithContext(context.Background(), api)
}

func newLoginViewControllerWithContext(ctx context.Context, api *Api) *LoginViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &LoginViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		api:    api,
	}
	// vc.drawController = vc
	return vc
}

func (self *LoginViewController) Start() {
	// FIXME
}

func (self *LoginViewController) Stop() {
	// FIXME
}

func (self *LoginViewController) Close() {
	glog.Info("[livc]close")

	self.cancel()
}
