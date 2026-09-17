package sdk

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const subprotocolRPCMaxBytes = 65535
const subprotocolRPCMaxMessages = 64
const subprotocolRPCMaxQueuedBytes = 1 << 20
const subprotocolRPCMaxSubscriptions = 16

// Subprotocol messages are discrete records. Unlike state notifications, they
// must never be coalesced. The bounded session queue fails explicitly if its
// consumer falls behind. Polls keep the sequential control RPC available.
//
//gomobile:noexport
type DeviceSubprotocolRequest struct {
	ID, ClientID, InstanceID connect.Id
	Op                       string
	Protocol                 int32
	Destination              connect.Id
	Data                     []byte
	Start                    bool
	TimeoutMillis            int64
}

//gomobile:noexport
type DeviceSubprotocolResponse struct {
	Pending, OK bool
	Source      connect.Id
	Data        []byte
	Protocols   []int32
	Error       string
}

type subprotocolRpcRegistry struct {
	mu      sync.Mutex
	once    sync.Once
	closed  bool
	entries map[connect.Id]*subprotocolRpcEntry
}

type subprotocolRpcEntry struct {
	mu          sync.Mutex
	protocolID  int32
	sub         Sub
	closed      bool
	err         error
	queue       []DeviceSubprotocolResponse
	queuedBytes int
	query       *DeviceSubprotocolResponse
}

func (e *subprotocolRpcEntry) SubprotocolMessage(_ int32, source *Id, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.err != nil {
		return
	}
	if source == nil || len(data) > subprotocolRPCMaxBytes || len(e.queue) >= subprotocolRPCMaxMessages || e.queuedBytes+len(data) > subprotocolRPCMaxQueuedBytes {
		e.err = errors.New("subprotocol receive queue overflow or oversized frame; subscription must be reopened")
		e.queue = nil
		e.queuedBytes = 0
		return
	}
	// The native callback owns ephemeral input; copy before returning to it.
	e.queue = append(e.queue, DeviceSubprotocolResponse{Source: source.toConnectId(), Data: append([]byte{}, data...)})
	e.queuedBytes += len(data)
}

func (e *subprotocolRpcEntry) close() {
	e.mu.Lock()
	e.closed = true
	sub := e.sub
	e.sub = nil
	e.queue = nil
	e.query = nil
	e.mu.Unlock()
	if sub != nil {
		sub.Close()
	}
}

func (s *subprotocolRpcRegistry) close() {
	s.mu.Lock()
	s.closed = true
	entries := s.entries
	s.entries = nil
	s.mu.Unlock()
	for _, e := range entries {
		e.close()
	}
}

type subprotocolRpcQuery struct {
	entry    *subprotocolRpcEntry
	response *DeviceSubprotocolResponse
}

func (q *subprotocolRpcQuery) Result(ids *IntList, ok bool) {
	q.entry.mu.Lock()
	defer q.entry.mu.Unlock()
	if q.entry.closed || q.entry.query != q.response {
		return
	}
	q.response.OK = ok
	q.response.Pending = false
	if ids != nil {
		for i := 0; i < ids.Len(); i++ {
			q.response.Protocols = append(q.response.Protocols, int32(ids.Get(i)))
		}
	}
}

// Subprotocol is an optional additive RPC capability. Hosted proxies remain
// ineligible: only an explicitly provider-capable, visible DeviceLocal serves
// application messages. Closing an RPC session removes all its registrations.
func (d *DeviceLocalRpc) Subprotocol(req *DeviceSubprotocolRequest, reply *DeviceSubprotocolResponse) error {
	if err := d.subprotocolRequest(req, reply); err != nil {
		reply.Error = err.Error()
	}
	return nil
}

func (d *DeviceLocalRpc) subprotocolRequest(req *DeviceSubprotocolRequest, reply *DeviceSubprotocolResponse) error {
	if d.deviceLocal == nil || d.deviceLocal.settings == nil || d.deviceLocal.settings.HostedIncompatible || !d.deviceLocal.settings.AllowProvider {
		return errors.New("subprotocol RPC requires a provider-capable native DeviceLocal; hosted proxy devices do not support messaging")
	}
	s := &d.subprotocols
	s.mu.Lock()
	if s.closed || d.ctx.Err() != nil {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.once.Do(func() {
		s.entries = make(map[connect.Id]*subprotocolRpcEntry)
		context.AfterFunc(d.ctx, s.close)
	})
	if req.Op == "open" {
		if req.ClientID != d.deviceLocal.clientId || req.InstanceID != d.deviceLocal.instanceId {
			s.mu.Unlock()
			return errors.New("subprotocol companion client or instance identity mismatch")
		}
		if err := checkDeviceSubprotocolId(req.Protocol); err != nil {
			s.mu.Unlock()
			return err
		}
		if req.ID == (connect.Id{}) || s.entries[req.ID] != nil || len(s.entries) >= subprotocolRPCMaxSubscriptions {
			s.mu.Unlock()
			return errors.New("invalid, duplicate, or excess subprotocol subscription")
		}
		e := &subprotocolRpcEntry{protocolID: req.Protocol}
		s.entries[req.ID] = e
		s.mu.Unlock()
		sub, err := d.deviceLocal.EnableSubprotocol(req.Protocol, e)
		if err != nil {
			s.mu.Lock()
			delete(s.entries, req.ID)
			s.mu.Unlock()
			e.close()
			return err
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			sub.Close()
			return net.ErrClosed
		}
		e.sub = sub
		e.mu.Unlock()
		return nil
	}
	e := s.entries[req.ID]
	if req.Op == "close" {
		delete(s.entries, req.ID)
	}
	s.mu.Unlock()
	if req.Op == "close" {
		if e != nil {
			e.close()
		}
		return nil
	}
	if e == nil {
		return net.ErrClosed
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return net.ErrClosed
	}
	if e.err != nil {
		err := e.err
		e.mu.Unlock()
		return err
	}
	switch req.Op {
	case "receive":
		defer e.mu.Unlock()
		if len(e.queue) == 0 {
			reply.Pending = true
			return nil
		}
		*reply = e.queue[0]
		e.queue[0] = DeviceSubprotocolResponse{}
		e.queue = e.queue[1:]
		e.queuedBytes -= len(reply.Data)
		return nil
	case "send":
		e.mu.Unlock()
		if req.Destination == (connect.Id{}) || len(req.Data) > subprotocolRPCMaxBytes {
			return errors.New("invalid subprotocol destination or frame exceeds 65535 bytes")
		}
		// A subscription's protocol is bound at open; never allow it to send
		// through an arbitrary protocol by changing a later RPC request.
		if req.Protocol != 0 {
			return errors.New("send protocol must be inherited from subscription")
		}
		reply.OK = d.deviceLocal.SendSubprotocolBytes(e.protocolID, newId(req.Destination), req.Data)
		return nil
	case "query":
		if req.Start {
			if e.query != nil || req.Destination == (connect.Id{}) || req.TimeoutMillis < 1 || req.TimeoutMillis > 60000 {
				e.mu.Unlock()
				return errors.New("invalid or concurrent subprotocol query")
			}
			q := &DeviceSubprotocolResponse{Pending: true}
			e.query = q
			e.mu.Unlock()
			d.deviceLocal.QuerySubprotocols(newId(req.Destination), req.TimeoutMillis, &subprotocolRpcQuery{e, q})
			e.mu.Lock()
		}
		defer e.mu.Unlock()
		if e.query == nil {
			return errors.New("no pending subprotocol query")
		}
		*reply = *e.query
		if !reply.Pending {
			e.query = nil
		}
		return nil
	default:
		e.mu.Unlock()
		return errors.New("unknown subprotocol operation")
	}
}

// RemoteSubprotocol owns one subscription on one RPC connection. It never
// silently migrates to a reconnected companion. Receive preserves complete
// messages; callers must Close and reopen after a transport failure.
//
//gomobile:noexport
type RemoteSubprotocol struct {
	service         *rpcClient
	id              connect.Id
	ctx             context.Context
	cancel          context.CancelFunc
	closeOnce       sync.Once
	readMu, queryMu sync.Mutex
}

// OpenSubprotocolContext subscribes on a provider-capable native RPC endpoint.
// It is intentionally asynchronous at the JS binding boundary.
//
//gomobile:noexport
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
//
//gomobile:noexport
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
//
//gomobile:noexport
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
//
//gomobile:noexport
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
