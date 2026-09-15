//go:build js

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"syscall/js"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

type socketWasmTunDevice struct {
	sdk.Device
	tun *connect.Tun
	ctx context.Context
}

func (d *socketWasmTunDevice) Ctx() context.Context { return d.ctx }
func (d *socketWasmTunDevice) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.tun.DialContext(ctx, network, address)
}

// Exercise real QUIC through the WASM Promise/handle bridge. Delay application
// packets until JS has closed and released its send stream: release must retain
// queued bytes and FIN rather than resetting the stream before delivery.
func TestSocketWasmWebTransportGracefulStreamRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := connect.DefaultTunSettings()
	client, err := connect.CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := connect.CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var paused atomic.Bool
	resume := make(chan struct{})
	pump := func(from, to *connect.Tun, outbound bool) {
		for {
			p, err := from.Read()
			if err != nil {
				return
			}
			if outbound && paused.Load() {
				select {
				case <-resume:
				case <-ctx.Done():
					connect.MessagePoolReturn(p)
					return
				}
			}
			_, _ = to.Write(p)
			connect.MessagePoolReturn(p)
		}
	}
	go pump(client, peer, true)
	go pump(peer, client, false)
	ip := net.IP(peer.LocalAddresses()[0].AsSlice())
	pc, err := peer.ListenUDP(&net.UDPAddr{IP: ip, Port: 8443})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IPAddresses: []net.IP{ip}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	server := &webtransport.Server{H3: &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h3"}}, QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, InitialPacketSize: 1200}}, CheckOrigin: func(*http.Request) bool { return true }}
	defer server.Close()
	type result struct {
		data []byte
		err  error
	}
	received := make(chan result, 1)
	server.H3.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := server.Upgrade(w, r)
		if err != nil {
			received <- result{err: err}
			return
		}
		stream, err := s.AcceptUniStream(ctx)
		if err != nil {
			received <- result{err: err}
			return
		}
		data, err := io.ReadAll(stream)
		received <- result{data, err}
	})
	go server.Serve(pc)
	m := map[string]any{}
	h := jsBindSocketDevice(&socketWasmTunDevice{tun: client, ctx: ctx}, m)
	defer h.close()
	fn := m["socketOperation"].(js.Func)
	defer fn.Release()
	hash := sha256.Sum256(der)
	session, err := awaitSocketPromise(t, fn.Invoke("webTransport", 0, map[string]any{"address": "https://" + pc.LocalAddr().String() + "/echo", "hashes": []any{base64.StdEncoding.EncodeToString(hash[:])}}))
	if err != nil {
		t.Fatal(err)
	}
	paused.Store(true)
	stream, err := awaitSocketPromise(t, fn.Invoke("openUni", session.Get("id").Int(), js.Null()))
	if err != nil {
		t.Fatal(err)
	}
	id := stream.Get("id").Int()
	// Fit within the initial stream window so Write can enqueue everything
	// without requiring a flow-control update across the paused transport.
	data := bytes.Repeat([]byte{0x63}, 1024)
	jsData := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(jsData, data)
	if _, err = awaitSocketPromise(t, fn.Invoke("write", id, jsData)); err != nil {
		t.Fatal(err)
	}
	if _, err = awaitSocketPromise(t, fn.Invoke("closeWrite", id, js.Null())); err != nil {
		t.Fatal(err)
	}
	if _, err = awaitSocketPromise(t, fn.Invoke("release", id, "finished")); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case got := <-received:
		if got.err != nil || !bytes.Equal(data, got.data) {
			t.Fatalf("graceful delivery: %d bytes, %v", len(got.data), got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("released stream did not deliver FIN")
	}
}
