//go:build !ios_extension && !js

package sdk

// FS-124's app-host vectors (SDK-M8-15; §16.6's two profiles, §6.9's WebAuthnEvidence, §18.5's SidecarPresenceSigner;
// owner decisions 185, 187): an app's SignWithPresence answers base64 of the evidence, which the sdk decodes, checks
// for shape and hands the core as the countersignature each statement of the act carries. Every countersignature
// vector of both profiles, its evidence answered by an app's signer, crosses back as the same bytes, the statement
// re-encoded with it is the vector's own bytes, and the root's verifier gives the vector's verdict on this leg. The
// vectors are the sidecar module's testdata/presence/vectors.json, embedded so the cells' binary carries them to a
// phone (cmd/t4cell); sidecar_presence_vectors_host_test.go holds the copy to its source.

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

//go:embed testdata/presence/vectors.json
var testPresenceVectorsJson []byte

// The vectors file, as far as countersignatures read it: hex strings for bytes.
type testPresenceVectorFile struct {
	Keys []struct {
		Name      string `json:"name"`
		Alg       string `json:"alg"`
		KeyId     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	} `json:"keys"`
	Countersignatures []struct {
		Name    string `json:"name"`
		Key     string `json:"key"`
		Purpose string `json:"purpose"`
		Signed  string `json:"signed"`
		Refusal string `json:"refusal"`
	} `json:"countersignatures"`
}

// What a vector's refusal names, as the root's own vector test reads it.
var testPresenceVectorRefusals = map[string]error{
	"presence_key":     sidecar.ErrPresenceKey,
	"signed_evidence":  bridge.ErrSignedEvidence,
	"signed_signature": bridge.ErrSignedSignature,
	"signed_act":       bridge.ErrSignedAct,
}

// An app's presence key answering a fixed evidence, recording the messages it was asked to sign.
type testVectorPresenceSigner struct {
	answer string

	stateLock sync.Mutex
	asked     []string
}

func (self *testVectorPresenceSigner) GetAttestation() string {
	return ""
}

func (self *testVectorPresenceSigner) SignWithPresence(messageBase64 string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked = append(self.asked, messageBase64)
	return self.answer
}

// Each countersignature vector of both profiles crosses the app's signer byte-equal: the evidence the sdk hands the
// core is the vector's, the statement it completes encodes as the vector's, and the root's check gives the vector's
// verdict, a pass or the refusal it names.
func TestPresenceVectorsCrossTheAppSignerByteEqual(t *testing.T) {
	vectors := &testPresenceVectorFile{}
	if err := json.Unmarshal(testPresenceVectorsJson, vectors); err != nil {
		t.Fatal(err)
	}
	keyRows := map[string]*bridge.KeyRow{}
	for _, key := range vectors.Keys {
		alg := bridge.KeyAlg_KEY_ALG_WEBAUTHN_ES256
		if key.Alg == "ecdsa-p256-sha256" {
			alg = bridge.KeyAlg_KEY_ALG_ECDSA_P256_SHA256
		}
		keyId, err := hex.DecodeString(key.KeyId)
		if err != nil {
			t.Fatal(err)
		}
		publicKey, err := hex.DecodeString(key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		keyRows[key.Name] = &bridge.KeyRow{KeyId: keyId, Alg: alg, PublicKey: publicKey, Purpose: bridge.KeyPurpose_KEY_PURPOSE_PRESENCE}
	}
	// the bytes of field number in a message's encoding as they are on the wire: the vector's evidence exactly, never
	// a re-encoding of it
	rawField := func(encoded []byte, number protowire.Number) []byte {
		for 0 < len(encoded) {
			fieldNumber, fieldType, size := protowire.ConsumeTag(encoded)
			if size < 0 {
				t.Fatal(protowire.ParseError(size))
			}
			encoded = encoded[size:]
			if fieldNumber == number && fieldType == protowire.BytesType {
				value, size := protowire.ConsumeBytes(encoded)
				if size < 0 {
					t.Fatal(protowire.ParseError(size))
				}
				return value
			}
			size = protowire.ConsumeFieldValue(fieldNumber, fieldType, encoded)
			if size < 0 {
				t.Fatal(protowire.ParseError(size))
			}
			encoded = encoded[size:]
		}
		return nil
	}
	profiles := map[string]int{}
	for _, vector := range vectors.Countersignatures {
		keyRow := keyRows[vector.Key]
		if keyRow == nil {
			t.Fatalf("%s: no key %q", vector.Name, vector.Key)
		}
		signedBytes, err := hex.DecodeString(vector.Signed)
		if err != nil {
			t.Fatal(err)
		}
		signed := &bridge.Signed{}
		if err := proto.Unmarshal(signedBytes, signed); err != nil {
			t.Fatalf("%s: %v", vector.Name, err)
		}
		// the evidence as the vector encodes it, which the app's key answers
		evidenceBytes := rawField(signedBytes, 5)
		if len(evidenceBytes) == 0 {
			t.Fatalf("%s: no countersignature", vector.Name)
		}
		signer := &testVectorPresenceSigner{answer: base64.StdEncoding.EncodeToString(evidenceBytes)}
		// the act's bytes stand in here: the evidence answers whatever it is asked, and the crossing is the test
		countersignedBytes := []byte("synthetic act bytes of " + vector.Name)
		crossed, err := sidecarSignWithPresence(signer, countersignedBytes)
		if err != nil || !bytes.Equal(crossed, evidenceBytes) {
			t.Errorf("%s: the evidence crossed as %x (%v)", vector.Name, crossed, err)
			continue
		}
		evidence, err := sidecarPresenceCheckOf(signer)(countersignedBytes)
		if err != nil {
			t.Errorf("%s: the core's check: %v", vector.Name, err)
			continue
		}
		reencoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(evidence)
		if err != nil || !bytes.Equal(reencoded, evidenceBytes) {
			t.Errorf("%s: the evidence the core took encodes as %x (%v)", vector.Name, reencoded, err)
		}
		completed := proto.Clone(signed).(*bridge.Signed)
		completed.Webauthn = evidence
		completedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(completed)
		if err != nil || !bytes.Equal(completedBytes, signedBytes) {
			t.Errorf("%s: the statement with the crossed evidence encodes as %x (%v)", vector.Name, completedBytes, err)
		}
		for _, asked := range signer.asked {
			if message, err := base64.StdEncoding.DecodeString(asked); err != nil || !bytes.Equal(message, countersignedBytes) {
				t.Errorf("%s: the app was asked to sign %q", vector.Name, asked)
			}
		}
		verdict := sidecar.VerifyPresenceCountersignature(completed, vector.Purpose, keyRow)
		if vector.Refusal == "" {
			if verdict != nil {
				t.Errorf("%s: refused %v", vector.Name, verdict)
			}
		} else if refusal, ok := testPresenceVectorRefusals[vector.Refusal]; !ok || !errors.Is(verdict, refusal) {
			t.Errorf("%s: %v, not the vector's %s", vector.Name, verdict, vector.Refusal)
		}
		profiles[vector.Key] += 1
	}
	if profiles["webauthn-es256"] == 0 || profiles["ecdsa-p256-sha256"] == 0 {
		t.Errorf("the vectors cover %v", profiles)
	}
}

// An answer that is not an app's evidence ends the act with nothing for the core: empty, not base64, no evidence, or
// evidence naming no key or carrying no signature.
func TestPresenceAnswerThatIsNoEvidenceRefused(t *testing.T) {
	noKey, err := proto.Marshal(&bridge.WebAuthnEvidence{Signature: []byte{0x30, 0x06}})
	if err != nil {
		t.Fatal(err)
	}
	noSignature, err := proto.Marshal(&bridge.WebAuthnEvidence{KeyId: bytes.Repeat([]byte{0x11}, 16)})
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte{0xff, 0xff}), base64.StdEncoding.EncodeToString(noKey), base64.StdEncoding.EncodeToString(noSignature)} {
		signer := &testVectorPresenceSigner{answer: answer}
		if evidence, err := sidecarPresenceCheckOf(signer)([]byte("synthetic act")); err == nil || evidence != nil {
			t.Errorf("%q gave the core %v", answer, evidence)
		}
	}
	if sidecarPresenceCheckOf(nil) != nil {
		t.Error("no signer gave the core a check")
	}
}
