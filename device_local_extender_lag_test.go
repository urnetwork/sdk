//go:build !ios && !android && !js

package sdk

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/protocol"
)

// The lag.dev measurement API of the provider extender role (lag.dev/PLAN.md
// C4, M3, M10): on for a space that keeps its identity in a file, off
// otherwise, and a ticket the operator signed admitted end to end through the
// settings the role wires.
//
// The fixture operator of device_local_extender_test.go predates the
// measurement, so its family hosts are fronted by servers that pass every
// request on and add the extender's id to the activations it answered (N4).
// The plain api url, which the measurement routes are on, is a server of the
// test's: it answers the certificate route (A8) with a certificate issued at
// once for the request an activation carried. The fixture's hello host, which
// its loop barrier counts, is never read by the measurement.

// The origin a browser on lag.dev sends (M2).
const testProvideMeasurementOrigin = "https://lag.dev"

// The measurement side of the operator.
type testProvideMeasurementOperator struct {
	t          *testing.T
	extenderId connect.Id

	caKey         *ecdsa.PrivateKey
	caCertificate *x509.Certificate

	// the fronts of the operator's family hosts, and the plain api url
	v4Url  string
	v6Url  string
	apiUrl string

	stateLock sync.Mutex
	// the fixture operator's family hosts, which the fronts pass requests to;
	// known once the role is configured
	targetUrls map[int]string
	certResult map[string]any
	certPolls  int
}

// The fronts and the plain api url, built before the role so that nothing
// here runs on the provider's goroutine but setTargets.
func newTestProvideMeasurementOperator(t *testing.T) *testProvideMeasurementOperator {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "provide measurement test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caBytes)
	if err != nil {
		t.Fatal(err)
	}
	measurement := &testProvideMeasurementOperator{
		t:             t,
		extenderId:    connect.NewId(),
		caKey:         caKey,
		caCertificate: caCertificate,
		targetUrls:    map[int]string{},
		certResult:    map[string]any{"state": "none", "names": []string{}},
	}
	measurement.v4Url = measurement.front(4)
	measurement.v6Url = measurement.front(6)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != extender.ExtenderMeasurementCertPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		measurement.stateLock.Lock()
		measurement.certPolls += 1
		certResult := measurement.certResult
		measurement.stateLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(certResult)
	}))
	t.Cleanup(apiServer.Close)
	measurement.apiUrl = apiServer.URL
	return measurement
}

// Points the fronts at the fixture operator's family hosts. It runs where the
// role is configured, so it only records them.
func (self *testProvideMeasurementOperator) setTargets(v4Url string, v6Url string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.targetUrls[4] = v4Url
	self.targetUrls[6] = v6Url
}

// A server that passes each request to the operator's host of the family and
// adds the extender's id to an activation the operator answered, issuing at
// once for the request the activation carried.
func (self *testProvideMeasurementOperator) front(ipVersion int) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		self.stateLock.Lock()
		operatorUrl := self.targetUrls[ipVersion]
		self.stateLock.Unlock()
		if operatorUrl == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		requestBytes, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, err := http.NewRequest(r.Method, operatorUrl+r.URL.RequestURI(), bytes.NewReader(requestBytes))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		responseBytes, err := io.ReadAll(response.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path == connect.ExtenderActivatePath && response.StatusCode == http.StatusOK {
			args := &connect.ExtenderActivateArgs{}
			result := &connect.ExtenderActivateResult{}
			if json.Unmarshal(requestBytes, args) == nil && json.Unmarshal(responseBytes, result) == nil && result.Activated {
				extenderId := self.extenderId
				result.ExtenderId = &extenderId
				if resultBytes, err := json.Marshal(result); err == nil {
					responseBytes = resultBytes
				}
				if args.MeasurementCsr != "" {
					self.issue(args.MeasurementCsr)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		w.Write(responseBytes)
	}))
	self.t.Cleanup(server.Close)
	return server.URL
}

// Issues a chain for a request: its key and its one name, the extender's (C5).
func (self *testProvideMeasurementOperator) issue(csr string) {
	csrBytes, err := base64.StdEncoding.DecodeString(csr)
	if err != nil {
		self.t.Error(err)
		return
	}
	request, err := x509.ParseCertificateRequest(csrBytes)
	if err != nil || request.CheckSignature() != nil {
		self.t.Errorf("the measurement request does not verify: %v", err)
		return
	}
	name := connect.ExtenderMeasurementName(self.extenderId, "")
	if !slices.Equal(request.DNSNames, []string{name}) {
		self.t.Errorf("request names = %v, expected %s", request.DNSNames, name)
		return
	}
	leafBytes, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		DNSNames:     request.DNSNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, self.caCertificate, request.PublicKey, self.caKey)
	if err != nil {
		self.t.Error(err)
		return
	}
	chainPem := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafBytes})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.caCertificate.Raw}))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.certResult = map[string]any{
		"state":     "issued",
		"names":     request.DNSNames,
		"chain_pem": chainPem,
	}
}

func (self *testProvideMeasurementOperator) certPollCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.certPolls
}

// A short certificate poll for one test: a read once the request has landed,
// and soon after on a backoff.
func testShortenExtenderLagLifecycle(t *testing.T) {
	t.Helper()
	configureExtenderLagLifecycle = func(settings *extender.ExtenderLagLifecycleSettings) {
		settings.CertOfferPollTimeout = 20 * time.Millisecond
		settings.CertPollMinBackoff = 20 * time.Millisecond
		settings.CertPollMaxBackoff = 200 * time.Millisecond
	}
	t.Cleanup(func() {
		configureExtenderLagLifecycle = nil
	})
}

// The role's fixture with the measurement side of the operator in front of
// it. `newSpace` is the fixture's (nil for the manager-backed space, which
// keeps local state). The role's first pass has started when this returns.
func newTestProvideMeasurementFixture(
	t *testing.T,
	newSpace func(ctx context.Context) *NetworkSpace,
	configureDevice func(settings *DeviceLocalSettings),
) (*testProvideExtenderFixture, *testProvideMeasurementOperator) {
	t.Helper()
	measurement := newTestProvideMeasurementOperator(t)
	fixture := newTestProvideExtenderFixtureWithSpace(t, newSpace, configureDevice, func(settings *deviceLocalExtenderSettings) {
		measurement.setTargets(settings.ApiUrlV4, settings.ApiUrlV6)
		settings.ApiUrlV4 = measurement.v4Url
		settings.ApiUrlV6 = measurement.v6Url
		// the measurement routes are on the plain api url; the activation
		// keeps to the family urls, which are set
		settings.ApiUrl = measurement.apiUrl
	})
	fixture.waitPass()
	return fixture, measurement
}

// Opens a lag session on the role's tcp carrier under its measurement name, as
// a browser on lag.dev does (M1, M4), and returns the first message.
func testOpenProvideMeasurementSession(
	t *testing.T,
	tcpPort int,
	name string,
	rootCas *x509.CertPool,
	ticketText string,
) map[string]any {
	t.Helper()
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort))
		},
		TLSClientConfig:  &tls.Config{RootCAs: rootCas},
		HandshakeTimeout: 30 * time.Second,
	}
	header := http.Header{}
	header.Set("Origin", testProvideMeasurementOrigin)
	conn, _, err := dialer.Dial("wss://"+name+"/lag/v1/session?ticket="+url.QueryEscape(ticketText), header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	messageType, messageBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("message type = %d, expected text", messageType)
	}
	message := map[string]any{}
	if err := json.Unmarshal(messageBytes, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

// A ticket for one session on the extender (A6), signed by `rootPrivateKey`.
func testProvideMeasurementTicket(
	t *testing.T,
	rootPrivateKey ed25519.PrivateKey,
	extenderPublicKey ed25519.PublicKey,
	networkHost string,
) string {
	t.Helper()
	randomBytes := func() []byte {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	now := time.Now()
	ticket, err := connect.SignLagTicket(&protocol.LagTicketBody{
		TicketId:          randomBytes(),
		ClientId:          randomBytes(),
		ExtenderPublicKey: extenderPublicKey,
		NetworkHost:       networkHost,
		IssueTimeMs:       uint64(now.UnixMilli()),
		ExpireTimeMs:      uint64(now.Add(2 * time.Minute).UnixMilli()),
		TrackId:           randomBytes(),
		PingIntervalMs:    250,
		DownloadMs:        5000,
		UploadMs:          5000,
		MaxStreams:        4,
	}, rootPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ticketText, err := connect.EncodeLagTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	return ticketText
}

// Waits for the role's measurement certificate to be installed, and returns
// the names it serves.
func testWaitProvideMeasurementCertificate(t *testing.T, measurementCert *extender.ExtenderMeasurementCert) []string {
	t.Helper()
	for {
		_, change := measurementCert.ChangeMonitor().Get()
		if names, certificate := measurementCert.Certificate(); certificate != nil {
			return names
		}
		select {
		case <-change:
		case <-time.After(60 * time.Second):
			t.Fatal("the measurement certificate was not installed")
			return nil
		}
	}
}

// End to end: on a space that keeps its identity in a file the role keeps the
// measurement key beside it, the request rides the post after the one that
// reported the id, the certificate the operator issues is installed and
// presented on the tcp carrier for the measurement name, and a session opened
// there with a ticket the operator's root key signed is admitted. A ticket
// another key signed is refused as a ticket: the API trusts the space's root
// keys.
func TestDeviceLocalProviderExtenderMeasurementAdmitsATicket(t *testing.T) {
	testShortenExtenderLagLifecycle(t)
	fixture, measurement := newTestProvideMeasurementFixture(t, nil, nil)
	role := fixture.extender()
	if role == nil || role.measurementCert == nil {
		t.Fatal("measurement is off on a space that keeps its identity in a file")
	}
	keyFile := filepath.Join(fixture.networkSpace.asyncLocalState.GetLocalState().localStorageDir, extender.ExtenderMeasurementKeyFileName)
	if role.measurementCert.KeyFile() != keyFile {
		t.Fatalf("measurement key file = %s, expected %s", role.measurementCert.KeyFile(), keyFile)
	}
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("measurement key file = %v %v, expected mode 600", info, err)
	}

	// the first post learns the id and the next carries the request
	if post := fixture.waitPost(); post.args.MeasurementCsr != "" {
		t.Fatalf("the first activation carried a request before its id was known")
	}
	if post := fixture.waitPost(); post.args.MeasurementCsr == "" {
		t.Fatal("the activation after the first carried no request")
	}
	name := connect.ExtenderMeasurementName(measurement.extenderId, "")
	if names := testWaitProvideMeasurementCertificate(t, role.measurementCert); !slices.Equal(names, []string{name}) {
		t.Fatalf("served names = %v, expected %s", names, name)
	}

	rootCas := x509.NewCertPool()
	rootCas.AddCert(measurement.caCertificate)
	message := testOpenProvideMeasurementSession(t, fixture.tcpPort, name, rootCas, testProvideMeasurementTicket(
		t, fixture.rootPrivateKey, role.publicKey, testProvideExtenderHost))
	if message["type"] != "hello" {
		t.Fatalf("session answer = %v, expected hello", message)
	}
	_, otherRootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message = testOpenProvideMeasurementSession(t, fixture.tcpPort, name, rootCas, testProvideMeasurementTicket(
		t, otherRootPrivateKey, role.publicKey, testProvideExtenderHost))
	if message["type"] != "error" || message["code"] != "ticket" {
		t.Fatalf("session answer = %v, expected a ticket error", message)
	}
}

// Reads one session's messages until it closes, and returns how it closed.
func testProvideMeasurementSessionClose(t *testing.T, conn *websocket.Conn) *websocket.CloseError {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			closeErr := &websocket.CloseError{}
			if !errors.As(err, &closeErr) {
				t.Fatalf("the session ended without a close: %v", err)
			}
			return closeErr
		}
	}
}

// Opens a lag session that stays open, and returns its connection once the
// extender has said hello.
func testOpenProvideMeasurementSessionConn(
	t *testing.T,
	tcpPort int,
	name string,
	rootCas *x509.CertPool,
	ticketText string,
) (*websocket.Conn, error) {
	t.Helper()
	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort))
		},
		TLSClientConfig:  &tls.Config{RootCAs: rootCas},
		HandshakeTimeout: 30 * time.Second,
	}
	header := http.Header{}
	header.Set("Origin", testProvideMeasurementOrigin)
	conn, _, err := dialer.Dial("wss://"+name+"/lag/v1/session?ticket="+url.QueryEscape(ticketText), header)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		conn.Close()
	})
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, messageBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	message := map[string]any{}
	if err := json.Unmarshal(messageBytes, &message); err != nil {
		t.Fatal(err)
	}
	if message["type"] != "hello" {
		t.Fatalf("session answer = %v, expected hello", message)
	}
	return conn, nil
}

// The owner's measurements setting applies to a running role at once
// (lag.dev/PLAN.md C4): off takes the certificate down, so a session open
// under the measurement name is closed and a new one is refused, and the pass
// it asks for at once tells the operator; on asks for the certificate anew in
// the pass it asks for, and the role serves sessions again.
func TestDeviceLocalProviderExtenderMeasurementsToggleAtRuntime(t *testing.T) {
	testShortenExtenderLagLifecycle(t)
	fixture, measurement := newTestProvideMeasurementFixture(t, nil, nil)
	role := fixture.extender()
	if !fixture.device.GetMeasurementsEnabled() {
		t.Fatal("measurements are off by default")
	}
	fixture.waitPost()
	if post := fixture.waitPost(); post.args.MeasurementCsr == "" {
		t.Fatal("the activation after the first carried no request")
	}
	name := connect.ExtenderMeasurementName(measurement.extenderId, "")
	testWaitProvideMeasurementCertificate(t, role.measurementCert)
	rootCas := x509.NewCertPool()
	rootCas.AddCert(measurement.caCertificate)
	conn, err := testOpenProvideMeasurementSessionConn(t, fixture.tcpPort, name, rootCas, testProvideMeasurementTicket(
		t, fixture.rootPrivateKey, role.publicKey, testProvideExtenderHost))
	if err != nil {
		t.Fatal(err)
	}

	fixture.device.SetMeasurementsEnabled(false)
	if fixture.device.GetMeasurementsEnabled() {
		t.Fatal("the setting did not take")
	}
	if closeErr := testProvideMeasurementSessionClose(t, conn); closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("the open session closed with %d %q, expected going away", closeErr.Code, closeErr.Text)
	}
	if _, err := testOpenProvideMeasurementSessionConn(t, fixture.tcpPort, name, rootCas, testProvideMeasurementTicket(
		t, fixture.rootPrivateKey, role.publicKey, testProvideExtenderHost)); err == nil {
		t.Fatal("a session was admitted after opting out")
	}
	// the pass asked for at once says so on every post, and asks for nothing
	for range 2 {
		if post := fixture.waitPost(); !post.args.MeasurementDisabled || post.args.MeasurementCsr != "" {
			t.Fatalf("post = disabled %t, request %q, expected the opt-out alone", post.args.MeasurementDisabled, post.args.MeasurementCsr)
		}
	}

	version, _ := role.measurementCert.ChangeMonitor().Get()
	fixture.device.SetMeasurementsEnabled(true)
	if post := fixture.waitPost(); post.args.MeasurementDisabled || post.args.MeasurementCsr == "" {
		t.Fatalf("post = disabled %t, request %q, expected the request again", post.args.MeasurementDisabled, post.args.MeasurementCsr)
	}
	waitTestProvideMeasurementChange(t, role.measurementCert, version)
	testWaitProvideMeasurementCertificate(t, role.measurementCert)
	if _, err := testOpenProvideMeasurementSessionConn(t, fixture.tcpPort, name, rootCas, testProvideMeasurementTicket(
		t, fixture.rootPrivateKey, role.publicKey, testProvideExtenderHost)); err != nil {
		t.Fatalf("no session after opting back in: %v", err)
	}
}

// The role reads the setting as it starts: a role started with measurements
// off keeps the measurement key, says measurement_disabled from its first post,
// asks for no certificate and reads none.
func TestDeviceLocalProviderExtenderMeasurementsOffAtStart(t *testing.T) {
	testShortenExtenderLagLifecycle(t)
	fixture, measurement := newTestProvideMeasurementFixture(t, nil, nil)
	fixture.waitPost()
	fixture.waitPost()

	// off, then the role started again
	fixture.device.SetMeasurementsEnabled(false)
	fixture.waitPost()
	fixture.waitPost()
	fixture.device.SetProvideExtender(false)
	if role := fixture.extender(); role != nil {
		t.Fatal("the role still runs with the provider extender setting off")
	}
	polls := measurement.certPollCount()
	fixture.device.SetProvideExtender(true)
	fixture.waitPass()
	role := fixture.extender()
	if role == nil || role.measurementCert == nil || role.measurementCert.Enabled() {
		t.Fatal("the restarted role did not start with measurements off")
	}
	for range 2 {
		if post := fixture.waitPost(); !post.args.MeasurementDisabled || post.args.MeasurementCsr != "" {
			t.Fatalf("post = disabled %t, request %q, expected the opt-out alone", post.args.MeasurementDisabled, post.args.MeasurementCsr)
		}
	}
	if names, certificate := role.measurementCert.Certificate(); names != nil || certificate != nil {
		t.Fatal("a certificate was served with measurements off")
	}
	if count := measurement.certPollCount(); count != polls {
		t.Fatalf("the certificate route was read %d times with measurements off", count-polls)
	}
}

// Waits for the role's certificate to change after `version`.
func waitTestProvideMeasurementChange(t *testing.T, measurementCert *extender.ExtenderMeasurementCert, version uint64) {
	t.Helper()
	for {
		current, change := measurementCert.ChangeMonitor().Get()
		if version < current {
			return
		}
		select {
		case <-change:
		case <-time.After(60 * time.Second):
			t.Fatal("the measurement certificate did not change")
		}
	}
}

// A space that keeps no local state has no identity file -- its identity is
// whatever the embedder supplied, or new at every start -- so the role serves
// no measurement API and asks for no certificate.
func TestDeviceLocalProviderExtenderMeasurementNeedsAKeptIdentity(t *testing.T) {
	testShortenExtenderLagLifecycle(t)
	fixture, measurement := newTestProvideMeasurementFixture(t, newTestProvideExtenderUrlSpace, nil)
	if fixture.networkSpace.asyncLocalState != nil {
		t.Fatal("the url-only space kept local state")
	}
	role := fixture.extender()
	if role == nil || role.measurementCert != nil {
		t.Fatal("a measurement certificate on a space with no identity file")
	}
	// and no opt-out either: with no identity file there is no name to
	// withdraw
	for range 2 {
		if post := fixture.waitPost(); post.args.MeasurementCsr != "" || post.args.MeasurementDisabled {
			t.Fatal("an activation carried a request or an opt-out with no identity file")
		}
	}
	if count := measurement.certPollCount(); count != 0 {
		t.Fatalf("the certificate route was read %d times", count)
	}
}
