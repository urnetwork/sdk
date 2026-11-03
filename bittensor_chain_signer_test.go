//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"

	"github.com/urnetwork/sdk/internal/subtensor"
)

// substrate's published development phrase, nobody's funds, and the account
// sp-core publishes for it.
const (
	convertTestDevPhrase = "bottom drive obey lake curtain smoke basket hold race lonely fit walk"
	convertTestDevPublic = "46ebddef8cd9bb167dc30878d7113b7e168e6f0646beffd77d69d39bad76b47a"
)

func convertVerifySr25519(t *testing.T, public [32]byte, payload []byte, signature [64]byte) bool {
	t.Helper()
	publicKey, err := schnorrkel.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &schnorrkel.Signature{}
	if err := decoded.Decode(signature); err != nil {
		t.Fatal(err)
	}
	ok, err := publicKey.Verify(decoded, schnorrkel.NewSigningContext([]byte("substrate"), payload))
	if err != nil {
		return false
	}
	return ok
}

func TestBittensorSignerFromPhrase(t *testing.T) {
	signer, err := bittensorSignerFromPhrase(convertTestDevPhrase)
	if err != nil {
		t.Fatal(err)
	}
	public := signer.PublicKey()
	if hex.EncodeToString(public[:]) != convertTestDevPublic {
		t.Fatalf("public %x", public)
	}
	payload := []byte("the exact bytes, with no wrapper")
	signature, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !convertVerifySr25519(t, public, payload, signature) {
		t.Fatal("the signature does not verify over the payload as it is")
	}
	if convertVerifySr25519(t, public, []byte("<Bytes>the exact bytes, with no wrapper</Bytes>"), signature) {
		t.Fatal("CONTROL FAILED: it verified over the wrapped message too")
	}
}

func TestBittensorSignerFromSeed(t *testing.T) {
	// the mini secret of //Alice's published seed, and its account
	seed := [32]byte{}
	copy(seed[:], mustConvertHex(t, "e5be9a5092b81bca64be81d212e7f2f9eba183bb7a90954f7b76361f6edb5c0a"))
	signer, err := bittensorSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	public := signer.PublicKey()
	if hex.EncodeToString(public[:]) != "d43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d" {
		t.Fatalf("public %x", public)
	}
	payload := []byte{1, 2, 3}
	signature, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !convertVerifySr25519(t, public, payload, signature) {
		t.Fatal("the signature does not verify")
	}
}

func mustConvertHex(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The signature of the sdk's real key over the sdk's real payload is the one a
// runtime takes: the vector's signer is this key, and its extrinsic differs only
// in the signature, which verifies over the vector's payload.
func TestConvertRealKeySignsTheVectorsPayload(t *testing.T) {
	raw, err := os.ReadFile("internal/subtensor/testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Phrase  string `json:"phrase"`
		Vectors []struct {
			Name      string `json:"name"`
			Signer    string `json:"signer"`
			Payload   string `json:"payload"`
			Signature string `json:"signature"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	signer, err := bittensorSignerFromPhrase(file.Phrase)
	if err != nil {
		t.Fatal(err)
	}
	public := signer.PublicKey()
	for _, vector := range file.Vectors {
		if hex.EncodeToString(public[:]) != vector.Signer {
			t.Fatalf("%s: the phrase opens %x, the vector's signer is %s", vector.Name, public, vector.Signer)
		}
		payload := mustConvertHex(t, vector.Payload)
		// another implementation's signature verifies under this key and payload
		if !convertVerifySr25519(t, public, payload, [64]byte(mustConvertHex(t, vector.Signature))) {
			t.Fatalf("%s: substrate-interface's signature does not verify here", vector.Name)
		}
		// and this key's verifies too
		signature, err := signer.Sign(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !convertVerifySr25519(t, public, payload, signature) {
			t.Fatalf("%s: the signature does not verify", vector.Name)
		}
	}
}

// A phrase reaches nothing a person or a log can read: not the callback, not an
// error, whatever is wrong with it.
func TestConvertSubmitHoldsNoPhraseInItsErrors(t *testing.T) {
	const word = "abandon"
	chain := newConvertFakeChain(t)
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	for _, phrase := range []string{
		"",
		word,
		strings.Repeat(word+" ", 11) + word,
		strings.Repeat(word+" ", 11) + "notaword",
		strings.ToUpper(convertTestDevPhrase),
		" " + convertTestDevPhrase,
	} {
		result := make(chan *BittensorConvertSubmission, 1)
		BittensorConvertSubmit(DefaultSnChainSettings(), phrase, estimate, convertSubmitCallbackFunc(func(submission *BittensorConvertSubmission) {
			result <- submission
		}))
		submission := <-result
		if submission.Error == nil || submission.Error.Code != BittensorLocalKeyErrorInvalidPhrase || submission.TxHash != "" {
			t.Fatalf("%q: %+v", phrase, submission)
		}
		encoded, err := json.Marshal(submission)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(encoded))
		if strings.Contains(lower, word) || strings.Contains(lower, "bottom") || strings.Contains(lower, "notaword") {
			t.Fatalf("%q reached %s", phrase, encoded)
		}
	}
	// the right phrase for the wrong key refuses without saying anything of either
	signer, err := bittensorSignerFromPhrase(convertTestDevPhrase)
	if err != nil {
		t.Fatal(err)
	}
	submission := convertEngine(chain).convertSubmit(context.Background(), signer, estimate)
	if submission.Error == nil || submission.Error.Code != BittensorConvertErrorKeyMismatch || strings.Contains(submission.Error.Message, "bottom") {
		t.Fatalf("%+v", submission)
	}
}

type convertSubmitCallbackFunc func(submission *BittensorConvertSubmission)

func (self convertSubmitCallbackFunc) Result(submission *BittensorConvertSubmission, err error) {
	self(submission)
}

// The real path end to end with the real key on the fake chain: what is sent
// carries a signature that verifies over what the fake chain's runtime would check.
func TestConvertSubmitWithTheRealKey(t *testing.T) {
	signer, err := bittensorSignerFromPhrase(convertTestDevPhrase)
	if err != nil {
		t.Fatal(err)
	}
	public := signer.PublicKey()
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	engine := convertEngine(chain)
	ctx := context.Background()
	estimate := engine.convertEstimate(ctx, public, 1_000_000_000)
	if !estimate.CanConvert {
		t.Fatal(estimate.Error)
	}
	birth := chain.head
	callback := &convertTestCallback{}
	recording := &convertRecordingSigner{Signer: signer}
	callback.run(ctx, engine, recording, estimate)
	if len(callback.sent) != 1 || len(chain.submitted) != 1 {
		t.Fatalf("sent %v, failed %v", callback.sent, callback.failed)
	}
	sent := chain.submitted[0]
	// compact length (2), version (1), address (33), signature type (1)
	signatureAt := 2 + 1 + 33 + 1
	signature := [64]byte(sent[signatureAt : signatureAt+64])
	if !convertVerifySr25519(t, public, recording.payload, signature) {
		t.Fatal("what was sent does not verify over what was signed")
	}
	if want := convertBlockHash(birth); !strings.HasSuffix(hex.EncodeToString(recording.payload), hex.EncodeToString(want[:])+"00") {
		t.Fatalf("the payload does not end with the era's block: %x", recording.payload)
	}
}

type convertRecordingSigner struct {
	subtensor.Signer
	payload []byte
}

func (self *convertRecordingSigner) Sign(payload []byte) ([64]byte, error) {
	self.payload = append([]byte(nil), payload...)
	return self.Signer.Sign(payload)
}
