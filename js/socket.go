//go:build js

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"

	webtransport "github.com/quic-go/webtransport-go"
	"github.com/urnetwork/sdk/v2026"
	_ "golang.org/x/crypto/x509roots/fallback"
)

// One dispatcher per device avoids retaining js.Func closures for each socket
// and stream. Handles are monotonically allocated and explicitly released.
type jsSocketHandles struct {
	mu      sync.Mutex
	next    int
	values  map[int]any
	parents map[int]int
	ctx     context.Context
	cancel  context.CancelFunc
}

func (h *jsSocketHandles) add(value any, parent ...int) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		closeJSResource(value)
		return nil, net.ErrClosed
	}
	if len(parent) != 0 && h.values[parent[0]] == nil {
		closeJSResource(value)
		return nil, net.ErrClosed
	}
	if len(h.values) >= 512 {
		closeJSResource(value)
		return nil, errors.New("socket/stream handle limit reached")
	}
	h.next++
	h.values[h.next] = value
	if len(parent) != 0 {
		h.parents[h.next] = parent[0]
	}
	m := map[string]any{"id": h.next}
	if c, ok := value.(net.Conn); ok {
		m["localAddr"], m["remoteAddr"] = c.LocalAddr().String(), c.RemoteAddr().String()
	}
	if s, ok := value.(*webtransport.Session); ok {
		m["protocol"] = s.SessionState().ApplicationProtocol
	}
	return m, nil
}
func closeJSResource(value any) {
	switch c := value.(type) {
	case *webtransport.Session:
		_ = c.CloseWithError(0, "")
	case *webtransport.Stream:
		c.CancelRead(0)
		c.CancelWrite(0)
	case *webtransport.ReceiveStream:
		c.CancelRead(0)
	case *webtransport.SendStream:
		c.CancelWrite(0)
	case io.Closer:
		_ = c.Close()
	}
}
func (h *jsSocketHandles) close() {
	h.cancel()
	h.mu.Lock()
	values := h.values
	h.values = make(map[int]any)
	h.parents = make(map[int]int)
	h.mu.Unlock()
	for _, v := range values {
		closeJSResource(v)
	}
}

func jsBindSocketDevice(device sdk.Device, m map[string]any) *jsSocketHandles {
	parent := context.Background()
	if d, ok := device.(interface{ Ctx() context.Context }); ok {
		parent = d.Ctx()
	}
	ctx, cancel := context.WithCancel(parent)
	h := &jsSocketHandles{values: make(map[int]any), parents: make(map[int]int), ctx: ctx, cancel: cancel}
	go func() { <-ctx.Done(); h.close() }()
	m["socketOperation"] = js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 3 {
			return jsRejected(errors.New("socketOperation requires an operation, handle, and argument"))
		}
		op, id, arg := args[0].String(), args[1].Int(), args[2]
		// Copy bytes before returning to JS: callers may mutate their view as
		// soon as write() returns its Promise.
		var data []byte
		if op == "write" || op == "sendDatagram" {
			if !arg.InstanceOf(js.Global().Get("Uint8Array")) {
				return jsRejected(errors.New("expected Uint8Array"))
			}
			if arg.Length() > 65535 {
				return jsRejected(errors.New("socket buffer exceeds 65535 bytes"))
			}
			data = make([]byte, arg.Length())
			js.CopyBytesToGo(data, arg)
		}
		return jsPromise(func(resolve func(any), reject func(error)) {
			value, err := h.operation(device, op, id, arg, data)
			if err != nil {
				reject(err)
			} else {
				resolve(value)
			}
		})
	})
	return h
}

func (h *jsSocketHandles) operation(device sdk.Device, op string, id int, arg js.Value, data []byte) (any, error) {
	if op == "dial" || op == "dialTls" || op == "webTransport" {
		var options struct {
			Network, Address string
			TimeoutMillis    int64
			TLS              *sdk.SocketTLSOptions
			Protocols        []string
			Hashes           [][]byte
		}
		if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", arg).String()), &options); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		if options.TimeoutMillis < 0 || options.TimeoutMillis > 2147483647 {
			return nil, errors.New("timeoutMillis must be between 0 and 2147483647")
		}
		if options.TimeoutMillis > 0 {
			var stop context.CancelFunc
			ctx, stop = context.WithTimeout(ctx, time.Duration(options.TimeoutMillis)*time.Millisecond)
			defer stop()
		}
		signal := arg.Get("signal")
		if !signal.IsUndefined() && !signal.IsNull() {
			if signal.Get("aborted").Bool() {
				return nil, context.Canceled
			}
			abort := js.FuncOf(func(js.Value, []js.Value) any { cancel(); return nil })
			signal.Call("addEventListener", "abort", abort)
			defer func() { signal.Call("removeEventListener", "abort", abort); abort.Release() }()
		}
		if op == "dial" {
			c, err := device.DialContext(ctx, options.Network, options.Address)
			if err != nil {
				return nil, err
			}
			return h.add(c)
		}
		config, err := options.TLS.TLSConfig()
		if err != nil {
			return nil, err
		}
		if op == "dialTls" {
			c, err := device.DialTlsContext(ctx, options.Network, options.Address, config)
			if err != nil {
				return nil, err
			}
			return h.add(c)
		}
		origin := ""
		if location := js.Global().Get("location"); !location.IsUndefined() {
			origin = location.Get("origin").String()
		}
		s, err := sdk.DialWebTransport(ctx, device, options.Address, &sdk.WebTransportOptions{TLSConfig: config, Origin: origin, Protocols: options.Protocols, ServerCertificateHashes: options.Hashes})
		if err != nil {
			return nil, err
		}
		return h.add(s)
	}
	h.mu.Lock()
	resource := h.values[id]
	var children []any
	if op == "release" {
		delete(h.values, id)
		delete(h.parents, id)
		for child, parent := range h.parents {
			if parent == id {
				children = append(children, h.values[child])
				delete(h.values, child)
				delete(h.parents, child)
			}
		}
	}
	h.mu.Unlock()
	if op == "release" {
		for _, child := range children {
			closeJSResource(child)
		}
		if arg.Type() == js.TypeString && arg.String() == "finished" {
			// Close() on a QUIC send stream queues FIN; delivery may still be
			// pending. Do not turn a graceful FIN into RESET_STREAM when JS
			// drops its finished handle. The session owns delivery/cleanup.
			switch resource.(type) {
			case *webtransport.Stream, *webtransport.SendStream, *webtransport.ReceiveStream:
				return nil, nil
			}
		}
		closeJSResource(resource)
		return nil, nil
	}
	if resource == nil {
		return nil, net.ErrClosed
	}
	if op == "read" {
		r, ok := resource.(io.Reader)
		if !ok {
			return nil, errors.New("resource is not readable")
		}
		size := arg.Int()
		if size < 1 || size > 65535 {
			return nil, errors.New("read size must be between 1 and 65535")
		}
		buf := make([]byte, size)
		n, err := r.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) && n == 0 {
			return nil, err
		}
		out := js.Global().Get("Uint8Array").New(n)
		js.CopyBytesToJS(out, buf[:n])
		result := map[string]any{"data": out, "eof": errors.Is(err, io.EOF)}
		if err != nil && !errors.Is(err, io.EOF) {
			result["error"] = err.Error()
		}
		return result, nil
	}
	if op == "write" {
		w, ok := resource.(io.Writer)
		if !ok {
			return nil, errors.New("resource is not writable")
		}
		n, err := w.Write(data)
		if err != nil {
			return map[string]any{"bytesWritten": n, "error": err.Error()}, nil
		}
		return n, nil
	}
	if conn, ok := resource.(net.Conn); ok {
		switch op {
		case "addresses":
			return map[string]any{"localAddr": conn.LocalAddr().String(), "remoteAddr": conn.RemoteAddr().String()}, nil
		case "deadline", "readDeadline", "writeDeadline":
			var deadline time.Time
			if arg.Float() != 0 {
				deadline = time.UnixMilli(int64(arg.Float()))
			}
			switch op {
			case "deadline":
				return nil, conn.SetDeadline(deadline)
			case "readDeadline":
				return nil, conn.SetReadDeadline(deadline)
			default:
				return nil, conn.SetWriteDeadline(deadline)
			}
		case "closeWrite":
			if c, ok := conn.(interface{ CloseWrite() error }); ok {
				return nil, c.CloseWrite()
			}
			return nil, errors.New("socket does not support CloseWrite")
		case "closeRead":
			if c, ok := conn.(interface{ CloseRead() error }); ok {
				return nil, c.CloseRead()
			}
			return nil, errors.New("socket does not support CloseRead")
		}
	}
	if session, ok := resource.(*webtransport.Session); ok {
		switch op {
		case "openBi":
			s, err := session.OpenStreamSync(h.ctx)
			if err != nil {
				return nil, err
			}
			return h.add(s, id)
		case "openUni":
			s, err := session.OpenUniStreamSync(h.ctx)
			if err != nil {
				return nil, err
			}
			return h.add(s, id)
		case "acceptBi":
			s, err := session.AcceptStream(h.ctx)
			if err != nil {
				var closed *webtransport.SessionError
				if errors.As(err, &closed) {
					return map[string]any{"closed": true}, nil
				}
				return nil, err
			}
			return h.add(s, id)
		case "acceptUni":
			s, err := session.AcceptUniStream(h.ctx)
			if err != nil {
				var closed *webtransport.SessionError
				if errors.As(err, &closed) {
					return map[string]any{"closed": true}, nil
				}
				return nil, err
			}
			return h.add(s, id)
		case "sendDatagram":
			return nil, session.SendDatagram(data)
		case "receiveDatagram":
			buf, err := session.ReceiveDatagram(h.ctx)
			if err != nil {
				var closed *webtransport.SessionError
				if errors.As(err, &closed) {
					return map[string]any{"closed": true}, nil
				}
				return nil, err
			}
			out := js.Global().Get("Uint8Array").New(len(buf))
			js.CopyBytesToJS(out, buf)
			return out, nil
		case "sessionClose":
			return nil, session.CloseWithError(webtransport.SessionErrorCode(arg.Get("closeCode").Int()), arg.Get("reason").String())
		case "sessionClosed":
			<-session.Context().Done()
			// v0.12 closes Context without a cause. A stream-open after closure
			// returns the stored session error, including the peer's code/reason.
			_, cause := session.OpenStream()
			var sessionError *webtransport.SessionError
			if errors.As(cause, &sessionError) {
				return map[string]any{"closeCode": uint32(sessionError.ErrorCode), "reason": sessionError.Message}, nil
			}
			return nil, cause
		}
	}
	switch op {
	case "closeWrite":
		if c, ok := resource.(interface{ Close() error }); ok {
			return nil, c.Close()
		}
	case "cancelWrite":
		if c, ok := resource.(interface {
			CancelWrite(webtransport.StreamErrorCode)
		}); ok {
			c.CancelWrite(webtransport.StreamErrorCode(arg.Int()))
			return nil, nil
		}
	case "cancelRead":
		if c, ok := resource.(interface {
			CancelRead(webtransport.StreamErrorCode)
		}); ok {
			c.CancelRead(webtransport.StreamErrorCode(arg.Int()))
			return nil, nil
		}
	}
	return nil, errors.New("unsupported socket operation")
}
