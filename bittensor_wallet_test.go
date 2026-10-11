package sdk

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/sn/ss58"
)

// Well-known Substrate dev accounts (subkey //Alice, //Bob).
const (
	bittensorTestAliceSs58       = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	bittensorTestAlicePubkey     = "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d"
	bittensorTestAlicePolkadot   = "15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5"
	bittensorTestBobSs58         = "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"
	bittensorTestBobPubkey       = "8eaf04151687736326c9fea17e25fc5287613693c912909cb226aa4794f26a48"
	bittensorTestMessage         = "Sign in to URnetwork\nChallenge: q1w2e3r4t5y6u7i8o9p0a1s2d3f4g5h6j7k8l9z0x1c\nTimestamp: 1757340000"
	bittensorTestRedirectLink    = "urnetwork://bittensor-sign-message"
	bittensorTestNowMillis       = int64(1757340000000)
	bittensorTestExpiresInMillis = int64(300 * 1000)
)

var bittensorTestSignature = "0x" + strings.Repeat("ab", 64)

func bittensorTestChallenge() *AuthWalletChallengeResult {
	return &AuthWalletChallengeResult{
		Challenge:       "q1w2e3r4t5y6u7i8o9p0a1s2d3f4g5h6j7k8l9z0x1c",
		Timestamp:       1757340000,
		ExpiresIn:       300,
		MessageTemplate: bittensorTestMessage,
	}
}

func TestBittensorSs58KnownVectorsRoundTrip(t *testing.T) {
	for _, v := range []struct {
		address string
		pubkey  string
	}{
		{bittensorTestAliceSs58, bittensorTestAlicePubkey},
		{bittensorTestBobSs58, bittensorTestBobPubkey},
	} {
		pubkey, err := ss58.DecodeWithPrefix(v.address, SnSs58Prefix)
		if err != nil {
			t.Fatalf("decode %s: %v", v.address, err)
		}
		if got := hex.EncodeToString(pubkey[:]); got != v.pubkey {
			t.Fatalf("decode %s: pubkey %s, want %s", v.address, got, v.pubkey)
		}
		address, err := ss58.Encode(pubkey, SnSs58Prefix)
		if err != nil || address != v.address {
			t.Fatalf("encode %s: got %q (%v)", v.pubkey, address, err)
		}
		if !ValidateSs58(v.address) {
			t.Fatalf("ValidateSs58(%s) = false", v.address)
		}
		if !ValidateSs58("  " + v.address + "\n") {
			t.Fatalf("ValidateSs58 does not trim")
		}
	}
	// the same key under the polkadot prefix (0) is a different, refused address
	var alice [32]byte
	raw, _ := hex.DecodeString(bittensorTestAlicePubkey)
	copy(alice[:], raw)
	polkadot, err := ss58.Encode(alice, 0)
	if err != nil || polkadot != bittensorTestAlicePolkadot {
		t.Fatalf("encode prefix 0: got %q (%v)", polkadot, err)
	}
	if ValidateSs58(bittensorTestAlicePolkadot) {
		t.Fatal("a prefix 0 address validated as Bittensor")
	}
	// one changed character breaks the checksum
	if ValidateSs58(bittensorTestAliceSs58[:20] + "x" + bittensorTestAliceSs58[21:]) {
		t.Fatal("a corrupted address validated")
	}
}

func TestBittensorWalletCatalog(t *testing.T) {
	walletIds := BittensorWalletIdList()
	if walletIds.Len() != 3 || walletIds.Get(0) != BittensorWalletTalisman || walletIds.Get(1) != BittensorWalletTaoCom || walletIds.Get(2) != BittensorWalletWalletConnect {
		t.Fatalf("wallets: %v", walletIds.values)
	}
	if BittensorWalletDisplayName(BittensorWalletTalisman) != "Talisman" || BittensorWalletDisplayName(BittensorWalletTaoCom) != "TAO.com" || BittensorWalletDisplayName(BittensorWalletWalletConnect) != "WalletConnect" {
		t.Fatal("display names")
	}
	if BittensorWalletInjectedName(BittensorWalletTalisman) != "talisman" {
		t.Fatal("talisman injected name")
	}
	// tao.com documents no extension api: never guess one
	if BittensorWalletInjectedName(BittensorWalletTaoCom) != "" {
		t.Fatal("tao.com must have no injected name")
	}
	if BittensorWalletDisplayName("subwallet-js") != "" || BittensorWalletTransportFor("subwallet-js", BittensorWalletPlatformWeb) != "" {
		t.Fatal("an unsupported wallet resolved")
	}
}

func TestBittensorWalletTransportSelection(t *testing.T) {
	want := map[string][3]string{
		BittensorWalletPlatformWeb:     {BittensorWalletTransportExtension, BittensorWalletTransportManual, BittensorWalletTransportWalletConnect},
		BittensorWalletPlatformMacos:   {BittensorWalletTransportBrowserBridge, BittensorWalletTransportManual, BittensorWalletTransportBrowserBridge},
		BittensorWalletPlatformWindows: {BittensorWalletTransportBrowserBridge, BittensorWalletTransportManual, BittensorWalletTransportBrowserBridge},
		BittensorWalletPlatformLinux:   {BittensorWalletTransportBrowserBridge, BittensorWalletTransportManual, BittensorWalletTransportBrowserBridge},
		BittensorWalletPlatformIos:     {BittensorWalletTransportManual, BittensorWalletTransportManual, BittensorWalletTransportBrowserBridge},
		BittensorWalletPlatformAndroid: {BittensorWalletTransportManual, BittensorWalletTransportManual, BittensorWalletTransportBrowserBridge},
	}
	for platform, transports := range want {
		if got := BittensorWalletTransportFor(BittensorWalletTalisman, platform); got != transports[0] {
			t.Errorf("talisman on %s: %q, want %q", platform, got, transports[0])
		}
		if got := BittensorWalletTransportFor(BittensorWalletTaoCom, platform); got != transports[1] {
			t.Errorf("taocom on %s: %q, want %q", platform, got, transports[1])
		}
		if got := BittensorWalletTransportFor(BittensorWalletWalletConnect, platform); got != transports[2] {
			t.Errorf("walletconnect on %s: %q, want %q", platform, got, transports[2])
		}
	}
	if BittensorWalletTransportFor(BittensorWalletTalisman, "tvos") != "" {
		t.Fatal("an unknown platform resolved")
	}
	if _, err := NewBittensorWalletSession("polkadot-js", BittensorWalletPlatformWeb, BittensorWalletPurposeLogin, ""); err == nil {
		t.Fatal("session for an unsupported wallet")
	}
	if _, err := NewBittensorWalletSession(BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeLogin, ""); err == nil {
		t.Fatal("a browser bridge session without a redirect link")
	}
	if _, err := NewBittensorWalletSession(BittensorWalletTalisman, BittensorWalletPlatformWeb, "steal", ""); err == nil {
		t.Fatal("session for an unknown purpose")
	}
}

func TestBittensorChallengeMessageParse(t *testing.T) {
	m, err := ParseBittensorChallengeMessage(bittensorTestMessage)
	if err != nil {
		t.Fatal(err)
	}
	if m.Challenge != "q1w2e3r4t5y6u7i8o9p0a1s2d3f4g5h6j7k8l9z0x1c" || m.Timestamp != 1757340000 {
		t.Fatalf("parsed %+v", m)
	}
	for _, bad := range []string{
		"",
		"hello",
		strings.ReplaceAll(bittensorTestMessage, "\n", "\r\n"),
		"<Bytes>" + bittensorTestMessage + "</Bytes>",
		bittensorTestMessage + "\n",
		"Sign in to URnetwork\nChallenge: \nTimestamp: 1",
		"Sign in to URnetwork\nChallenge: x\nTimestamp: soon",
		"Sign in to UR\nChallenge: x\nTimestamp: 1",
	} {
		if _, err := ParseBittensorChallengeMessage(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestBittensorSignRawDataAndSignatureNormalize(t *testing.T) {
	if got := BittensorSignRawData("Hi\nü"); got != "0x48690ac3bc" {
		t.Fatalf("sign raw data: %s", got)
	}
	want := "0x" + strings.Repeat("ab", 64)
	for _, in := range []string{
		want,
		strings.TrimPrefix(want, "0x"),
		"0X" + strings.ToUpper(strings.Repeat("ab", 64)),
		"  " + want + "\n",
	} {
		if got := NormalizeBittensorSignature(in); got != want {
			t.Errorf("normalize %q: %q", in, got)
		}
	}
	for _, bad := range []string{
		"",
		"0x",
		"0x" + strings.Repeat("ab", 63),
		"0x" + strings.Repeat("ab", 65),
		"0x" + strings.Repeat("zz", 64),
		// a base64 (solana style) signature is not hex
		"q83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vq83vqw==",
	} {
		if got := NormalizeBittensorSignature(bad); got != "" {
			t.Errorf("normalize accepted %q", bad)
		}
	}
}

func TestBittensorWalletReturnParse(t *testing.T) {
	uri := bittensorTestRedirectLink + "?" + url.Values{
		"address":   {bittensorTestAliceSs58},
		"signature": {bittensorTestSignature},
		"message":   {bittensorTestMessage},
		"purpose":   {"connect"},
		"wallet":    {"talisman"},
	}.Encode()
	r, err := ParseBittensorWalletReturn(uri, bittensorTestRedirectLink)
	if err != nil {
		t.Fatal(err)
	}
	if r.Address != bittensorTestAliceSs58 || r.Signature != bittensorTestSignature || r.Message != bittensorTestMessage || r.Purpose != "connect" || r.WalletId != "talisman" {
		t.Fatalf("parsed %+v", r)
	}
	// URLSearchParams encodes a space as "+"
	r, err = ParseBittensorWalletReturn("URNETWORK://Bittensor-Sign-Message?errorCode=-1&errorMessage=User+rejected+the+request", bittensorTestRedirectLink)
	if err != nil || r.ErrorCode != "-1" || r.ErrorMessage != "User rejected the request" {
		t.Fatalf("error return: %+v %v", r, err)
	}
	// android's scheme
	if _, err := ParseBittensorWalletReturn("ur://bittensor-sign-message?address=x", "ur://bittensor-sign-message"); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{
		"urnetwork://phantom-sign-message?nonce=a&data=b",
		"urnetwork://checkout?status=complete",
		"https://bittensor-sign-message/?address=x",
		"urnetwork://bittensor-sign-message/extra?address=x",
	} {
		if _, err := ParseBittensorWalletReturn(other, bittensorTestRedirectLink); err == nil {
			t.Errorf("accepted %q", other)
		}
	}
}

func newBittensorTestSession(t *testing.T, walletId string, platform string, purpose string) *BittensorWalletSession {
	t.Helper()
	session, err := NewBittensorWalletSession(walletId, platform, purpose, bittensorTestRedirectLink)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestBittensorWalletBridgeSession(t *testing.T) {
	session := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWindows, BittensorWalletPurposeConnect)
	if session.Transport() != BittensorWalletTransportBrowserBridge {
		t.Fatalf("transport %s", session.Transport())
	}
	if _, err := session.BridgeUrl(); err == nil {
		t.Fatal("bridge url before a challenge")
	}
	args := session.ChallengeArgs(" " + bittensorTestAliceSs58 + " ")
	if args.Blockchain != TAO || args.WalletAddress != bittensorTestAliceSs58 || args.Purpose != "connect" {
		t.Fatalf("challenge args %+v", args)
	}
	if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	if session.ExpiresAtMillis() != bittensorTestNowMillis+bittensorTestExpiresInMillis {
		t.Fatalf("expires at %d", session.ExpiresAtMillis())
	}
	bridgeUrl, err := session.BridgeUrl()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(bridgeUrl)
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != BittensorWalletBridgeUrl ||
		q.Get("provider") != "bittensor" || q.Get("method") != "signMessage" ||
		q.Get("wallet") != "talisman" || q.Get("message") != bittensorTestMessage ||
		q.Get("purpose") != "connect" || q.Get("redirect_link") != bittensorTestRedirectLink ||
		q.Get("address") != bittensorTestAliceSs58 {
		t.Fatalf("bridge url %s", bridgeUrl)
	}
	if session.State() != BittensorWalletStateAwaitingWallet {
		t.Fatalf("state %s", session.State())
	}
	returnUri := func(values url.Values) string {
		return bittensorTestRedirectLink + "?" + values.Encode()
	}
	good := url.Values{
		"address":   {bittensorTestAliceSs58},
		"signature": {strings.TrimPrefix(bittensorTestSignature, "0x")},
		"message":   {bittensorTestMessage},
		"purpose":   {"connect"},
		"wallet":    {"talisman"},
	}
	// a return for the login flow belongs to another session: refused, no state change
	login := url.Values{}
	for k, v := range good {
		login[k] = v
	}
	login.Set("purpose", "login")
	if r := session.HandleBridgeReturn(returnUri(login), bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorPurposeMismatch {
		t.Fatalf("purpose mismatch: %+v", r)
	}
	if session.State() != BittensorWalletStateAwaitingWallet {
		t.Fatalf("state after foreign return %s", session.State())
	}
	r := session.HandleBridgeReturn(returnUri(good), bittensorTestNowMillis+1000)
	if !r.Ok() {
		t.Fatalf("good return: %+v", r)
	}
	if r.Proof.Signature != bittensorTestSignature || r.Proof.Address != bittensorTestAliceSs58 || r.Proof.Message != bittensorTestMessage {
		t.Fatalf("proof %+v", r.Proof)
	}
	auth := r.Proof.WalletAuthArgs()
	if auth.Blockchain != TAO || auth.PublicKey != bittensorTestAliceSs58 || auth.Message != bittensorTestMessage || auth.Signature != bittensorTestSignature {
		t.Fatalf("wallet auth %+v", auth)
	}
	// a replayed return after the proof is refused
	if r := session.HandleBridgeReturn(returnUri(good), bittensorTestNowMillis+2000); r.ErrorCode != BittensorWalletErrorNotAwaiting {
		t.Fatalf("replay: %+v", r)
	}
}

func TestBittensorWalletBridgeReturnRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(url.Values)
		now    int64
		code   string
	}{
		{"other message", func(v url.Values) { v.Set("message", strings.Replace(bittensorTestMessage, "q1", "zz", 1)) }, 0, BittensorWalletErrorMessageMismatch},
		{"no message", func(v url.Values) { v.Del("message") }, 0, BittensorWalletErrorMessageMismatch},
		{"other account", func(v url.Values) { v.Set("address", bittensorTestBobSs58) }, 0, BittensorWalletErrorAddressMismatch},
		{"polkadot prefix", func(v url.Values) { v.Set("address", bittensorTestAlicePolkadot) }, 0, BittensorWalletErrorInvalidAddress},
		{"short signature", func(v url.Values) { v.Set("signature", "0xabcd") }, 0, BittensorWalletErrorInvalidSignature},
		{"other wallet", func(v url.Values) { v.Set("wallet", "subwallet-js") }, 0, BittensorWalletErrorUnsupportedWallet},
		{"no purpose", func(v url.Values) { v.Del("purpose") }, 0, BittensorWalletErrorPurposeMismatch},
		{"expired", func(v url.Values) {}, bittensorTestExpiresInMillis, BittensorWalletErrorExpired},
		{"wallet error", func(v url.Values) { v.Set("errorCode", "-1"); v.Set("errorMessage", "Cancelled") }, 0, BittensorWalletErrorWallet},
	}
	for _, c := range cases {
		session := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformLinux, BittensorWalletPurposeLogin)
		session.ChallengeArgs(bittensorTestAliceSs58)
		if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
			t.Fatal(err)
		}
		if _, err := session.BridgeUrl(); err != nil {
			t.Fatal(err)
		}
		values := url.Values{
			"address":   {bittensorTestAliceSs58},
			"signature": {bittensorTestSignature},
			"message":   {bittensorTestMessage},
			"purpose":   {"login"},
		}
		c.mutate(values)
		r := session.HandleBridgeReturn(bittensorTestRedirectLink+"?"+values.Encode(), bittensorTestNowMillis+c.now)
		if r.Ok() || r.ErrorCode != c.code {
			t.Errorf("%s: got %+v, want %s", c.name, r, c.code)
		}
		if c.code == BittensorWalletErrorWallet && r.ErrorMessage != "Cancelled" {
			t.Errorf("%s: message %q", c.name, r.ErrorMessage)
		}
	}
	// a cancelled session refuses a late return
	session := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformMacos, BittensorWalletPurposeLogin)
	session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	session.BridgeUrl()
	session.Cancel()
	values := url.Values{"address": {bittensorTestAliceSs58}, "signature": {bittensorTestSignature}, "message": {bittensorTestMessage}, "purpose": {"login"}}
	if r := session.HandleBridgeReturn(bittensorTestRedirectLink+"?"+values.Encode(), bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorNotAwaiting {
		t.Fatalf("cancelled: %+v", r)
	}
}

// The bridge page hands a failure back with its own code and its English
// text. The session answers wallet_error and passes both on, so the app can
// show its own translation, or the text for a code it does not know. A page
// before the codes sends errorCode=-1, which is no code: the text alone.
func TestBittensorWalletBridgeReturnPassesThePageErrorCode(t *testing.T) {
	// the codes are a contract with ur.io's BittensorConnect.jsx
	for code, want := range map[string]string{
		BittensorWalletBridgeErrorAddressNotInWallet:       "address_not_in_wallet",
		BittensorWalletBridgeErrorAddressMismatch:          "address_mismatch",
		BittensorWalletBridgeErrorExtensionNotFound:        "extension_not_found",
		BittensorWalletBridgeErrorNoAccount:                "no_account",
		BittensorWalletBridgeErrorUserRejected:             "user_rejected",
		BittensorWalletBridgeErrorWalletConnectExpired:     "walletconnect_expired",
		BittensorWalletBridgeErrorWalletConnectUnavailable: "walletconnect_unavailable",
		BittensorWalletBridgeErrorInvalidRequest:           "invalid_request",
		BittensorWalletBridgeErrorWallet:                   "wallet_error",
	} {
		if code != want {
			t.Errorf("bridge error code %q, want %q", code, want)
		}
	}

	notInWalletMessage := "Your Talisman wallet doesn't have the address you entered. Add or connect that account in the wallet and try again, or enter your address manually."
	failureUri := func(errorCode string, errorMessage string) string {
		values := url.Values{"purpose": {"connect"}}
		if errorCode != "" {
			values.Set("errorCode", errorCode)
		}
		if errorMessage != "" {
			values.Set("errorMessage", errorMessage)
		}
		return bittensorTestRedirectLink + "?" + values.Encode()
	}
	for _, c := range []struct {
		name            string
		walletId        string
		errorCode       string
		errorMessage    string
		bridgeErrorCode string
	}{
		{
			name:            "not in the wallet",
			walletId:        BittensorWalletTalisman,
			errorCode:       "address_not_in_wallet",
			errorMessage:    notInWalletMessage,
			bridgeErrorCode: BittensorWalletBridgeErrorAddressNotInWallet,
		},
		{
			name:            "no extension",
			walletId:        BittensorWalletTalisman,
			errorCode:       "extension_not_found",
			errorMessage:    "The Talisman extension was not found in this browser. Install it, or enter your address manually.",
			bridgeErrorCode: BittensorWalletBridgeErrorExtensionNotFound,
		},
		{
			name:            "declined",
			walletId:        BittensorWalletWalletConnect,
			errorCode:       "user_rejected",
			errorMessage:    "User rejected.",
			bridgeErrorCode: BittensorWalletBridgeErrorUserRejected,
		},
		{
			name:            "expired pairing",
			walletId:        BittensorWalletWalletConnect,
			errorCode:       "walletconnect_expired",
			errorMessage:    "The wallet did not respond before the pairing expired.",
			bridgeErrorCode: BittensorWalletBridgeErrorWalletConnectExpired,
		},
		{
			name:            "another failure",
			walletId:        BittensorWalletWalletConnect,
			errorCode:       "wallet_error",
			errorMessage:    "WalletConnect relay: connection failed",
			bridgeErrorCode: BittensorWalletBridgeErrorWallet,
		},
		{
			name:            "a code this sdk does not know",
			walletId:        BittensorWalletWalletConnect,
			errorCode:       "wallet_locked",
			errorMessage:    "The wallet is locked.",
			bridgeErrorCode: "wallet_locked",
		},
		{
			name:            "a page before the codes",
			walletId:        BittensorWalletTalisman,
			errorCode:       "-1",
			errorMessage:    notInWalletMessage,
			bridgeErrorCode: "",
		},
		{
			name:            "a text with no code",
			walletId:        BittensorWalletTalisman,
			errorCode:       "",
			errorMessage:    "Cancelled",
			bridgeErrorCode: "",
		},
	} {
		session := newBittensorTestSession(t, c.walletId, BittensorWalletPlatformMacos, BittensorWalletPurposeConnect)
		session.ChallengeArgs(bittensorTestAliceSs58)
		if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
			t.Fatal(err)
		}
		if _, err := session.BridgeUrl(); err != nil {
			t.Fatal(err)
		}
		r := session.HandleBridgeReturn(failureUri(c.errorCode, c.errorMessage), bittensorTestNowMillis+1000)
		if r.Ok() || r.ErrorCode != BittensorWalletErrorWallet || r.ErrorMessage != c.errorMessage || r.BridgeErrorCode != c.bridgeErrorCode {
			t.Errorf("%s: got %+v, want wallet_error %q with %q", c.name, r, c.bridgeErrorCode, c.errorMessage)
		}
		if session.State() != BittensorWalletStateFailed || session.ErrorCode() != BittensorWalletErrorWallet {
			t.Errorf("%s: state %s, code %s", c.name, session.State(), session.ErrorCode())
		}
		// a second hand-back of the same failure is not this session's any more
		replay := session.HandleBridgeReturn(failureUri(c.errorCode, c.errorMessage), bittensorTestNowMillis+2000)
		if replay.ErrorCode != BittensorWalletErrorNotAwaiting || replay.BridgeErrorCode != "" {
			t.Errorf("%s: replay %+v", c.name, replay)
		}
	}

	// another flow's failure is refused unchanged and carries no page code
	session := newBittensorTestSession(t, BittensorWalletWalletConnect, BittensorWalletPlatformIos, BittensorWalletPurposeConnect)
	if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	if _, err := session.BridgeUrl(); err != nil {
		t.Fatal(err)
	}
	otherFlowValues := url.Values{"purpose": {"login"}, "errorCode": {"user_rejected"}, "errorMessage": {"User rejected."}}
	if r := session.HandleBridgeReturn(bittensorTestRedirectLink+"?"+otherFlowValues.Encode(), bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorPurposeMismatch || r.BridgeErrorCode != "" {
		t.Fatalf("another flow's failure: %+v", r)
	}
	if session.State() != BittensorWalletStateAwaitingWallet {
		t.Fatalf("state after another flow's failure %s", session.State())
	}
}

func TestBittensorWalletExtensionSession(t *testing.T) {
	session := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWeb, BittensorWalletPurposeLogin)
	if _, err := session.BridgeUrl(); err == nil {
		t.Fatal("bridge url on the extension transport")
	}
	session.ChallengeArgs("")
	if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	// a result before the request was sent is refused
	if r := session.HandleSignature(bittensorTestBobSs58, bittensorTestSignature, bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorNotAwaiting {
		t.Fatalf("early: %+v", r)
	}
	req, err := session.SignRequest()
	if err != nil {
		t.Fatal(err)
	}
	if req.InjectedName != "talisman" || req.DappName != "URnetwork" || req.Address != "" || req.Type != "bytes" || req.Data != BittensorSignRawData(bittensorTestMessage) {
		t.Fatalf("sign request %+v", req)
	}
	// no address was typed: any valid account may sign
	r := session.HandleSignature(bittensorTestBobSs58, bittensorTestSignature, bittensorTestNowMillis)
	if !r.Ok() || r.Proof.Address != bittensorTestBobSs58 || r.Proof.Message != bittensorTestMessage {
		t.Fatalf("extension result: %+v", r)
	}
}

func TestBittensorWalletManualSession(t *testing.T) {
	session := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformIos, BittensorWalletPurposeConnect)
	if session.Transport() != BittensorWalletTransportManual {
		t.Fatalf("transport %s", session.Transport())
	}
	if _, err := session.SignRequest(); err == nil {
		t.Fatal("sign request on the manual transport")
	}
	if err := session.SetChallenge(&AuthWalletChallengeResult{MessageTemplate: "please sign"}, bittensorTestNowMillis); err == nil {
		t.Fatal("accepted a malformed challenge")
	}
	if err := session.SetChallenge(&AuthWalletChallengeResult{Error: &ApiError{Message: "429"}}, bittensorTestNowMillis); err == nil {
		t.Fatal("accepted an error result")
	}
	session.ChallengeArgs(bittensorTestAliceSs58)
	result := bittensorTestChallenge()
	result.ExpiresIn = 0
	if err := session.SetChallenge(result, bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	// expires_in omitted: the server's 5 minutes
	if session.ExpiresAtMillis() != bittensorTestNowMillis+bittensorTestExpiresInMillis {
		t.Fatalf("default expiry %d", session.ExpiresAtMillis())
	}
	if session.Message() != bittensorTestMessage {
		t.Fatal("message to show")
	}
	// a typo is refused and can be corrected against the same challenge
	if r := session.HandleSignature(bittensorTestAliceSs58, "0x1234", bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorInvalidSignature {
		t.Fatalf("bad signature: %+v", r)
	}
	if session.State() != BittensorWalletStateFailed || session.ErrorCode() != BittensorWalletErrorInvalidSignature {
		t.Fatalf("state %s %s", session.State(), session.ErrorCode())
	}
	if r := session.HandleSignature(bittensorTestBobSs58, bittensorTestSignature, bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorAddressMismatch {
		t.Fatalf("other address: %+v", r)
	}
	r := session.HandleSignature(" "+bittensorTestAliceSs58+" ", " "+strings.ToUpper(strings.TrimPrefix(bittensorTestSignature, "0x")), bittensorTestNowMillis+1)
	if !r.Ok() || r.Proof.Signature != bittensorTestSignature || r.Proof.Address != bittensorTestAliceSs58 || r.Proof.WalletId != BittensorWalletTaoCom || r.Proof.Purpose != "connect" {
		t.Fatalf("manual: %+v", r)
	}
	// manual entry after expiry
	late := newBittensorTestSession(t, BittensorWalletTaoCom, BittensorWalletPlatformWeb, BittensorWalletPurposeLogin)
	late.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	if r := late.HandleSignature(bittensorTestAliceSs58, bittensorTestSignature, bittensorTestNowMillis+bittensorTestExpiresInMillis); r.ErrorCode != BittensorWalletErrorExpired {
		t.Fatalf("late: %+v", r)
	}
	// the bridge transport cannot take a direct signature
	bridge := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformMacos, BittensorWalletPurposeLogin)
	if r := bridge.HandleSignature(bittensorTestAliceSs58, bittensorTestSignature, bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorWrongTransport {
		t.Fatalf("bridge direct: %+v", r)
	}
}

func TestBittensorWalletConnectWebSession(t *testing.T) {
	session := newBittensorTestSession(t, BittensorWalletWalletConnect, BittensorWalletPlatformWeb, BittensorWalletPurposeLogin)
	if session.Transport() != BittensorWalletTransportWalletConnect {
		t.Fatalf("transport %s", session.Transport())
	}
	if _, err := session.BridgeUrl(); err == nil {
		t.Fatal("bridge url on the web walletconnect transport")
	}
	session.ChallengeArgs("")
	if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
		t.Fatal(err)
	}
	req, err := session.SignRequest()
	if err != nil {
		t.Fatal(err)
	}
	if req.Chain != "polkadot:2f0555cc76fc2840a25a6ea3b9637146" || req.Method != "polkadot_signMessage" || req.Data != BittensorSignRawData(bittensorTestMessage) || req.InjectedName != "" {
		t.Fatalf("sign request %+v", req)
	}
	r := session.HandleSignature(bittensorTestAliceSs58, bittensorTestSignature, bittensorTestNowMillis)
	if !r.Ok() || r.Proof.WalletId != BittensorWalletWalletConnect {
		t.Fatalf("walletconnect result: %+v", r)
	}
	// the extension transport carries no walletconnect fields
	talisman := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformWeb, BittensorWalletPurposeLogin)
	talisman.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	if req, _ := talisman.SignRequest(); req.Chain != "" || req.Method != "" {
		t.Fatalf("talisman request %+v", req)
	}
}

func TestBittensorWalletConnectBridgeSession(t *testing.T) {
	for _, platform := range []string{BittensorWalletPlatformIos, BittensorWalletPlatformAndroid, BittensorWalletPlatformWindows} {
		session := newBittensorTestSession(t, BittensorWalletWalletConnect, platform, BittensorWalletPurposeCreate)
		if session.Transport() != BittensorWalletTransportBrowserBridge {
			t.Fatalf("%s: transport %s", platform, session.Transport())
		}
		session.SetWalletConnectProjectId(" app-project ")
		session.ChallengeArgs(bittensorTestAliceSs58)
		if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
			t.Fatal(err)
		}
		bridgeUrl, err := session.BridgeUrl()
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(bridgeUrl)
		q := u.Query()
		if u.Scheme+"://"+u.Host+u.Path != "https://ur.io/wallet-connect" || q.Get("provider") != "bittensor" || q.Get("wallet") != "walletconnect" || q.Get("wc_project_id") != "app-project" ||
			q.Get("purpose") != "create" || q.Get("address") != bittensorTestAliceSs58 || q.Get("message") != bittensorTestMessage {
			t.Fatalf("%s: bridge url %s", platform, bridgeUrl)
		}
		values := url.Values{
			"address":   {bittensorTestAliceSs58},
			"signature": {bittensorTestSignature},
			"message":   {bittensorTestMessage},
			"purpose":   {"create"},
		}
		// a return naming another wallet is not this session's
		values.Set("wallet", "talisman")
		if r := session.HandleBridgeReturn(bittensorTestRedirectLink+"?"+values.Encode(), bittensorTestNowMillis); r.ErrorCode != BittensorWalletErrorUnsupportedWallet {
			t.Fatalf("%s: other wallet: %+v", platform, r)
		}
		values.Set("wallet", "walletconnect")
		r := session.HandleBridgeReturn(bittensorTestRedirectLink+"?"+values.Encode(), bittensorTestNowMillis+1)
		if !r.Ok() || r.Proof.WalletId != BittensorWalletWalletConnect || r.Proof.Purpose != "create" {
			t.Fatalf("%s: walletconnect return: %+v", platform, r)
		}
	}
	// the project id is only for the walletconnect page
	talisman := newBittensorTestSession(t, BittensorWalletTalisman, BittensorWalletPlatformMacos, BittensorWalletPurposeLogin)
	talisman.SetWalletConnectProjectId("app-project")
	talisman.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	bridgeUrl, _ := talisman.BridgeUrl()
	if u, _ := url.Parse(bridgeUrl); u.Query().Has("wc_project_id") {
		t.Fatalf("talisman bridge url carries a project id: %s", bridgeUrl)
	}
	// no app id: the page uses its own
	noId := newBittensorTestSession(t, BittensorWalletWalletConnect, BittensorWalletPlatformLinux, BittensorWalletPurposeLogin)
	noId.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis)
	bridgeUrl, _ = noId.BridgeUrl()
	if u, _ := url.Parse(bridgeUrl); u.Query().Has("wc_project_id") || u.Query().Get("wallet") != "walletconnect" {
		t.Fatalf("no-id bridge url %s", bridgeUrl)
	}
}

// Adding a Bittensor wallet as a sign-in method (POST /auth/add-auth) runs the
// same proof under its own purpose. The helper refused any purpose but
// login, create and connect, so an app could only borrow "login" for it, and
// an add return was then indistinguishable from a sign-in return. The add
// purpose is spelled out here ("add", the bridge's wire value) so the test
// compiles against the helper without it.
func TestBittensorWalletAddPurposeSession(t *testing.T) {
	for _, wallet := range []string{BittensorWalletTalisman, BittensorWalletTaoCom} {
		for _, platform := range []string{
			BittensorWalletPlatformWeb,
			BittensorWalletPlatformMacos,
			BittensorWalletPlatformWindows,
			BittensorWalletPlatformLinux,
			BittensorWalletPlatformIos,
			BittensorWalletPlatformAndroid,
		} {
			session, err := NewBittensorWalletSession(wallet, platform, "add", bittensorTestRedirectLink)
			if err != nil {
				t.Fatalf("%s on %s: %s", wallet, platform, err)
			}
			if session.Purpose() != "add" || session.ChallengeArgs("").Purpose != "add" {
				t.Fatalf("%s on %s: purpose %q", wallet, platform, session.Purpose())
			}
		}
	}
}

// An add return must never complete a sign-in, and a sign-in return must
// never be added: each session refuses the other's bridge return without
// changing state.
func TestBittensorWalletAddReturnNeverSignsIn(t *testing.T) {
	bridgeSession := func(purpose string) *BittensorWalletSession {
		session, err := NewBittensorWalletSession(BittensorWalletTalisman, BittensorWalletPlatformWindows, purpose, bittensorTestRedirectLink)
		if err != nil {
			t.Fatal(err)
		}
		session.ChallengeArgs("")
		if err := session.SetChallenge(bittensorTestChallenge(), bittensorTestNowMillis); err != nil {
			t.Fatal(err)
		}
		if _, err := session.BridgeUrl(); err != nil {
			t.Fatal(err)
		}
		return session
	}
	returnFor := func(purpose string) string {
		values := url.Values{
			"address":   {bittensorTestAliceSs58},
			"signature": {bittensorTestSignature},
			"message":   {bittensorTestMessage},
			"purpose":   {purpose},
			"wallet":    {BittensorWalletTalisman},
		}
		return bittensorTestRedirectLink + "?" + values.Encode()
	}

	for _, signIn := range []string{BittensorWalletPurposeLogin, BittensorWalletPurposeCreate} {
		session := bridgeSession(signIn)
		if r := session.HandleBridgeReturn(returnFor("add"), bittensorTestNowMillis); r.Ok() || r.ErrorCode != BittensorWalletErrorPurposeMismatch {
			t.Fatalf("%s session took an add return: %+v", signIn, r)
		}
		if session.State() != BittensorWalletStateAwaitingWallet {
			t.Fatalf("%s session state after an add return: %s", signIn, session.State())
		}
	}

	add := bridgeSession("add")
	if r := add.HandleBridgeReturn(returnFor(BittensorWalletPurposeLogin), bittensorTestNowMillis); r.Ok() || r.ErrorCode != BittensorWalletErrorPurposeMismatch {
		t.Fatalf("add session took a login return: %+v", r)
	}
	r := add.HandleBridgeReturn(returnFor("add"), bittensorTestNowMillis)
	if !r.Ok() || r.Proof.Purpose != "add" {
		t.Fatalf("add return: %+v", r)
	}
	auth := r.Proof.WalletAuthArgs()
	if auth.Blockchain != TAO || auth.PublicKey != bittensorTestAliceSs58 || auth.Message != bittensorTestMessage {
		t.Fatalf("wallet auth %+v", auth)
	}
}
