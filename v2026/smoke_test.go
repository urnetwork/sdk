package sdk

import (
	"crypto/tls"
	"testing"
)

// TestSDKSmoke is the fast, offline check used by the SDK test entry point.
// It deliberately exercises only constructors and public interfaces so it can
// run before a network space or provider is configured.
func TestSDKSmoke(t *testing.T) {
	id := NewId()
	if id == nil || len(id.Bytes()) != 16 || len(id.String()) != 36 {
		t.Fatalf("NewId returned an invalid id: %#v", id)
	}
	parsed, err := ParseId(id.String())
	if err != nil {
		t.Fatalf("ParseId: %v", err)
	}
	if parsed.String() != id.String() {
		t.Fatalf("ParseId changed id: got %q, want %q", parsed, id)
	}

	key := NewNetworkSpaceKey("smoke.ur.io", "")
	if key.HostName != "smoke.ur.io" || key.EnvName != "main" {
		t.Fatalf("NewNetworkSpaceKey = %#v", key)
	}

	manager := NewNetworkSpaceManagerNoStorage()
	if manager == nil {
		t.Fatal("NewNetworkSpaceManagerNoStorage returned nil")
	}
	manager.Close()

	config, err := (&SocketTLSOptions{ServerName: "smoke.ur.io"}).TLSConfig()
	if err != nil {
		t.Fatalf("SocketTLSOptions.TLSConfig: %v", err)
	}
	if config.MinVersion != tls.VersionTLS12 || config.ServerName != "smoke.ur.io" {
		t.Fatalf("unexpected TLS config: %#v", config)
	}

	// Keep the net.Dialer-compatible surface checked at compile time. The
	// constructors for these devices need live credentials and are intentionally
	// outside an offline smoke test.
	var _ Dialer = (*DeviceLocal)(nil)
	var _ Dialer = (*DeviceRemote)(nil)
	var _ TLSDialer = (*DeviceLocal)(nil)
	var _ TLSDialer = (*DeviceRemote)(nil)
}
