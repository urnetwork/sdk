// Device RPC TLS tests guard the immutable reconnect configuration and measure
// the work removed from every failed or resumed connection attempt.
package sdk

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/gorilla/websocket"
)

var benchmarkDeviceRpcTlsConfig *tls.Config

// TestWebsocketDeviceRpcDialerCachesTlsConfig verifies key and pin parsing is
// completed once when the dialer's immutable transport configuration is built.
func TestWebsocketDeviceRpcDialerCachesTlsConfig(t *testing.T) {
	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}

	settings := defaultDeviceRpcSettings()
	dialer := NewWebsocketDeviceRpcDialer(
		settings.Address,
		keyMaterial.GetClientPem(),
		keyMaterial.GetServerCertPem(),
		settings,
	)
	if dialer.tlsConfigErr != nil {
		t.Fatal(dialer.tlsConfigErr)
	}
	if dialer.tlsConfig == nil {
		t.Fatal("expected cached tls config")
	}
	if !dialer.useMtls {
		t.Fatal("expected mutual tls")
	}
	if len(dialer.tlsConfig.Certificates) != 1 {
		t.Fatalf("expected one cached client certificate, got %d", len(dialer.tlsConfig.Certificates))
	}
	if dialer.tlsConfig.VerifyPeerCertificate == nil {
		t.Fatal("expected cached server pin verifier")
	}
}

// TestWebsocketDeviceRpcDialerCachesTlsConfigError verifies invalid immutable
// credentials fail consistently without reparsing on every reconnect.
func TestWebsocketDeviceRpcDialerCachesTlsConfigError(t *testing.T) {
	logger := &testingCountingDeviceRpcLogger{}
	settings := defaultDeviceRpcSettings()
	settings.ClientSettings.Log = logger
	dialer := NewWebsocketDeviceRpcDialer(
		settings.Address,
		"invalid client pem",
		"invalid server certificate pem",
		settings,
	)
	if dialer.tlsConfigErr == nil {
		t.Fatal("expected cached tls config error")
	}
	if dialer.tlsConfig != nil {
		t.Fatal("invalid credentials produced a tls config")
	}
	for range 4 {
		forward, reverse, err := dialer.Dial(t.Context())
		if err != dialer.tlsConfigErr || forward != nil || reverse != nil {
			t.Fatalf("cached tls error changed: (%v, %v, %v)", forward, reverse, err)
		}
	}
	if count := logger.infoCount.Load(); count != 1 {
		t.Fatalf("cached server pin error logged %d times, want one", count)
	}
}

// Invalid client credentials also log once at construction and retain the
// identical cached error across reconnects.
func TestWebsocketDeviceRpcDialerCachesClientTlsConfigError(t *testing.T) {
	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		t.Fatal(err)
	}
	logger := &testingCountingDeviceRpcLogger{}
	settings := defaultDeviceRpcSettings()
	settings.ClientSettings.Log = logger
	dialer := NewWebsocketDeviceRpcDialer(settings.Address, "invalid client pem", keyMaterial.GetServerCertPem(), settings)
	if dialer.tlsConfigErr == nil || dialer.tlsConfig != nil {
		t.Fatal("invalid client credentials did not produce a cached tls error")
	}
	for range 4 {
		if _, _, err := dialer.Dial(t.Context()); err != dialer.tlsConfigErr {
			t.Fatalf("cached client keypair error changed: %v", err)
		}
	}
	if count := logger.infoCount.Load(); count != 1 {
		t.Fatalf("cached client keypair error logged %d times, want one", count)
	}
}

// A continuous outage logs once even when successive attempts fail differently.
func TestWebsocketDeviceRpcDialerLogsFailureStreakOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	logger := &testingCountingDeviceRpcLogger{}
	settings := defaultDeviceRpcSettings()
	settings.ClientSettings.Log = logger
	dialer := NewWebsocketDeviceRpcDialer(
		requireRemoteAddress(server.Listener.Addr().String()),
		"",
		"",
		settings,
	)

	for _, attempt := range []struct {
		ctx context.Context
		err error
	}{
		{ctx: canceledCtx, err: context.Canceled},
		{ctx: t.Context(), err: websocket.ErrBadHandshake},
		{ctx: canceledCtx, err: context.Canceled},
		{ctx: t.Context(), err: websocket.ErrBadHandshake},
	} {
		forward, reverse, err := dialer.Dial(attempt.ctx)
		if !errors.Is(err, attempt.err) || forward != nil || reverse != nil {
			t.Fatalf("dial returned (%v, %v, %v), want (nil, nil, %v)", forward, reverse, err, attempt.err)
		}
		t.Logf("scripted dial error: %v", err)
	}
	if infoCount := logger.infoCount.Load(); infoCount != 1 {
		t.Fatalf("repeated dial failure logged %d times, want one", infoCount)
	}
}

// Holds the first diagnostic after the dial has failed so another call can
// observe whether the streak was reserved before invoking the external logger.
type testingBlockedDeviceRpcLogger struct {
	testingCountingDeviceRpcLogger
	entered chan struct{}
	release chan struct{}
}

// Blocks only the first diagnostic; later calls can reveal duplicate logging.
func (self *testingBlockedDeviceRpcLogger) Infof(format string, args ...any) {
	if self.infoCount.Add(1) == 1 {
		close(self.entered)
		<-self.release
	}
}

// An overlapping failure must finish without logging again or waiting for the
// first logger call; the canceled contexts avoid any network scheduling.
func TestWebsocketDeviceRpcDialerConcurrentFailuresLogOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		logger := &testingBlockedDeviceRpcLogger{
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		settings := defaultDeviceRpcSettings()
		settings.ClientSettings.Log = logger
		dialer := NewWebsocketDeviceRpcDialer(requireRemoteAddress("192.0.2.1:1234"), "", "", settings)
		firstDone := make(chan error, 1)
		go func() {
			_, _, err := dialer.Dial(ctx)
			firstDone <- err
		}()
		defer func() {
			close(logger.release)
			if err := <-firstDone; !errors.Is(err, context.Canceled) {
				t.Errorf("first dial error = %v, want canceled", err)
			}
		}()
		<-logger.entered
		if _, _, err := dialer.Dial(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("second dial error = %v, want canceled", err)
		}
		if count := logger.infoCount.Load(); count != 1 {
			t.Fatalf("overlapping failures logged %d times, want one", count)
		}
	})
}

// A successful plain websocket handshake permits one diagnostic for the next
// outage, while repeated failures in that new outage remain quiet.
func TestWebsocketDeviceRpcDialerLogsNewFailureAfterRecovery(t *testing.T) {
	testingDeviceRpcDialerLogRecovery(t, false)
}

// Cached mutual-tls credentials survive the same failure/recovery boundary.
func TestWebsocketDeviceRpcDialerLogsNewFailureAfterTlsRecovery(t *testing.T) {
	testingDeviceRpcDialerLogRecovery(t, true)
}

// In-memory transport keeps the real websocket/tls handshake and both mux
// workers inside the bubble, which joins them when each connection closes.
func testingDeviceRpcDialerLogRecovery(t *testing.T, useTls bool) {
	t.Helper()
	runDeviceRpcSendDrainSynctest(t, func(t *testing.T) {
		settings := defaultDeviceRpcSettings()
		settings.DisableLogging = true
		var clientPem, serverCertPem string
		var serverConfig *tls.Config
		if useTls {
			keyMaterial, err := GenerateDeviceRpcKeyMaterial()
			if err != nil {
				t.Fatal(err)
			}
			clientPem = keyMaterial.GetClientPem()
			serverCertPem = keyMaterial.GetServerCertPem()
			serverConfig, err = serverTlsConfig(settings.logger(), keyMaterial.GetServerPem(), keyMaterial.GetClientCertPem())
			if err != nil {
				t.Fatal(err)
			}
		}
		dialer := NewWebsocketDeviceRpcDialer(requireRemoteAddress("192.0.2.1:1234"), clientPem, serverCertPem, settings)
		logger := &testingCountingDeviceRpcLogger{}
		dialer.log = logger
		connected := false
		netDialContext := func(context.Context, string, string) (net.Conn, error) {
			if !connected {
				return nil, io.EOF
			}
			clientConn, serverConn := net.Pipe()
			go func() {
				defer serverConn.Close()
				if serverConfig != nil {
					serverConn = tls.Server(serverConn, serverConfig)
				}
				request, err := http.ReadRequest(bufio.NewReader(serverConn))
				if err != nil {
					t.Errorf("read websocket handshake: %v", err)
					return
				}
				defer request.Body.Close()
				// The standard websocket accept hash completes the real dialer's
				// upgrade before the synthetic peer closes its connection.
				accept := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, err = fmt.Fprintf(serverConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
				if err != nil {
					t.Errorf("write websocket handshake: %v", err)
				}
			}()
			return clientConn, nil
		}
		for range 2 {
			if _, _, err := dialer.dial(t.Context(), netDialContext); !errors.Is(err, io.EOF) {
				t.Fatalf("first outage error = %v, want eof", err)
			}
		}
		connected = true
		forward, reverse, err := dialer.dial(t.Context(), netDialContext)
		if err != nil || forward == nil || reverse == nil {
			t.Fatalf("recovery dial = (%v, %v, %v)", forward, reverse, err)
		}
		forward.Close()
		reverse.Close()
		connected = false
		for range 2 {
			if _, _, err := dialer.dial(t.Context(), netDialContext); !errors.Is(err, io.EOF) {
				t.Fatalf("second outage error = %v, want eof", err)
			}
		}
		if count := logger.infoCount.Load(); count != 3 {
			t.Fatalf("two outages and a successful connection logged %d times, want three", count)
		}
	})
}

// BenchmarkWebsocketDeviceRpcDialerTlsConfigRebuild measures the work that the
// old reconnect path performed before every websocket dial.
func BenchmarkWebsocketDeviceRpcDialerTlsConfigRebuild(b *testing.B) {
	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		b.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	log := settings.logger()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		tlsConfig, configErr := clientTlsConfig(
			log,
			keyMaterial.GetServerCertPem(),
			keyMaterial.GetClientPem(),
		)
		if configErr != nil {
			b.Fatal(configErr)
		}
		benchmarkDeviceRpcTlsConfig = tlsConfig
	}
}

// BenchmarkWebsocketDeviceRpcDialerTlsConfigClone measures the remaining
// per-handshake clone that gorilla performs with the cached configuration.
func BenchmarkWebsocketDeviceRpcDialerTlsConfigClone(b *testing.B) {
	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	if err != nil {
		b.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	tlsConfig, err := clientTlsConfig(
		settings.logger(),
		keyMaterial.GetServerCertPem(),
		keyMaterial.GetClientPem(),
	)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkDeviceRpcTlsConfig = tlsConfig.Clone()
	}
}
