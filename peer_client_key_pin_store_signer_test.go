// Signer decoding shares the bounded stream without changing address or file semantics.
package sdk

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Use maximum-value numeric fields and put fields after the signer to expose overconsumption.
func testBoundedPeerPinSignerJson(signer string) string {
	octets := "[" + strings.Repeat("255,", 31) + "255]"
	return `{"public_key":` + octets + `,"signer":` + signer +
		`,"generation":18446744073709551614,"domain_digest":` + octets + "}"
}

// The streaming field must match the public address parser, including JSON string escapes.
func TestBoundedPeerPinStoreSignerJsonCompatibility(t *testing.T) {
	_, wantPin := testBoundedPin(0)
	canonical := `"0x` + strings.Repeat("f", 40) + `"`
	cases := []struct {
		name    string
		encoded string
		valid   bool
	}{
		{name: "canonical", encoded: canonical, valid: true},
		{name: "zero", encoded: `"0x` + strings.Repeat("0", 40) + `"`, valid: true},
		{name: "mixed hex case", encoded: `"0x` + strings.Repeat("fF", 20) + `"`, valid: true},
		{name: "escaped prefix", encoded: `"\u0030\u0078` + strings.Repeat("f", 40) + `"`, valid: true},
		{name: "escaped hex", encoded: `"0x` + strings.Repeat(`\u0066\u0046`, 20) + `"`, valid: true},
		{name: "surrounding whitespace", encoded: " \t\n" + canonical + "\r ", valid: true},
		{name: "null", encoded: "null"},
		{name: "boolean", encoded: "true"},
		{name: "number", encoded: "17"},
		{name: "array", encoded: "[" + canonical + "]"},
		{name: "object", encoded: `{"address":` + canonical + "}"},
		{name: "empty", encoded: `""`},
		{name: "short", encoded: `"0x` + strings.Repeat("f", 39) + `"`},
		{name: "long", encoded: `"0x` + strings.Repeat("f", 41) + `"`},
		{name: "missing prefix", encoded: `"` + strings.Repeat("f", 40) + `"`},
		{name: "uppercase prefix", encoded: `"0X` + strings.Repeat("f", 40) + `"`},
		{name: "wrong prefix", encoded: `"1x` + strings.Repeat("f", 40) + `"`},
		{name: "nonhex", encoded: `"0xg` + strings.Repeat("f", 39) + `"`},
		{name: "escaped nonhex", encoded: `"0x\u0067` + strings.Repeat("f", 39) + `"`},
		{name: "non JSON hex escape", encoded: `"0x\x66` + strings.Repeat("f", 39) + `"`},
		{name: "escaped newline", encoded: `"0x\n` + strings.Repeat("f", 39) + `"`},
		{name: "raw control", encoded: `"0x` + "\n" + strings.Repeat("f", 39) + `"`},
		{name: "invalid utf8", encoded: `"0x` + string([]byte{255}) + strings.Repeat("f", 39) + `"`},
		{name: "unpaired surrogate", encoded: `"0x\ud800` + strings.Repeat("f", 39) + `"`},
	}
	for _, c := range cases {
		var wantSigner connect.ClientKeyAddress
		referenceErr := wantSigner.UnmarshalJSON([]byte(c.encoded))
		if (referenceErr == nil) != c.valid {
			t.Fatalf("%s: address reference error=%v valid=%t", c.name, referenceErr, c.valid)
		}
		decoder := json.NewDecoder(strings.NewReader("[" + testBoundedPeerPinSignerJson(c.encoded) + "," + testBoundedPeerPinSignerJson(canonical) + "]"))
		if !pinJSONDelimiter(decoder, '[') {
			t.Fatalf("%s: missing fixture array", c.name)
		}
		pin, err := decodeBoundedPeerPin(decoder)
		if !c.valid {
			if !errors.Is(err, errPeerPinStoreCorrupt) {
				t.Fatalf("%s: invalid signer error=%v", c.name, err)
			}
			continue
		}
		expected := wantPin
		expected.Signer = wantSigner
		if err != nil || pin != expected {
			t.Fatalf("%s: pin=%+v error=%v want=%+v", c.name, pin, err, expected)
		}
		next, err := decodeBoundedPeerPin(decoder)
		if err != nil || next != wantPin || !pinJSONDelimiter(decoder, ']') {
			t.Fatalf("%s: signer consumed the next record: pin=%+v error=%v", c.name, next, err)
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			t.Fatalf("%s: trailing token error=%v", c.name, err)
		}
	}
}

// Invalid field identity refuses before consuming its value, even when the key is escaped.
func TestBoundedPeerPinStoreSignerFieldRefusesBeforeValue(t *testing.T) {
	_, pin := testBoundedPin(0)
	for _, field := range []string{`"signer"`, `"sign\u0065r"`, `"unknown"`} {
		decoder := json.NewDecoder(strings.NewReader(`{"signer":"` + pin.Signer.String() + `",` + field + ":17}"))
		if _, err := decodeBoundedPeerPin(decoder); !errors.Is(err, errPeerPinStoreCorrupt) {
			t.Fatalf("field %s: error=%v", field, err)
		}
		token, err := decoder.Token()
		if err != nil || token != float64(17) {
			t.Fatalf("field %s consumed the refused value: token=%v error=%v", field, token, err)
		}
	}
}

// Full construction retains strict outer syntax and balances its exact claim on refusal.
func TestBoundedPeerPinStoreSignerFileBoundaries(t *testing.T) {
	peer, wantPin := testBoundedPin(0)
	signer := `"` + wantPin.Signer.String() + `"`
	encoded := `{"signed_history_seen":true,"pins":{"` + peer.String() + `":` + testBoundedPeerPinSignerJson(signer) + "}}"
	cases := []struct {
		name    string
		encoded string
		valid   bool
	}{
		{name: "canonical", encoded: encoded, valid: true},
		{name: "escaped key", encoded: strings.Replace(encoded, `"signer"`, `"sig\u006eer"`, 1), valid: true},
		{name: "escaped value", encoded: strings.Replace(encoded, signer, `"\u0030\u0078`+strings.Repeat(`\u0066`, 40)+`"`, 1), valid: true},
		{name: "missing signer", encoded: strings.Replace(encoded, `"signer":`+signer+",", "", 1)},
		{name: "unknown signer key", encoded: strings.Replace(encoded, `"signer"`, `"address"`, 1)},
		{name: "trailing object", encoded: encoded + "{}"},
		{name: "trailing scalar", encoded: encoded + " true"},
		{name: "trailing zero byte", encoded: encoded + "\x00"},
	}
	for _, c := range cases {
		state, budget := pinStoreTestOwner(t)
		if err := os.WriteFile(filepath.Join(state.localStorageDir, peerClientKeyPinsFileName), []byte(c.encoded), 0600); err != nil {
			t.Fatalf("%s: fixture write: %v", c.name, err)
		}
		store, err := newBoundedPeerClientKeyPinStore(state, nil, budget)
		if store != nil {
			t.Cleanup(store.Close)
		}
		if !c.valid {
			if store != nil || !errors.Is(err, errPeerPinStoreCorrupt) {
				t.Fatalf("%s: refused file returned store=%v error=%v", c.name, store != nil, err)
			}
		} else {
			if err != nil || store == nil || budget.UsedByteCount() != peerPinStoreMemoryByteCount {
				t.Fatalf("%s: valid file admission error=%v", c.name, err)
			}
			pin, ok, err := store.GetPeerClientKeyPinChecked(peer)
			if err != nil || !ok || pin != wantPin {
				t.Fatalf("%s: stored pin=%+v present=%t error=%v", c.name, pin, ok, err)
			}
			store.Close()
		}
		if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("%s: unbalanced file admission: %+v", c.name, stats)
		}
	}
}
