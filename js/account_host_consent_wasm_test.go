//go:build js

// The account host's network consent methods (CONSENT.md §9) as a page calls
// them. Nothing leaves the machine: the refusals the sdk makes before any
// request (a page credential that is not the network owner's, a message that
// is not a network consent), and a status read of a closed loopback port.
package main

import (
	"encoding/base64"
	"strings"
	"syscall/js"
	"testing"
	"time"
)

// An unsigned token with the claims the sdk reads (unverified) to tell a
// client token from the network's.
func consentTestJwt(claims string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + encode([]byte(claims)) + "."
}

func TestAccountHostNetworkConsentRefusesLocally(t *testing.T) {
	const coldkeySs58 = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	const signatureHex = "0xabababababababababababababababababababababababababababababababababababababababababababababababababababababababababababababab"
	clientJwt := consentTestJwt(`{"network_id":"00000000-0000-0000-0000-00000000a001","user_id":"00000000-0000-0000-0000-00000000b001","client_id":"00000000-0000-0000-0000-00000000c001"}`)
	networkJwt := consentTestJwt(`{"network_id":"00000000-0000-0000-0000-00000000a001","user_id":"00000000-0000-0000-0000-00000000b001"}`)
	// loopback urls: a request the test did not mean to send fails there
	host := NewAccountHost(js.Null(), []js.Value{js.ValueOf("http://127.0.0.1:9"), js.ValueOf("ws://127.0.0.1:9"), js.ValueOf(clientJwt)}).(js.Value)
	defer awaitApiPromise(t, host.Call("close"))
	for _, method := range []string{"snNetworkConsentChallenge", "snNetworkConsentSubmit", "snNetworkConsentStatus"} {
		if host.Get(method).Type() != js.TypeFunction {
			t.Fatalf("the account host has no %s", method)
		}
	}

	// a client token is not the network owner's credential
	for _, promise := range []js.Value{
		host.Call("snNetworkConsentChallenge", coldkeySs58),
		host.Call("snNetworkConsentSubmit", coldkeySs58, "Approve URnetwork network wallet mapping\n{}", signatureHex),
	} {
		value, rejected := awaitApiPromise(t, promise)
		if !value.IsUndefined() || rejected.IsUndefined() || !strings.HasPrefix(rejected.Get("message").String(), "network_owner_only: ") {
			t.Fatalf("client token consent call = %v, rejected %v", value, rejected)
		}
	}

	// the network owner's credential still never submits another message
	host.Call("setByJwt", networkJwt)
	value, rejected := awaitApiPromise(t, host.Call("snNetworkConsentSubmit", coldkeySs58, "Sign in to URnetwork\nChallenge: x\nTimestamp: 1", signatureHex))
	if !value.IsUndefined() || rejected.IsUndefined() || !strings.Contains(rejected.Get("message").String(), "the message is not a network consent") {
		t.Fatalf("submission of a login message = %v, rejected %v", value, rejected)
	}
}

// The status reads the operator, here a closed loopback port: the read's
// failure rejects the promise with the transport's kind fields.
func TestAccountHostNetworkConsentStatusRejectsAFailedRead(t *testing.T) {
	host := NewAccountHost(js.Null(), []js.Value{js.ValueOf("http://127.0.0.1:9"), js.ValueOf("ws://127.0.0.1:9"), js.ValueOf("")}).(js.Value)
	defer awaitApiPromise(t, host.Call("close"))
	settled := make(chan js.Value, 1)
	onRejected := js.FuncOf(func(_ js.Value, args []js.Value) any {
		settled <- args[0]
		return nil
	})
	defer onRejected.Release()
	host.Call("snNetworkConsentStatus").Call("catch", onRejected)
	select {
	case rejected := <-settled:
		if rejected.Get("kind").Type() != js.TypeString || rejected.Get("message").String() == "" {
			t.Fatalf("status rejection %v carries no kind or message", rejected)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the status read did not fail")
	}
}
