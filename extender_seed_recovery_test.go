//go:build !ios && !android && !js

package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/connect/protocol"
)

// The recovery of EXTENDER-UPDATE2.md §2.4: a client with empty local state,
// the dns bootstrap blocked and the control route unreachable, given a valid
// bundle by a path that needs neither, recovers a verified extender, reaches
// its operator through it, and asks for the gated release. Everything runs in
// the process: the operator's api is an httptest server, the extender is the
// real extender server on loopback, and every dial goes through the recorded
// dialer, which reaches the extender only at the address its signed record
// names.

// The operator's api host of the fixture network, which no resolver answers.
const testSeedApiHost = "api." + testSeedNetworkHost

// One request the fixture operator served.
type testSeedApiRequest struct {
	method string
	path   string
	auth   bool
}

// The fixture operator: `/hello` with its root key, and the release route,
// which answers empty, the way a spent epoch does.
type testSeedApi struct {
	server *httptest.Server
	roots  *x509.CertPool

	stateLock sync.Mutex
	requests  []testSeedApiRequest
}

func newTestSeedApi(t *testing.T) *testSeedApi {
	t.Helper()
	certificate, roots := testProviderMoveCertificate(t, testSeedApiHost)
	self := &testSeedApi{roots: roots}
	self.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.requests = append(self.requests, testSeedApiRequest{
				method: r.Method,
				path:   r.URL.Path,
				auth:   r.Header.Get("Authorization") != "",
			})
		}()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/hello":
			json.NewEncoder(w).Encode(map[string]any{
				"extender_root_public_keys": []string{testSeedRootPublicKeyHex()},
			})
		case "/network/extender-release":
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(map[string]any{
				"records":   []string{},
				"epoch":     1,
				"count":     0,
				"probation": true,
				"outcome":   "empty",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	self.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	self.server.StartTLS()
	t.Cleanup(self.server.Close)
	return self
}

func (self *testSeedApi) served(method string, path string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, request := range self.requests {
		if request.method == method && request.path == path {
			count += 1
		}
	}
	return count
}

func (self *testSeedApi) address() string {
	return self.server.Listener.Addr().String()
}

// The real extender server of the fixture extender `index` on a loopback
// listener, with only the tcp carrier, forwarding what it is asked to reach of
// the fixture network to the fixture operator, and serving the feed from a
// directory that holds `records`, as a deployed extender does.
func newTestSeedExtender(t *testing.T, index int, api *testSeedApi, records ...*protocol.ExtenderRecord) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	feedDirectorySettings := connect.DefaultExtenderDirectorySettings()
	feedDirectorySettings.NetworkHosts = []string{testSeedNetworkHost}
	feedDirectory := connect.NewExtenderDirectory(ctx, feedDirectorySettings)
	feedDirectory.SetRootKeys(testSeedRootKeySet(t))
	for _, record := range records {
		if _, err := feedDirectory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
			t.Fatal(err)
		}
	}
	feedServer := gossip.NewFeedServer(
		ctx,
		feedDirectory,
		testSeedExtenderPrivateKey(index).Public().(ed25519.PublicKey),
		nil,
	)

	settings := extender.DefaultExtenderSettings()
	settings.IdentityKeySeed = testSeedExtenderPrivateKey(index).Seed()
	settings.ForwardDialTimeout = 5 * time.Second
	settings.FeedConnHandler = feedServer.Serve
	var listenOnce sync.Once
	settings.Listen = func(network string, address string) (net.Listener, error) {
		var taken net.Listener
		listenOnce.Do(func() {
			taken = listener
		})
		if taken == nil {
			return nil, fmt.Errorf("the seed test extender binds one listener")
		}
		return taken, nil
	}
	settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
		return nil, fmt.Errorf("the seed test extender binds no udp carrier")
	}
	settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host != testSeedApiHost {
			return nil, fmt.Errorf("the seed test extender forwards to the operator only, not %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", api.address())
	}
	server := extender.NewExtenderServer(
		ctx,
		nil,
		[]string{testSeedNetworkHost, "*." + testSeedNetworkHost},
		map[int][]connect.ExtenderConnectMode{443: {connect.ExtenderConnectModeTcpTls}},
		&net.Dialer{},
		settings,
	)
	go server.ListenAndServe()
	t.Cleanup(func() {
		cancel()
		server.Close()
		feedServer.Close()
		feedDirectory.Close()
	})
	return listener.Addr().String()
}

// The mirror of the fixture network: an https server for its own name that
// serves the artifact at the fixed path, reachable only at its configured
// address, the dns-independent route.
type testSeedMirror struct {
	server *httptest.Server
	roots  *x509.CertPool

	stateLock sync.Mutex
	artifact  []byte
	hitCount  int
}

const testSeedMirrorHost = "mirror." + testSeedNetworkHost

func newTestSeedMirror(t *testing.T, artifact []byte) *testSeedMirror {
	t.Helper()
	certificate, roots := testProviderMoveCertificate(t, testSeedMirrorHost)
	self := &testSeedMirror{roots: roots, artifact: artifact}
	self.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifact := func() []byte {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.hitCount += 1
			return self.artifact
		}()
		if r.URL.Path != extenderSeedMirrorPath || r.Host != testSeedMirrorHost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(artifact)
	}))
	self.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	self.server.StartTLS()
	t.Cleanup(self.server.Close)
	return self
}

func (self *testSeedMirror) address() string {
	return self.server.Listener.Addr().String()
}

func (self *testSeedMirror) hits() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hitCount
}

// A cold start with nothing usable and nothing reachable -- empty local state,
// the dns bootstrap blocked, hello failing, every direct dial refused --
// recovered through a valid bundle that arrives by a path that needs neither:
// the bundle a release carries, or a mirror at its configured address. The
// recovery ends with the operator reached through the seed's extender and the
// gated release requested; no unsigned address is dialed on the way.
func TestExtenderSeedRecoveryWithDnsAndControlBlocked(t *testing.T) {
	for _, source := range []string{ExtenderSeedSourceEmbedded, ExtenderSeedSourceMirror} {
		t.Run(source, func(t *testing.T) {
			testSeedEnableBlockedNetworkClient(t, true)
			clock := newTestSeedClock()
			now := clock.Now()
			record := testSeedRecord(t, testSeedRecordSpec{
				index:      0,
				issueTime:  now.Add(-2 * time.Hour),
				expireTime: now.Add(48 * time.Hour),
			})
			envelope := testSeedEnvelope(t, now, testSeedBundleSpec{
				sequence: 31,
				records:  []*protocol.ExtenderRecord{record},
			})

			api := newTestSeedApi(t)
			extenderAddress := newTestSeedExtender(t, 0, api, record)
			dialer := newTestSeedDialer()
			// the signed record names a documentation address; only the
			// recorded dialer knows it is this extender
			dialer.route(testSeedExtenderAddress(0), extenderAddress)
			roots := api.roots.Clone()

			var mirror *testSeedMirror
			switch source {
			case ExtenderSeedSourceEmbedded:
				testSeedInstallEmbedded(t, testSeedText(t, envelope)+"\n")
				testSeedInstallMirrors(t)
			case ExtenderSeedSourceMirror:
				testSeedInstallEmbedded(t, "")
				mirror = newTestSeedMirror(t, testSeedBytes(t, envelope))
				// the mirror is reached at its configured address and nowhere
				// else; its name resolves nowhere
				dialer.route(mirror.address(), mirror.address())
				testSeedInstallMirrors(t, ExtenderSeedMirrorValue{
					Url:     "https://" + testSeedMirrorHost,
					Address: mirror.address(),
				})
				for _, certificate := range mirror.server.TLS.Certificates {
					roots.AddCert(certificate.Leaf)
				}
			}
			testSeedInstallConnectSettings(t, dialer, func(connectSettings *connect.ConnectSettings) {
				connectSettings.TlsConfig = &tls.Config{RootCAs: roots}
			})
			releases := &testSeedReleaseCounter{}
			testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
				settings.Now = clock.Now
				settings.ControlProbeTimeout = 15 * time.Second
				settings.RequestRelease = releases.request
			})

			space := newTestSeedSpace(t, testSeedStoragePath(t))
			// an unsigned hint, as a K7 import leaves one: it must never be
			// dialed, whatever the recovery does
			hint := netip.MustParseAddr("198.51.100.99")
			space.extenderDirectory.AddBootstrap(hint, connect.ExtenderSourceImport)

			// The woken network client still runs its dns bootstrap first while
			// fewer than LowWaterCount (4) records are active, so with the dns
			// bootstrap blocked and a one-record seed, its feed sample of the
			// seed's extender can wait out HelloTimeout (30 s) before the
			// sample lands. The trigger's own ask through the strategy does
			// not wait on it. The waits below allow for both.
			recoveryStart := time.Now()
			testSeedWaitForTimeout(t, 60*time.Second, func() bool {
				return 0 < releases.requests()
			})
			t.Logf("the gated release was requested %s after the cold start", time.Since(recoveryStart).Round(time.Millisecond))

			// the operator answered the trigger's ask through the seed's
			// extender, and then the network client's own hello, which the
			// seed woke rather than leaving to its failure's backoff: only an
			// ask that reached the operator is served, so a second one is the
			// client's
			testSeedWaitForTimeout(t, 60*time.Second, func() bool {
				return 2 <= api.served("GET", "/hello")
			})
			if !dialer.dialedAddress(testSeedExtenderAddress(0)) {
				t.Errorf("the seed's extender was never dialed: %v", dialer.dialed())
			}
			for _, address := range dialer.dialed() {
				if strings.HasPrefix(address, hint.String()+":") {
					t.Errorf("the unsigned hint was dialed: %s", address)
				}
				if host, _, err := net.SplitHostPort(address); err == nil && host == testSeedMirrorHost {
					t.Errorf("the mirror was resolved by name: %s", address)
				}
			}
			if mirror != nil && mirror.hits() == 0 {
				t.Error("the mirror was never fetched")
			}
			status := space.GetExtenderSeedStatus()
			if status.LastPassOutcome != ExtenderSeedPassOutcomeApplied || status.Sequence != 31 || status.Source != source {
				t.Errorf("the status after the recovery is %+v", status)
			}
			if source == ExtenderSeedSourceMirror && (status.MirrorCount != 1 || status.DnsIndependentMirrorCount != 1) {
				t.Errorf("the status counts the mirrors as %+v", status)
			}
			entry := testSeedDirectoryEntry(space, 0)
			if entry == nil || entry.Source != connect.ExtenderSourceSeed || entry.SuccessCount == 0 {
				t.Errorf("the seed's extender is %+v", entry)
			}
		})
	}
}

func testSeedWaitForTimeout(t *testing.T, timeout time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if deadline.Before(time.Now()) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
