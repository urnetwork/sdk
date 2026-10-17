package sdk

import (
	"context"
	"fmt"
	"net"
)

// HostedDeviceRpcListener is a DeviceRpcListener whose connections are pushed
// in by the device rpc http handler rather than accepted from a bound socket.
// The proxy host installs one per hosted DeviceLocal (via
// DeviceLocal.StartHostedRpc) and calls ServeWs for each device-rpc websocket a
// DeviceRemote opens to it directly. ServeWs builds the mux and blocks until
// the session ends, so the caller (the http handler) keeps the websocket alive
// for the session's lifetime.
type HostedDeviceRpcListener struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *deviceRpcSettings

	accepts chan [2]net.Conn
}

var _ deviceRpcListener = (*HostedDeviceRpcListener)(nil)

func NewHostedDeviceRpcListener(ctx context.Context) *HostedDeviceRpcListener {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &HostedDeviceRpcListener{
		ctx:      cancelCtx,
		cancel:   cancel,
		settings: defaultDeviceRpcSettings(),
		accepts:  make(chan [2]net.Conn),
	}
}

// ServeWs builds the rpc mux over ws (a device-rpc websocket relayed from the
// resident), hands the forward/reverse streams to the next Accept, and blocks
// until the session ends (the mux closes) or the listener is closed. The bridge
// http handler calls this and returns when it does, closing the websocket.
func (self *HostedDeviceRpcListener) ServeWs(ws DeviceRpcWs) error {
	// see the companion convention note in device_rpc_transport.go
	wsFull, ok := ws.(deviceRpcWs)
	if !ok {
		return fmt.Errorf("ws must implement the deviceRpcWs companion")
	}
	mux := newDeviceRpcMux(self.ctx, wsFull, self.settings)
	pair := [2]net.Conn{
		mux.conns[deviceRpcStreamForward],
		mux.conns[deviceRpcStreamReverse],
	}
	select {
	case self.accepts <- pair:
	case <-self.ctx.Done():
		mux.close()
		return fmt.Errorf("hosted device rpc listener closed")
	case <-mux.ctx.Done():
		return fmt.Errorf("hosted device rpc mux closed before accept")
	}
	// block until the session ends so the caller keeps the websocket open
	select {
	case <-mux.ctx.Done():
	case <-self.ctx.Done():
		mux.close()
	}
	return nil
}

func (self *HostedDeviceRpcListener) Accept(ctx context.Context) (net.Conn, net.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-self.ctx.Done():
		return nil, nil, fmt.Errorf("hosted device rpc listener closed")
	case pair := <-self.accepts:
		return pair[0], pair[1], nil
	}
}

func (self *HostedDeviceRpcListener) Close() error {
	self.cancel()
	return nil
}
