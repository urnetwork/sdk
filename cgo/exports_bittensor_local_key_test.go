// The local Bittensor key through the real c abi: a desktop app gets a new key as json with the
// sdk's names, the phrase gives back the same address and a signature, and a refused phrase reaches
// out_error as the sdk's fixed text with none of the phrase in it.
package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

func TestBittensorLocalKeyThroughTheAbi(t *testing.T) {
	takeString := func(result reflect.Value) string {
		t.Helper()
		text := goStringAt(result.UnsafePointer())
		socketABICall(urnet_free_string, result)
		return text
	}

	outError := socketABIOut(urnet_bittensor_local_key_generate, 0)
	result := socketABICall(urnet_bittensor_local_key_generate, outError)[0]
	if failure := socketABIError(outError); failure != "" || result.IsNil() {
		t.Fatalf("generate: %q", failure)
	}
	key := struct {
		Mnemonic string `json:"mnemonic"`
		Address  string `json:"address"`
	}{}
	if err := json.Unmarshal([]byte(takeString(result)), &key); err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(key.Mnemonic, " ")) != 12 || !sdk.ValidateSs58(key.Address) {
		t.Fatalf("generate gave %d words and the address %q", len(strings.Split(key.Mnemonic, " ")), key.Address)
	}

	mnemonic := cString(key.Mnemonic)
	defer cStringFree(mnemonic)
	outError = socketABIOut(urnet_bittensor_local_key_address, 1)
	result = socketABICall(urnet_bittensor_local_key_address, mnemonic, outError)[0]
	if failure := socketABIError(outError); failure != "" || result.IsNil() {
		t.Fatalf("address: %q", failure)
	}
	if address := takeString(result); address != key.Address {
		t.Fatalf("address %q, generated %q", address, key.Address)
	}

	message := cString("Sign in to URnetwork\nChallenge: abi\nTimestamp: 1793664000")
	defer cStringFree(message)
	outError = socketABIOut(urnet_bittensor_local_key_sign, 2)
	result = socketABICall(urnet_bittensor_local_key_sign, mnemonic, message, outError)[0]
	if failure := socketABIError(outError); failure != "" || result.IsNil() {
		t.Fatalf("sign: %q", failure)
	}
	signature := takeString(result)
	if sdk.NormalizeBittensorSignature(signature) != signature || signature == "" {
		t.Fatalf("signature %q is not in the form the apps pass around", signature)
	}

	// a published test phrase with its last word changed, so the checksum fails
	badChecksum := cString("hotel forest jar hover kite book view eight stuff angle legend abandon")
	defer cStringFree(badChecksum)
	requireRefusal := func(name string, failure string, result reflect.Value) {
		t.Helper()
		if !result.IsNil() || !strings.HasPrefix(failure, sdk.BittensorLocalKeyErrorInvalidPhrase+": ") {
			t.Fatalf("%s: result nil %v, out_error %q", name, result.IsNil(), failure)
		}
		for _, word := range []string{"hotel", "forest", "jar", "hover", "kite", "abandon"} {
			if strings.Contains(failure, word) {
				t.Fatalf("%s: out_error %q holds the word %q", name, failure, word)
			}
		}
	}
	outError = socketABIOut(urnet_bittensor_local_key_address, 1)
	result = socketABICall(urnet_bittensor_local_key_address, badChecksum, outError)[0]
	requireRefusal("address", socketABIError(outError), result)
	outError = socketABIOut(urnet_bittensor_local_key_sign, 2)
	result = socketABICall(urnet_bittensor_local_key_sign, badChecksum, message, outError)[0]
	requireRefusal("sign", socketABIError(outError), result)
}
