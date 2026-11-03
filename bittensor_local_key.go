//go:build !ios_extension && !js

package sdk

// A Bittensor coldkey made on this device, for a casual miner with no wallet and no TAO (apple
// BTEASY.md). The app holds the phrase and asks here for what it needs. URnetwork is not a wallet:
// the one thing this signs is a text message, for URnetwork's own payout flow, and the server only
// ever sees the address and the signature.
//
// The phrase has to open the same account in btcli, Talisman and Nova, so the format is theirs: a
// 12 word BIP39 phrase, the substrate-bip39 derivation (the mini secret comes from the phrase's
// entropy, not from the standard BIP39 seed), sr25519, and ss58 prefix 42. No password: wallets
// import a phrase without one. bittensor_local_key_test.go pins this against their vectors.
//
// Every call is stateless on purpose. Nothing here is cached or written: the phrase is not in
// LocalState or the device rpc, so it cannot reach the tunnel extension or a file. Go strings cannot
// be wiped, so a phrase passed in stays in memory until the runtime reuses it. Keep it in as few
// places as possible, for as short a time as possible, and claim no more than that.
//
// The build constraint keeps this and its libraries out of the two builds that must not have them.
// The tunnel extension must never hold a phrase, and its binary has a hard size ceiling
// (build/check_apple_size.sh). ur.io offers no local key, since a browser has no safe place for one.
// TestBittensorLocalKeyStaysOutOfTheExtensionAndTheBrowser holds both.
//
// All exported functions are pure and safe for concurrent use.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/ChainSafe/go-schnorrkel"
	bip39 "github.com/cosmos/go-bip39"

	"github.com/urnetwork/sdk/sn/ss58"
)

const (
	// 128 bits, which BIP39 writes as 12 words
	bittensorLocalKeyEntropyByteCount = 16
	bittensorLocalKeyWordCount        = 12
	// no password: what wallets assume when they import a bare phrase
	bittensorLocalKeyPassword = ""
	// the context the server and wallets verify sr25519 signatures in
	bittensorLocalKeySigningContext = "substrate"
	// wallets wrap a signed message in these, so a message can never be the bytes of a transaction
	bittensorLocalKeyBytesWrapPrefix = "<Bytes>"
	bittensorLocalKeyBytesWrapSuffix = "</Bytes>"
)

// Stable codes at the start of an error's text, in the sdk's "code: message" form.
const (
	// the phrase is not one this sdk could have made
	BittensorLocalKeyErrorInvalidPhrase = "invalid_recovery_phrase"
	// the system could not supply what a new key or a signature needs
	BittensorLocalKeyErrorFailed = "local_key_failed"
)

// These are the only errors the calls here answer, and they are fixed text. An error goes to logs
// and crash reports, so none may hold any part of a phrase: the library's own errors can quote a
// word, and they are dropped rather than wrapped.
var (
	errBittensorLocalKeyWordCount = errors.New(BittensorLocalKeyErrorInvalidPhrase + ": it does not have the expected number of words")
	errBittensorLocalKeyWord      = errors.New(BittensorLocalKeyErrorInvalidPhrase + ": it holds something that is not on the BIP39 list")
	errBittensorLocalKeyChecksum  = errors.New(BittensorLocalKeyErrorInvalidPhrase + ": its checksum does not match")
	errBittensorLocalKeyGenerate  = errors.New(BittensorLocalKeyErrorFailed + ": could not make a new key")
	errBittensorLocalKeySign      = errors.New(BittensorLocalKeyErrorFailed + ": could not sign")
)

// A new key: the secret and the account it opens. A struct because gomobile can return only one
// value beside an error.
type BittensorLocalKey struct {
	// the recovery phrase, 12 words separated by one space
	Mnemonic string `json:"mnemonic"`
	// ss58 prefix 42
	Address string `json:"address"`
}

// The address alone, so a key that reaches a log line through %v or %s does not carry its phrase.
func (self BittensorLocalKey) String() string {
	return "BittensorLocalKey(" + self.Address + ")"
}

// As String, for %#v.
func (self BittensorLocalKey) GoString() string {
	return self.String()
}

// A new key from the system's secure randomness. The caller must have the phrase safe, stored and
// read back or confirmed by the user, before it signs anything with it: earnings could otherwise be
// paid to a key nobody holds.
func BittensorLocalKeyGenerate() (*BittensorLocalKey, error) {
	entropy := make([]byte, bittensorLocalKeyEntropyByteCount)
	if _, err := rand.Read(entropy); err != nil {
		return nil, errBittensorLocalKeyGenerate
	}
	return bittensorLocalKeyFromEntropy(entropy)
}

func bittensorLocalKeyFromEntropy(entropy []byte) (*BittensorLocalKey, error) {
	if len(entropy) != bittensorLocalKeyEntropyByteCount {
		return nil, errBittensorLocalKeyGenerate
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, errBittensorLocalKeyGenerate
	}
	// through the same checks as a phrase read back later, so a key this returns is one they accept
	address, err := BittensorLocalKeyAddress(mnemonic)
	if err != nil {
		return nil, errBittensorLocalKeyGenerate
	}
	return &BittensorLocalKey{
		Mnemonic: mnemonic,
		Address:  address,
	}, nil
}

// The ss58 address the phrase opens, or an invalid phrase error.
func BittensorLocalKeyAddress(mnemonic string) (string, error) {
	miniSecret, err := bittensorLocalKeyMiniSecret(mnemonic)
	if err != nil {
		return "", err
	}
	// nil only if the library's expansion made a scalar it cannot read
	publicKey := miniSecret.Public()
	if publicKey == nil {
		return "", errBittensorLocalKeyGenerate
	}
	address, err := ss58.Encode(publicKey.Encode(), SnSs58Prefix)
	if err != nil {
		return "", errBittensorLocalKeyGenerate
	}
	return address, nil
}

// Signs the message's UTF-8 bytes, wrapped in <Bytes>...</Bytes> the way wallets do, in the
// "substrate" context. Always wrapped, even a message that already looks wrapped, so this can never
// make a signature over the bare bytes of a chain transaction. The result is 0x + 128 lowercase hex
// characters (NormalizeBittensorSignature's form). It differs on every call, since sr25519 signing is
// randomized, and every result verifies for the same address and message.
func BittensorLocalKeySign(mnemonic string, message string) (string, error) {
	miniSecret, err := bittensorLocalKeyMiniSecret(mnemonic)
	if err != nil {
		return "", err
	}
	wrapped := bittensorLocalKeyBytesWrapPrefix + message + bittensorLocalKeyBytesWrapSuffix
	transcript := schnorrkel.NewSigningContext([]byte(bittensorLocalKeySigningContext), []byte(wrapped))
	signature, err := miniSecret.ExpandEd25519().Sign(transcript)
	if err != nil {
		return "", errBittensorLocalKeySign
	}
	encoded := signature.Encode()
	return "0x" + hex.EncodeToString(encoded[:]), nil
}

// Checks the phrase, then derives the mini secret from its entropy. Only the exact form this sdk
// makes passes: lower case listed words, one space apart, nothing around them. Whitespace and case
// do not change the key, but nothing here reads a phrase a person typed, so anything else means what
// was stored is not what was written.
func bittensorLocalKeyMiniSecret(mnemonic string) (*schnorrkel.MiniSecretKey, error) {
	words := strings.Split(mnemonic, " ")
	if len(words) != bittensorLocalKeyWordCount {
		return nil, errBittensorLocalKeyWordCount
	}
	for _, word := range words {
		if _, listed := bip39.ReverseWordMap[word]; !listed {
			return nil, errBittensorLocalKeyWord
		}
	}
	if _, err := bip39.MnemonicToByteArray(mnemonic); err != nil {
		return nil, errBittensorLocalKeyChecksum
	}
	miniSecret, err := schnorrkel.MiniSecretKeyFromMnemonic(mnemonic, bittensorLocalKeyPassword)
	if err != nil {
		return nil, errBittensorLocalKeyChecksum
	}
	return miniSecret, nil
}
