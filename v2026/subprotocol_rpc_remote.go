//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// RemoteSubprotocol owns one subscription on one RPC connection. It never
// silently migrates to a reconnected companion. Receive preserves complete
// messages; callers must Close and reopen after a transport failure. Native
// bindings use DeviceLocal's portable subprotocol methods instead.
type RemoteSubprotocol struct {
	service         *rpcClient
	id              connect.Id
	ctx             context.Context
	cancel          context.CancelFunc
	closeOnce       sync.Once
	readMu, queryMu sync.Mutex
}

// OpenSubprotocolContext subscribes on a provider-capable native RPC endpoint.
// It is intentionally asynchronous at the JavaScript binding boundary.
func (d *DeviceRemote) OpenSubprotocolContext(ctx context.Context, protocol int32) (*RemoteSubprotocol, error) {
	if err := checkDeviceSubprotocolId(protocol); err != nil {
		return nil, err
	}
	d.stateLock.Lock()
	service := d.service
	if service == nil {
		service = d.browserService
	}
	d.stateLock.Unlock()
	if service == nil {
		return nil, errors.New("device RPC subprotocol service is unavailable")
	}
	lifetime, cancel := context.WithCancel(d.ctx)
	c := &RemoteSubprotocol{service: service, id: connect.NewId(), ctx: lifetime, cancel: cancel}
	_, err := c.call(ctx, &DeviceSubprotocolRequest{Op: "open", Protocol: protocol, ClientID: d.clientId, InstanceID: d.instanceId})
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *RemoteSubprotocol) call(ctx context.Context, req *DeviceSubprotocolRequest) (*DeviceSubprotocolResponse, error) {
	req.ID = c.id
	reply := new(DeviceSubprotocolResponse)
	done := make(chan error, 1)
	go func() {
		err := c.service.Call("DeviceLocalRpc.Subprotocol", req, reply)
		done <- err
		// Handle cancellation before a slow open reaches the native service.
		if err == nil && req.Op == "open" && c.ctx.Err() != nil {
			_ = c.closeRequest()
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (c *RemoteSubprotocol) operation(ctx context.Context, req *DeviceSubprotocolRequest) (*DeviceSubprotocolResponse, error) {
	for {
		if c.ctx.Err() != nil {
			return nil, net.ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reply, err := c.call(ctx, req)
		if err != nil || !reply.Pending {
			return reply, err
		}
		req = &DeviceSubprotocolRequest{Op: req.Op}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-c.ctx.Done():
			timer.Stop()
			return nil, net.ErrClosed
		}
	}
}

// Receive returns one owned message and its authenticated network source.
func (c *RemoteSubprotocol) Receive(ctx context.Context) (*Id, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	r, err := c.operation(ctx, &DeviceSubprotocolRequest{Op: "receive"})
	if err != nil {
		return nil, nil, err
	}
	return newId(r.Source), r.Data, nil
}

// Send reports acceptance into the transport queue, not application receipt.
func (c *RemoteSubprotocol) Send(ctx context.Context, destination *Id, data []byte) (bool, error) {
	if destination == nil || len(data) > subprotocolRPCMaxBytes {
		return false, errors.New("invalid subprotocol destination or frame exceeds 65535 bytes")
	}
	r, err := c.operation(ctx, &DeviceSubprotocolRequest{Op: "send", Destination: destination.toConnectId(), Data: append([]byte{}, data...)})
	if err != nil {
		return false, err
	}
	return r.OK, nil
}

// Query distinguishes an unanswered query (ok=false) from an empty supported set.
func (c *RemoteSubprotocol) Query(ctx context.Context, destination *Id, timeoutMillis int64) ([]int32, bool, error) {
	if destination == nil || timeoutMillis < 1 || timeoutMillis > 60000 {
		return nil, false, errors.New("invalid query destination or timeout")
	}
	c.queryMu.Lock()
	defer c.queryMu.Unlock()
	r, err := c.operation(ctx, &DeviceSubprotocolRequest{Op: "query", Start: true, Destination: destination.toConnectId(), TimeoutMillis: timeoutMillis})
	if err != nil {
		return nil, false, err
	}
	return r.Protocols, r.OK, nil
}

func (c *RemoteSubprotocol) closeRequest() error {
	return c.service.Call("DeviceLocalRpc.Subprotocol", &DeviceSubprotocolRequest{ID: c.id, Op: "close"}, new(DeviceSubprotocolResponse))
}

func (c *RemoteSubprotocol) Close() error {
	var err error
	c.closeOnce.Do(func() { c.cancel(); err = c.closeRequest() })
	return err
}
