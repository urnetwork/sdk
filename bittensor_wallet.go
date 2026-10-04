package sdk

// The Bittensor wallet-connect session helper: the protocol every app runs to
// prove it holds a Bittensor (TAO) coldkey, for sign-in (POST /auth/login and
// /auth/network-create) and for the subnet payout wallet (POST /sn/wallet).
// The apps own only the wallet transport (open a page, read an extension,
// show a form); everything that decides what is signed and whether a result
// is acceptable lives here.
//
// Supported wallets (product decision 2026-10-03, UPGRADE.md §4.6):
//   - Talisman. Documented interface: the browser extension injects
//     window.injectedWeb3["talisman"] (the Polkadot.js extension API:
//     enable, accounts.get, signer.signRaw). Talisman publishes no deep link
//     or WalletConnect interface for its mobile app.
//   - TAO.com. The extension and the iOS app publish no programmatic
//     interface (no injection name, deep link scheme, WalletConnect or SDK in
//     the official docs or support center), so TAO.com is manual entry: the
//     user pastes the coldkey address and a signature over the challenge.
//     The same manual entry serves any other wallet.
//   - WalletConnect (product decision 2026-10-04): any substrate wallet that
//     speaks WalletConnect v2 (Nova, Nightly, ...), over the polkadot
//     namespace (polkadot_signMessage) on the Bittensor finney chain.
//
// Transport per platform (BittensorWalletTransportFor):
//
//	               web            macos/windows/linux   ios/android
//	talisman       extension      browser_bridge        manual
//	taocom         manual         manual                manual
//	walletconnect  walletconnect  browser_bridge        browser_bridge
//
// On the apps, WalletConnect runs on the bridge page (a QR on desktop, an
// "Open wallet" deep link on a phone) with the app's configured
// WalletConnect project id (SetWalletConnectProjectId), as the pre-helper
// bridge did.
//
// browser_bridge opens https://ur.io/bittensor-connect (the Bittensor-only
// bridge page; /wallet-connect is the Solana page and keeps a provider=bittensor
// path for app versions before this helper) in the system browser, where the
// page drives the Talisman extension and returns to the app on the
// app's own registered redirect link (the caller passes it: the apps keep
// their existing schemes, e.g. urnetwork://bittensor-sign-message on apple
// and the desktop apps, ur://bittensor-sign-message on android).
//
// The proof is always the same: an sr25519 signature by the coldkey over the
// exact single-use message_template issued by POST /auth/wallet-challenge
// (blockchain TAO). The server verifies it (model.UseWalletAuthChallenge);
// nothing here replaces that check. The client-side checks exist so a wrong
// result (another challenge, another purpose, a different account than the
// one the user typed, a malformed address or signature, an expired
// challenge) is refused with a precise reason before a round trip.
//
// All exported functions are pure and safe for concurrent use. A
// BittensorWalletSession is safe for concurrent use.

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	BittensorWalletTalisman = "talisman"
	BittensorWalletTaoCom   = "taocom"
	// any WalletConnect v2 substrate wallet (Nova, Nightly, ...)
	BittensorWalletWalletConnect = "walletconnect"
)

const (
	// the wallet's browser extension, in the same browser as the page
	BittensorWalletTransportExtension = "extension"
	// open BittensorWalletBridgeUrl in the system browser (which has the
	// extension) and receive the result on the app's redirect link
	BittensorWalletTransportBrowserBridge = "browser_bridge"
	// the user pastes the address and a signature over the challenge
	BittensorWalletTransportManual = "manual"
	// a WalletConnect v2 session run by the page itself (web)
	BittensorWalletTransportWalletConnect = "walletconnect"
)

const (
	BittensorWalletPlatformWeb     = "web"
	BittensorWalletPlatformIos     = "ios"
	BittensorWalletPlatformAndroid = "android"
	BittensorWalletPlatformMacos   = "macos"
	BittensorWalletPlatformWindows = "windows"
	BittensorWalletPlatformLinux   = "linux"
)

// Purposes, echoed through the bridge so a return reaches the flow that
// asked. "login" and "create" sign in (create = the second signature for a
// new network); "connect" proves the subnet payout wallet.
const (
	BittensorWalletPurposeLogin   = "login"
	BittensorWalletPurposeCreate  = "create"
	BittensorWalletPurposeConnect = "connect"
)

const (
	BittensorWalletStateIdle           = "idle"
	BittensorWalletStateChallenge      = "challenge"
	BittensorWalletStateAwaitingWallet = "awaiting_wallet"
	BittensorWalletStateSigned         = "signed"
	BittensorWalletStateFailed         = "failed"
	BittensorWalletStateCancelled      = "cancelled"
)

// Stable codes in BittensorWalletResult.ErrorCode, for localization.
const (
	BittensorWalletErrorWallet              = "wallet_error"
	BittensorWalletErrorNotReturn           = "not_bittensor_return"
	BittensorWalletErrorNoChallenge         = "no_challenge"
	BittensorWalletErrorInvalidChallenge    = "invalid_challenge"
	BittensorWalletErrorExpired             = "challenge_expired"
	BittensorWalletErrorPurposeMismatch     = "purpose_mismatch"
	BittensorWalletErrorMessageMismatch     = "message_mismatch"
	BittensorWalletErrorInvalidAddress      = SnErrorCodeInvalidAddress
	BittensorWalletErrorAddressMismatch     = "address_mismatch"
	BittensorWalletErrorInvalidSignature    = "invalid_signature"
	BittensorWalletErrorUnsupportedWallet   = "unsupported_wallet"
	BittensorWalletErrorUnsupportedPlatform = "unsupported_platform"
	BittensorWalletErrorWrongTransport      = "wrong_transport"
	BittensorWalletErrorNotAwaiting         = "not_awaiting_wallet"
)

const (
	BittensorWalletBridgeUrl = "https://ur.io/bittensor-connect"
	// the Polkadot.js extension api name Talisman injects under
	// window.injectedWeb3
	BittensorTalismanInjectedName = "talisman"
	// the app name shown by the extension's authorization prompt
	BittensorWalletDappName = "URnetwork"

	bittensorChallengeHeader = "Sign in to URnetwork"
	// the server's challenge lifetime when the result omits expires_in
	bittensorChallengeDefaultExpiresInSeconds = 300
	// sr25519 signatures are 64 bytes
	bittensorSignatureByteCount = 64
)

const (
	// the WalletConnect polkadot-namespace chain id of Bittensor finney
	// (CAIP-13: the first 32 hex characters of the genesis hash)
	BittensorWalletConnectChain  = "polkadot:2f0555cc76fc2840a25a6ea3b9637146"
	BittensorWalletConnectMethod = "polkadot_signMessage"
)

// BittensorWalletIdList returns the supported wallet ids in display order.
func BittensorWalletIdList() *StringList {
	walletIds := NewStringList()
	walletIds.Add(BittensorWalletTalisman)
	walletIds.Add(BittensorWalletTaoCom)
	walletIds.Add(BittensorWalletWalletConnect)
	return walletIds
}

// BittensorWalletDisplayName is the wallet's product name ("" if unknown).
// Product names are not translated.
func BittensorWalletDisplayName(walletId string) string {
	switch walletId {
	case BittensorWalletTalisman:
		return "Talisman"
	case BittensorWalletTaoCom:
		return "TAO.com"
	case BittensorWalletWalletConnect:
		return "WalletConnect"
	default:
		return ""
	}
}

// BittensorWalletInjectedName is the window.injectedWeb3 key of the wallet's
// documented extension, or "" when the wallet documents none.
func BittensorWalletInjectedName(walletId string) string {
	if walletId == BittensorWalletTalisman {
		return BittensorTalismanInjectedName
	}
	return ""
}

// BittensorWalletTransportFor picks the transport for a wallet on a
// platform. Returns "" for an unknown wallet or platform.
func BittensorWalletTransportFor(walletId string, platform string) string {
	switch walletId {
	case BittensorWalletTalisman, BittensorWalletTaoCom, BittensorWalletWalletConnect:
	default:
		return ""
	}
	switch platform {
	case BittensorWalletPlatformWeb:
		switch walletId {
		case BittensorWalletTalisman:
			return BittensorWalletTransportExtension
		case BittensorWalletWalletConnect:
			return BittensorWalletTransportWalletConnect
		default:
			return BittensorWalletTransportManual
		}
	case BittensorWalletPlatformMacos, BittensorWalletPlatformWindows, BittensorWalletPlatformLinux:
		switch walletId {
		case BittensorWalletTalisman, BittensorWalletWalletConnect:
			return BittensorWalletTransportBrowserBridge
		default:
			return BittensorWalletTransportManual
		}
	case BittensorWalletPlatformIos, BittensorWalletPlatformAndroid:
		// Talisman and TAO.com document no mobile deep link; WalletConnect
		// pairs on the bridge page, which deep links to the wallet app
		if walletId == BittensorWalletWalletConnect {
			return BittensorWalletTransportBrowserBridge
		}
		return BittensorWalletTransportManual
	default:
		return ""
	}
}

// BittensorChallengeMessage is a parsed /auth/wallet-challenge
// message_template.
type BittensorChallengeMessage struct {
	Challenge string
	// unix seconds
	Timestamp int64
}

// ParseBittensorChallengeMessage checks the message is exactly the server's
// three LF-separated lines (header, "Challenge: ", "Timestamp: "), the same
// rule as the server's parser. A CRLF, an extra line or a <Bytes> wrapper is
// refused: the server would reject a signature over it.
func ParseBittensorChallengeMessage(message string) (*BittensorChallengeMessage, error) {
	lines := strings.Split(message, "\n")
	if len(lines) != 3 ||
		lines[0] != bittensorChallengeHeader ||
		!strings.HasPrefix(lines[1], "Challenge: ") ||
		!strings.HasPrefix(lines[2], "Timestamp: ") {
		return nil, fmt.Errorf("%s: not a wallet challenge message", BittensorWalletErrorInvalidChallenge)
	}
	challenge := strings.TrimPrefix(lines[1], "Challenge: ")
	if challenge == "" {
		return nil, fmt.Errorf("%s: empty challenge", BittensorWalletErrorInvalidChallenge)
	}
	timestamp, err := strconv.ParseInt(strings.TrimPrefix(lines[2], "Timestamp: "), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s: bad timestamp", BittensorWalletErrorInvalidChallenge)
	}
	return &BittensorChallengeMessage{
		Challenge: challenge,
		Timestamp: timestamp,
	}, nil
}

// BittensorSignRawData is the signRaw / polkadot_signMessage `data` for a
// message: its UTF-8 bytes as 0x-prefixed lowercase hex. With type "bytes"
// the extension wraps it in <Bytes>…</Bytes> before signing, which the
// server accepts; the message submitted is always the unwrapped text.
func BittensorSignRawData(message string) string {
	return "0x" + hex.EncodeToString([]byte(message))
}

// NormalizeBittensorSignature returns the signature as 0x + 128 lowercase hex
// characters, or "" when it is not a 64-byte hex signature. Accepts the
// wallet forms: with or without 0x, either case, surrounding whitespace.
func NormalizeBittensorSignature(signature string) string {
	s := strings.TrimSpace(signature)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		s = s[2:]
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != bittensorSignatureByteCount {
		return ""
	}
	return "0x" + hex.EncodeToString(raw)
}

// BittensorSignRequest is what the extension transport passes to
// injectedWeb3[InjectedName].enable(DappName) and then signer.signRaw, or
// what the web walletconnect transport sends as Method on Chain with params
// { address, message: Data }.
type BittensorSignRequest struct {
	WalletId     string
	InjectedName string
	DappName     string
	// walletconnect transport only
	Chain  string
	Method string
	// the account to sign with; "" = the extension's first account
	Address string
	// signRaw { address, data, type }
	Data string
	Type string
}

// BittensorWalletProof is a signed challenge ready to submit.
type BittensorWalletProof struct {
	WalletId string
	Purpose  string
	// ss58, prefix 42
	Address string
	// the issued message_template, byte for byte
	Message string
	// 0x + 128 hex
	Signature string
}

// WalletAuthArgs is the wallet_auth object for /auth/login,
// /auth/network-create and /auth/add-auth.
func (self *BittensorWalletProof) WalletAuthArgs() *WalletAuthArgs {
	return &WalletAuthArgs{
		PublicKey:  self.Address,
		Signature:  self.Signature,
		Message:    self.Message,
		Blockchain: TAO,
	}
}

// BittensorWalletResult is the outcome of handing a wallet result to the
// session: a Proof, or an ErrorCode (BittensorWalletError*) with a detail
// message. ErrorMessage carries the wallet's own text for wallet_error.
type BittensorWalletResult struct {
	Proof        *BittensorWalletProof
	ErrorCode    string
	ErrorMessage string
}

func (self *BittensorWalletResult) Ok() bool {
	return self.Proof != nil && self.ErrorCode == ""
}

// BittensorWalletReturn is the parsed query of a bridge hand-back:
// ?address&signature&message&purpose&wallet on success,
// ?errorCode&errorMessage[&purpose] on failure.
type BittensorWalletReturn struct {
	Address      string
	Signature    string
	Message      string
	Purpose      string
	WalletId     string
	ErrorCode    string
	ErrorMessage string
}

// ParseBittensorWalletReturn parses a bridge hand-back uri that starts with
// redirectLink (scheme, host and path compared case-insensitively for the
// scheme and host). The page builds the query with URLSearchParams, so "+"
// is a space.
func ParseBittensorWalletReturn(uri string, redirectLink string) (*BittensorWalletReturn, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorNotReturn, err)
	}
	r, err := url.Parse(redirectLink)
	if err != nil || r.Scheme == "" {
		return nil, fmt.Errorf("%s: bad redirect link", BittensorWalletErrorNotReturn)
	}
	if !strings.EqualFold(u.Scheme, r.Scheme) ||
		!strings.EqualFold(u.Host, r.Host) ||
		strings.TrimSuffix(u.Path, "/") != strings.TrimSuffix(r.Path, "/") {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorNotReturn, uri)
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorNotReturn, err)
	}
	return &BittensorWalletReturn{
		Address:      values.Get("address"),
		Signature:    values.Get("signature"),
		Message:      values.Get("message"),
		Purpose:      values.Get("purpose"),
		WalletId:     values.Get("wallet"),
		ErrorCode:    values.Get("errorCode"),
		ErrorMessage: values.Get("errorMessage"),
	}, nil
}

// BittensorWalletSession runs one proof: pick a wallet, take a challenge,
// hand the wallet's answer back, get a proof or a precise refusal. One
// session per challenge; a new attempt is a new session (the server
// consumes each challenge once).
//
// Flow:
//
//	session := NewBittensorWalletSession(wallet, platform, purpose, redirectLink)
//	api.AuthWalletChallenge(session.ChallengeArgs(typedAddress), ...)
//	session.SetChallenge(result, nowMillis)
//	switch session.Transport():
//	case extension:      req := session.SignRequest(); ...signRaw...; session.HandleSignature(addr, sig, now)
//	case browser_bridge: open session.BridgeUrl(); on return session.HandleBridgeReturn(uri, now)
//	case manual:         show session.Message(); session.HandleSignature(typedAddr, pastedSig, now)
//
// Times are passed in (unix millis) so the expiry rule is deterministic.
type BittensorWalletSession struct {
	walletId     string
	platform     string
	transport    string
	purpose      string
	redirectLink string

	stateLock              sync.Mutex
	walletConnectProjectId string
	state                  string
	expectedAddress        string
	message                string
	expiresAtMillis        int64
	proof                  *BittensorWalletProof
	errorCode              string
}

// NewBittensorWalletSession checks the wallet, platform and purpose.
// redirectLink is the app's registered hand-back link (only the
// browser_bridge transport uses it).
func NewBittensorWalletSession(walletId string, platform string, purpose string, redirectLink string) (*BittensorWalletSession, error) {
	transport := BittensorWalletTransportFor(walletId, BittensorWalletPlatformWeb)
	if transport == "" {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorUnsupportedWallet, walletId)
	}
	transport = BittensorWalletTransportFor(walletId, platform)
	if transport == "" {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorUnsupportedPlatform, platform)
	}
	switch purpose {
	case BittensorWalletPurposeLogin, BittensorWalletPurposeCreate, BittensorWalletPurposeConnect:
	default:
		return nil, fmt.Errorf("unknown purpose: %s", purpose)
	}
	if transport == BittensorWalletTransportBrowserBridge {
		r, err := url.Parse(redirectLink)
		if err != nil || r.Scheme == "" {
			return nil, fmt.Errorf("the browser bridge needs a redirect link")
		}
	}
	return &BittensorWalletSession{
		walletId:     walletId,
		platform:     platform,
		transport:    transport,
		purpose:      purpose,
		redirectLink: redirectLink,
		state:        BittensorWalletStateIdle,
	}, nil
}

func (self *BittensorWalletSession) WalletId() string {
	return self.walletId
}

func (self *BittensorWalletSession) Platform() string {
	return self.platform
}

func (self *BittensorWalletSession) Transport() string {
	return self.transport
}

func (self *BittensorWalletSession) Purpose() string {
	return self.purpose
}

func (self *BittensorWalletSession) State() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.state
}

// ErrorCode is the code of the last refusal ("" if none).
func (self *BittensorWalletSession) ErrorCode() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.errorCode
}

// Message is the challenge text to sign ("" before SetChallenge). Manual
// entry shows it for the user to sign elsewhere.
func (self *BittensorWalletSession) Message() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.message
}

// ExpiresAtMillis is when the challenge expires (0 before SetChallenge).
func (self *BittensorWalletSession) ExpiresAtMillis() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.expiresAtMillis
}

// Proof is the accepted proof (nil until signed).
func (self *BittensorWalletSession) Proof() *BittensorWalletProof {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.proof
}

// ChallengeArgs is the /auth/wallet-challenge request. A non-empty
// expectedAddress (the address the user typed) binds the challenge to it on
// the server, and the session then refuses any other signing account.
func (self *BittensorWalletSession) ChallengeArgs(expectedAddress string) *AuthWalletChallengeArgs {
	expectedAddress = strings.TrimSpace(expectedAddress)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.expectedAddress = expectedAddress
	args := &AuthWalletChallengeArgs{
		Blockchain: TAO,
		Purpose:    self.purpose,
	}
	if expectedAddress != "" {
		args.WalletAddress = expectedAddress
	}
	return args
}

// SetChallenge takes the /auth/wallet-challenge result. The message must be
// a well-formed challenge; the expiry is nowMillis + expires_in.
func (self *BittensorWalletSession) SetChallenge(result *AuthWalletChallengeResult, nowMillis int64) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if result == nil || result.Error != nil || result.MessageTemplate == "" {
		self.failWithLock(BittensorWalletErrorNoChallenge)
		return fmt.Errorf("%s: the server did not return a challenge", BittensorWalletErrorNoChallenge)
	}
	if _, err := ParseBittensorChallengeMessage(result.MessageTemplate); err != nil {
		self.failWithLock(BittensorWalletErrorInvalidChallenge)
		return err
	}
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = bittensorChallengeDefaultExpiresInSeconds
	}
	self.message = result.MessageTemplate
	self.expiresAtMillis = nowMillis + expiresIn*1000
	self.proof = nil
	self.errorCode = ""
	self.state = BittensorWalletStateChallenge
	return nil
}

// SetWalletConnectProjectId sets the app's WalletConnect Cloud project id,
// passed to the bridge page as wc_project_id for the walletconnect wallet
// ("" = the page's own configured id).
func (self *BittensorWalletSession) SetWalletConnectProjectId(projectId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.walletConnectProjectId = strings.TrimSpace(projectId)
}

// SignRequest is the extension transport's signRaw request, or the web
// walletconnect transport's polkadot_signMessage request. It moves the
// session to awaiting_wallet.
func (self *BittensorWalletSession) SignRequest() (*BittensorSignRequest, error) {
	if self.transport != BittensorWalletTransportExtension && self.transport != BittensorWalletTransportWalletConnect {
		return nil, fmt.Errorf("%s: %s", BittensorWalletErrorWrongTransport, self.transport)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.message == "" {
		return nil, fmt.Errorf("%s: no challenge", BittensorWalletErrorNoChallenge)
	}
	self.state = BittensorWalletStateAwaitingWallet
	request := &BittensorSignRequest{
		WalletId:     self.walletId,
		InjectedName: BittensorWalletInjectedName(self.walletId),
		DappName:     BittensorWalletDappName,
		Address:      self.expectedAddress,
		Data:         BittensorSignRawData(self.message),
		Type:         "bytes",
	}
	if self.transport == BittensorWalletTransportWalletConnect {
		request.Chain = BittensorWalletConnectChain
		request.Method = BittensorWalletConnectMethod
	}
	return request, nil
}

// BridgeUrl is the ur.io page the browser_bridge transport opens:
//
//	https://ur.io/bittensor-connect?provider=bittensor&method=signMessage
//	  &wallet=<id>&message=<text>&purpose=<purpose>&redirect_link=<link>
//	  [&address=<expected ss58>] [&wc_project_id=<id>, walletconnect only]
//
// `wallet` makes the page use only that wallet's extension. It moves the
// session to awaiting_wallet.
func (self *BittensorWalletSession) BridgeUrl() (string, error) {
	if self.transport != BittensorWalletTransportBrowserBridge {
		return "", fmt.Errorf("%s: %s", BittensorWalletErrorWrongTransport, self.transport)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.message == "" {
		return "", fmt.Errorf("%s: no challenge", BittensorWalletErrorNoChallenge)
	}
	values := url.Values{}
	values.Set("provider", "bittensor")
	values.Set("method", "signMessage")
	values.Set("wallet", self.walletId)
	values.Set("message", self.message)
	values.Set("purpose", self.purpose)
	values.Set("redirect_link", self.redirectLink)
	if self.expectedAddress != "" {
		values.Set("address", self.expectedAddress)
	}
	if self.walletId == BittensorWalletWalletConnect && self.walletConnectProjectId != "" {
		values.Set("wc_project_id", self.walletConnectProjectId)
	}
	self.state = BittensorWalletStateAwaitingWallet
	return BittensorWalletBridgeUrl + "?" + values.Encode(), nil
}

// IsReturn reports whether uri is a hand-back on this session's redirect
// link (for routing an incoming url to the session).
func (self *BittensorWalletSession) IsReturn(uri string) bool {
	if self.redirectLink == "" {
		return false
	}
	_, err := ParseBittensorWalletReturn(uri, self.redirectLink)
	return err == nil
}

// HandleBridgeReturn checks a browser_bridge hand-back. A return for
// another purpose is refused without changing the session (it belongs to
// another flow).
func (self *BittensorWalletSession) HandleBridgeReturn(uri string, nowMillis int64) *BittensorWalletResult {
	r, err := ParseBittensorWalletReturn(uri, self.redirectLink)
	if err != nil {
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorNotReturn, ErrorMessage: err.Error()}
	}
	if r.Purpose != "" && r.Purpose != self.purpose {
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorPurposeMismatch, ErrorMessage: r.Purpose}
	}
	if r.ErrorCode != "" || r.ErrorMessage != "" {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.state != BittensorWalletStateAwaitingWallet {
			return &BittensorWalletResult{ErrorCode: BittensorWalletErrorNotAwaiting}
		}
		self.failWithLock(BittensorWalletErrorWallet)
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorWallet, ErrorMessage: r.ErrorMessage}
	}
	if r.Purpose == "" {
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorPurposeMismatch}
	}
	if r.WalletId != "" && r.WalletId != self.walletId {
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorUnsupportedWallet, ErrorMessage: r.WalletId}
	}
	return self.accept(r.Address, r.Signature, r.Message, true, nowMillis)
}

// HandleSignature checks an extension signRaw result or a manual entry
// (typed address, pasted signature) against the issued challenge.
func (self *BittensorWalletSession) HandleSignature(address string, signature string, nowMillis int64) *BittensorWalletResult {
	if self.transport == BittensorWalletTransportBrowserBridge {
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorWrongTransport, ErrorMessage: self.transport}
	}
	return self.accept(address, signature, "", false, nowMillis)
}

// Cancel abandons the session; a late return is then refused.
func (self *BittensorWalletSession) Cancel() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.state == BittensorWalletStateSigned {
		return
	}
	self.state = BittensorWalletStateCancelled
}

func (self *BittensorWalletSession) accept(address string, signature string, returnedMessage string, checkMessage bool, nowMillis int64) *BittensorWalletResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	fail := func(code string, detail string) *BittensorWalletResult {
		self.failWithLock(code)
		return &BittensorWalletResult{ErrorCode: code, ErrorMessage: detail}
	}

	switch self.state {
	case BittensorWalletStateAwaitingWallet:
	case BittensorWalletStateChallenge:
		// manual entry needs no wallet hop
		if self.transport != BittensorWalletTransportManual {
			return &BittensorWalletResult{ErrorCode: BittensorWalletErrorNotAwaiting}
		}
	case BittensorWalletStateFailed:
		// manual entry may be corrected and retried against the same challenge
		if self.transport != BittensorWalletTransportManual || self.message == "" {
			return &BittensorWalletResult{ErrorCode: BittensorWalletErrorNotAwaiting}
		}
	default:
		return &BittensorWalletResult{ErrorCode: BittensorWalletErrorNotAwaiting}
	}
	if self.message == "" {
		return fail(BittensorWalletErrorNoChallenge, "")
	}
	if self.expiresAtMillis <= nowMillis {
		return fail(BittensorWalletErrorExpired, "")
	}
	// the bridge echoes the message it signed; it must be the one issued
	if checkMessage && returnedMessage != self.message {
		return fail(BittensorWalletErrorMessageMismatch, "")
	}
	address = strings.TrimSpace(address)
	if !ValidateSs58(address) {
		return fail(BittensorWalletErrorInvalidAddress, address)
	}
	if self.expectedAddress != "" && address != self.expectedAddress {
		return fail(BittensorWalletErrorAddressMismatch, address)
	}
	normalizedSignature := NormalizeBittensorSignature(signature)
	if normalizedSignature == "" {
		return fail(BittensorWalletErrorInvalidSignature, "")
	}
	self.proof = &BittensorWalletProof{
		WalletId:  self.walletId,
		Purpose:   self.purpose,
		Address:   address,
		Message:   self.message,
		Signature: normalizedSignature,
	}
	self.errorCode = ""
	self.state = BittensorWalletStateSigned
	return &BittensorWalletResult{Proof: self.proof}
}

func (self *BittensorWalletSession) failWithLock(code string) {
	self.errorCode = code
	self.state = BittensorWalletStateFailed
}
