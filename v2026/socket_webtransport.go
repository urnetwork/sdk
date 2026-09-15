package sdk

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	webtransport "github.com/quic-go/webtransport-go"
)

// WebTransportOptions configures an HTTP/3 WebTransport session. Browser
// bindings supply Origin from the calling page. Hashes are SHA-256 of the
// complete DER leaf certificate, subject to WebTransport's validity limits.
//
//gomobile:noexport
type WebTransportOptions struct {
	TLSConfig               *tls.Config
	Origin                  string
	Protocols               []string
	ServerCertificateHashes [][]byte
}

// DialWebTransport implements WebTransport over HTTP/3, using only the supplied
// device for UDP and DNS. It does not use the browser's native WebTransport or
// open a kernel socket. The returned session owns its QUIC transport and socket.
//
//gomobile:noexport
func DialWebTransport(ctx context.Context, device Dialer, address string, options *WebTransportOptions) (*webtransport.Session, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(address, "#") {
		return nil, errors.New("WebTransport requires an HTTPS URL without credentials or fragment")
	}
	if options == nil {
		options = &WebTransportOptions{}
	}
	tlsConfig := options.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	if tlsConfig.ServerName != "" && tlsConfig.ServerName != u.Hostname() {
		return nil, errors.New("WebTransport TLS serverName must match the URL hostname")
	}
	tlsConfig.ServerName = u.Hostname()
	tlsConfig.MinVersion = tls.VersionTLS13
	tlsConfig.NextProtos = []string{"h3"}
	if len(options.ServerCertificateHashes) > 0 {
		for _, hash := range options.ServerCertificateHashes {
			if len(hash) != sha256.Size {
				return nil, errors.New("certificate hash must contain 32 SHA-256 bytes")
			}
		}
		hashes := make([][]byte, len(options.ServerCertificateHashes))
		for i, hash := range options.ServerCertificateHashes {
			hashes[i] = append([]byte(nil), hash...)
		}
		// Pin verification replaces PKI chain and hostname verification, exactly
		// for the explicit WebTransport certificate-hash mode.
		tlsConfig.InsecureSkipVerify = true
		verifyConnection := tlsConfig.VerifyConnection
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("missing WebTransport certificate")
			}
			if err := verifyWebTransportCertificate(state.PeerCertificates[0], hashes, time.Now()); err != nil {
				return err
			}
			if verifyConnection != nil {
				return verifyConnection(state)
			}
			return nil
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	destination := net.JoinHostPort(u.Hostname(), port)
	transport := &webtransport.Transport{
		TLSClientConfig:      tlsConfig,
		ApplicationProtocols: append([]string(nil), options.Protocols...),
		Config:               &webtransport.Config{MaxIncomingStreams: 64, MaxIncomingUniStreams: 64, MaxIncomingData: 4 << 20},
		QUICConfig: &quic.Config{
			EnableDatagrams: true, EnableStreamResetPartialDelivery: true,
			DisablePathMTUDiscovery: true, InitialPacketSize: 1200,
			MaxIncomingStreams: 64, MaxIncomingUniStreams: 64,
		},
	}
	transport.DialAddr = func(ctx context.Context, _ string, config *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
		return raceSocketFamilies(ctx, "udp", destination, func(ctx context.Context, family string) (*quic.Conn, error) {
			conn, err := device.DialContext(ctx, family, destination)
			if err != nil {
				return nil, err
			}
			qtransport := &quic.Transport{Conn: &connectedPacketConn{Conn: conn}}
			qconn, err := qtransport.Dial(ctx, conn.RemoteAddr(), config, quicConfig)
			if err != nil {
				_ = qtransport.Close()
				_ = conn.Close()
				return nil, err
			}
			context.AfterFunc(qconn.Context(), func() { _ = qtransport.Close(); _ = conn.Close() })
			return qconn, nil
		}, func(c *quic.Conn) { _ = c.CloseWithError(0, "") })
	}
	headers := make(http.Header)
	if options.Origin != "" {
		headers.Set("Origin", options.Origin)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, session, err := transport.Dial(ctx, u.String(), headers)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		_ = transport.Close()
		return nil, err
	}
	context.AfterFunc(session.Context(), func() { _ = transport.Close() })
	return session, nil
}

func verifyWebTransportCertificate(cert *x509.Certificate, hashes [][]byte, now time.Time) error {
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) || cert.NotAfter.Sub(cert.NotBefore) > 14*24*time.Hour {
		return errors.New("WebTransport pinned certificate must be current and valid for at most 14 days")
	}
	key, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return errors.New("WebTransport pinned certificate must use ECDSA P-256")
	}
	hash := sha256.Sum256(cert.Raw)
	for _, expected := range hashes {
		if subtle.ConstantTimeCompare(hash[:], expected) == 1 {
			return nil
		}
	}
	return errors.New("WebTransport certificate hash mismatch")
}
