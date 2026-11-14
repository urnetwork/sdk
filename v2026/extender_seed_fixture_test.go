package sdk

// Fixtures of the seed bootstrap tests (connect EXTENDER-UPDATE2.md §2.4):
// fixed signing keys, a fake clock, signed records and bundles, an
// adversarial publisher that signs what the real one refuses to, a recorded
// dialer, and spaces whose network client runs with the dns bootstrap and the
// control route blocked. Nothing here reaches past loopback.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The network the seed tests run under. A dotted host, so its space runs an
// extender network.
const testSeedNetworkHost = "seed.example"

// Another operator's network, for the foreign-network refusals.
const testSeedForeignNetworkHost = "foreign.example"

// The operator's root key, which the test spaces trust, and a key nobody
// trusts, which forges.
var (
	testSeedRootPrivateKey   = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x51}, ed25519.SeedSize))
	testSeedForgerPrivateKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x52}, ed25519.SeedSize))
)

func testSeedRootPublicKeyHex() string {
	return hex.EncodeToString(testSeedRootPrivateKey.Public().(ed25519.PublicKey))
}

// An extender identity of the fixtures, by index.
func testSeedExtenderPrivateKey(index int) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(0x60 + index)}, ed25519.SeedSize))
}

// The documentation address (RFC 5737) the fixture extender of `index`
// publishes. Public by connect's seed rule, and dialed only through the
// recorded dialer, which never lets it off the machine.
func testSeedExtenderIp(index int) netip.Addr {
	return netip.AddrFrom4([4]byte{198, 51, 100, byte(20 + index)})
}

// The fixture clock. It starts at the real instant, since the directory
// judges record expiry by the process clock.
type testSeedClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func newTestSeedClock() *testSeedClock {
	return &testSeedClock{now: time.Now()}
}

func (self *testSeedClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testSeedClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// What one fixture record is.
type testSeedRecordSpec struct {
	index       int
	networkHost string
	issueTime   time.Time
	expireTime  time.Time
	tcpPort     uint32
	tier        uint32
	canary      string
	// signs with this instead of the operator's key
	signer ed25519.PrivateKey
}

// A signed open-tier record of the fixture extender `spec.index`.
func testSeedRecord(t *testing.T, spec testSeedRecordSpec) *protocol.ExtenderRecord {
	t.Helper()
	if spec.networkHost == "" {
		spec.networkHost = testSeedNetworkHost
	}
	if spec.tcpPort == 0 {
		spec.tcpPort = 443
	}
	signer := spec.signer
	if signer == nil {
		signer = testSeedRootPrivateKey
	}
	body := &protocol.ExtenderRecordBody{
		PublicKey: testSeedExtenderPrivateKey(spec.index).Public().(ed25519.PublicKey),
		Addresses: []*protocol.ExtenderAddress{{
			Ip:        testSeedExtenderIp(spec.index).String(),
			IpVersion: 4,
			Carriers:  []string{connect.ExtenderCarrierTcp},
		}},
		TcpPort:       spec.tcpPort,
		IssueTimeMs:   uint64(spec.issueTime.UnixMilli()),
		ExpireTimeMs:  uint64(spec.expireTime.UnixMilli()),
		NetworkHost:   spec.networkHost,
		DirectoryTier: spec.tier,
		CanaryChannel: spec.canary,
	}
	record, err := connect.SignExtenderRecord(signer, body)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A signed revocation of the fixture extender `index`.
func testSeedRevocation(t *testing.T, index int, issueTime time.Time) *protocol.ExtenderRevocation {
	t.Helper()
	revocation, err := connect.SignExtenderRevocation(testSeedRootPrivateKey, &protocol.ExtenderRevocationBody{
		PublicKey:   testSeedExtenderPrivateKey(index).Public().(ed25519.PublicKey),
		IssueTimeMs: uint64(issueTime.UnixMilli()),
		NetworkHost: testSeedNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	return revocation
}

// What one fixture bundle is. The zero value of each field takes the
// fixture's default: the fixture network, sequence 1, issued an hour before
// `now` and valid for a day, signed by the operator.
type testSeedBundleSpec struct {
	networkHost string
	sequence    uint64
	issueTime   time.Time
	expireTime  time.Time
	records     []*protocol.ExtenderRecord
	revocations []*protocol.ExtenderRevocation
	signer      ed25519.PrivateKey
}

// Fills the defaults of a spec against `now`, with one record of extender 0
// when the spec names none.
func (self testSeedBundleSpec) withDefaults(t *testing.T, now time.Time) testSeedBundleSpec {
	t.Helper()
	if self.networkHost == "" {
		self.networkHost = testSeedNetworkHost
	}
	if self.sequence == 0 {
		self.sequence = 1
	}
	if self.issueTime.IsZero() {
		self.issueTime = now.Add(-time.Hour)
	}
	if self.expireTime.IsZero() {
		self.expireTime = self.issueTime.Add(24 * time.Hour)
	}
	if self.signer == nil {
		self.signer = testSeedRootPrivateKey
	}
	if self.records == nil {
		self.records = []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
			index:       0,
			networkHost: self.networkHost,
			issueTime:   self.issueTime.Add(-time.Minute),
			expireTime:  self.expireTime.Add(time.Hour),
		})}
	}
	return self
}

// A bundle signed the way connect's publisher signs one, which refuses
// anything a reader would refuse for its shape.
func testSeedEnvelope(t *testing.T, now time.Time, spec testSeedBundleSpec) *protocol.ExtenderSeedEnvelope {
	t.Helper()
	spec = spec.withDefaults(t, now)
	body, err := connect.NewExtenderSeedBody(
		spec.networkHost,
		spec.sequence,
		spec.issueTime,
		spec.expireTime,
		spec.records,
		spec.revocations,
	)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := connect.SignExtenderSeed(spec.signer, body)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

// A bundle signed by a hostile or careless publisher: whatever body it is
// given, under the seed domain, with none of the publisher's checks. It is how
// the tests hand an importer a correctly signed bundle that carries what no
// bundle may.
func testSeedAdversarialEnvelope(signer ed25519.PrivateKey, body *protocol.ExtenderSeedBody) *protocol.ExtenderSeedEnvelope {
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		panic(err)
	}
	return testSeedAdversarialEnvelopeBytes(signer, bodyBytes)
}

// The same over body bytes the publisher wrote itself, which may carry fields
// no reader knows.
func testSeedAdversarialEnvelopeBytes(signer ed25519.PrivateKey, bodyBytes []byte) *protocol.ExtenderSeedEnvelope {
	signingBytes := append([]byte(connect.ExtenderSeedSignatureDomain), bodyBytes...)
	return &protocol.ExtenderSeedEnvelope{
		Body:      bodyBytes,
		Signature: ed25519.Sign(signer, signingBytes),
		KeyId:     connect.ExtenderKeyId(signer.Public().(ed25519.PublicKey)),
	}
}

// The body a spec describes, with its records and revocations serialized, for
// the adversarial publisher.
func testSeedBody(t *testing.T, now time.Time, spec testSeedBundleSpec) *protocol.ExtenderSeedBody {
	t.Helper()
	spec = spec.withDefaults(t, now)
	body := &protocol.ExtenderSeedBody{
		FormatVersion:      connect.ExtenderSeedFormatVersion,
		NetworkHost:        spec.networkHost,
		Sequence:           spec.sequence,
		IssuedAtUnixMilli:  uint64(spec.issueTime.UnixMilli()),
		ExpiresAtUnixMilli: uint64(spec.expireTime.UnixMilli()),
	}
	for _, record := range spec.records {
		recordBytes, err := proto.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body.Records = append(body.Records, recordBytes)
	}
	for _, revocation := range spec.revocations {
		revocationBytes, err := proto.Marshal(revocation)
		if err != nil {
			t.Fatal(err)
		}
		body.Revocations = append(body.Revocations, revocationBytes)
	}
	return body
}

// The text form of any envelope, however large: connect's encoder refuses one
// over the cap, which is the kind a hostile paste carries.
func testSeedAdversarialText(t *testing.T, envelope *protocol.ExtenderSeedEnvelope) string {
	t.Helper()
	envelopeBytes, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return connect.ExtenderSeedTextPrefix + base64.RawURLEncoding.EncodeToString(envelopeBytes)
}

func testSeedText(t *testing.T, envelope *protocol.ExtenderSeedEnvelope) string {
	t.Helper()
	text, err := connect.EncodeExtenderSeedText(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func testSeedBytes(t *testing.T, envelope *protocol.ExtenderSeedEnvelope) []byte {
	t.Helper()
	envelopeBytes, err := connect.MarshalExtenderSeedEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return envelopeBytes
}

// Every dial a space's strategy and its seed bootstrap made, in order, and
// where each went. A dial whose address `route` maps is connected there;
// every other dial is refused, so nothing leaves the process.
type testSeedDialer struct {
	stateLock sync.Mutex
	addresses []string
	routes    map[string]string
}

func newTestSeedDialer() *testSeedDialer {
	return &testSeedDialer{
		routes: map[string]string{},
	}
}

// Connects dials of `address` (host:port) to `target`, a loopback listener.
func (self *testSeedDialer) route(address string, target string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.routes[address] = target
}

func (self *testSeedDialer) dialed() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.addresses...)
}

func (self *testSeedDialer) dialedAddress(address string) bool {
	for _, dialed := range self.dialed() {
		if dialed == address {
			return true
		}
	}
	return false
}

func (self *testSeedDialer) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	target := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addresses = append(self.addresses, address)
		return self.routes[address]
	}()
	if target == "" {
		return nil, fmt.Errorf("the seed test dials nothing off the machine, refused %s %s", network, address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

func (self *testSeedDialer) dialContextSettings() *connect.DialContextSettings {
	return &connect.DialContextSettings{
		DialContext: self.DialContext,
		PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
			return nil, fmt.Errorf("the seed test opens no packet socket")
		},
	}
}

// The connect settings of a seed test space: the offline resolver, which is
// the blocked dns, and the recorded dialer. Installed for the test's spaces
// and restored after.
func testSeedInstallConnectSettings(t *testing.T, dialer *testSeedDialer, configure func(*connect.ConnectSettings)) {
	t.Helper()
	previous := newNetworkSpaceConnectSettings
	newNetworkSpaceConnectSettings = func() *connect.ConnectSettings {
		connectSettings := testOfflineConnectSettings()
		connectSettings.DialContextSettings = dialer.dialContextSettings()
		if configure != nil {
			configure(connectSettings)
		}
		return connectSettings
	}
	t.Cleanup(func() {
		newNetworkSpaceConnectSettings = previous
	})
}

// Installs seed settings for the spaces a test builds, and restores the
// defaults after.
func testSeedInstallSettings(t *testing.T, configure func(settings *extenderSeedSettings)) {
	t.Helper()
	previous := extenderSeedSettingsConfigure
	extenderSeedSettingsConfigure = configure
	t.Cleanup(func() {
		extenderSeedSettingsConfigure = previous
	})
}

// Installs an embedded resource for the spaces a test builds.
func testSeedInstallEmbedded(t *testing.T, resource string) {
	t.Helper()
	previous := extenderSeedEmbeddedResource
	extenderSeedEmbeddedResource = func() string {
		return resource
	}
	t.Cleanup(func() {
		extenderSeedEmbeddedResource = previous
	})
}

// The mirrors the seed test spaces a test builds configure (testSeedInstallMirrors).
var testSeedMirrorValues []ExtenderSeedMirrorValue

// Configures `mirrorValues` as the mirrors of the seed test spaces a test
// builds, and none after it.
func testSeedInstallMirrors(t *testing.T, mirrorValues ...ExtenderSeedMirrorValue) {
	t.Helper()
	previous := testSeedMirrorValues
	testSeedMirrorValues = mirrorValues
	t.Cleanup(func() {
		testSeedMirrorValues = previous
	})
}

// The values of a seed test space: the fixture network, trusting the
// operator's key, with the mirrors the test installed.
func testSeedSpaceValues() NetworkSpaceValues {
	return NetworkSpaceValues{
		NetExposeServerIps:       true,
		NetExposeServerHostNames: true,
		ExtenderRootPublicKeys:   []string{testSeedRootPublicKeyHex()},
		ExtenderSeedMirrors:      slices.Clone(testSeedMirrorValues),
	}
}

// A seed test space on `storagePath`, closed with the test.
func newTestSeedSpace(t *testing.T, storagePath string) *NetworkSpace {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	space := newNetworkSpace(
		ctx,
		NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"},
		testSeedSpaceValues(),
		storagePath,
	)
	t.Cleanup(func() {
		space.close()
		cancel()
	})
	return space
}

// A temporary storage directory, removed with the test.
func testSeedStoragePath(t *testing.T) string {
	t.Helper()
	storagePath, err := os.MkdirTemp("", "extender-seed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(storagePath)
	})
	return storagePath
}

// The directory entries of the fixture extender `index`, by its address.
func testSeedDirectoryEntry(space *NetworkSpace, index int) *connect.ExtenderDirectoryEntry {
	ip := testSeedExtenderIp(index)
	for _, entry := range space.extenderDirectory.Snapshot().Entries {
		if entry.Ip == ip {
			return entry
		}
	}
	return nil
}
