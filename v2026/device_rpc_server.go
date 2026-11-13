package sdk

import (
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
)

// device_rpc_server.go -- the serving side of the device rpc. See
// device_rpc_client.go for why this isn't net/rpc.
//
// One difference from net/rpc/server.go: a service's methods come from a table
// (device_rpc_methods.go) and not from walking the receiver's type with
// reflect.Type.Method, and the table is all the linker needs to keep them. A
// method is a method expression, the same function reflect would have found, so
// the rest of a call follows net/rpc: arguments are decoded and the method is
// invoked with reflect.Value.Call, the error texts are the same, and a panic in
// a method isn't recovered here. (DeviceLocalRpc.run recovers around
// ServeRequest.)
//
// A server may be used by multiple goroutines at once.
//
// Derived from Go's src/net/rpc/server.go under this license:
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

// The header before every request.
type deviceRpcRequest struct {
	// "Service.Method"
	ServiceMethod string
	// chosen by the client
	Seq uint64
	// free list in the server
	next *deviceRpcRequest
}

// The header before every response.
type deviceRpcResponse struct {
	// echoes the request
	ServiceMethod string
	// echoes the request
	Seq   uint64
	Error string
	// free list in the server
	next *deviceRpcResponse
}

// The placeholder body of a response that has an error. A client doesn't decode
// it into anything.
var deviceRpcInvalidRequest = struct{}{}

// One method of a service.
type deviceRpcMethod struct {
	function  reflect.Value
	argType   reflect.Type
	replyType reflect.Type
}

// Each function must be a method expression, func(receiver, arg, *reply) error.
// A function of another shape is a defect in the table, so this panics, and
// TestDeviceRpcMethodTablesServeEveryMethod runs every table through this.
func newDeviceRpcMethods(methodFunctions map[string]any) map[string]*deviceRpcMethod {
	methods := make(map[string]*deviceRpcMethod, len(methodFunctions))
	for methodName, methodFunction := range methodFunctions {
		function := reflect.ValueOf(methodFunction)
		functionType := function.Type()
		if functionType.Kind() != reflect.Func || functionType.NumIn() != 3 || functionType.NumOut() != 1 ||
			functionType.In(2).Kind() != reflect.Pointer || functionType.Out(0) != reflect.TypeFor[error]() {
			panic(fmt.Sprintf("rpc method %s is a %s", methodName, functionType))
		}
		methods[methodName] = &deviceRpcMethod{
			function:  function,
			argType:   functionType.In(1),
			replyType: functionType.In(2),
		}
	}
	return methods
}

// Serves one service. Make one per connection.
type deviceRpcServer struct {
	serviceName string
	receiver    reflect.Value
	methods     map[string]*deviceRpcMethod

	freeRequestLock sync.Mutex
	freeRequest     *deviceRpcRequest

	freeResponseLock sync.Mutex
	freeResponse     *deviceRpcResponse
}

func newDeviceRpcServer(serviceName string, receiver any, methods map[string]*deviceRpcMethod) *deviceRpcServer {
	return &deviceRpcServer{
		serviceName: serviceName,
		receiver:    reflect.ValueOf(receiver),
		methods:     methods,
	}
}

func (self *deviceRpcServer) sendResponse(sendLock *sync.Mutex, request *deviceRpcRequest, reply any, codec *gobServerCodec, errorMessage string) {
	response := self.getResponse()
	response.ServiceMethod = request.ServiceMethod
	if errorMessage != "" {
		response.Error = errorMessage
		reply = deviceRpcInvalidRequest
	}
	response.Seq = request.Seq
	sendLock.Lock()
	// a failed write closes the codec, and the read of the next request reports it
	codec.WriteResponse(response, reply)
	sendLock.Unlock()
	self.putResponse(response)
}

func (self *deviceRpcServer) call(sendLock *sync.Mutex, method *deviceRpcMethod, request *deviceRpcRequest, argValue reflect.Value, replyValue reflect.Value, codec *gobServerCodec) {
	returnValues := method.function.Call([]reflect.Value{self.receiver, argValue, replyValue})
	errorMessage := ""
	if err := returnValues[0].Interface(); err != nil {
		errorMessage = err.(error).Error()
	}
	self.sendResponse(sendLock, request, replyValue.Interface(), codec, errorMessage)
	self.putRequest(request)
}

// Serves the connection until the client hangs up, each call on its own
// goroutine, and closes it after the last response.
func (self *deviceRpcServer) ServeConn(conn net.Conn) {
	codec := newGobServerCodec(conn)
	sendLock := new(sync.Mutex)
	calls := new(sync.WaitGroup)
	for {
		method, request, argValue, replyValue, keepReading, err := self.readRequest(codec)
		if err != nil {
			if !keepReading {
				break
			}
			if request != nil {
				self.sendResponse(sendLock, request, deviceRpcInvalidRequest, codec, err.Error())
				self.putRequest(request)
			}
			continue
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			self.call(sendLock, method, request, argValue, replyValue, codec)
		}()
	}
	calls.Wait()
	codec.Close()
}

// Serves one request and returns after its response. This leaves the codec open.
func (self *deviceRpcServer) ServeRequest(codec *gobServerCodec) error {
	sendLock := new(sync.Mutex)
	method, request, argValue, replyValue, keepReading, err := self.readRequest(codec)
	if err != nil {
		if !keepReading {
			return err
		}
		if request != nil {
			self.sendResponse(sendLock, request, deviceRpcInvalidRequest, codec, err.Error())
			self.putRequest(request)
		}
		return err
	}
	self.call(sendLock, method, request, argValue, replyValue, codec)
	return nil
}

func (self *deviceRpcServer) getRequest() *deviceRpcRequest {
	self.freeRequestLock.Lock()
	defer self.freeRequestLock.Unlock()
	request := self.freeRequest
	if request == nil {
		return new(deviceRpcRequest)
	}
	self.freeRequest = request.next
	*request = deviceRpcRequest{}
	return request
}

func (self *deviceRpcServer) putRequest(request *deviceRpcRequest) {
	self.freeRequestLock.Lock()
	defer self.freeRequestLock.Unlock()
	request.next = self.freeRequest
	self.freeRequest = request
}

func (self *deviceRpcServer) getResponse() *deviceRpcResponse {
	self.freeResponseLock.Lock()
	defer self.freeResponseLock.Unlock()
	response := self.freeResponse
	if response == nil {
		return new(deviceRpcResponse)
	}
	self.freeResponse = response.next
	*response = deviceRpcResponse{}
	return response
}

func (self *deviceRpcServer) putResponse(response *deviceRpcResponse) {
	self.freeResponseLock.Lock()
	defer self.freeResponseLock.Unlock()
	response.next = self.freeResponse
	self.freeResponse = response
}

func (self *deviceRpcServer) readRequest(codec *gobServerCodec) (method *deviceRpcMethod, request *deviceRpcRequest, argValue reflect.Value, replyValue reflect.Value, keepReading bool, err error) {
	method, request, keepReading, err = self.readRequestHeader(codec)
	if err != nil {
		if !keepReading {
			return
		}
		// discard the body
		codec.ReadRequestBody(nil)
		return
	}

	// argValue is a pointer for the decode, and the method may take the value
	argIsValue := false
	if method.argType.Kind() == reflect.Pointer {
		argValue = reflect.New(method.argType.Elem())
	} else {
		argValue = reflect.New(method.argType)
		argIsValue = true
	}
	if err = codec.ReadRequestBody(argValue.Interface()); err != nil {
		return
	}
	if argIsValue {
		argValue = argValue.Elem()
	}

	replyValue = reflect.New(method.replyType.Elem())
	switch method.replyType.Elem().Kind() {
	case reflect.Map:
		replyValue.Elem().Set(reflect.MakeMap(method.replyType.Elem()))
	case reflect.Slice:
		replyValue.Elem().Set(reflect.MakeSlice(method.replyType.Elem(), 0, 0))
	}
	return
}

func (self *deviceRpcServer) readRequestHeader(codec *gobServerCodec) (method *deviceRpcMethod, request *deviceRpcRequest, keepReading bool, err error) {
	request = self.getRequest()
	err = codec.ReadRequestHeader(request)
	if err != nil {
		request = nil
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return
		}
		err = errors.New("rpc: server cannot decode request: " + err.Error())
		return
	}

	// after a read of the header, a failure leaves the next request readable
	keepReading = true

	dot := strings.LastIndex(request.ServiceMethod, ".")
	if dot < 0 {
		err = errors.New("rpc: service/method request ill-formed: " + request.ServiceMethod)
		return
	}
	if request.ServiceMethod[:dot] != self.serviceName {
		err = errors.New("rpc: can't find service " + request.ServiceMethod)
		return
	}
	method = self.methods[request.ServiceMethod[dot+1:]]
	if method == nil {
		err = errors.New("rpc: can't find method " + request.ServiceMethod)
	}
	return
}
