package sdk

import (
	"go/token"
	"net"
	"net/rpc"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The methods net/rpc's Register serves for a receiver type: its suitableMethods,
// which is what the device rpc served before the tables, and what a peer of a
// previous release expects.
func testingDeviceRpcSuitableMethods(receiverType reflect.Type) map[string]reflect.Method {
	isExportedOrBuiltin := func(t reflect.Type) bool {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		return token.IsExported(t.Name()) || t.PkgPath() == ""
	}
	suitableMethods := map[string]reflect.Method{}
	for i := range receiverType.NumMethod() {
		method := receiverType.Method(i)
		methodType := method.Type
		if method.IsExported() && methodType.NumIn() == 3 &&
			isExportedOrBuiltin(methodType.In(1)) &&
			methodType.In(2).Kind() == reflect.Pointer && isExportedOrBuiltin(methodType.In(2)) &&
			methodType.NumOut() == 1 && methodType.Out(0) == reflect.TypeFor[error]() {
			suitableMethods[method.Name] = method
		}
	}
	return suitableMethods
}

// A method missing from a table answers "can't find method", which a caller of an
// optional method takes for an older peer and says nothing about, so every table
// must hold exactly the methods of its type, each under its own name.
func TestDeviceRpcMethodTablesServeEveryMethod(t *testing.T) {
	for _, service := range []struct {
		name         string
		receiverType reflect.Type
		methods      map[string]*deviceRpcMethod
	}{
		{"DeviceLocalRpc", reflect.TypeFor[*DeviceLocalRpc](), deviceLocalRpcMethods()},
		{"DeviceRemoteRpc", reflect.TypeFor[*DeviceRemoteRpc](), deviceRemoteRpcMethods()},
	} {
		t.Run(service.name, func(t *testing.T) {
			suitableMethods := testingDeviceRpcSuitableMethods(service.receiverType)
			if len(suitableMethods) == 0 {
				t.Fatal("the type has no suitable methods, so this test reads nothing")
			}
			for methodName, suitableMethod := range suitableMethods {
				method, ok := service.methods[methodName]
				if !ok {
					t.Errorf("%s.%s isn't served: add it to device_rpc_methods.go", service.name, methodName)
					continue
				}
				if method.function.Type() != suitableMethod.Type {
					t.Errorf("%s.%s is a %s, want %s", service.name, methodName, method.function.Type(), suitableMethod.Type)
				}
				if method.argType != suitableMethod.Type.In(1) || method.replyType != suitableMethod.Type.In(2) {
					t.Errorf("%s.%s decodes %s and %s", service.name, methodName, method.argType, method.replyType)
				}
				functionName := runtime.FuncForPC(method.function.Pointer()).Name()
				if !strings.HasSuffix(functionName, ".(*"+service.name+")."+methodName) {
					t.Errorf("%s.%s calls %s", service.name, methodName, functionName)
				}
			}
			for methodName := range service.methods {
				if _, ok := suitableMethods[methodName]; !ok {
					t.Errorf("%s.%s is served but isn't a suitable method of the type", service.name, methodName)
				}
			}
		})
	}
}

// The receiver below is a test double of the rpc, so these types are exported for
// net/rpc's Register.
type TestingDeviceRpcWireArgs struct {
	Value int
}

type TestingDeviceRpcWireReply struct {
	Value int
}

type testingDeviceRpcWireService struct {
	entered chan struct{}
	release chan struct{}
}

// The argument by pointer, and a struct reply.
func (self *testingDeviceRpcWireService) Pointer(args *TestingDeviceRpcWireArgs, reply *TestingDeviceRpcWireReply) error {
	reply.Value = args.Value + 1
	return nil
}

// The argument by value, and a pointer reply as the sdk's rpc replies are.
func (self *testingDeviceRpcWireService) Value(args TestingDeviceRpcWireArgs, reply **TestingDeviceRpcWireReply) error {
	*reply = &TestingDeviceRpcWireReply{Value: args.Value + 2}
	return nil
}

// A reply the server must allocate before the call.
func (self *testingDeviceRpcWireService) Map(args int, reply *map[string]int) error {
	(*reply)["value"] = args
	return nil
}

// A reply the server must allocate before the call.
func (self *testingDeviceRpcWireService) Slice(args int, reply *[]int) error {
	*reply = append(*reply, args)
	return nil
}

// The sdk's void shape: the reply is an untyped nil.
func (self *testingDeviceRpcWireService) Void(args bool, reply RpcVoid) error {
	return nil
}

func (self *testingDeviceRpcWireService) Fail(args int, reply *TestingDeviceRpcWireReply) error {
	reply.Value = args
	return &testingDeviceRpcWireError{}
}

// Waits for Release, which only a concurrent call can do.
func (self *testingDeviceRpcWireService) Block(args int, reply *TestingDeviceRpcWireReply) error {
	close(self.entered)
	<-self.release
	reply.Value = args
	return nil
}

func (self *testingDeviceRpcWireService) Release(args int, reply *TestingDeviceRpcWireReply) error {
	close(self.release)
	reply.Value = args
	return nil
}

type testingDeviceRpcWireError struct{}

func (self *testingDeviceRpcWireError) Error() string {
	return "the method failed"
}

func testingDeviceRpcWireMethods() map[string]*deviceRpcMethod {
	return newDeviceRpcMethods(map[string]any{
		"Pointer": (*testingDeviceRpcWireService).Pointer,
		"Value":   (*testingDeviceRpcWireService).Value,
		"Map":     (*testingDeviceRpcWireService).Map,
		"Slice":   (*testingDeviceRpcWireService).Slice,
		"Void":    (*testingDeviceRpcWireService).Void,
		"Fail":    (*testingDeviceRpcWireService).Fail,
		"Block":   (*testingDeviceRpcWireService).Block,
		"Release": (*testingDeviceRpcWireService).Release,
	})
}

func testingNewDeviceRpcWireService() *testingDeviceRpcWireService {
	return &testingDeviceRpcWireService{entered: make(chan struct{}), release: make(chan struct{})}
}

// The two client implementations, behind what a test needs of them.
type testingDeviceRpcCaller interface {
	Call(serviceMethod string, args any, reply any) error
	Close() error
}

// Serves the service on the connection until it closes, by one of the three ways
// to serve. Each is what a real peer is: a previous release, the reverse rpc, and
// the forward rpc's run.
var testingDeviceRpcServers = []struct {
	name  string
	serve func(t *testing.T, conn net.Conn, service *testingDeviceRpcWireService)
}{
	{"net_rpc_serve_conn", func(t *testing.T, conn net.Conn, service *testingDeviceRpcWireService) {
		server := rpc.NewServer()
		if err := server.RegisterName("Wire", service); err != nil {
			t.Error(err)
			return
		}
		server.ServeConn(conn)
	}},
	{"device_rpc_serve_conn", func(t *testing.T, conn net.Conn, service *testingDeviceRpcWireService) {
		newDeviceRpcServer("Wire", service, testingDeviceRpcWireMethods()).ServeConn(conn)
	}},
	{"device_rpc_serve_request", func(t *testing.T, conn net.Conn, service *testingDeviceRpcWireService) {
		server := newDeviceRpcServer("Wire", service, testingDeviceRpcWireMethods())
		codec := newGobServerCodec(conn)
		for {
			err := server.ServeRequest(codec)
			// as run: the session outlives a failed lookup and ends with the connection
			if err != nil && !strings.HasPrefix(err.Error(), "rpc: ") {
				return
			}
			if err != nil && strings.HasPrefix(err.Error(), "rpc: server cannot decode request") {
				return
			}
		}
	}},
}

var testingDeviceRpcClients = []struct {
	name       string
	newCaller  func(conn net.Conn) testingDeviceRpcCaller
	serverText func(err error) (string, bool)
}{
	{"net_rpc_client", func(conn net.Conn) testingDeviceRpcCaller { return rpc.NewClient(conn) }, func(err error) (string, bool) {
		serverError, ok := err.(rpc.ServerError)
		return string(serverError), ok
	}},
	{"device_rpc_client", func(conn net.Conn) testingDeviceRpcCaller { return newDeviceRpcClient(conn) }, func(err error) (string, bool) {
		serverError, ok := err.(deviceRpcServerError)
		return string(serverError), ok
	}},
}

// Every client against every server, so the protocol is net/rpc's in both
// directions, which a peer of the previous release depends on, and each outcome
// reads the same from all of them.
func TestDeviceRpcSpeaksNetRpc(t *testing.T) {
	for _, server := range testingDeviceRpcServers {
		for _, client := range testingDeviceRpcClients {
			t.Run(client.name+"_to_"+server.name, func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				defer serverConn.Close()
				served := make(chan struct{})
				go func() {
					defer close(served)
					server.serve(t, serverConn, testingNewDeviceRpcWireService())
				}()
				caller := client.newCaller(clientConn)

				pointerReply := new(TestingDeviceRpcWireReply)
				if err := caller.Call("Wire.Pointer", &TestingDeviceRpcWireArgs{Value: 1}, pointerReply); err != nil || pointerReply.Value != 2 {
					t.Errorf("Pointer: %v, %d", err, pointerReply.Value)
				}

				var valueReply *TestingDeviceRpcWireReply
				if err := caller.Call("Wire.Value", TestingDeviceRpcWireArgs{Value: 1}, &valueReply); err != nil || valueReply == nil || valueReply.Value != 3 {
					t.Errorf("Value: %v, %v", err, valueReply)
				}

				var mapReply map[string]int
				if err := caller.Call("Wire.Map", 4, &mapReply); err != nil || mapReply["value"] != 4 {
					t.Errorf("Map: %v, %v", err, mapReply)
				}

				var sliceReply []int
				if err := caller.Call("Wire.Slice", 5, &sliceReply); err != nil || !slices.Equal(sliceReply, []int{5}) {
					t.Errorf("Slice: %v, %v", err, sliceReply)
				}

				var void RpcVoid
				if err := caller.Call("Wire.Void", true, &void); err != nil {
					t.Errorf("Void: %v", err)
				}

				for _, failure := range []struct {
					serviceMethod string
					text          string
				}{
					{"Wire.Fail", "the method failed"},
					// rpcMissingMethodError and DeviceLocalRpc.run read this prefix
					{"Wire.Missing", "rpc: can't find method Wire.Missing"},
					{"Other.Pointer", "rpc: can't find service Other.Pointer"},
					{"NoDot", "rpc: service/method request ill-formed: NoDot"},
				} {
					reply := new(TestingDeviceRpcWireReply)
					err := caller.Call(failure.serviceMethod, 1, reply)
					text, ok := client.serverText(err)
					if !ok || text != failure.text {
						t.Errorf("%s: %T %v", failure.serviceMethod, err, err)
					}
					if failure.serviceMethod == "Wire.Fail" && reply.Value != 0 {
						t.Errorf("%s: the reply of a failure carries %d", failure.serviceMethod, reply.Value)
					}
				}

				// the session outlives every failure above
				if err := caller.Call("Wire.Pointer", &TestingDeviceRpcWireArgs{Value: 6}, pointerReply); err != nil || pointerReply.Value != 7 {
					t.Errorf("Pointer after the failures: %v, %d", err, pointerReply.Value)
				}

				if err := caller.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				<-served
			})
		}
	}
}

// The reverse rpc serves each call on its own goroutine: a call that waits for
// another call, as a slow listener does, doesn't hold up the connection.
func TestDeviceRpcServeConnServesCallsConcurrently(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	service := testingNewDeviceRpcWireService()
	served := make(chan struct{})
	go func() {
		defer close(served)
		newDeviceRpcServer("Wire", service, testingDeviceRpcWireMethods()).ServeConn(serverConn)
	}()
	client := newDeviceRpcClient(clientConn)

	blockReply := new(TestingDeviceRpcWireReply)
	blocked := client.Go("Wire.Block", 1, blockReply, nil)
	<-service.entered

	// finishes only because the server took this while Block is still running
	releaseReply := new(TestingDeviceRpcWireReply)
	if err := client.Call("Wire.Release", 2, releaseReply); err != nil || releaseReply.Value != 2 {
		t.Fatalf("Release: %v, %d", err, releaseReply.Value)
	}
	call := <-blocked.Done
	if call.Error != nil || blockReply.Value != 1 {
		t.Fatalf("Block: %v, %d", call.Error, blockReply.Value)
	}

	// ServeConn closes the connection after the last response
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-served
}

// A pending call ends with the error net/rpc gives it when the connection does,
// which deviceRpcAttemptErrorResult reads as "eof".
func TestDeviceRpcClientEndsPendingCallsWithTheConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	service := testingNewDeviceRpcWireService()
	served := make(chan struct{})
	go func() {
		defer close(served)
		newDeviceRpcServer("Wire", service, testingDeviceRpcWireMethods()).ServeConn(serverConn)
	}()
	client := newDeviceRpcClient(clientConn)
	reply := new(TestingDeviceRpcWireReply)
	blocked := client.Go("Wire.Block", 1, reply, nil)
	<-service.entered

	serverConn.Close()
	call := <-blocked.Done
	if call.Error == nil || deviceRpcAttemptErrorResult(call.Error) != "eof" {
		t.Fatalf("the pending call ended with %v", call.Error)
	}

	// the connection is gone for every later call
	if err := client.Call("Wire.Pointer", &TestingDeviceRpcWireArgs{}, reply); err != errDeviceRpcShutdown {
		t.Fatalf("a call after the connection ended: %v", err)
	}

	close(service.release)
	<-served
	client.Close()
}

func TestDeviceRpcClientCloseEndsItOnce(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	client := newDeviceRpcClient(clientConn)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != errDeviceRpcShutdown {
		t.Fatalf("the second Close: %v", err)
	}
	var void RpcVoid
	if err := client.Call("Wire.Void", true, &void); err != errDeviceRpcShutdown {
		t.Fatalf("a call after Close: %v", err)
	}
}

// A function that isn't a method of the rpc's shape is a defect in the table.
func TestDeviceRpcMethodsRefuseAnotherShape(t *testing.T) {
	for name, methodFunction := range map[string]any{
		"not_a_function": 1,
		"no_arg":         func(*testingDeviceRpcWireService, *int) error { return nil },
		"reply_by_value": func(*testingDeviceRpcWireService, int, int) error { return nil },
		"no_error":       func(*testingDeviceRpcWireService, int, *int) int { return 0 },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("the table was accepted")
				}
			}()
			newDeviceRpcMethods(map[string]any{"Method": methodFunction})
		})
	}
}
