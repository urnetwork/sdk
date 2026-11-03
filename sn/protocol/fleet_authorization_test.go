package protocol

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
)

func testingFleetAuthorization() FleetHotkeyAuthorization {
	authorization := FleetHotkeyAuthorization{ChainID: 964, Netuid: 25}
	copy(authorization.Coordinator[:], testingHexBytes("c18925925e2b7bb9059b7d696b8c92762ae86406"))
	copy(authorization.NetworkID[:], testingHexBytes("019b2e105a7c4d3e8f0123456789abcd"))
	// the well known //Alice and //Bob accounts
	copy(authorization.Coldkey[:], testingHexBytes("d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"))
	copy(authorization.Hotkey[:], testingHexBytes("8eaf04151687736326c9fea17e25fc5287613693c912909cb226aa4794f26a48"))
	return authorization
}

func testingHexBytes(value string) []byte {
	out, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return out
}

// The exact bytes wallets sign. Changing a byte of this breaks every authorization
// already signed, so the format is pinned here.
func TestFleetHotkeyAuthorizationVector(t *testing.T) {
	message, err := testingFleetAuthorization().Message()
	if err != nil {
		t.Fatal(err)
	}
	want := "Authorize URnetwork fleet hotkey\n" +
		"This lets the hotkey below claim a Top 200 spot for this network's devices. It can't move funds.\n" +
		"schema: urnetwork-fleet-hotkey-authorization-v1\n" +
		"chain_id: 964\n" +
		"netuid: 25\n" +
		"coordinator: 0xc18925925e2b7bb9059b7d696b8c92762ae86406\n" +
		"network_id: 019b2e10-5a7c-4d3e-8f01-23456789abcd\n" +
		"coldkey: 5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY\n" +
		"hotkey: 5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"
	if message != want {
		t.Fatalf("message:\n%s\nwant:\n%s", message, want)
	}
	parsed, err := ParseFleetHotkeyAuthorization(message)
	if err != nil {
		t.Fatal(err)
	}
	if *parsed != testingFleetAuthorization() {
		t.Fatalf("round trip: %+v", parsed)
	}
}

// Only the exact encoding of one authorization parses: nothing else a coldkey signed
// can be presented as one, whatever its signature.
func TestFleetHotkeyAuthorizationRefusesOtherText(t *testing.T) {
	message, err := testingFleetAuthorization().Message()
	if err != nil {
		t.Fatal(err)
	}
	consentFixture, err := os.ReadFile("testdata/network-consent-challenge-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var consent struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(consentFixture, &consent); err != nil || consent.Message == "" {
		t.Fatalf("consent fixture: %v", err)
	}
	cases := map[string]string{
		"a network consent":  consent.Message,
		"empty":              "",
		"trailing newline":   message + "\n",
		"leading space":      " " + message,
		"wrapped":            "<Bytes>" + message + "</Bytes>",
		"no notice":          strings.Replace(message, "This lets the hotkey below claim a Top 200 spot for this network's devices. It can't move funds.\n", "", 1),
		"another schema":     strings.Replace(message, "authorization-v1", "authorization-v2", 1),
		"repeated line":      message + "\nhotkey: 5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty",
		"extra line":         message + "\nnote: x",
		"lines out of order": strings.Replace(message, "chain_id: 964\nnetuid: 25", "netuid: 25\nchain_id: 964", 1),
		"upper case address": strings.Replace(message, "0xc18925925e2b7bb9059b7d696b8c92762ae86406", "0xC18925925E2B7BB9059B7D696B8C92762AE86406", 1),
		"bare network id":    strings.Replace(message, "019b2e10-5a7c-4d3e-8f01-23456789abcd", "019b2e105a7c4d3e8f0123456789abcd", 1),
		"chain id with zero": strings.Replace(message, "chain_id: 964", "chain_id: 0964", 1),
		"zero chain":         strings.Replace(message, "chain_id: 964", "chain_id: 0", 1),
		"bad ss58":           strings.Replace(message, "5GrwvaEF", "5GrwvaEG", 1),
		"hotkey is coldkey":  strings.Replace(message, "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", 1),
		"oversize":           message + strings.Repeat("x", fleetHotkeyAuthorizationMaxBytes),
	}
	for name, text := range cases {
		if _, err := ParseFleetHotkeyAuthorization(text); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func testingSr25519Sign(t *testing.T, seed [32]byte, message []byte) ([]byte, [32]byte) {
	t.Helper()
	mini, err := schnorrkel.NewMiniSecretKeyFromRaw(seed)
	if err != nil {
		t.Fatal(err)
	}
	secret := mini.ExpandEd25519()
	signature, err := secret.Sign(schnorrkel.NewSigningContext([]byte("substrate"), message))
	if err != nil {
		t.Fatal(err)
	}
	encoded := signature.Encode()
	public, err := secret.Public()
	if err != nil {
		t.Fatal(err)
	}
	return encoded[:], public.Encode()
}

// The forms the server's wallet check takes, and only over the exact text.
func TestVerifyWalletSignatureForms(t *testing.T) {
	seed := [32]byte{1, 2, 3}
	text := "the text"
	wrapped, public := testingSr25519Sign(t, seed, []byte("<Bytes>"+text+"</Bytes>"))
	bare, _ := testingSr25519Sign(t, seed, []byte(text))
	if !VerifyWalletSignature(public, text, wrapped) {
		t.Fatal("sr25519 over the wrapped text")
	}
	if !VerifyWalletSignature(public, text, bare) {
		t.Fatal("sr25519 over the bare text")
	}
	if VerifyWalletSignature(public, "other text", wrapped) {
		t.Fatal("sr25519 verified over other text")
	}
	_, other := testingSr25519Sign(t, [32]byte{9}, []byte(text))
	if VerifyWalletSignature(other, text, wrapped) {
		t.Fatal("sr25519 verified for another key")
	}
	if VerifyWalletSignature(public, text, wrapped[:63]) {
		t.Fatal("short signature verified")
	}

	edPrivate := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	var edPublic [32]byte
	copy(edPublic[:], edPrivate.Public().(ed25519.PublicKey))
	if !VerifyWalletSignature(edPublic, text, ed25519.Sign(edPrivate, []byte("<Bytes>"+text+"</Bytes>"))) {
		t.Fatal("ed25519 over the wrapped text")
	}
	if !VerifyWalletSignature(edPublic, text, ed25519.Sign(edPrivate, []byte(text))) {
		t.Fatal("ed25519 over the bare text")
	}
	if VerifyWalletSignature(edPublic, "other", ed25519.Sign(edPrivate, []byte(text))) {
		t.Fatal("ed25519 verified over other text")
	}
}

// A coldkey's signature on a consent, or on any other text, is never an authorization.
func TestVerifyFleetHotkeyAuthorizationNeedsItsOwnSignature(t *testing.T) {
	seed := [32]byte{1}
	_, coldkey := testingSr25519Sign(t, seed, []byte("x"))
	authorization := testingFleetAuthorization()
	authorization.Coldkey = coldkey
	message, err := authorization.Message()
	if err != nil {
		t.Fatal(err)
	}
	signature, _ := testingSr25519Sign(t, seed, []byte("<Bytes>"+message+"</Bytes>"))
	if verified, err := VerifyFleetHotkeyAuthorization(message, signature); err != nil || verified.Hotkey != authorization.Hotkey {
		t.Fatalf("own signature: %v", err)
	}
	other, _ := testingSr25519Sign(t, seed, []byte("<Bytes>Approve URnetwork network wallet mapping\n{}</Bytes>"))
	if _, err := VerifyFleetHotkeyAuthorization(message, other); err == nil {
		t.Fatal("a signature over other text verified as an authorization")
	}
	stranger, _ := testingSr25519Sign(t, [32]byte{2}, []byte("<Bytes>"+message+"</Bytes>"))
	if _, err := VerifyFleetHotkeyAuthorization(message, stranger); err == nil {
		t.Fatal("another key's signature verified")
	}
}

// //Alice's seed gives //Alice's account, so a hotkey here is the same account in
// subkey, btcli and wallets.
func TestFleetHotkeyPublicKeyVector(t *testing.T) {
	var seed [32]byte
	copy(seed[:], testingHexBytes("e5be9a5092b81bca64be81d212e7f2f9eba183bb7a90954f7b76361f6edb5c0a"))
	public, err := FleetHotkeyPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(public[:]) != "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d" {
		t.Fatalf("public %x", public)
	}
	if _, err := FleetHotkeyPublicKey([32]byte{}); err == nil {
		t.Fatal("an empty seed made a hotkey")
	}
}

func testingFleetManifest() FleetManifest {
	manifest := FleetManifest{Schema: FleetManifestSchema, ChainID: 964, Netuid: 25, Generation: 3}
	manifest.Coordinator[19], manifest.FleetID[31], manifest.Hotkey[31] = 1, 2, 3
	for _, id := range []byte{2, 1, 3} {
		var member FleetMember
		member.ClientID[15], member.ClientKey[31] = id, id+10
		manifest.Members = append(manifest.Members, member)
	}
	return manifest
}

func TestFleetBindingSignedByHotkeyVerifies(t *testing.T) {
	seed := [32]byte{7}
	public, err := FleetHotkeyPublicKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testingFleetManifest()
	manifest.Hotkey = public
	binding, err := manifest.Binding(manifest.Members[0], 5, 16)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := SignFleetBindingWithHotkey(seed, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.VerifyHotkey(signature) {
		t.Fatal("the hotkey's signature does not verify")
	}
	other := binding
	other.ValidToEpoch = 15
	if other.VerifyHotkey(signature) {
		t.Fatal("the signature verified over another binding")
	}
	stranger := binding
	stranger.Hotkey[0] ^= 1
	if stranger.VerifyHotkey(signature) {
		t.Fatal("the signature verified for another hotkey")
	}
}

// Only a manifest's exact canonical bytes parse, and two slots of a host, which share
// a key, can't both be members.
func TestFleetManifestStrict(t *testing.T) {
	manifest := testingFleetManifest()
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseFleetManifest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := parsed.Canonical(); string(again) != string(canonical) {
		t.Fatalf("round trip %s", again)
	}
	if parsed.Members[0].ClientID[15] != 1 {
		t.Fatalf("members not ordered by client id: %v", parsed.Members[0])
	}
	for name, text := range map[string]string{
		"trailing newline": string(canonical) + "\n",
		"whitespace":       " " + string(canonical),
		"unknown field":    strings.Replace(string(canonical), `{"schema"`, `{"x":1,"schema"`, 1),
		"second value":     string(canonical) + "{}",
		"another schema":   strings.Replace(string(canonical), "manifest-v1", "manifest-v2", 1),
		"upper case":       strings.Replace(string(canonical), `"coordinator":"0x00`, `"coordinator":"0X00`, 1),
	} {
		if _, err := ParseFleetManifest([]byte(text)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	shared := testingFleetManifest()
	shared.Members[1].ClientKey = shared.Members[0].ClientKey
	if _, err := shared.Canonical(); err == nil {
		t.Fatal("two members shared a client key")
	}
	if _, err := manifest.Binding(FleetMember{ClientID: [16]byte{9}, ClientKey: [32]byte{9}}, 5, 16); err == nil {
		t.Fatal("bound a device outside the manifest")
	}
}

func TestFleetRevokeDigest(t *testing.T) {
	revoke := FleetRevoke{ChainID: 945, Netuid: 25, Generation: 3, EffectiveEpoch: 150}
	copy(revoke.Coordinator[:], testingHexBytes("c0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3"))
	copy(revoke.ClientID[:], testingHexBytes("019b2e105a7c4d3e8f0123456789abcd"))
	payload, err := revoke.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != FleetRevokePayloadSize || !strings.HasPrefix(string(payload), FleetRevokeDomain) {
		t.Fatalf("payload %x", payload)
	}
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signature, err := revoke.SignClient(private)
	if err != nil {
		t.Fatal(err)
	}
	public := private.Public().(ed25519.PublicKey)
	if !revoke.VerifyClient(public, signature) {
		t.Fatal("does not verify")
	}
	later := revoke
	later.EffectiveEpoch = 151
	if later.VerifyClient(public, signature) {
		t.Fatal("verified for another epoch")
	}
	zero := revoke
	zero.EffectiveEpoch = 0
	if _, err := zero.Digest(); err == nil {
		t.Fatal("digested a zero epoch")
	}
}
