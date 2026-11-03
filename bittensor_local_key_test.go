//go:build !ios_extension && !js

// A key made on the device has to open the same account in btcli, Talisman and Nova, and the
// server has to accept its signatures. So every expected value here comes from another
// implementation, none from this sdk. Every phrase is published test data: nobody's funds are
// behind any of them, and none may ever be funded.
package sdk

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	bip39 "github.com/cosmos/go-bip39"

	"github.com/urnetwork/sdk/sn/ss58"
)

const (
	bittensorLocalKeyTestSubkeySource     = "subkey README, https://github.com/paritytech/polkadot-sdk/blob/master/substrate/bin/utils/subkey/README.md"
	bittensorLocalKeyTestSpCoreSource     = "sp-core DEV_PHRASE and DEV_ADDRESS, https://github.com/paritytech/polkadot-sdk/blob/master/substrate/primitives/core/src/crypto.rs"
	bittensorLocalKeyTestPolkadotJsSource = "@polkadot/util-crypto toMiniSecret.spec.ts, https://github.com/polkadot-js/common/blob/master/packages/util-crypto/src/mnemonic/toMiniSecret.spec.ts"
	bittensorLocalKeyTestInterfaceSource  = "py-substrate-interface test_keypair.py, https://github.com/polkascan/py-substrate-interface/blob/master/test/test_keypair.py"
	// The same output from the three tools, run offline on 2026-11-02, for phrases whose address no
	// source publishes: `btcli wallet regen-coldkey` (bittensor-cli 9.23.2, bittensor-wallet 4.1.0)
	// in a scratch wallet path, Keypair.create_from_mnemonic (substrate-interface 1.8.1), and
	// Keyring.addFromUri (@polkadot/keyring 14.0.3).
	bittensorLocalKeyTestRecordedSource = "recorded from btcli, substrate-interface and @polkadot/keyring"
)

// A phrase and what each source says it opens, with no password. "" is what the source does not give.
type bittensorLocalKeyTestAccount struct {
	source        string
	phrase        string
	miniSecretHex string
	publicKeyHex  string
	address       string
	// the account published under another network prefix
	polkadotAddress string
}

var bittensorLocalKeyTestAccounts = []bittensorLocalKeyTestAccount{
	{
		source:        bittensorLocalKeyTestSubkeySource,
		phrase:        "hotel forest jar hover kite book view eight stuff angle legend defense",
		miniSecretHex: "a05c75731970cc7868a2fb7cb577353cd5b31f62dccced92c441acd8fee0c92d",
		publicKeyHex:  "fec70cfbf1977c6965b5af10a4534a6a35d548eb14580594d0bc543286892515",
		address:       "5Hpm9fq3W3dQgwWpAwDS2ZHKAdnk86QRCu7iX4GnmDxycrte",
	},
	{
		source:  bittensorLocalKeyTestSubkeySource,
		phrase:  "soup lyrics media market way crouch elevator put moon useful question wide",
		address: "5Fe4sqj2K4fRuzEGvToi4KATqZfiDU7TqynjXG6PZE2dxwyh",
	},
	{
		source:  bittensorLocalKeyTestSpCoreSource,
		phrase:  "bottom drive obey lake curtain smoke basket hold race lonely fit walk",
		address: "5DfhGyQdFobKM8NsWvEeAKk5EQQgYe9AydgJ7rMB6E1EqRzV",
	},
	{
		source:        bittensorLocalKeyTestPolkadotJsSource,
		phrase:        "seed sock milk update focus rotate barely fade car face mechanic mercy",
		miniSecretHex: "4d1ab2a57929edfd018aaa974e62ed557e3f54b4104acabedf73c8f5a1dbb029",
	},
	{
		source:          bittensorLocalKeyTestInterfaceSource,
		phrase:          "old leopard transfer rib spatial phone calm indicate online fire caution review",
		publicKeyHex:    "e4359ad3e2716c539a1d663ebd0a51bdc5c98a12e663bb4c4402db47828c9446",
		polkadotAddress: "16ADqpMa4yzfmWs3nuTSMhfZ2ckeGtvqhPWCNqECEGDcGgU2",
	},
	{
		source:       bittensorLocalKeyTestRecordedSource,
		phrase:       "old leopard transfer rib spatial phone calm indicate online fire caution review",
		publicKeyHex: "e4359ad3e2716c539a1d663ebd0a51bdc5c98a12e663bb4c4402db47828c9446",
		address:      "5HDvhV6WDCjCKyrXqGQSDYqQAzkzabNhctmiDYEqgBC66BsX",
	},
	{
		source:       bittensorLocalKeyTestRecordedSource,
		phrase:       "seed sock milk update focus rotate barely fade car face mechanic mercy",
		publicKeyHex: "9c25e4a0d216aa8b1b2f4d3a2eb9dc85c16272438ab81fc73e20b7603ed22531",
		address:      "5FbSap4BsWfjyRhCchoVdZHkDnmDm3NEgLZ25mesq4aw2WvX",
	},
	{
		// all-zero entropy, the shortest big integer the phrase decoding handles
		source:       bittensorLocalKeyTestRecordedSource,
		phrase:       "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		publicKeyHex: "66933bd1f37070ef87bd1198af3dacceb095237f803f3d32b173e6b425ed7972",
		address:      "5EPCUjPxiHAcNooYipQFWr9NmmXJKpNG5RhcntXwbtUySrgH",
	},
	{
		source:       bittensorLocalKeyTestRecordedSource,
		phrase:       "legal winner thank year wave sausage worth useful legal winner thank yellow",
		publicKeyHex: "625e25364c7b68e0a83065ccb40afed43f8fe933e669b24f3d69a57eddb3b715",
		address:      "5EHgWw2Af1pnoc7f1A8bfmM97W3DAYW8xr82RfhLL9oAabAe",
	},
	{
		// all-one entropy, the longest
		source:       bittensorLocalKeyTestRecordedSource,
		phrase:       "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong",
		publicKeyHex: "fefd19b87ac6f83c8a3cc4e9603a00d4a9e6f8322a625786300380722550c47d",
		address:      "5Hq3Cveh6yM1Do6ijhATJ8y9rzk6sSkuuuUwQF5k5ChLqodv",
	},
}

// The phrase opens each source's account.
func TestBittensorLocalKeyAddressMatchesOtherImplementations(t *testing.T) {
	for _, account := range bittensorLocalKeyTestAccounts {
		address, err := BittensorLocalKeyAddress(account.phrase)
		if err != nil {
			t.Fatalf("%s: %v", account.source, err)
		}
		if account.address != "" && address != account.address {
			t.Errorf("%s: address %s, want %s", account.source, address, account.address)
		}
		publicKey, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
		if err != nil {
			t.Fatalf("%s: the address %s does not decode under prefix 42: %v", account.source, address, err)
		}
		if account.publicKeyHex != "" && hex.EncodeToString(publicKey[:]) != account.publicKeyHex {
			t.Errorf("%s: public key %x, want %s", account.source, publicKey, account.publicKeyHex)
		}
		if account.polkadotAddress != "" {
			polkadotPublicKey, _, err := ss58.Decode(account.polkadotAddress)
			if err != nil {
				t.Fatal(err)
			}
			if publicKey != polkadotPublicKey {
				t.Errorf("%s: public key %x, published as %x", account.source, publicKey, polkadotPublicKey)
			}
		}
		if account.miniSecretHex != "" {
			miniSecret, err := bittensorLocalKeyMiniSecret(account.phrase)
			if err != nil {
				t.Fatal(err)
			}
			encoded := miniSecret.Encode()
			if hex.EncodeToString(encoded[:]) != account.miniSecretHex {
				t.Errorf("%s: mini secret %x, want %s", account.source, encoded, account.miniSecretHex)
			}
		}
	}
}

// The 12-word vectors of the substrate-bip39 crate, the derivation Substrate tools use: each phrase
// holds this entropy, and the mini secret comes from that entropy and not from the standard BIP39
// seed. The crate's table is computed with the password "Substrate", which this key never takes,
// so the library is asked directly. https://github.com/paritytech/substrate-bip39/blob/master/src/lib.rs
var bittensorLocalKeyTestSubstrateBip39Vectors = []struct {
	phrase     string
	entropyHex string
	seedHex    string
}{
	{"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about", "00000000000000000000000000000000", "44e9d125f037ac1d51f0a7d3649689d422c2af8b1ec8e00d71db4d7bf6d127e33f50c3d5c84fa3e5399c72d6cbbbbc4a49bf76f76d952f479d74655a2ef2d453"},
	{"legal winner thank year wave sausage worth useful legal winner thank yellow", "7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f", "4313249608fe8ac10fd5886c92c4579007272cb77c21551ee5b8d60b780416850f1e26c1f4b8d88ece681cb058ab66d6182bc2ce5a03181f7b74c27576b5c8bf"},
	{"letter advice cage absurd amount doctor acoustic avoid letter advice cage above", "80808080808080808080808080808080", "27f3eb595928c60d5bc91a4d747da40ed236328183046892ed6cd5aa9ae38122acd1183adf09a89839acb1e6eaa7fb563cc958a3f9161248d5a036e0d0af533d"},
	{"zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong", "ffffffffffffffffffffffffffffffff", "227d6256fd4f9ccaf06c45eaa4b2345969640462bbb00c5f51f43cb43418c7a753265f9b1e0c0822c155a9cabc769413ecc14553e135fe140fc50b6722c6b9df"},
	{"ozone drill grab fiber curtain grace pudding thank cruise elder eight picnic", "9e885d952ad362caeb4efe34a8e91bd2", "f4956be6960bc145cdab782e649a5056598fd07cd3f32ceb73421c3da27833241324dc2c8b0a4d847eee457e6d4c5429f5e625ece22abaa6a976e82f1ec5531d"},
	{"scheme spot photo card baby mountain device kick cradle pact join borrow", "c0ba5a8e914111210f2bd131f3d5e08d", "c12157bf2506526c4bd1b79a056453b071361538e9e2c19c28ba2cfa39b5f23034b974e0164a1e8acd30f5b4c4de7d424fdb52c0116bfc6a965ba8205e6cc121"},
	{"cat swing flag economy stadium alone churn speed unique patch report train", "23db8160a31d3e0dca3688ed941adbf3", "719d4d4de0638a1705bf5237262458983da76933e718b2d64eb592c470f3c5d222e345cc795337bb3da393b94375ff4a56cfcd68d5ea25b577ee9384d35f4246"},
	{"vessel ladder alter error federal sibling chat ability sun glass valve picture", "f30f8c1da665478f49b001d94c5fc452", "4e2314ca7d9eebac6fe5a05a5a8d3546bc891785414d82207ac987926380411e559c885190d641ff7e686ace8c57db6f6e4333c1081e3d88d7141a74cf339c8f"},
}

func TestBittensorLocalKeySubstrateBip39Vectors(t *testing.T) {
	for _, vector := range bittensorLocalKeyTestSubstrateBip39Vectors {
		entropy, err := hex.DecodeString(vector.entropyHex)
		if err != nil {
			t.Fatal(err)
		}
		// the generator's direction: this entropy makes this phrase, and that phrase is accepted
		key, err := bittensorLocalKeyFromEntropy(entropy)
		if err != nil {
			t.Fatalf("%s: %v", vector.phrase, err)
		}
		if key.Mnemonic != vector.phrase {
			t.Errorf("entropy %s makes %q, want %q", vector.entropyHex, key.Mnemonic, vector.phrase)
		}
		address, err := BittensorLocalKeyAddress(vector.phrase)
		if err != nil || address != key.Address {
			t.Errorf("%q: address %q (%v), the generator gave %q", vector.phrase, address, err, key.Address)
		}

		decodedEntropy, err := schnorrkel.MnemonicToEntropy(vector.phrase)
		if err != nil {
			t.Fatalf("%q: %v", vector.phrase, err)
		}
		if hex.EncodeToString(decodedEntropy) != vector.entropyHex {
			t.Errorf("%q: entropy %x, want %s", vector.phrase, decodedEntropy, vector.entropyHex)
		}
		seed, err := schnorrkel.SeedFromMnemonic(vector.phrase, "Substrate")
		if err != nil {
			t.Fatalf("%q: %v", vector.phrase, err)
		}
		if hex.EncodeToString(seed[:]) != vector.seedHex {
			t.Errorf("%q: seed %x, want %s", vector.phrase, seed, vector.seedHex)
		}
	}
}

// A wallet checks a phrase against this list, so ours has to be the same one. The hash is of
// english.txt in the BIP's own repository:
// https://github.com/bitcoin/bips/blob/master/bip-0039/english.txt
func TestBittensorLocalKeyWordListIsTheBip39EnglishList(t *testing.T) {
	if len(bip39.WordList) != 2048 {
		t.Fatalf("%d words", len(bip39.WordList))
	}
	digest := sha256.Sum256([]byte(strings.Join(bip39.WordList, "\n") + "\n"))
	if hex.EncodeToString(digest[:]) != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("the word list hashes to %x", digest)
	}
}

// The mini secret to public key step on its own, Substrate's sr_test_vector_should_work:
// https://github.com/paritytech/polkadot-sdk/blob/master/substrate/primitives/core/src/sr25519.rs
func TestBittensorLocalKeyPublicKeyFromMiniSecretVector(t *testing.T) {
	var raw [32]byte
	if _, err := hex.Decode(raw[:], []byte("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")); err != nil {
		t.Fatal(err)
	}
	miniSecret, err := schnorrkel.NewMiniSecretKeyFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := miniSecret.Public().Encode()
	if hex.EncodeToString(publicKey[:]) != "44a996beb1eef7bdcab976ab6d2ca26104834164ecf28fb375600576fcc6eb0f" {
		t.Fatalf("public key %x", publicKey)
	}
}

// The steps of the server's check (server model/auth_bittensor.go VerifyBittensorSignature), which
// this module cannot import: the address must be prefix 42, the signature 64 hex bytes with or
// without 0x, the schnorrkel marker tells sr25519 from ed25519, and the message is tried wrapped in
// <Bytes>...</Bytes> and then bare, in the context "substrate". Keep them in step with that file.
func testingBittensorServerVerify(address string, message string, signature string) (bool, error) {
	publicKeyBytes, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix)
	if err != nil {
		return false, err
	}
	signatureBytes, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(signature), "0x"))
	if err != nil {
		return false, err
	}
	if len(signatureBytes) != 64 {
		return false, errors.New("signature must be 64 bytes")
	}
	var signatureFixed [64]byte
	copy(signatureFixed[:], signatureBytes)
	candidates := [][]byte{[]byte("<Bytes>" + message + "</Bytes>"), []byte(message)}
	if signatureFixed[63]&0x80 == 0 {
		for _, candidate := range candidates {
			if ed25519.Verify(ed25519.PublicKey(publicKeyBytes[:]), candidate, signatureFixed[:]) {
				return true, nil
			}
		}
		return false, nil
	}
	publicKey := &schnorrkel.PublicKey{}
	if err := publicKey.Decode(publicKeyBytes); err != nil {
		return false, err
	}
	decoded := &schnorrkel.Signature{}
	if err := decoded.Decode(signatureFixed); err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		ok, err := publicKey.Verify(decoded, schnorrkel.NewSigningContext([]byte("substrate"), candidate))
		if err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}

// Signatures other implementations made. The server's check has to accept each one, for the account
// this sdk derives from the signer's phrase where the signer's phrase is known, which also shows
// both sides hold the same secret key.
func TestBittensorServerCheckAcceptsOtherImplementationsSignatures(t *testing.T) {
	const message = "Sign in to URnetwork\nChallenge: bteasy-interop-vector\nTimestamp: 1793664000"
	hotelForest := "hotel forest jar hover kite book view eight stuff angle legend defense"
	address, err := BittensorLocalKeyAddress(hotelForest)
	if err != nil {
		t.Fatal(err)
	}
	// polkadot-js's signatureVerify vectors are published under other prefixes for the same 32 bytes
	polkadotJsAddress := func(published string) string {
		t.Helper()
		publicKey, _, err := ss58.Decode(published)
		if err != nil {
			t.Fatal(err)
		}
		reencoded, err := ss58.Encode(publicKey, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		return reencoded
	}
	for _, vector := range []struct {
		source    string
		address   string
		message   string
		signature string
	}{
		{
			// published: "echo -n hello | subkey verify"
			source:    bittensorLocalKeyTestSubkeySource,
			address:   address,
			message:   "hello",
			signature: "9201af3788ad4f986b800853c79da47155f2e08fde2070d866be4c27ab060466fea0623dc2b51f4392f4c61f25381a62848dd66c5d8217fae3858e469ebd668c",
		},
		{
			// published, bare: https://github.com/polkadot-js/common/blob/master/packages/util-crypto/src/signature/verify.spec.ts
			source:    "@polkadot/util-crypto verify.spec.ts, SIG_SR",
			address:   polkadotJsAddress("EK1bFgKm2FsghcttHT7TB7rNyXApFgs9fCbijMGQNyFGBQm"),
			message:   "hello world",
			signature: "0xca01419b5a17219f7b78335658cab3b126db523a5df7be4bfc2bef76c2eb3b1dcf4ca86eb877d0a6cf6df12db5995c51d13b00e005d053b892bd09c594434288",
		},
		{
			// published, wrapped: what a wallet's signRaw makes
			source:    "@polkadot/util-crypto verify.spec.ts, SIG_SR_WRAP",
			address:   polkadotJsAddress("J9nD3s7zssCX7bion1xctAF6xcVexcpy2uwy4jTm9JL8yuK"),
			message:   "hello world",
			signature: "0x84b6afb1c8e54bbcb3f4872baf172580e21310e9387a53742627d6652d121447fa406b82805ed3184fb7bd519175cc9f99f283f97954d95cf966ee164df85489",
		},
		{
			// recorded 2026-11-02: Keypair.sign, substrate-interface 1.8.1, wrapped
			source:    "substrate-interface, wrapped",
			address:   address,
			message:   message,
			signature: "a8f5d7519e90f56fdddb01525aff2101c49d7acbbe77980df3cf75d54db67029ae1b7cefcd1f0ff0eda7e47855a7e96a50b570bfd5b596afc237a542663a408c",
		},
		{
			// recorded 2026-11-02: Keypair.sign, substrate-interface 1.8.1, bare
			source:    "substrate-interface, bare",
			address:   address,
			message:   message,
			signature: "0ed7e688da49da9462c8c12f44da0b1d09a9539131425f6e7f99f7946783f32d291f69c0917af046d21df86258f7c2d01e7c0647262217a32a6182f70f30e383",
		},
		{
			// recorded 2026-11-02: pair.sign(u8aWrapBytes(message)), @polkadot/keyring 14.0.3
			source:    "@polkadot/keyring, wrapped",
			address:   address,
			message:   message,
			signature: "5263f3b92c39e7563d784ab8c4f1e81e2794213b357a054a0cc412bb7dcef52b71af5fcb761ead8d7f08d0c369b852f61d2aa99e91f198a59e3699d03c7bc981",
		},
	} {
		valid, err := testingBittensorServerVerify(vector.address, vector.message, vector.signature)
		if err != nil || !valid {
			t.Errorf("%s: did not verify: %v, %v", vector.source, valid, err)
		}
		valid, err = testingBittensorServerVerify(vector.address, vector.message+".", vector.signature)
		if err != nil || valid {
			t.Errorf("%s: verified over another message: %v, %v", vector.source, valid, err)
		}
	}
}

// One signature this code made, kept so the other implementations can check it again by hand. On
// 2026-11-02 py-sr25519-bindings 0.2.4 and @polkadot/util-crypto 14.0.3 accepted it, for this address
// and this message, as a wrapped sr25519 signature, and so did the server's own
// VerifyBittensorSignature. It is not reproduced here since schnorrkel signatures are randomized.
const (
	bittensorLocalKeyTestPinnedPhrase    = "hotel forest jar hover kite book view eight stuff angle legend defense"
	bittensorLocalKeyTestPinnedMessage   = "Sign in to URnetwork\nChallenge: bteasy-interop-vector\nTimestamp: 1793664000"
	bittensorLocalKeyTestPinnedSignature = "0x3e285a3532f8d690807c438ed41774c29c3db424682a143b8211f3f6f652bf6b3b84ba1002650e004055d671d24ae5d97444782adbc1c429b6571e24951c898f"
)

func TestBittensorLocalKeyPinnedSignatureStillVerifies(t *testing.T) {
	address, err := BittensorLocalKeyAddress(bittensorLocalKeyTestPinnedPhrase)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := testingBittensorServerVerify(address, bittensorLocalKeyTestPinnedMessage, bittensorLocalKeyTestPinnedSignature)
	if err != nil || !valid {
		t.Fatalf("did not verify: %v, %v", valid, err)
	}
}

// A signature from this code passes the server's check for the sign-in challenge's three lines and
// for the network consent, and only for the account and the message it was made for.
func TestBittensorLocalKeySignatureRoundTripsTheServerCheck(t *testing.T) {
	account := bittensorLocalKeyTestAccounts[0]
	other := bittensorLocalKeyTestAccounts[2]
	if _, err := ParseBittensorChallengeMessage(bittensorTestMessage); err != nil {
		t.Fatalf("the sign-in fixture stopped being the server's format: %v", err)
	}
	consent := snConsentTestMessage(t, account.address, snConsentTestEpoch+1, snConsentTestEpoch+1+SnNetworkConsentEpochSpan, nil)
	for _, message := range []string{bittensorTestMessage, consent, "", "non-ascii: é世\n"} {
		signature, err := BittensorLocalKeySign(account.phrase, message)
		if err != nil {
			t.Fatal(err)
		}
		if signature != NormalizeBittensorSignature(signature) {
			t.Errorf("%q: signature %q is not the form every app and the server pass around", message, signature)
		}
		valid, err := testingBittensorServerVerify(account.address, message, signature)
		if err != nil || !valid {
			t.Errorf("%q: the server's check refused it: %v, %v", message, valid, err)
		}
		// the wrapped candidate is the one that matched: the same bytes pass as the bare candidate
		valid, err = testingBittensorServerVerify(account.address, "<Bytes>"+message+"</Bytes>", signature)
		if err != nil || !valid {
			t.Errorf("%q: not a signature over the wrapped bytes: %v, %v", message, valid, err)
		}
		valid, err = testingBittensorServerVerify(other.address, message, signature)
		if err != nil || valid {
			t.Errorf("%q: verified for another account: %v, %v", message, valid, err)
		}
		valid, err = testingBittensorServerVerify(account.address, message+"x", signature)
		if err != nil || valid {
			t.Errorf("%q: verified over another message: %v, %v", message, valid, err)
		}
	}
}

// Only the wrapped form leaves this signer. A bare signature over caller-chosen bytes is what a
// chain transaction is, so this key never makes one, while the server's bare candidate stays open
// for signers that do not wrap: the same key signing the bare bytes passes the same check.
func TestBittensorLocalKeySignsOnlyTheWrappedBytes(t *testing.T) {
	account := bittensorLocalKeyTestAccounts[0]
	message := bittensorTestMessage
	signature, err := BittensorLocalKeySign(account.phrase, message)
	if err != nil {
		t.Fatal(err)
	}
	signatureBytes, err := hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	if err != nil || len(signatureBytes) != 64 {
		t.Fatalf("signature %q: %v", signature, err)
	}
	if signatureBytes[63]&0x80 == 0 {
		t.Fatal("the schnorrkel marker is clear, so the server would read this as ed25519")
	}
	publicKey, err := ss58.DecodeWithPrefix(account.address, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	verifyOver := func(signed string) bool {
		t.Helper()
		key := &schnorrkel.PublicKey{}
		if err := key.Decode(publicKey); err != nil {
			t.Fatal(err)
		}
		decoded := &schnorrkel.Signature{}
		if err := decoded.Decode([64]byte(signatureBytes)); err != nil {
			t.Fatal(err)
		}
		ok, err := key.Verify(decoded, schnorrkel.NewSigningContext([]byte("substrate"), []byte(signed)))
		return err == nil && ok
	}
	if !verifyOver("<Bytes>" + message + "</Bytes>") {
		t.Error("not a signature over <Bytes>message</Bytes>")
	}
	if verifyOver(message) {
		t.Error("a signature over the bare message")
	}

	// a message that already looks wrapped is still wrapped: nothing passes through bare
	wrappedAlready := "<Bytes>" + message + "</Bytes>"
	signature, err = BittensorLocalKeySign(account.phrase, wrappedAlready)
	if err != nil {
		t.Fatal(err)
	}
	signatureBytes, err = hex.DecodeString(strings.TrimPrefix(signature, "0x"))
	if err != nil || len(signatureBytes) != 64 {
		t.Fatalf("signature %q: %v", signature, err)
	}
	if verifyOver(wrappedAlready) {
		t.Error("a message that looks wrapped was signed as it is")
	}
	if !verifyOver("<Bytes>" + wrappedAlready + "</Bytes>") {
		t.Error("a message that looks wrapped was not wrapped")
	}

	// the server's bare candidate, for a wallet holding this phrase that signs without wrapping
	miniSecret, err := bittensorLocalKeyMiniSecret(account.phrase)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := miniSecret.ExpandEd25519().Sign(schnorrkel.NewSigningContext([]byte("substrate"), []byte(message)))
	if err != nil {
		t.Fatal(err)
	}
	bareBytes := bare.Encode()
	valid, err := testingBittensorServerVerify(account.address, message, hex.EncodeToString(bareBytes[:]))
	if err != nil || !valid {
		t.Errorf("the bare form did not verify: %v, %v", valid, err)
	}
}

// Replacing the word at index with word. The vector phrases have a valid checksum.
func testingBittensorLocalKeyPhraseWithWord(phrase string, index int, word string) string {
	words := strings.Split(phrase, " ")
	words[index] = word
	return strings.Join(words, " ")
}

// What every malformed phrase must get from both calls: nothing but one fixed error, the same text
// whatever the input was, so it cannot hold the phrase or any word of it.
func testingBittensorLocalKeyRefusals(t *testing.T, name string, phrase string, want error) {
	t.Helper()
	address, err := BittensorLocalKeyAddress(phrase)
	if address != "" || !errors.Is(err, want) {
		t.Errorf("%s: address %q, error %v, want %v", name, address, err, want)
	}
	signature, signErr := BittensorLocalKeySign(phrase, bittensorTestMessage)
	if signature != "" || !errors.Is(signErr, want) {
		t.Errorf("%s: signature %q, error %v, want %v", name, signature, signErr, want)
	}
	for _, failure := range []error{err, signErr} {
		if failure == nil {
			continue
		}
		if failure.Error() != want.Error() {
			t.Errorf("%s: the error text %q is not the fixed text %q", name, failure, want)
		}
		if !strings.HasPrefix(failure.Error(), BittensorLocalKeyErrorInvalidPhrase+": ") {
			t.Errorf("%s: %q is not the sdk's \"code: message\" form", name, failure)
		}
	}
}

// No word of the phrase in text.
func testingRequireNoPhraseText(t *testing.T, name string, text string, phrase string) {
	t.Helper()
	for _, word := range strings.Fields(phrase) {
		if strings.Contains(text, word) {
			t.Errorf("%s: the word %q is in %q", name, word, text)
		}
	}
}

func TestBittensorLocalKeyRefusesMalformedPhrases(t *testing.T) {
	valid := "hotel forest jar hover kite book view eight stuff angle legend defense"
	words := strings.Split(valid, " ")

	// 12 listed words whose last 4 bits are wrong
	badChecksum := testingBittensorLocalKeyPhraseWithWord(valid, 11, "abandon")
	swapped := testingBittensorLocalKeyPhraseWithWord(testingBittensorLocalKeyPhraseWithWord(valid, 0, words[1]), 1, words[0])
	for _, phrase := range []string{badChecksum, swapped} {
		if !bip39.IsMnemonicValid(phrase) {
			t.Fatalf("%q must be 12 listed words, or its row tests something else", phrase)
		}
	}

	// from the substrate-bip39 table: real checksums, and only the length is wrong for this key
	eighteen := "gravity machine north sort system female filter attitude volume fold club stay feature office ecology stable narrow fog"
	twentyFour := "hamster diagram private dutch cause delay private meat slide toddler razor book happy fancy gospel tennis maple dilemma loan word shrug inflict delay length"
	if _, err := bip39.MnemonicToByteArray(eighteen); err != nil {
		t.Fatal(err)
	}
	if _, err := bip39.MnemonicToByteArray(twentyFour); err != nil {
		t.Fatal(err)
	}

	// why the library's errors never come out: given this, its error quotes two words of the phrase
	tabbed := strings.Replace(strings.Replace(valid, " ", "\t", 1), " ", "  ", 1)
	if _, err := bip39.MnemonicToByteArray(tabbed); err == nil || !strings.Contains(err.Error(), "hotel") {
		t.Fatalf("CONTROL FAILED: the library's error for this phrase is %v, so the rows below show less than they say", err)
	}

	for _, row := range []struct {
		name   string
		phrase string
		want   error
	}{
		{"empty", "", errBittensorLocalKeyWordCount},
		{"one word", "hotel", errBittensorLocalKeyWordCount},
		{"eleven words", strings.Join(words[:11], " "), errBittensorLocalKeyWordCount},
		{"thirteen words", valid + " defense", errBittensorLocalKeyWordCount},
		{"a valid 18 word phrase", eighteen, errBittensorLocalKeyWordCount},
		{"a valid 24 word phrase", twentyFour, errBittensorLocalKeyWordCount},
		{"tab separated", strings.Join(words, "\t"), errBittensorLocalKeyWordCount},
		{"line separated", strings.Join(words, "\n"), errBittensorLocalKeyWordCount},

		{"a word not on the list", testingBittensorLocalKeyPhraseWithWord(valid, 3, "bittensor"), errBittensorLocalKeyWord},
		{"a misspelled word", testingBittensorLocalKeyPhraseWithWord(valid, 11, "defence"), errBittensorLocalKeyWord},
		{"upper case", strings.ToUpper(valid), errBittensorLocalKeyWord},
		{"one capital", testingBittensorLocalKeyPhraseWithWord(valid, 0, "Hotel"), errBittensorLocalKeyWord},
		{"a word from another list", testingBittensorLocalKeyPhraseWithWord(valid, 5, "秘"), errBittensorLocalKeyWord},
		{"a tab in one gap", testingBittensorLocalKeyPhraseWithWord(valid, 2, "jar\thover"), errBittensorLocalKeyWord},
		// one space apart and nothing around them: a split on any whitespace would let these through
		{"a double space", strings.Replace(valid, " ", "  ", 1), errBittensorLocalKeyWordCount},
		{"a tab for one space", strings.Replace(valid, " ", "\t", 1), errBittensorLocalKeyWordCount},
		{"a tab and a double space", tabbed, errBittensorLocalKeyWord},
		{"a leading space", " " + valid, errBittensorLocalKeyWordCount},
		{"a trailing space", valid + " ", errBittensorLocalKeyWordCount},
		{"a trailing line", valid + "\n", errBittensorLocalKeyWord},

		{"a bad checksum", badChecksum, errBittensorLocalKeyChecksum},
		{"two words swapped", swapped, errBittensorLocalKeyChecksum},
	} {
		testingBittensorLocalKeyRefusals(t, row.name, row.phrase, row.want)
	}
}

// Every word of the list as the last word of a phrase: the checksum has 4 bits, so 128 of the 2048
// pass, and each of the others gets the fixed text, which holds none of what was passed in.
func TestBittensorLocalKeyErrorsNeverHoldThePhrase(t *testing.T) {
	valid := "hotel forest jar hover kite book view eight stuff angle legend defense"
	checksumFailures := 0
	for _, word := range bip39.WordList {
		phrase := testingBittensorLocalKeyPhraseWithWord(valid, 11, word)
		_, err := BittensorLocalKeyAddress(phrase)
		if err == nil {
			continue
		}
		checksumFailures += 1
		if !errors.Is(err, errBittensorLocalKeyChecksum) || err.Error() != errBittensorLocalKeyChecksum.Error() {
			t.Fatalf("the last word %q: %q", word, err)
		}
		if strings.Contains(err.Error(), phrase) {
			t.Fatalf("the phrase is in %q", err)
		}
	}
	if checksumFailures != 2048-128 {
		t.Fatalf("%d of 2048 last words failed the checksum", checksumFailures)
	}
}

// %v and its relatives print the address, not the phrase: a key that reaches a log line is not a
// leak. The c abi's json does carry the phrase, by design, for the one caller that made the key.
func TestBittensorLocalKeyFormattingHidesThePhrase(t *testing.T) {
	account := bittensorLocalKeyTestAccounts[0]
	key := &BittensorLocalKey{Mnemonic: account.phrase, Address: account.address}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{key, *key} {
			text := fmt.Sprintf(format, value)
			testingRequireNoPhraseText(t, format, text, account.phrase)
			if !strings.Contains(text, account.address) {
				t.Errorf("%s: %q lost the address", format, text)
			}
		}
	}
}

// A generated key: 12 listed words, a checksum, prefix 42, the same answer on every later call, and
// a new key each time. Its entropy is random, so this holds what must be true of any value.
func TestBittensorLocalKeyGenerate(t *testing.T) {
	seenPhrases := map[string]bool{}
	for range 16 {
		key, err := BittensorLocalKeyGenerate()
		if err != nil {
			t.Fatal(err)
		}
		words := strings.Split(key.Mnemonic, " ")
		if len(words) != 12 {
			t.Fatalf("%d words", len(words))
		}
		for _, word := range words {
			if _, listed := bip39.ReverseWordMap[word]; !listed {
				t.Fatal("a word not on the list")
			}
		}
		if seenPhrases[key.Mnemonic] {
			t.Fatal("the same phrase twice")
		}
		seenPhrases[key.Mnemonic] = true

		if !ValidateSs58(key.Address) {
			t.Fatalf("address %q does not validate", key.Address)
		}
		address, err := BittensorLocalKeyAddress(key.Mnemonic)
		if err != nil || address != key.Address {
			t.Fatalf("address %q (%v), generated %q", address, err, key.Address)
		}
		signature, err := BittensorLocalKeySign(key.Mnemonic, bittensorTestMessage)
		if err != nil {
			t.Fatal(err)
		}
		valid, err := testingBittensorServerVerify(key.Address, bittensorTestMessage, signature)
		if err != nil || !valid {
			t.Fatalf("the server's check refused a new key's signature: %v, %v", valid, err)
		}
	}
}

func TestBittensorLocalKeyFromEntropyRefusesAnotherLength(t *testing.T) {
	for _, size := range []int{0, 15, 17, 20, 32} {
		key, err := bittensorLocalKeyFromEntropy(make([]byte, size))
		if key != nil || !errors.Is(err, errBittensorLocalKeyGenerate) {
			t.Errorf("%d bytes: %v, %v", size, key, err)
		}
	}
}

// A local key is one more way to make the proof a wallet makes: the session takes its address and
// signature like a manual entry, bound to the coldkey the server's consent names.
func TestBittensorLocalKeyProofPassesTheConsentSession(t *testing.T) {
	key, err := bittensorLocalKeyFromEntropy(make([]byte, bittensorLocalKeyEntropyByteCount))
	if err != nil {
		t.Fatal(err)
	}
	message := snConsentTestMessage(t, key.Address, snConsentTestEpoch+1, snConsentTestEpoch+1+SnNetworkConsentEpochSpan, nil)
	statement, err := decodeSnNetworkConsentStatement(message)
	if err != nil {
		t.Fatal(err)
	}
	session := newBittensorTestSession(t, BittensorWalletManual, BittensorWalletPlatformIos, BittensorWalletPurposeConsent)
	challenge := &SnNetworkConsentChallengeResult{
		Message:      message,
		ColdkeySs58:  key.Address,
		FromEpoch:    int64(statement.FromEpoch),
		ThroughEpoch: int64(statement.ThroughEpoch),
		ExpiresAt:    statement.ExpiresAt,
	}
	const receivedMillis = int64(1_800_000_000_000)
	if err := session.SetNetworkConsentChallenge(challenge, receivedMillis); err != nil {
		t.Fatal(err)
	}
	signature, err := BittensorLocalKeySign(key.Mnemonic, session.Message())
	if err != nil {
		t.Fatal(err)
	}
	result := session.HandleSignature(key.Address, signature, receivedMillis+1)
	if !result.Ok() {
		t.Fatalf("the session refused the local key's proof: %+v", result)
	}
	proof := session.Proof()
	if proof.Address != key.Address || proof.Message != message || proof.Signature != signature {
		t.Fatalf("proof %+v", proof)
	}
	valid, err := testingBittensorServerVerify(proof.Address, proof.Message, proof.Signature)
	if err != nil || !valid {
		t.Fatalf("the server's check refused the session's proof: %v, %v", valid, err)
	}
	testingRequireNoPhraseText(t, "proof", fmt.Sprintf("%+v", *proof), key.Mnemonic)
}

// The phrase must never reach the tunnel extension (apple BTEASY.md, Design), whose binary also has
// a hard size ceiling, and ur.io is not offered a local key. So neither build links the key's code
// or its libraries, while every app build does. A file added without this file's constraint that
// imports the library fails here.
func TestBittensorLocalKeyStaysOutOfTheExtensionAndTheBrowser(t *testing.T) {
	keyModules := []string{
		"github.com/ChainSafe/go-schnorrkel",
		"github.com/cosmos/go-bip39",
		"github.com/gtank/merlin",
		"github.com/gtank/ristretto255",
		"github.com/mimoo/StrobeGo",
	}
	for _, row := range []struct {
		name        string
		directory   string
		environment []string
		tags        string
		linksKey    bool
	}{
		{"ios extension", ".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind,ios_extension", false},
		{"macos extension", ".", []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind,ios_extension", false},
		{"browser", "js", []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"}, "", false},
		{"ios app", ".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"macos app", ".", []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"android", ".", []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"c library", "cgo", []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}, "", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			arguments := []string{"list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}{{end}}"}
			if row.tags != "" {
				arguments = append(arguments, "-tags="+row.tags)
			}
			command := exec.CommandContext(t.Context(), "go", append(arguments, ".")...)
			command.Dir = row.directory
			command.Env = append(os.Environ(), row.environment...)
			command.Env = append(command.Env, "GOWORK=off")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go list: %v\n%s", err, output)
			}
			linkedModules := strings.Fields(string(output))
			if !slices.Contains(linkedModules, "golang.org/x/crypto") {
				t.Fatal("CONTROL FAILED: the closure does not hold x/crypto, so this reads nothing real")
			}
			for _, keyModule := range keyModules {
				if linked := slices.Contains(linkedModules, keyModule); linked != row.linksKey {
					t.Errorf("%s linked: %v, want %v", keyModule, linked, row.linksKey)
				}
			}
		})
	}
}
