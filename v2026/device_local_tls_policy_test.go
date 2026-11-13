// TLS policy comparisons preserve trust and every security field while ignoring
// proved cache contents. These controls use ordinary SDK factories and no
// private dependency observations or transport workers.
package sdk

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// CertPool.Equal compares certificate DER identities and the system-root flag,
// but not AddCertWithConstraint callbacks. The fixed constructor profiles use
// immutable, unconstrained roots. Unknown pool layouts or constraints cannot
// establish their policy; never equate callbacks by their code address.
func testDeviceLocalTrustPolicyKnown(pool *x509.CertPool) bool {
	if pool == nil {
		return true
	}
	lazy := reflect.ValueOf(pool).Elem().FieldByName("lazyCerts")
	if !lazy.IsValid() || lazy.Kind() != reflect.Slice {
		return false
	}
	for i := 0; i < lazy.Len(); i++ {
		constraint := lazy.Index(i).FieldByName("constraint")
		if !constraint.IsValid() || constraint.Kind() != reflect.Func || !constraint.IsNil() {
			return false
		}
	}
	return true
}

// Only the standard library's bounded LRU has a known policy here. Capacity
// is immutable after construction; its mutex, ticket contents and LRU order
// are runtime state. An unknown implementation is unavailable, never equal.
func testDeviceLocalTlsCachePolicy(cache tls.ClientSessionCache) (present bool, capacity int, known bool) {
	if cache == nil {
		return false, 0, true
	}
	value := reflect.ValueOf(cache)
	if value.Kind() != reflect.Pointer || value.IsNil() ||
		value.Type().Elem().PkgPath() != "crypto/tls" || value.Type().Elem().Name() != "lruSessionCache" {
		return true, 0, false
	}
	field := value.Elem().FieldByName("capacity")
	if !field.IsValid() || field.Kind() != reflect.Int || field.Int() <= 0 {
		return true, 0, false
	}
	return true, int(field.Int()), true
}

// Keep every TLS security field. Only after proving trust and cache policy do
// comparison copies omit those opaque owners. Clone takes the config lock and
// preserves its private ticket-key policy without copying synchronization state.
func testDeviceLocalTlsPolicyDifference(actual, expected *tls.Config) string {
	if actual == nil || expected == nil {
		if actual == expected {
			return ""
		}
		return "TLS config presence differs"
	}
	a, b := actual.Clone(), expected.Clone()
	for _, pair := range []struct {
		name string
		a, b *x509.CertPool
	}{{name: "RootCAs", a: a.RootCAs, b: b.RootCAs}, {name: "ClientCAs", a: a.ClientCAs, b: b.ClientCAs}} {
		if !testDeviceLocalTrustPolicyKnown(pair.a) || !testDeviceLocalTrustPolicyKnown(pair.b) {
			return pair.name + " has an unproved constraint or pool layout"
		}
		if !pair.a.Equal(pair.b) {
			return pair.name + " trust policy differs"
		}
	}
	ap, ac, ak := testDeviceLocalTlsCachePolicy(a.ClientSessionCache)
	bp, bc, bk := testDeviceLocalTlsCachePolicy(b.ClientSessionCache)
	if !ak || !bk {
		return "ClientSessionCache policy is unavailable"
	}
	if ap != bp || ac != bc {
		return "ClientSessionCache presence or capacity differs"
	}
	av, bv := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	for i := 0; i < av.NumField(); i++ {
		field := av.Type().Field(i)
		if field.PkgPath == "" && field.Type.Kind() == reflect.Func &&
			(!av.Field(i).IsNil() || !bv.Field(i).IsNil()) {
			return field.Name + " callback policy is unavailable"
		}
	}
	if a.Rand != nil || b.Rand != nil || a.KeyLogWriter != nil || b.KeyLogWriter != nil {
		return "custom TLS randomness or key-log policy is unavailable"
	}
	a.RootCAs, b.RootCAs, a.ClientCAs, b.ClientCAs = nil, nil, nil, nil
	a.ClientSessionCache, b.ClientSessionCache = nil, nil
	if reflect.DeepEqual(a, b) {
		return ""
	}
	for i := 0; i < av.NumField(); i++ {
		field := av.Type().Field(i)
		if field.PkgPath == "" && !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			return field.Name + " TLS policy differs"
		}
	}
	return "private TLS ticket-key policy differs"
}

func newTestDeviceLocalCa(t *testing.T, seed byte) (*x509.Certificate, []byte) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	template := &x509.Certificate{SerialNumber: big.NewInt(int64(seed)),
		Subject:   pkix.Name{CommonName: "policy.example"},
		NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:  time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:      true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Fixed labels describe actual SDK platform settings under a host with no
// process target. No worker starts and no process policy is changed.
type testDeviceLocalTlsProfile struct {
	name     string
	platform string
	target   ByteCount
}

func testDeviceLocalTlsProfiles() []testDeviceLocalTlsProfile {
	return []testDeviceLocalTlsProfile{
		{name: "Ios32", platform: "ios", target: 32 * 1024 * 1024},
		{name: "Android64", platform: "android", target: 64 * 1024 * 1024},
		{name: "Desktop128", platform: "linux", target: 128 * 1024 * 1024},
	}
}

// These are the SDK's factory inputs to TCP/UDP and the platform carrier.
// Normalized private NAT/window observations stay in the research suite.
func testDeviceLocalTlsConfigurations(t *testing.T, profile testDeviceLocalTlsProfile) [3]*tls.Config {
	t.Helper()
	if connect.MemoryBudget() != 0 {
		t.Fatal("TLS policy controls require a host with no process target")
	}
	settings := defaultDeviceLocalSettingsWithMemoryTargetForPlatform(profile.target, profile.platform)
	if settings.MemoryTargetByteCount != profile.target {
		t.Fatalf("fixed TLS profile %s changed its target", profile.name)
	}
	_, _, _, providerShare := deviceMemoryShares(settings)
	nat := providerLocalUserNatSettingsForProfile(providerShare, profile.target, settings.LowMemoryProfile, connect.NewNoopLogger())
	platform := newDeviceLocalPlatformTransportSettingsForProfile(profile.target, nil, nil, "", "", settings.LowMemoryProfile, nil)
	return [3]*tls.Config{nat.UdpBufferSettings.TlsConfig, nat.TcpBufferSettings.TlsConfig, platform.QuicTlsConfig}
}

// Real SDK factories independently parse the same configured certificate.
// Equality concerns trust policy, not the certificate pool's lazy functions.
func TestDeviceLocalTlsPolicyEquivalentLoadedRoots(t *testing.T) {
	cert, contents := newTestDeviceLocalCa(t, 1)
	path := filepath.Join(t.TempDir(), "policy-ca.pem")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(connect.ExtraRootCAFileEnv, path)
	for _, profile := range testDeviceLocalTlsProfiles() {
		actual, expected := testDeviceLocalTlsConfigurations(t, profile), testDeviceLocalTlsConfigurations(t, profile)
		for index, protocol := range []string{"UDP", "TCP", "QUIC"} {
			a, b := actual[index], expected[index]
			if a == nil || b == nil || a.RootCAs == b.RootCAs || !a.RootCAs.Equal(b.RootCAs) || reflect.DeepEqual(a, b) {
				t.Fatalf("%s %s did not create independent equivalent trust pools", profile.name, protocol)
			}
			for _, config := range []*tls.Config{a, b} {
				if _, err := cert.Verify(x509.VerifyOptions{Roots: config.RootCAs}); err != nil {
					t.Fatalf("%s %s did not load the actual configured CA: %v", profile.name, protocol, err)
				}
			}
			if difference := testDeviceLocalTlsPolicyDifference(a, b); difference != "" {
				t.Errorf("TLS_POLICY_EQUIVALENT_ROOTS_REJECTED: profile=%s protocol=%s difference=%s", profile.name, protocol, difference)
			}
		}
	}
}

// Every protocol's real factory configuration must reject changed security
// fields; opaque verification semantics remain unavailable, even on one owner.
func TestDeviceLocalTlsPolicyRejectsSecurityDrift(t *testing.T) {
	t.Setenv(connect.ExtraRootCAFileEnv, "")
	for _, profile := range testDeviceLocalTlsProfiles() {
		configs := testDeviceLocalTlsConfigurations(t, profile)
		for index, protocol := range []string{"UDP", "TCP", "QUIC"} {
			base := configs[index]
			if difference := testDeviceLocalTlsPolicyDifference(base, base); difference != "" {
				t.Fatalf("%s %s unaltered security policy unavailable: %s", profile.name, protocol, difference)
			}
			for _, fault := range []struct {
				name   string
				change func(*tls.Config)
			}{
				{name: "InsecureSkipVerify", change: func(c *tls.Config) { c.InsecureSkipVerify = true }},
				{name: "MinVersion", change: func(c *tls.Config) { c.MinVersion = tls.VersionTLS10 }},
				{name: "MaxVersion", change: func(c *tls.Config) { c.MaxVersion = tls.VersionTLS12 }},
				{name: "ServerName", change: func(c *tls.Config) { c.ServerName = "wrong.example" }},
				{name: "NextProtos", change: func(c *tls.Config) { c.NextProtos = []string{"wrong"} }},
				{name: "ClientAuth", change: func(c *tls.Config) { c.ClientAuth = tls.RequireAndVerifyClientCert }},
				{name: "CipherSuites", change: func(c *tls.Config) { c.CipherSuites = []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256} }},
				{name: "CurvePreferences", change: func(c *tls.Config) { c.CurvePreferences = []tls.CurveID{tls.CurveP256} }},
				{name: "SessionTicketsDisabled", change: func(c *tls.Config) { c.SessionTicketsDisabled = true }},
				{name: "SessionTicketKey", change: func(c *tls.Config) { c.SessionTicketKey[0] = 1 }},
				{name: "RootCAs", change: func(c *tls.Config) { c.RootCAs = x509.NewCertPool() }},
				{name: "ClientCAs", change: func(c *tls.Config) { c.ClientCAs = x509.NewCertPool() }},
				{name: "VerifyConnection", change: func(c *tls.Config) { c.VerifyConnection = func(tls.ConnectionState) error { return nil } }},
				{name: "VerifyPeerCertificate", change: func(c *tls.Config) {
					c.VerifyPeerCertificate = func([][]byte, [][]*x509.Certificate) error { return nil }
				}},
				{name: "GetCertificate", change: func(c *tls.Config) {
					c.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }
				}},
				{name: "Time", change: func(c *tls.Config) { c.Time = time.Now }},
				{name: "ClientSessionCache", change: func(c *tls.Config) { c.ClientSessionCache = tls.NewLRUClientSessionCache(1) }},
				{name: "ticket-key", change: func(c *tls.Config) { c.SetSessionTicketKeys([][32]byte{{1}}) }},
			} {
				changed := base.Clone()
				fault.change(changed)
				if difference := testDeviceLocalTlsPolicyDifference(changed, base); difference == "" || !strings.Contains(difference, fault.name) {
					t.Fatalf("%s %s ignored TLS security mutation %s: %s", profile.name, protocol, fault.name, difference)
				}
			}
		}
	}
	base := connect.DefaultConnectSettings().TlsConfig
	first, _ := newTestDeviceLocalCa(t, 1)
	second, _ := newTestDeviceLocalCa(t, 2)
	a, b := base.Clone(), base.Clone()
	a.RootCAs, b.RootCAs = x509.NewCertPool(), x509.NewCertPool()
	a.RootCAs.AddCert(first)
	b.RootCAs.AddCert(second)
	if !reflect.DeepEqual(a.RootCAs.Subjects(), b.RootCAs.Subjects()) || testDeviceLocalTlsPolicyDifference(a, b) == "" {
		t.Fatal("same subject concealed different trusted certificates")
	}
	b.RootCAs = x509.NewCertPool()
	b.RootCAs.AddCertWithConstraint(first, func([]*x509.Certificate) error { return errors.New("reject chain") })
	if !a.RootCAs.Equal(b.RootCAs) || testDeviceLocalTlsPolicyDifference(a, b) == "" || testDeviceLocalTlsPolicyDifference(b, b) == "" {
		t.Fatal("CertPool.Equal or owner identity concealed an opaque constraint")
	}
	b = a.Clone()
	b.VerifyConnection = func(tls.ConnectionState) error { return nil }
	if testDeviceLocalTlsPolicyDifference(b, b) == "" {
		t.Fatal("same callback identity claimed unproved verification semantics")
	}
}

// Mutable session tickets differ from the cache capacity that bounds them.
func TestDeviceLocalTlsPolicyCacheRuntimeState(t *testing.T) {
	t.Setenv(connect.ExtraRootCAFileEnv, "")
	a, b := connect.DefaultConnectSettings().TlsConfig, connect.DefaultConnectSettings().TlsConfig
	a.ClientSessionCache.Put("policy-state.example", &tls.ClientSessionState{})
	if reflect.DeepEqual(a, b) || testDeviceLocalTlsPolicyDifference(a, b) != "" {
		t.Fatal("TLS cache contents were confused with its bounded policy")
	}
	a.ClientSessionCache = tls.NewLRUClientSessionCache(1)
	if testDeviceLocalTlsPolicyDifference(a, b) == "" {
		t.Fatal("ignoring runtime tickets also ignored cache capacity")
	}
}
