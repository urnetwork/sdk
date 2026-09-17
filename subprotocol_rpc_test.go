//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"net"
	"net/rpc"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func subprotocolTestRemote(t *testing.T, pair *subprotocolTestPair) (*DeviceRemote, *DeviceLocalRpc) {
	t.Helper()
	ctx, cancel := context.WithCancel(pair.ctx)
	registry := newTestDeviceLocalSubprotocols(ctx)
	registry.attach(pair.a)
	device := &DeviceLocal{ctx: ctx, clientId: pair.a.ClientId(), instanceId: connect.NewId(), settings: DefaultDeviceLocalSettings(), subprotocols: registry}
	local := &DeviceLocalRpc{ctx: ctx, cancel: cancel, deviceLocal: device}
	a, b := net.Pipe()
	server := rpc.NewServer()
	if err := server.RegisterName("DeviceLocalRpc", local); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		codec := newGobServerCodec(b)
		for server.ServeRequest(codec) == nil {
		}
	}()
	remote := &DeviceRemote{ctx: ctx, clientId: device.clientId, instanceId: device.instanceId, service: &rpcClient{ctx: ctx, client: rpc.NewClient(a), timeout: time.Second, closeClient: a.Close}}
	t.Cleanup(func() { cancel(); local.subprotocols.close(); a.Close(); b.Close(); <-done })
	return remote, local
}

func TestSubprotocolRPCMessagesQueryAndClose(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()
	remote, local := subprotocolTestRemote(t, pair)
	peer := newTestDeviceLocalSubprotocols(pair.ctx)
	peer.attach(pair.b)
	listener := newRecordingSubprotocolListener()
	sub, err := peer.enable(4096, listener)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	channel, err := remote.OpenSubprotocolContext(t.Context(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	ids, ok, err := channel.Query(t.Context(), newId(pair.b.ClientId()), 1000)
	if err != nil || !ok || !slices.Equal(ids, []int32{4096}) {
		t.Fatalf("query: %v %v %v", ids, ok, err)
	}
	for _, frame := range [][]byte{{0, 1, 255}, {}, {4, 3, 2, 1}} {
		original := slices.Clone(frame)
		ok, err = channel.Send(t.Context(), newId(pair.b.ClientId()), frame)
		if err != nil || !ok {
			t.Fatalf("send: %v %v", ok, err)
		}
		clear(frame)
		got := listener.wait(t, time.Second)
		if got.sourceId != pair.a.ClientId().String() || !slices.Equal(got.messageBytes, original) {
			t.Fatalf("send corrupted: %+v", got)
		}
		sendSubprotocolFrom(t, pair.b, pair.a, 4096, original)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		source, data, err := channel.Receive(ctx)
		cancel()
		if err != nil || source.String() != pair.b.ClientId().String() || !slices.Equal(data, original) {
			t.Fatalf("receive: %v %v %v", source, data, err)
		}
	}
	_, ok, err = channel.Query(t.Context(), NewId(), 10)
	if err != nil || ok {
		t.Fatalf("unanswered query must differ from empty support: %v %v", ok, err)
	}
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	if ids := local.deviceLocal.subprotocols.enabledIds(); len(ids) != 0 {
		t.Fatalf("registration leaked: %v", ids)
	}
	if _, _, err := channel.Receive(t.Context()); err == nil {
		t.Fatal("closed subscription accepted receive")
	}
}

func TestSubprotocolRPCQueueCopiesAndFailsOverflow(t *testing.T) {
	e := &subprotocolRpcEntry{}
	input := []byte{0, 255, 8}
	e.SubprotocolMessage(4096, NewId(), input)
	clear(input)
	if !slices.Equal(e.queue[0].Data, []byte{0, 255, 8}) {
		t.Fatal("callback retained ephemeral bytes")
	}
	for i := 0; i < subprotocolRPCMaxMessages; i++ {
		e.SubprotocolMessage(4096, NewId(), nil)
	}
	if e.err == nil || len(e.queue) != 0 {
		t.Fatal("overflow silently dropped/coalesced messages")
	}
	e.close()
	e.SubprotocolMessage(4096, NewId(), []byte{1})
	if len(e.queue) != 0 {
		t.Fatal("delivery after unsubscribe")
	}
}

func TestSubprotocolRPCRejectsHostedAndWrongIdentity(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()
	remote, local := subprotocolTestRemote(t, pair)
	remote.instanceId = connect.NewId()
	if _, err := remote.OpenSubprotocolContext(t.Context(), 4096); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("foreign instance: %v", err)
	}
	remote.instanceId = local.deviceLocal.instanceId
	local.deviceLocal.settings.AllowProvider = false
	if _, err := remote.OpenSubprotocolContext(t.Context(), 4096); err == nil || !strings.Contains(err.Error(), "hosted proxy") {
		t.Fatalf("hosted accepted: %v", err)
	}
	local.deviceLocal.settings.AllowProvider = true
	local.deviceLocal.settings.HostedIncompatible = true
	if _, err := remote.OpenSubprotocolContext(t.Context(), 4096); err == nil {
		t.Fatal("hosted-incompatible accepted")
	}
}

func TestSubprotocolRPCSessionClosureRemovesListeners(t *testing.T) {
	pair := newSubprotocolTestPair(t)
	defer pair.close()
	remote, local := subprotocolTestRemote(t, pair)
	channel, err := remote.OpenSubprotocolContext(t.Context(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := channel.Receive(t.Context()); done <- err }()
	local.cancel()
	local.subprotocols.close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending receive succeeded after disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not unblock receive")
	}
	if len(local.deviceLocal.subprotocols.enabledIds()) != 0 {
		t.Fatal("session closure leaked listener")
	}
}
