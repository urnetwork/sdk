package sdk

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// Socket exposes the same connection to bindings that cannot represent
// context.Context, net.Addr or time.Time. Use worker threads for blocking I/O.
type Socket struct{ conn net.Conn }

// SocketRead preserves an empty UDP datagram separately from TCP EOF.
type SocketRead struct {
	Data []byte
	Eof  bool
}

func (d *DeviceLocal) OpenSocket(network, address string, timeoutMillis int64, tlsOptions *SocketTLSOptions) (*Socket, error) {
	return openSocket(d, network, address, timeoutMillis, tlsOptions)
}
func (d *DeviceRemote) OpenSocket(network, address string, timeoutMillis int64, tlsOptions *SocketTLSOptions) (*Socket, error) {
	return openSocket(d, network, address, timeoutMillis, tlsOptions)
}
func openSocket(d interface {
	Dialer
	TLSDialer
}, network, address string, timeoutMillis int64, options *SocketTLSOptions) (*Socket, error) {
	if timeoutMillis < 0 || timeoutMillis > 2147483647 {
		return nil, errors.New("invalid socket timeout")
	}
	ctx := context.Background()
	if timeoutMillis > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMillis)*time.Millisecond)
		defer cancel()
	}
	var conn net.Conn
	var err error
	if options == nil {
		conn, err = d.DialContext(ctx, network, address)
	} else {
		config, configErr := options.TLSConfig()
		if configErr != nil {
			return nil, configErr
		}
		conn, err = d.DialTlsContext(ctx, network, address, config)
	}
	if err != nil {
		return nil, err
	}
	return &Socket{conn: conn}, nil
}

func (s *Socket) Read(maxBytes int) (*SocketRead, error) {
	if maxBytes < 1 || maxBytes > 65535 {
		return nil, errors.New("read size must be between 1 and 65535")
	}
	p := make([]byte, maxBytes)
	n, err := s.conn.Read(p)
	eof := errors.Is(err, io.EOF)
	if eof {
		err = nil
	}
	return &SocketRead{Data: p[:n], Eof: eof}, err
}
func (s *Socket) Write(data []byte) (int, error) { return s.conn.Write(data) }
func (s *Socket) GetLocalAddr() string           { return s.conn.LocalAddr().String() }
func (s *Socket) GetRemoteAddr() string          { return s.conn.RemoteAddr().String() }
func socketMillis(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.UnixMilli(t)
}
func (s *Socket) SetDeadlineMillis(t int64) error     { return s.conn.SetDeadline(socketMillis(t)) }
func (s *Socket) SetReadDeadlineMillis(t int64) error { return s.conn.SetReadDeadline(socketMillis(t)) }
func (s *Socket) SetWriteDeadlineMillis(t int64) error {
	return s.conn.SetWriteDeadline(socketMillis(t))
}
func (s *Socket) Close() error      { return s.conn.Close() }
func (s *Socket) CloseRead() error  { return socketHalfClose(s.conn, true) }
func (s *Socket) CloseWrite() error { return socketHalfClose(s.conn, false) }
func (o *SocketTLSOptions) SetNextProtos(protocols *StringList) {
	if protocols == nil {
		o.NextProtos = nil
	} else {
		o.NextProtos = append([]string(nil), protocols.getAll()...)
	}
}
