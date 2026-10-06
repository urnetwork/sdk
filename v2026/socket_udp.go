package sdk

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Plain UDP has no connect handshake. For a dual-stack hostname the first
// datagram is sent over IPv6, then over IPv4 after 250 ms without a reply.
// The first replying peer wins permanently. Only that initial datagram may be
// duplicated; later writes wait for selection and are sent once.
func dialHappyUDP(ctx context.Context, network, address string, dial connect.DialContextFunction) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
		i   int
	}
	results := make(chan result)
	for i, family := range []string{network + "6", network + "4"} {
		go func() {
			c, err := dial(ctx, family, address)
			select {
			case results <- result{c, err, i}:
			case <-ctx.Done():
				if c != nil {
					_ = c.Close()
				}
			}
		}()
	}
	var candidates [2]net.Conn
	var lastErr error
	for range 2 {
		select {
		case <-ctx.Done():
			for _, c := range candidates {
				if c != nil {
					_ = c.Close()
				}
			}
			return nil, ctx.Err()
		case r := <-results:
			candidates[r.i] = r.c
			if r.err != nil {
				lastErr = r.err
			}
		}
	}
	if candidates[0] == nil {
		return candidates[1], lastErrIfNil(candidates[1], lastErr)
	}
	if candidates[1] == nil {
		return candidates[0], nil
	}
	return newHappyUDPConn(candidates[0], candidates[1]), nil
}
func lastErrIfNil(c net.Conn, err error) error {
	if c != nil {
		return nil
	}
	return err
}

type happyUDPConn struct {
	mu, readMu, writeMu         sync.Mutex
	candidates                  [2]net.Conn
	winner                      net.Conn
	first                       []byte
	hasFirst, started, closed   bool
	terminal                    error
	readDeadline, writeDeadline time.Time
	changed                     chan struct{}
	selected                    chan struct{}
	selectOnce                  sync.Once
}

func newHappyUDPConn(v6, v4 net.Conn) *happyUDPConn {
	return &happyUDPConn{candidates: [2]net.Conn{v6, v4}, changed: make(chan struct{}), selected: make(chan struct{})}
}
func (c *happyUDPConn) notify() { close(c.changed); c.changed = make(chan struct{}) }

func (c *happyUDPConn) wait(read bool) (net.Conn, error) {
	for {
		c.mu.Lock()
		deadline := c.writeDeadline
		if read {
			deadline = c.readDeadline
		}
		winner, closed, terminal, changed := c.winner, c.closed, c.terminal, c.changed
		c.mu.Unlock()
		if closed {
			return nil, net.ErrClosed
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, os.ErrDeadlineExceeded
		}
		if terminal != nil {
			return nil, terminal
		}
		if winner != nil {
			return winner, nil
		}
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-changed:
		case <-timeout:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (c *happyUDPConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	winner, err := c.wait(true)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	if c.hasFirst {
		n := copy(p, c.first)
		c.first, c.hasFirst = nil, false
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return winner.Read(p)
}

func (c *happyUDPConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	started := c.started
	c.mu.Unlock()
	if started {
		winner, err := c.wait(false)
		if err != nil {
			return 0, err
		}
		return winner.Write(p)
	}
	initial := append([]byte(nil), p...)
	firstIndex := 0
	n, err := c.candidates[0].Write(initial)
	if err != nil {
		n, err = c.candidates[1].Write(initial)
		if err != nil {
			return n, err
		}
		firstIndex = 1
	}
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	go c.race(initial, firstIndex)
	return n, nil
}

func (c *happyUDPConn) race(initial []byte, firstIndex int) {
	type result struct {
		i    int
		data []byte
		err  error
	}
	results := make(chan result, 3)
	for i, conn := range c.candidates {
		go func() {
			p := make([]byte, 65535)
			n, err := conn.Read(p)
			results <- result{i, p[:n], err}
		}()
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	var failed [2]bool
	fallback := firstIndex == 1
	startFallback := func() {
		if fallback {
			return
		}
		fallback = true
		// UDP flow control must not hold up selection of a reply already
		// arriving on the other family. Closing the loser also unblocks Write.
		go func() {
			if _, err := c.candidates[1].Write(initial); err != nil {
				results <- result{i: 1, err: err}
			}
		}()
	}
	for {
		select {
		case <-c.selected:
			return
		case <-timer.C:
			startFallback()
		case r := <-results:
			if r.err != nil {
				failed[r.i] = true
				if r.i == 0 {
					startFallback()
				}
				if !failed[0] || !failed[1] {
					continue
				}
			}
			c.mu.Lock()
			if !c.closed {
				if r.err == nil {
					c.winner, c.first, c.hasFirst = c.candidates[r.i], r.data, true
					_ = c.winner.SetReadDeadline(c.readDeadline)
				} else {
					c.terminal = r.err
				}
				c.notify()
				c.selectOnce.Do(func() { close(c.selected) })
			}
			c.mu.Unlock()
			_ = c.candidates[1-r.i].Close()
			return
		}
	}
}

func (c *happyUDPConn) LocalAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.winner != nil {
		return c.winner.LocalAddr()
	}
	return c.candidates[0].LocalAddr()
}
func (c *happyUDPConn) RemoteAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.winner != nil {
		return c.winner.RemoteAddr()
	}
	return c.candidates[0].RemoteAddr()
}
func (c *happyUDPConn) SetDeadline(t time.Time) error      { return c.setDeadline(t, true, true) }
func (c *happyUDPConn) SetReadDeadline(t time.Time) error  { return c.setDeadline(t, true, false) }
func (c *happyUDPConn) SetWriteDeadline(t time.Time) error { return c.setDeadline(t, false, true) }
func (c *happyUDPConn) setDeadline(t time.Time, read, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if read {
		c.readDeadline = t
		if c.winner != nil {
			_ = c.winner.SetReadDeadline(t)
		}
	}
	if write {
		c.writeDeadline = t
		if c.winner != nil {
			_ = c.winner.SetWriteDeadline(t)
		} else {
			for _, conn := range c.candidates {
				_ = conn.SetWriteDeadline(t)
			}
		}
	}
	c.notify()
	return nil
}
func (c *happyUDPConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.first = nil
	c.notify()
	c.selectOnce.Do(func() { close(c.selected) })
	c.mu.Unlock()
	return errors.Join(c.candidates[0].Close(), c.candidates[1].Close())
}
