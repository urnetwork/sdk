package sdk

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/internal/subprotocolrpc"
)

const subprotocolRPCMaxBytes = 65535
const subprotocolRPCMaxMessages = 64
const subprotocolRPCMaxQueuedBytes = 1 << 20
const subprotocolRPCMaxSubscriptions = 16

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
	queue       []subprotocolrpc.Response
	queuedBytes int
	query       *subprotocolrpc.Response
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
	e.queue = append(e.queue, subprotocolrpc.Response{Source: source.toConnectId(), Data: append([]byte{}, data...)})
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
	response *subprotocolrpc.Response
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
func (d *DeviceLocalRpc) Subprotocol(req *subprotocolrpc.Request, reply *subprotocolrpc.Response) error {
	if err := d.subprotocolRequest(req, reply); err != nil {
		reply.Error = err.Error()
	}
	return nil
}

func (d *DeviceLocalRpc) subprotocolRequest(req *subprotocolrpc.Request, reply *subprotocolrpc.Response) error {
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
		e.queue[0] = subprotocolrpc.Response{}
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
			q := &subprotocolrpc.Response{Pending: true}
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
