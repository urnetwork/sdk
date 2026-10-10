//go:build !sdk_mobile_bind

package sdk

import "net"

// Conn is net.Conn, including concurrent operations, absolute deadlines,
// partial I/O, EOF, and connected UDP datagrams. TCP supports optional
// CloseRead and CloseWrite. Mobile bindings use the portable Socket instead.
type Conn = net.Conn

// All native Go targets retain the standard and secure Device dial methods.
// Lowercase embedding does not seal Device: its methods remain exported.
type deviceSocketDialer interface {
	Dialer
	TLSDialer
}
