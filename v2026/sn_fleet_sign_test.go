// Fleet binding signing tests (support inbox 7934). Apps sign a pasted binding
// after showing the user their own parse of it, so the device must sign only a
// binding of its own client id and client key, and only text whose keys every
// json parser reads the same way. The vector is the sn fleet_sign_test fixture.
package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const testingSnFleetClientId = "019b2e10-5a7c-4d3e-8f01-23456789abcd"

const testingSnFleetBinding = `{"chain_id":945,"netuid":25,"coordinator":"0xc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3","fleet_id":"0xf0f1f2f3f4f5f6f7f8f9fafbfcfdfeffe0e1e2e3e4e5e6e7e8e9eaebecedeeef","hotkey":"0x0a1c77edb48519069bbb154bf088c7f17678839de458e0679370ae989af3f55c","client_id":"019b2e10-5a7c-4d3e-8f01-23456789abcd","client_key":"0x79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664","generation":3,"valid_from_epoch":100,"valid_to_epoch":200,"commitment_hash":"0x9e1d4fa4a979f45cf003f26d826837250b08b45b8e896f98316cfc60005d3471"}`

const testingSnFleetSignature = "0x8a3e5b990f1a280f0eef9455c81dfbfd251a5ac5a9bc021770cf0a4dee1b4aafab04762d6ca8b587c1e04642d46cff87844b89aefcaddf26700fdbc849efbc0a"

const testingSnFleetOtherHotkey = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Seed bytes start..start+31.
func testingSnFleetSeed(start byte) []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = start + byte(i)
	}
	return seed
}

func testingSnFleetDevice(t *testing.T) *snDevice {
	t.Helper()
	clientId, err := connect.ParseId(testingSnFleetClientId)
	if err != nil {
		t.Fatal(err)
	}
	return &snDevice{
		clientId:      clientId,
		clientKeySeed: func() []byte { return testingSnFleetSeed(0x01) },
	}
}

// The vector binding with one field value replaced.
func testingSnFleetBindingWith(t *testing.T, field string, value string) string {
	t.Helper()
	prefix := `"` + field + `":`
	start := strings.Index(testingSnFleetBinding, prefix)
	if start < 0 {
		t.Fatalf("no field %s", field)
	}
	start += len(prefix)
	end := start + strings.IndexAny(testingSnFleetBinding[start:], ",}")
	return testingSnFleetBinding[:start] + value + testingSnFleetBinding[end:]
}

func TestSnFleetBindingSignsVector(t *testing.T) {
	device := testingSnFleetDevice(t)
	if key := device.GetSnClientKey(); key != "0x79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664" {
		t.Fatalf("client key %s", key)
	}
	signature, err := device.SignSnFleetBinding(testingSnFleetBinding)
	if err != nil {
		t.Fatal(err)
	}
	if signature != testingSnFleetSignature {
		t.Fatalf("signature %s", signature)
	}
	// the signature is over the canonical digest, under the binding's key
	digestHex, err := SnFleetBindingDigest(testingSnFleetBinding)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := hex.DecodeString(strings.TrimPrefix(digestHex, "0x"))
	signatureBytes, _ := hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	publicKey := ed25519.NewKeyFromSeed(testingSnFleetSeed(0x01)).Public().(ed25519.PublicKey)
	if !ed25519.Verify(publicKey, digest, signatureBytes) {
		t.Fatal("signature does not verify over the binding digest")
	}
	b, err := snParseFleetBinding(testingSnFleetBinding)
	if err != nil {
		t.Fatal(err)
	}
	if !b.VerifyClient(signatureBytes) {
		t.Fatal("signature does not verify against the binding client key")
	}
	// surrounding whitespace and a 0x client id are the same binding
	clientIdHex := `"0x` + strings.ReplaceAll(testingSnFleetClientId, "-", "") + `"`
	spaced := " \n" + testingSnFleetBindingWith(t, "client_id", clientIdHex) + "\n "
	if signature, err := device.SignSnFleetBinding(spaced); err != nil || signature != testingSnFleetSignature {
		t.Fatalf("equivalent binding: %s %v", signature, err)
	}
}

// A binding with this device's client id but another key must never get this
// device's signature.
func TestSnFleetBindingRefusesOtherClientKey(t *testing.T) {
	device := testingSnFleetDevice(t)
	otherKey := ed25519.NewKeyFromSeed(testingSnFleetSeed(0x21)).Public().(ed25519.PublicKey)
	bindingJson := testingSnFleetBindingWith(t, "client_key", `"0x`+hex.EncodeToString(otherKey)+`"`)
	signature, err := device.SignSnFleetBinding(bindingJson)
	if err == nil || signature != "" {
		t.Fatalf("signed a binding for another client key: %s", signature)
	}
	if !strings.Contains(err.Error(), "client_key") {
		t.Fatalf("error %v", err)
	}
}

func TestSnFleetBindingRefusesOtherClientId(t *testing.T) {
	device := testingSnFleetDevice(t)
	bindingJson := testingSnFleetBindingWith(t, "client_id", `"019b2e10-5a7c-4d3e-8f01-23456789abce"`)
	if signature, err := device.SignSnFleetBinding(bindingJson); err == nil || signature != "" {
		t.Fatalf("signed a binding for another client id: %s", signature)
	}
}

func TestSnFleetBindingRefusesWithoutClientKey(t *testing.T) {
	device := testingSnFleetDevice(t)
	device.clientKeySeed = func() []byte { return nil }
	if signature, err := device.SignSnFleetBinding(testingSnFleetBinding); err == nil || signature != "" {
		t.Fatalf("signed without a client key: %s", signature)
	}
}

// Text that json parsers can read differently must be refused: the app shows
// its own parse for review, so the sdk must not sign a different reading.
func TestSnFleetBindingRefusesAmbiguousJson(t *testing.T) {
	device := testingSnFleetDevice(t)
	body := strings.TrimSuffix(testingSnFleetBinding, "}")
	cases := map[string]string{
		// a later duplicate wins in Go; other parsers keep the first or fail
		"duplicate key": body + `,"hotkey":"` + testingSnFleetOtherHotkey + `"}`,
		// Go matches keys case-insensitively; other parsers see another key
		"case variant key":    body + `,"Hotkey":"` + testingSnFleetOtherHotkey + `"}`,
		"case variant only":   strings.Replace(testingSnFleetBinding, `"hotkey"`, `"HOTKEY"`, 1),
		"unknown field":       body + `,"note":"x"}`,
		"missing field":       strings.Replace(testingSnFleetBinding, `,"generation":3`, "", 1),
		"payload wrapper":     `{"binding":` + testingSnFleetBinding + `,"client_signature":"` + testingSnFleetSignature + `"}`,
		"trailing value":      testingSnFleetBinding + `{}`,
		"array":               "[" + testingSnFleetBinding + "]",
		"null field":          testingSnFleetBindingWith(t, "commitment_hash", "null"),
		"null":                "null",
		"empty":               "",
		"fractional number":   testingSnFleetBindingWith(t, "generation", "3.0"),
		"negative number":     testingSnFleetBindingWith(t, "valid_from_epoch", "-1"),
		"escaped duplicate":   body + `,"hot` + `\` + `u006bey":"` + testingSnFleetOtherHotkey + `"}`,
		"zero commitment":     testingSnFleetBindingWith(t, "commitment_hash", `"0x`+strings.Repeat("00", 32)+`"`),
		"inverted epochs":     testingSnFleetBindingWith(t, "valid_to_epoch", "99"),
		"short client key":    testingSnFleetBindingWith(t, "client_key", `"0x79b5562e"`),
		"netuid out of range": testingSnFleetBindingWith(t, "netuid", "65536"),
	}
	for name, bindingJson := range cases {
		signature, err := device.SignSnFleetBinding(bindingJson)
		if err == nil || signature != "" {
			t.Errorf("%s: signed %s", name, signature)
		}
		if digest, err := SnFleetBindingDigest(bindingJson); err == nil {
			t.Errorf("%s: digested %s", name, digest)
		}
	}
}

// The real DeviceLocal, served over the real rpc on in-memory streams: the
// process that holds the client key enforces the checks itself, whatever the
// DeviceRemote side passed.
func TestSnFleetBindingDeviceLocalRpcEnforcesChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// signing needs no network refresh worker
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	clientId, err := connect.ParseId(testingSnFleetClientId)
	if err != nil {
		t.Fatal(err)
	}
	device, err := NewDeviceLocalWithKeyMaterial(
		networkSpace,
		testingByJwt(clientId),
		"fleet-sign",
		"test",
		"0.0.0",
		NewId(),
		false,
		NewDeviceLocalKeyMaterial(testingSnFleetSeed(0x01), nil, nil),
	)
	if err != nil || device == nil {
		t.Fatalf("device: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	_, client := testingPreferenceRpc(t, device)
	sign := func(bindingJson string) (string, error) {
		var signature string
		err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SignSnFleetBinding", bindingJson, &signature)
		return signature, err
	}

	var key string
	var noArg RpcNoArg
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetSnClientKey", noArg, &key); err != nil {
		t.Fatal(err)
	}
	if key != "0x79b5562e8fe654f94078b112e8a98ba7901f853ae695bed7e0e3910bad049664" {
		t.Fatalf("client key %s", key)
	}
	if signature, err := sign(testingSnFleetBinding); err != nil || signature != testingSnFleetSignature {
		t.Fatalf("vector: %s %v", signature, err)
	}

	otherKey := ed25519.NewKeyFromSeed(testingSnFleetSeed(0x21)).Public().(ed25519.PublicKey)
	if signature, err := sign(testingSnFleetBindingWith(t, "client_key", `"0x`+hex.EncodeToString(otherKey)+`"`)); err == nil || signature != "" {
		t.Fatalf("signed another client key over rpc: %s", signature)
	}
	if signature, err := sign(testingSnFleetBindingWith(t, "client_id", `"019b2e10-5a7c-4d3e-8f01-23456789abce"`)); err == nil || signature != "" {
		t.Fatalf("signed another client id over rpc: %s", signature)
	}
	body := strings.TrimSuffix(testingSnFleetBinding, "}")
	if signature, err := sign(body + `,"Hotkey":"` + testingSnFleetOtherHotkey + `"}`); err == nil || signature != "" {
		t.Fatalf("signed an ambiguous binding over rpc: %s", signature)
	}
}
