package main

import (
	"bytes"
	"io"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Go test files cannot import C. Reflection supplies the exact generated C
// scalar/pointer types while calling the real exported ABI entry points.
func socketABICall(fn any, args ...any) []reflect.Value {
	f := reflect.ValueOf(fn)
	in := make([]reflect.Value, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case reflect.Value:
			in[i] = v
		case nil:
			in[i] = reflect.Zero(f.Type().In(i))
		default:
			in[i] = reflect.ValueOf(a).Convert(f.Type().In(i))
		}
	}
	return f.Call(in)
}
func socketABIOut(fn any, index int) reflect.Value {
	return reflect.New(reflect.TypeOf(fn).In(index).Elem())
}
func socketABIBuffer(fn any, index int, p []byte) reflect.Value {
	if len(p) == 0 {
		return reflect.Zero(reflect.TypeOf(fn).In(index))
	}
	return reflect.NewAt(reflect.TypeOf(fn).In(index).Elem(), unsafe.Pointer(&p[0]))
}
func socketABIError(out reflect.Value) string {
	p := out.Elem()
	if p.IsNil() {
		return ""
	}
	defer socketABICall(urnet_free_string, p)
	var data []byte
	base := p.UnsafePointer()
	for i := 0; i < 4096; i++ {
		b := *(*byte)(unsafe.Add(base, i))
		if b == 0 {
			break
		}
		data = append(data, b)
	}
	return string(data)
}

func TestSocketABIRoundTripEOFAndRelease(t *testing.T) {
	a, b := net.Pipe()
	id := newHandle(&socketHandle{Conn: a})
	defer handleRelease(id)
	go func() {
		defer b.Close()
		p := make([]byte, 5)
		if _, err := io.ReadFull(b, p); err == nil {
			_, _ = b.Write(p)
		}
	}()
	errOut := socketABIOut(urnet_conn_write, 3)
	p := []byte("hello")
	written := socketABICall(urnet_conn_write, id, socketABIBuffer(urnet_conn_write, 1, p), len(p), errOut)[0].Int()
	runtime.KeepAlive(p)
	if err := socketABIError(errOut); err != "" || written != 5 {
		t.Fatal(written, err)
	}
	buf := make([]byte, 10)
	eof := socketABIOut(urnet_conn_read, 3)
	errOut = socketABIOut(urnet_conn_read, 4)
	read := socketABICall(urnet_conn_read, id, socketABIBuffer(urnet_conn_read, 1, buf), len(buf), eof, errOut)[0].Int()
	runtime.KeepAlive(buf)
	if err := socketABIError(errOut); err != "" || !bytes.Equal(buf[:read], p) || eof.Elem().Bool() {
		t.Fatal(read, err)
	}
	read = socketABICall(urnet_conn_read, id, socketABIBuffer(urnet_conn_read, 1, buf), len(buf), eof, errOut)[0].Int()
	if read != 0 || !eof.Elem().Bool() || socketABIError(errOut) != "" {
		t.Fatal("EOF encoding")
	}
	if !socketABICall(urnet_release, id)[0].Bool() {
		t.Fatal("release failed")
	}
	errOut = socketABIOut(urnet_conn_read, 4)
	if socketABICall(urnet_conn_read, id, nil, 1, eof, errOut)[0].Int() != -1 || socketABIError(errOut) == "" {
		t.Fatal("stale handle accepted")
	}
}

func TestSocketABIReleaseUnblocksAndDeadlines(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	id := newHandle(&socketHandle{Conn: a})
	defer handleRelease(id)
	errOut := socketABIOut(urnet_conn_set_read_deadline, 2)
	if !socketABICall(urnet_conn_set_read_deadline, id, time.Now().Add(10*time.Millisecond).UnixMilli(), errOut)[0].Bool() {
		t.Fatal(socketABIError(errOut))
	}
	buf := make([]byte, 1)
	readError := socketABIOut(urnet_conn_read, 4)
	if socketABICall(urnet_conn_read, id, socketABIBuffer(urnet_conn_read, 1, buf), 1, nil, readError)[0].Int() != -1 || socketABIError(readError) == "" {
		t.Fatal("deadline ignored")
	}
	if !socketABICall(urnet_conn_set_read_deadline, id, 0, errOut)[0].Bool() {
		t.Fatal("deadline reset failed")
	}
	done := make(chan int64, 1)
	go func() {
		done <- socketABICall(urnet_conn_read, id, socketABIBuffer(urnet_conn_read, 1, buf), 1, nil, nil)[0].Int()
	}()
	handleRelease(id)
	select {
	case size := <-done:
		if size != -1 {
			t.Fatal(size)
		}
	case <-time.After(time.Second):
		t.Fatal("release left read blocked")
	}
}

type socketABIPartialConn struct{ net.Conn }

func (*socketABIPartialConn) Read(p []byte) (int, error) {
	return copy(p, []byte("part")), io.ErrUnexpectedEOF
}
func (*socketABIPartialConn) Write(p []byte) (int, error) { return len(p) / 2, io.ErrShortWrite }
func (*socketABIPartialConn) Close() error                { return nil }

func TestSocketABIPartialResultsAndInvalidBuffers(t *testing.T) {
	id := newHandle(&socketHandle{Conn: &socketABIPartialConn{}})
	defer handleRelease(id)
	buf := make([]byte, 8)
	errOut := socketABIOut(urnet_conn_read, 4)
	if n := socketABICall(urnet_conn_read, id, socketABIBuffer(urnet_conn_read, 1, buf), 8, nil, errOut)[0].Int(); n != 4 || socketABIError(errOut) == "" {
		t.Fatal("partial read lost", n)
	}
	errOut = socketABIOut(urnet_conn_write, 3)
	if n := socketABICall(urnet_conn_write, id, socketABIBuffer(urnet_conn_write, 1, buf), 8, errOut)[0].Int(); n != 4 || socketABIError(errOut) == "" {
		t.Fatal("partial write lost", n)
	}
	for _, size := range []int{-1, 1, 17 << 20} {
		errOut = socketABIOut(urnet_conn_read, 4)
		if n := socketABICall(urnet_conn_read, id, nil, size, nil, errOut)[0].Int(); n != -1 || socketABIError(errOut) == "" {
			t.Fatal("invalid buffer accepted", size)
		}
	}
	if n := socketABICall(urnet_conn_read, id, nil, 0, nil, nil)[0].Int(); n != 0 {
		t.Fatal("zero capacity consumed data")
	}
}
