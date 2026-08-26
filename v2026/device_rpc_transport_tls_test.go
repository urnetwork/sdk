// Device RPC TLS tests guard the immutable reconnect configuration and measure
// the work removed from every failed or resumed connection attempt.
package sdk

import (
	"crypto/tls"
	"testing"
	"time"
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
	settings := defaultDeviceRpcSettings()
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
}

// TestWebsocketDeviceRpcDialerLogsFailureStreakOnce verifies an unavailable
// local extension cannot generate two info log lines on every retry.
func TestWebsocketDeviceRpcDialerLogsFailureStreakOnce(t *testing.T) {
	logger := &testingCountingDeviceRpcLogger{}
	settings := defaultDeviceRpcSettings()
	settings.ClientSettings.Log = logger
	settings.RpcConnectTimeout = 50 * time.Millisecond
	dialer := NewWebsocketDeviceRpcDialer(
		requireRemoteAddress("127.0.0.1:1"),
		"",
		"",
		settings,
	)

	for range 4 {
		_, _, err := dialer.Dial(t.Context())
		if err == nil {
			t.Fatal("expected local dial failure")
		}
	}
	if infoCount := logger.infoCount.Load(); infoCount != 1 {
		t.Fatalf("repeated dial failure logged %d times, want one", infoCount)
	}
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
