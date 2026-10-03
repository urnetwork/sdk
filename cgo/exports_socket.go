package main

/*
#include <stdint.h>
#include <stdbool.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
	"unsafe"

	"github.com/urnetwork/sdk"
)

type socketHandle struct {
	net.Conn
	once sync.Once
}

func (s *socketHandle) Close() error {
	var err error
	s.once.Do(func() { err = s.Conn.Close() })
	return err
}
func (s *socketHandle) releaseHandle() { _ = s.Close() }

func cDialSocket(self C.uint64_t, network, address *C.char, timeout C.int64_t, tlsJSON *C.char, secure bool, outError **C.char) C.uint64_t {
	if outError != nil {
		*outError = nil
	}
	d, ok := resolveHandle[sdk.Device](uint64(self), "device_dial")
	if !ok {
		setErrorOut(outError, errors.New("invalid device handle"))
		return 0
	}
	if timeout < 0 || timeout > 2147483647 {
		setErrorOut(outError, errors.New("invalid dial timeout"))
		return 0
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
		defer cancel()
	}
	var conn net.Conn
	var err error
	if secure {
		var options sdk.SocketTLSOptions
		if s := goString(tlsJSON); s != "" {
			err = json.Unmarshal([]byte(s), &options)
		}
		if err == nil {
			config, configErr := options.TLSConfig()
			err = configErr
			if err == nil {
				conn, err = d.DialTlsContext(ctx, goString(network), goString(address), config)
			}
		}
	} else {
		conn, err = d.DialContext(ctx, goString(network), goString(address))
	}
	if err != nil {
		setErrorOut(outError, err)
		return 0
	}
	return C.uint64_t(newHandle(&socketHandle{Conn: conn}))
}

//export urnet_device_dial
func urnet_device_dial(self C.uint64_t, network, address *C.char, timeout C.int64_t, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_device_dial")
	return cDialSocket(self, network, address, timeout, nil, false, outError)
}

//export urnet_device_dial_tls
func urnet_device_dial_tls(self C.uint64_t, network, address *C.char, timeout C.int64_t, tlsJSON *C.char, outError **C.char) C.uint64_t {
	defer cgoGuard("urnet_device_dial_tls")
	return cDialSocket(self, network, address, timeout, tlsJSON, true, outError)
}

//export urnet_conn_read
func urnet_conn_read(self C.uint64_t, out *C.uint8_t, capacity C.int32_t, eof *C.bool, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_conn_read")
	if outError != nil {
		*outError = nil
	}
	if eof != nil {
		*eof = C.bool(false)
	}
	c, ok := resolveHandle[*socketHandle](uint64(self), "conn_read")
	if !ok || capacity < 0 || capacity > 16<<20 || (capacity > 0 && out == nil) {
		setErrorOut(outError, errors.New("invalid socket handle or read buffer"))
		return -1
	}
	if capacity == 0 {
		return 0
	}
	p := make([]byte, int(capacity))
	n, err := c.Read(p)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(out)), int(capacity)), p[:n])
	if errors.Is(err, io.EOF) {
		if eof != nil {
			*eof = C.bool(true)
		}
		err = nil
	}
	setErrorOut(outError, err)
	if n == 0 && err != nil {
		return -1
	}
	return C.int32_t(n)
}

//export urnet_conn_write
func urnet_conn_write(self C.uint64_t, data *C.uint8_t, length C.int32_t, outError **C.char) C.int32_t {
	defer cgoGuard("urnet_conn_write")
	if outError != nil {
		*outError = nil
	}
	c, ok := resolveHandle[*socketHandle](uint64(self), "conn_write")
	if !ok || length < 0 || length > 16<<20 || (length > 0 && data == nil) {
		setErrorOut(outError, errors.New("invalid socket handle or write buffer"))
		return -1
	}
	n, err := c.Write(goBytes(data, length))
	setErrorOut(outError, err)
	if n == 0 && err != nil {
		return -1
	}
	return C.int32_t(n)
}

func cSocketControl(self C.uint64_t, op string, millis C.int64_t, outError **C.char) C.bool {
	if outError != nil {
		*outError = nil
	}
	c, ok := resolveHandle[*socketHandle](uint64(self), op)
	if !ok {
		setErrorOut(outError, errors.New("invalid socket handle"))
		return C.bool(false)
	}
	var deadline time.Time
	if millis != 0 {
		deadline = time.UnixMilli(int64(millis))
	}
	var err error
	switch op {
	case "deadline":
		err = c.SetDeadline(deadline)
	case "readDeadline":
		err = c.SetReadDeadline(deadline)
	case "writeDeadline":
		err = c.SetWriteDeadline(deadline)
	case "close":
		err = c.Close()
	case "closeRead":
		if half, ok := c.Conn.(interface{ CloseRead() error }); ok {
			err = half.CloseRead()
		} else {
			err = errors.New("half-close unsupported")
		}
	case "closeWrite":
		if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
			err = half.CloseWrite()
		} else {
			err = errors.New("half-close unsupported")
		}
	}
	setErrorOut(outError, err)
	return C.bool(err == nil)
}

//export urnet_conn_set_deadline
func urnet_conn_set_deadline(self C.uint64_t, millis C.int64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_set_deadline")
	return cSocketControl(self, "deadline", millis, outError)
}

//export urnet_conn_set_read_deadline
func urnet_conn_set_read_deadline(self C.uint64_t, millis C.int64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_set_read_deadline")
	return cSocketControl(self, "readDeadline", millis, outError)
}

//export urnet_conn_set_write_deadline
func urnet_conn_set_write_deadline(self C.uint64_t, millis C.int64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_set_write_deadline")
	return cSocketControl(self, "writeDeadline", millis, outError)
}

//export urnet_conn_close
func urnet_conn_close(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_close")
	return cSocketControl(self, "close", 0, outError)
}

//export urnet_conn_close_read
func urnet_conn_close_read(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_close_read")
	return cSocketControl(self, "closeRead", 0, outError)
}

//export urnet_conn_close_write
func urnet_conn_close_write(self C.uint64_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_conn_close_write")
	return cSocketControl(self, "closeWrite", 0, outError)
}

//export urnet_conn_local_addr
func urnet_conn_local_addr(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_conn_local_addr")
	c, ok := resolveHandle[*socketHandle](uint64(self), "local_addr")
	if !ok {
		return nil
	}
	return cString(c.LocalAddr().String())
}

//export urnet_conn_remote_addr
func urnet_conn_remote_addr(self C.uint64_t) *C.char {
	defer cgoGuard("urnet_conn_remote_addr")
	c, ok := resolveHandle[*socketHandle](uint64(self), "remote_addr")
	if !ok {
		return nil
	}
	return cString(c.RemoteAddr().String())
}
