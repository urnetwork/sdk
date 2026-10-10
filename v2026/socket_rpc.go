package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const socketRPCMaxConnections = 128
const socketRPCMaxBytes = 65535

// Ctx returns the device lifetime, including cancellation by its proxy owner.
//
//gomobile:noexport
func (d *DeviceRemote) Ctx() context.Context { return d.ctx }

// Socket operations must not block the sequential device RPC dispatcher.
// Start queues one bounded operation; subsequent polls take its result once.
// IDs and pending operations belong to this RPC session, never to a reconnect.
//
//gomobile:noexport
type DeviceSocketRequest struct {
	ID               connect.Id
	Op               string
	Start            bool
	Network, Address string
	Data             []byte
	Size             int
	Deadline         time.Time
}

//gomobile:noexport
type DeviceSocketResponse struct {
	Pending                bool
	Data                   []byte
	N                      int
	Local, Remote, Network string
	Error, ErrorKind       string
}

func (r *DeviceSocketResponse) setError(err error) {
	if err == nil {
		return
	}
	r.Error = err.Error()
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF):
		r.ErrorKind = "eof"
	case errors.Is(err, net.ErrClosed):
		r.ErrorKind = "closed"
	case errors.Is(err, context.Canceled):
		r.ErrorKind = "canceled"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		r.ErrorKind = "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		r.ErrorKind = "timeout"
	}
}
func (r *DeviceSocketResponse) err() error {
	switch r.ErrorKind {
	case "eof":
		return io.EOF
	case "closed":
		return net.ErrClosed
	case "canceled":
		return context.Canceled
	case "timeout":
		return os.ErrDeadlineExceeded
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

type socketRpcOp struct {
	done   chan struct{}
	result DeviceSocketResponse
}
type socketRpcEntry struct {
	cancel            context.CancelFunc
	conn              net.Conn
	open, read, write *socketRpcOp
}
type socketRpcRegistry struct {
	mu      sync.Mutex
	once    sync.Once
	closed  bool
	entries map[connect.Id]*socketRpcEntry
	workers sync.WaitGroup
}

func (s *socketRpcRegistry) close() {
	s.mu.Lock()
	s.closed = true
	entries := s.entries
	s.entries = nil
	s.mu.Unlock()
	for _, e := range entries {
		e.cancel()
		if e.conn != nil {
			_ = e.conn.Close()
		}
	}
}

// Socket is an optional additive RPC capability. An older server rejects this
// method without closing the control session or falling back to host sockets.
func (d *DeviceLocalRpc) Socket(req *DeviceSocketRequest, reply *DeviceSocketResponse) error {
	if err := d.socketRequest(req, reply); err != nil {
		reply.setError(err)
	}
	return nil
}

func (d *DeviceLocalRpc) socketRequest(req *DeviceSocketRequest, reply *DeviceSocketResponse) error {
	s := &d.sockets
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d.ctx.Err() != nil {
		return net.ErrClosed
	}
	s.once.Do(func() {
		s.entries = make(map[connect.Id]*socketRpcEntry)
		context.AfterFunc(d.ctx, s.close)
	})
	if req.Op == "open" && req.Start {
		if _, exists := s.entries[req.ID]; exists {
			return errors.New("socket ID is already in use")
		}
		if len(s.entries) >= socketRPCMaxConnections {
			return errors.New("socket limit reached")
		}
		if err := socketNetwork(req.Network); err != nil {
			return err
		}
		if len(req.Address) > 1024 {
			return errors.New("socket address too long")
		}
		ctx, cancel := context.WithCancel(d.ctx)
		e := &socketRpcEntry{cancel: cancel, open: &socketRpcOp{done: make(chan struct{})}}
		s.entries[req.ID] = e
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
			defer dialCancel()
			c, err := d.deviceLocal.DialContext(dialCtx, req.Network, req.Address)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.closed || s.entries[req.ID] != e || ctx.Err() != nil {
				if c != nil {
					_ = c.Close()
				}
				return
			}
			e.conn = c
			e.open.result.setError(err)
			if c != nil {
				e.open.result.Local = c.LocalAddr().String()
				e.open.result.Remote = c.RemoteAddr().String()
				e.open.result.Network = c.RemoteAddr().Network()
			}
			close(e.open.done)
		}()
		reply.Pending = true
		return nil
	}
	e := s.entries[req.ID]
	if e == nil {
		return net.ErrClosed
	}
	if req.Op == "close" {
		delete(s.entries, req.ID)
		e.cancel()
		if e.conn != nil {
			return e.conn.Close()
		}
		return nil
	}
	if req.Op == "open" {
		select {
		case <-e.open.done:
			*reply = e.open.result
		default:
			reply.Pending = true
		}
		return nil
	}
	if e.conn == nil {
		return errors.New("socket is not connected")
	}
	switch req.Op {
	case "deadline":
		return e.conn.SetDeadline(req.Deadline)
	case "readDeadline":
		return e.conn.SetReadDeadline(req.Deadline)
	case "writeDeadline":
		return e.conn.SetWriteDeadline(req.Deadline)
	case "closeRead":
		return socketHalfClose(e.conn, true)
	case "closeWrite":
		return socketHalfClose(e.conn, false)
	case "read", "write":
		var slot **socketRpcOp
		if req.Op == "read" {
			slot = &e.read
		} else {
			slot = &e.write
		}
		if req.Start {
			if *slot != nil {
				return errors.New("socket operation already in progress")
			}
			if req.Size < 0 || req.Size > socketRPCMaxBytes || len(req.Data) > socketRPCMaxBytes {
				return errors.New("socket buffer exceeds 65535 bytes")
			}
			op := &socketRpcOp{done: make(chan struct{})}
			*slot = op
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				var err error
				if req.Op == "read" {
					buf := make([]byte, req.Size)
					op.result.N, err = e.conn.Read(buf)
					op.result.Data = buf[:op.result.N]
				} else {
					op.result.N, err = e.conn.Write(req.Data)
				}
				op.result.Local, op.result.Remote = e.conn.LocalAddr().String(), e.conn.RemoteAddr().String()
				op.result.setError(err)
				close(op.done)
			}()
		}
		if *slot == nil {
			return errors.New("no pending socket operation")
		}
		select {
		case <-(*slot).done:
			*reply = (*slot).result
			*slot = nil
		default:
			reply.Pending = true
		}
		return nil
	default:
		return errors.New("unknown socket operation")
	}
}

//gomobile:noexport
func (d *DeviceRemote) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

//gomobile:noexport
func (d *DeviceRemote) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := socketNetwork(network); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	d.stateLock.Lock()
	service := d.service
	if service == nil {
		service = d.browserService
	}
	d.stateLock.Unlock()
	if service == nil {
		return nil, errors.New("device RPC socket service is unavailable")
	}
	c := &remoteSocketConn{service: service, id: connect.NewId(), ctx: d.ctx, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := c.operation(ctx, &DeviceSocketRequest{Op: "open", Start: true, Network: network, Address: address})
	if err != nil {
		go c.Close()
		return nil, err
	}
	if strings.HasPrefix(network, "udp") {
		c.local, err = socketRPCAddr("udp", result.Local)
		if err == nil {
			c.remote, err = socketRPCAddr("udp", result.Remote)
		}
		c.datagram = true
	} else {
		c.local, err = socketRPCAddr("tcp", result.Local)
		if err == nil {
			c.remote, err = socketRPCAddr("tcp", result.Remote)
		}
	}
	if err != nil {
		go c.Close()
		return nil, err
	}
	c.ownerMu.Lock()
	c.stopOwner = context.AfterFunc(d.ctx, func() { _ = c.Close() })
	c.ownerMu.Unlock()
	return c, nil
}

// RPC endpoint metadata must be numeric. Parsing it must never initiate a
// hostname lookup in the caller's host resolver.
func socketRPCAddr(network, address string) (net.Addr, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid socket endpoint: %w", err)
	}
	if network == "udp" {
		return net.UDPAddrFromAddrPort(endpoint), nil
	}
	return &net.TCPAddr{IP: net.IP(endpoint.Addr().AsSlice()), Port: int(endpoint.Port()), Zone: endpoint.Addr().Zone()}, nil
}

type remoteSocketConn struct {
	service         *rpcClient
	id              connect.Id
	ctx             context.Context
	done            chan struct{}
	local, remote   net.Addr
	datagram        bool
	readMu, writeMu sync.Mutex
	ownerMu, addrMu sync.Mutex
	closeOnce       sync.Once
	stopOwner       func() bool
}

func (c *remoteSocketConn) call(ctx context.Context, req *DeviceSocketRequest) (*DeviceSocketResponse, error) {
	req.ID = c.id
	reply := new(DeviceSocketResponse)
	// Call installs the transport timeout before net/rpc writes its request.
	// That also bounds a blocked transport write during connection teardown.
	done := make(chan error, 1)
	go func() {
		err := c.service.Call("DeviceLocalRpc.Socket", req, reply)
		done <- err
		if err == nil && req.Op == "open" && req.Start {
			// A canceled open may reach a slow RPC transport after its first
			// close request. Close again after the start is acknowledged.
			select {
			case <-c.done:
				_ = c.closeRequest()
			default:
			}
		}
	}()
	closed := c.done
	if req.Op == "close" {
		closed = nil
	}
	select {
	case err := <-done:
		if err != nil {
			return nil, err
		}
		return reply, reply.err()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-closed:
		return nil, net.ErrClosed
	}
}
func (c *remoteSocketConn) operation(ctx context.Context, req *DeviceSocketRequest) (*DeviceSocketResponse, error) {
	for {
		select {
		case <-c.done:
			return nil, net.ErrClosed
		case <-c.ctx.Done():
			return nil, net.ErrClosed
		default:
		}
		r, err := c.call(ctx, req)
		if err != nil || !r.Pending {
			return r, err
		}
		req = &DeviceSocketRequest{Op: req.Op}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-c.done:
			timer.Stop()
			return nil, net.ErrClosed
		case <-c.ctx.Done():
			timer.Stop()
			return nil, net.ErrClosed
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}
func (c *remoteSocketConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	r, err := c.operation(c.ctx, &DeviceSocketRequest{Op: "read", Start: true, Size: min(len(p), socketRPCMaxBytes)})
	if r == nil {
		return 0, err
	}
	c.updateAddresses(r)
	return copy(p, r.Data), err
}
func (c *remoteSocketConn) updateAddresses(r *DeviceSocketResponse) {
	if c.datagram && r.Remote != "" {
		local, localErr := socketRPCAddr("udp", r.Local)
		remote, remoteErr := socketRPCAddr("udp", r.Remote)
		if localErr == nil && remoteErr == nil {
			c.addrMu.Lock()
			c.local, c.remote = local, remote
			c.addrMu.Unlock()
		}
	}
}
func (c *remoteSocketConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.datagram && len(p) > socketRPCMaxBytes {
		return 0, fmt.Errorf("UDP datagram too large")
	}
	n := 0
	for {
		data := p[:min(len(p), socketRPCMaxBytes)]
		r, err := c.operation(c.ctx, &DeviceSocketRequest{Op: "write", Start: true, Data: append([]byte(nil), data...)})
		if r != nil {
			if r.N < 0 || r.N > len(data) {
				return n, errors.New("invalid socket write count")
			}
			c.updateAddresses(r)
			n += r.N
			p = p[r.N:]
		}
		if err != nil {
			return n, err
		}
		if r.N != len(data) {
			return n, io.ErrShortWrite
		}
		if len(p) == 0 {
			return n, nil
		}
	}
}
func (c *remoteSocketConn) LocalAddr() net.Addr {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	return c.local
}
func (c *remoteSocketConn) RemoteAddr() net.Addr {
	c.addrMu.Lock()
	defer c.addrMu.Unlock()
	return c.remote
}
func (c *remoteSocketConn) control(op string, deadline time.Time) error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	_, err := c.call(c.ctx, &DeviceSocketRequest{Op: op, Deadline: deadline})
	return err
}
func (c *remoteSocketConn) SetDeadline(t time.Time) error      { return c.control("deadline", t) }
func (c *remoteSocketConn) SetReadDeadline(t time.Time) error  { return c.control("readDeadline", t) }
func (c *remoteSocketConn) SetWriteDeadline(t time.Time) error { return c.control("writeDeadline", t) }
func (c *remoteSocketConn) CloseRead() error                   { return c.control("closeRead", time.Time{}) }
func (c *remoteSocketConn) CloseWrite() error                  { return c.control("closeWrite", time.Time{}) }
func (c *remoteSocketConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		c.ownerMu.Lock()
		if c.stopOwner != nil {
			c.stopOwner()
		}
		c.ownerMu.Unlock()
		// The session owns final cleanup even if cancellation or a transport
		// failure prevents this best-effort release from reaching the peer.
		err = c.closeRequest()
	})
	return err
}

func (c *remoteSocketConn) closeRequest() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.call(ctx, &DeviceSocketRequest{Op: "close"})
	return err
}
