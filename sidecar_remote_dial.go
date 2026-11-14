//go:build !ios_extension && !js && !ios && !android

// The remote backend's control session on a desktop (§5.8): the root's DaemonClient on the instance's socket or pipe,
// or on the socket in a sandboxed app's app-group container, in control mode at this build's version. The daemon
// checks the peer's uid and refuses another version; nothing here spawns one (R-SDK-9).

package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/urnetwork/sidecar"
)

// The path of the control socket settings name: DaemonSocketDir's socket for the instance, else §5.9's.
func sidecarDaemonSocketPath(settings *SidecarSettings) (string, error) {
	if settings.DaemonSocketDir != "" {
		return filepath.Join(settings.DaemonSocketDir, sidecar.DaemonAppGroupSocketName(settings.Instance)), nil
	}
	return sidecar.DaemonSocketPath(settings.Instance)
}

// A dial of the control socket settings name.
func sidecarDaemonControlDial(settings *SidecarSettings) (sidecarControlDial, error) {
	socketPath, err := sidecarDaemonSocketPath(settings)
	if err != nil {
		return nil, err
	}
	return sidecarDaemonSocketDial(socketPath, sidecar.Version), nil
}

// A dial of the daemon at socketPath, in control mode at version.
func sidecarDaemonSocketDial(socketPath string, version string) sidecarControlDial {
	return func(ctx context.Context) (sidecarControlChannel, error) {
		hello := &sidecar.DaemonHello{Mode: sidecar.DaemonModeControl, Version: version, Pid: int64(os.Getpid())}
		client, err := sidecar.DialDaemon(ctx, socketPath, hello)
		if err != nil {
			return nil, err
		}
		return &sidecarDaemonChannel{client: client}, nil
	}
}

// A control session on the root's DaemonClient.
type sidecarDaemonChannel struct {
	client    *sidecar.DaemonClient
	closeOnce sync.Once
}

// See sidecarControlChannel. The daemon's refusal crosses as a *sidecarControlError.
func (self *sidecarDaemonChannel) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	err := self.client.Call(ctx, method, params, confirmed, result)
	var refusal *sidecar.DaemonRpcError
	if errors.As(err, &refusal) {
		return &sidecarControlError{Code: refusal.Code, Message: refusal.Message}
	}
	return err
}

// See sidecarControlChannel.
func (self *sidecarDaemonChannel) close() {
	self.closeOnce.Do(func() {
		self.client.Close()
	})
}
