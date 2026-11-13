package sdk

import (
	"bufio"
	"encoding/gob"
	"errors"
	"io"
	"log"
	"sync"
)

// device_rpc_client.go -- the calling side of the device rpc.
//
// The device rpc is net/rpc's protocol: a gob header and a gob body per request
// and per response, over one connection. The protocol is unchanged and either
// side can be net/rpc, which is how the apps of a previous release and the
// fakes in the tests talk to this code. The implementation lives here because
// net/rpc can't be linked into a mobile binding at its size budget:
//
//   - net/rpc finds a service's methods with reflect.Type.Method. One reachable
//     call of that makes the linker keep every exported method of every type in
//     the program, about 7 MiB of the iOS extension
//     (build/APPLE_EXTENSION_MEMORY_BUDGET.md).
//   - its debug page keeps html/template and text/template, which are parsed at
//     process start.
//
// Everything else follows net/rpc/client.go, including the lock order
// (requestLock, then stateLock) and the error of each outcome. A client may be
// used by multiple goroutines at once. The read and write halves of the
// connection are serialized independently, so the connection must allow one
// concurrent reader and one concurrent writer.
//
// Derived from Go's src/net/rpc/client.go under this license:
//
// Copyright 2009 The Go Authors.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//   - Redistributions of source code must retain the above copyright
//     notice, this list of conditions and the following disclaimer.
//   - Redistributions in binary form must reproduce the above
//     copyright notice, this list of conditions and the following disclaimer
//     in the documentation and/or other materials provided with the
//     distribution.
//   - Neither the name of Google LLC nor the names of its
//     contributors may be used to endorse or promote products derived from
//     this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

// An error returned from the remote side of the connection.
type deviceRpcServerError string

func (self deviceRpcServerError) Error() string {
	return string(self)
}

var errDeviceRpcShutdown = errors.New("connection is shut down")

// An active call.
type deviceRpcCall struct {
	ServiceMethod string
	Args          any
	Reply         any
	// after completion, the status of the call
	Error error
	// receives the call on completion
	Done chan *deviceRpcCall
}

// done never blocks. The caller provides the buffer (see Go).
func (self *deviceRpcCall) done() {
	select {
	case self.Done <- self:
	default:
	}
}

type deviceRpcClient struct {
	conn          io.ReadWriteCloser
	decoder       *gob.Decoder
	encoder       *gob.Encoder
	encoderBuffer *bufio.Writer

	// serializes the requests, and is held for the whole of one
	requestLock sync.Mutex
	request     deviceRpcRequest

	stateLock       sync.Mutex
	seq             uint64
	pendingSeqCalls map[uint64]*deviceRpcCall
	closing         bool // Close was called
	shutdown        bool // the connection ended
}

// The buffer on the write side sends a header and its body as a unit.
func newDeviceRpcClient(conn io.ReadWriteCloser) *deviceRpcClient {
	encoderBuffer := bufio.NewWriter(conn)
	client := &deviceRpcClient{
		conn:            conn,
		decoder:         gob.NewDecoder(conn),
		encoder:         gob.NewEncoder(encoderBuffer),
		encoderBuffer:   encoderBuffer,
		pendingSeqCalls: map[uint64]*deviceRpcCall{},
	}
	go client.run()
	return client
}

func (self *deviceRpcClient) writeRequest(body any) error {
	if err := self.encoder.Encode(&self.request); err != nil {
		return err
	}
	if err := self.encoder.Encode(body); err != nil {
		return err
	}
	return self.encoderBuffer.Flush()
}

// A nil body reads the body and discards it.
func (self *deviceRpcClient) readResponseBody(body any) error {
	return self.decoder.Decode(body)
}

func (self *deviceRpcClient) send(call *deviceRpcCall) {
	self.requestLock.Lock()
	defer self.requestLock.Unlock()

	self.stateLock.Lock()
	if self.shutdown || self.closing {
		self.stateLock.Unlock()
		call.Error = errDeviceRpcShutdown
		call.done()
		return
	}
	seq := self.seq
	self.seq++
	self.pendingSeqCalls[seq] = call
	self.stateLock.Unlock()

	self.request.Seq = seq
	self.request.ServiceMethod = call.ServiceMethod
	if err := self.writeRequest(call.Args); err != nil {
		self.stateLock.Lock()
		call = self.pendingSeqCalls[seq]
		delete(self.pendingSeqCalls, seq)
		self.stateLock.Unlock()
		// the response loop may already have ended the call
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// The response loop: it ends with the connection, and takes every pending call
// with it.
func (self *deviceRpcClient) run() {
	var err error
	var response deviceRpcResponse
	for err == nil {
		response = deviceRpcResponse{}
		err = self.decoder.Decode(&response)
		if err != nil {
			break
		}
		seq := response.Seq
		self.stateLock.Lock()
		call := self.pendingSeqCalls[seq]
		delete(self.pendingSeqCalls, seq)
		self.stateLock.Unlock()

		switch {
		case call == nil:
			// no pending call: the request's write partly failed and already ended
			// it, and this response is the server's error about reading its body.
			// the error body must still be read, and no one is waiting for it
			err = self.readResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
		case response.Error != "":
			// any later call gets the error of this read, if there is one
			call.Error = deviceRpcServerError(response.Error)
			err = self.readResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
			call.done()
		default:
			err = self.readResponseBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}
	self.requestLock.Lock()
	defer self.requestLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.shutdown = true
	if err == io.EOF {
		if self.closing {
			err = errDeviceRpcShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for _, call := range self.pendingSeqCalls {
		call.Error = err
		call.done()
	}
}

// If the connection is already shutting down, this returns errDeviceRpcShutdown.
func (self *deviceRpcClient) Close() error {
	self.stateLock.Lock()
	if self.closing {
		self.stateLock.Unlock()
		return errDeviceRpcShutdown
	}
	self.closing = true
	self.stateLock.Unlock()
	return self.conn.Close()
}

// Starts a call and returns without waiting for it. done receives the call on
// completion. It must have buffer for every simultaneous call that uses it, and
// an unbuffered channel panics here since a completion on it would be lost. nil
// allocates a channel.
func (self *deviceRpcClient) Go(serviceMethod string, args any, reply any, done chan *deviceRpcCall) *deviceRpcCall {
	if done == nil {
		done = make(chan *deviceRpcCall, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc: done channel is unbuffered")
	}
	call := &deviceRpcCall{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	self.send(call)
	return call
}

// Makes a call and waits for it to complete.
func (self *deviceRpcClient) Call(serviceMethod string, args any, reply any) error {
	call := <-self.Go(serviceMethod, args, reply, make(chan *deviceRpcCall, 1)).Done
	return call.Error
}
