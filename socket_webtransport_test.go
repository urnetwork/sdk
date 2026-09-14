package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	webtransport "github.com/quic-go/webtransport-go"
)

func TestSocketWebTransportOverTunAndRPC(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			n := newSocketTestNetwork(t)
			cert, roots := socketTestCertificate(t)
			pc, err := n.peer.ListenUDP(&net.UDPAddr{IP: n.ip(false), Port: 8443})
			if err != nil {
				t.Fatal(err)
			}
			server := &webtransport.Server{H3: &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h3"}}, QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true}}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://client.test" }, ApplicationProtocols: []string{"echo"}}
			sessions := make(chan *webtransport.Session, 1)
			server.H3.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s, err := server.Upgrade(w, r)
				if err != nil {
					return
				}
				sessions <- s
				go func() {
					for {
						data, err := s.ReceiveDatagram(t.Context())
						if err != nil {
							return
						}
						_ = s.SendDatagram(data)
					}
				}()
				go func() {
					for {
						stream, err := s.AcceptStream(t.Context())
						if err != nil {
							return
						}
						go func() { _, _ = io.Copy(stream, stream); _ = stream.Close() }()
					}
				}()
				go func() {
					stream, err := s.AcceptUniStream(t.Context())
					if err != nil {
						return
					}
					data, err := io.ReadAll(stream)
					if err != nil {
						return
					}
					out, err := s.OpenUniStreamSync(t.Context())
					if err != nil {
						return
					}
					_, _ = out.Write(data)
					_ = out.Close()
				}()
			})
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(pc) }()
			t.Cleanup(func() { _ = server.Close(); _ = pc.Close(); <-serveDone })
			var device Dialer = n.device
			if remote {
				d, _, _ := socketTestRemote(t, n)
				device = d
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			session, err := DialWebTransport(ctx, device, "https://socket.test:8443/echo", &WebTransportOptions{TLSConfig: &tls.Config{RootCAs: roots}, Origin: "https://client.test", Protocols: []string{"echo"}})
			if err != nil {
				t.Fatal(err)
			}
			defer session.CloseWithError(0, "")
			if session.SessionState().ApplicationProtocol != "echo" {
				t.Fatal("protocol negotiation failed")
			}
			for _, data := range [][]byte{[]byte("datagram"), {}} {
				if err := session.SendDatagram(data); err != nil {
					t.Fatal(err)
				}
				got, err := session.ReceiveDatagram(ctx)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("datagram %q %v", got, err)
				}
			}
			for i := 0; i < 3; i++ {
				s, err := session.OpenStreamSync(ctx)
				if err != nil {
					t.Fatal(err)
				}
				data := bytes.Repeat([]byte{byte(i)}, 3000)
				go func() { _, _ = s.Write(data); _ = s.Close() }()
				got, err := io.ReadAll(s)
				if err != nil || !bytes.Equal(data, got) {
					t.Fatalf("bidirectional data %d: %v", len(got), err)
				}
			}
			uni, err := session.OpenUniStreamSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = uni.Write([]byte("unidirectional"))
			_ = uni.Close()
			incoming, err := session.AcceptUniStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(incoming)
			if err != nil || string(got) != "unidirectional" {
				t.Fatal(string(got), err)
			}
			var peer *webtransport.Session
			select {
			case peer = <-sessions:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_ = peer.CloseWithError(42, "finished")
			select {
			case <-session.Context().Done():
			case <-ctx.Done():
				t.Fatal("session did not close")
			}
			var closed *webtransport.SessionError
			_, closeErr := session.OpenStream()
			if !errors.As(closeErr, &closed) || closed.ErrorCode != 42 {
				t.Fatalf("close status: %v", closeErr)
			}
		})
	}
}
func TestSocketWebTransportCertificateHashes(t *testing.T) {
	cert, _ := socketTestCertificate(t)
	hash := sha256.Sum256(cert.Leaf.Raw)
	if err := verifyWebTransportCertificate(cert.Leaf, [][]byte{hash[:]}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := verifyWebTransportCertificate(cert.Leaf, [][]byte{make([]byte, 32)}, time.Now()); err == nil {
		t.Fatal("wrong hash accepted")
	}
	expired := *cert.Leaf
	expired.NotAfter = time.Now().Add(-time.Minute)
	if err := verifyWebTransportCertificate(&expired, [][]byte{hash[:]}, time.Now()); err == nil {
		t.Fatal("expired pin accepted")
	}
	long := *cert.Leaf
	long.NotAfter = long.NotBefore.Add(15 * 24 * time.Hour)
	if err := verifyWebTransportCertificate(&long, [][]byte{hash[:]}, time.Now()); err == nil {
		t.Fatal("long-lived pin accepted")
	}
}
func TestSocketWebTransportURLValidation(t *testing.T) {
	for _, url := range []string{"http://socket.test", "https://user:pass@socket.test", "https://socket.test/#fragment", "https:///"} {
		_, err := DialWebTransport(t.Context(), nil, url, nil)
		if err == nil {
			t.Fatal("accepted", url)
		}
	}
}
func TestSocketTLSRejectsCertificateAndCancelsHandshake(t *testing.T) {
	n := newSocketTestNetwork(t)
	cert, _ := socketTestCertificate(t)
	ln, err := n.peer.ListenTCP(&net.TCPAddr{IP: n.ip(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		c := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
		defer c.Close()
		_ = c.Handshake()
	}()
	_, err = n.device.DialTls("tcp4", ln.Addr().String(), &tls.Config{ServerName: "wrong.test"})
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	ln2, err := n.peer.ListenTCP(&net.TCPAddr{IP: n.ip(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	go func() {
		c, err := ln2.Accept()
		if err == nil {
			defer c.Close()
			<-ctx.Done()
		}
	}()
	start := time.Now()
	_, err = n.device.DialTlsContext(ctx, "tcp4", ln2.Addr().String(), nil)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("stalled handshake cancellation %v", err)
	}
}
func TestSocketDTLSRejectsUnsupportedTLSVerification(t *testing.T) {
	n := newSocketTestNetwork(t)
	_, err := n.device.DialTls("udp4", n.echo(t, "udp4", 0), &tls.Config{VerifyConnection: func(tls.ConnectionState) error { return errors.New("custom") }})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal(err)
	}
}
