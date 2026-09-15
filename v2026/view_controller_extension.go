//go:build ios_extension

package sdk

import "context"

// The packet-tunnel extension never constructs UI view controllers. Keep only
// the lifecycle hook embedded by DeviceLocal and DeviceRemote so the
// extension-specific gomobile binding does not root the app UI API and its
// generated Objective-C metadata in the extension executable.
type viewControllerManager struct {
	cancel context.CancelFunc
}

type ViewControllerManager interface {
	Close()
}

func newViewControllerManager(
	ctx context.Context,
	device Device,
) *viewControllerManager {
	_, cancel := context.WithCancel(ctx)
	return &viewControllerManager{cancel: cancel}
}

func (self *viewControllerManager) Close() {
	self.cancel()
}
