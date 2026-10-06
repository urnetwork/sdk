//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// WHAT THIS FILE CAN HOLD AND WHAT IT CANNOT, SAID FIRST.
//
// [NewMessageClient] dials a real URnetwork operator with a credential an operator admin mints.
// NOTHING HERE HAS EVER RUN IT AGAINST ONE and nothing here can: there is no deployment access in
// this workspace and a ByJwt cannot be forged, because the platform is what verifies it. So every
// case below is about the SHAPE -- which arguments are refused, which url the derivation answers,
// which client_id the client carries, and whether the one line whose absence is silent was run --
// and not one of them says a frame crossed.
//
// THE TOKENS BELOW ARE NOT CREDENTIALS. They are unsigned three-part strings whose signature
// segment is the word "not-a-signature". connect.ParseByJwtUnverified reads the claims WITHOUT
// checking the signature -- that is what "Unverified" names -- which is exactly why this package
// must not treat a parse as an authentication, and why the only thing it takes from the token is
// the client_id. A real operator would refuse every one of these at the websocket.

// unsignedJwt builds a three-segment jwt carrying these claims and a signature segment that is
// not one. It is enough for connect.ParseByJwtUnverified and for nothing else.
func unsignedJwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	segment := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshalling a jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(encoded)
	}
	return strings.Join([]string{
		segment(map[string]any{"alg": "HS256", "typ": "JWT"}),
		segment(claims),
		base64.RawURLEncoding.EncodeToString([]byte("not-a-signature")),
	}, ".")
}

// EVERY REFUSAL [NewMessageClient] OWNS, BY NAME, AND THE ONE THAT IS NOT OBVIOUS IS THE POINT.
//
// connect.ParseByJwtUnverified fills its ByJwt field by field and SKIPS a claim that is absent or
// that does not parse -- so a token with no client_id, or with a client_id that is not a uuid,
// answers a ByJwt at the ZERO id and NO ERROR. Without ErrMessageClientNoClientId a caller would
// get a client at sixteen zero octets that dials, authenticates and is routed nothing, which is
// the same silent-green failure as the provide modes. This case is the only thing that says so.
func TestAMessageClientRefusesEveryCredentialThatWouldDialAsNobody(t *testing.T) {
	clientId := connect.NewId()
	for _, one := range []struct {
		name   string
		config *MessageClientConfig
		want   error
	}{
		{"no config at all", nil, ErrMessageClientNoJwt},
		{"no jwt", &MessageClientConfig{Host: "example.invalid"}, ErrMessageClientNoJwt},
		{"a jwt of spaces", &MessageClientConfig{ByClientJwt: "   \t\n ", Host: "example.invalid"}, ErrMessageClientNoJwt},
		{
			"a jwt with no client_id claim",
			&MessageClientConfig{
				ByClientJwt: unsignedJwt(t, map[string]any{"network_name": "someone"}),
				Host:        "example.invalid",
			},
			ErrMessageClientNoClientId,
		},
		{
			"a jwt whose client_id is not a uuid",
			&MessageClientConfig{
				ByClientJwt: unsignedJwt(t, map[string]any{"client_id": "not-a-uuid"}),
				Host:        "example.invalid",
			},
			ErrMessageClientNoClientId,
		},
		{
			"a good jwt and nowhere to dial",
			&MessageClientConfig{ByClientJwt: unsignedJwt(t, map[string]any{"client_id": clientId.String()})},
			ErrMessageClientNoHost,
		},
	} {
		client, err := NewMessageClient(context.Background(), one.config)
		if !errors.Is(err, one.want) {
			t.Errorf("%s: answered %v, want %v", one.name, err, one.want)
		}
		if client != nil {
			client.Close()
			t.Errorf("%s: answered a client as well as an error", one.name)
		}
	}

	// AND A TOKEN THAT IS NOT A JWT AT ALL, held apart because its answer is the parser's own
	// sentence rather than one of this package's -- and because of what that sentence must NOT
	// contain. A by_client_jwt is a live credential; an error that wrapped the token would
	// publish it into every log that catches the error. Measured rather than asserted: the
	// token below is a distinctive string and this searches the whole error for it.
	const looksLikeACredential = "eyJhbGciOiJIUzI1NiJ9.SUPERSECRETPAYLOAD.sig"
	client, err := NewMessageClient(context.Background(), &MessageClientConfig{
		ByClientJwt: looksLikeACredential,
		Host:        "example.invalid",
	})
	if err == nil {
		client.Close()
		t.Fatal("a token that is not a jwt was accepted")
	}
	if strings.Contains(err.Error(), "SUPERSECRETPAYLOAD") {
		t.Errorf("the refusal carries the credential into the error message: %v", err)
	}
}

// THE TWO SERVICE URLS, DERIVED THROUGH [ServiceUrl] RATHER THAN CONCATENATED.
//
// sdk/liveprobe built "wss://connect." + host by hand, which is right on the deployed env and
// silently dials the PRODUCTION authority from any other. The rows below are what the rest of this
// module derives for the same key, so a change to ServiceUrl moves both together.
//
// THE OVERRIDES ARE HELD TOO, INCLUDING THE CASE THAT USED TO NEED NO HOST: two absolute urls are
// a complete answer and must not require a host name that nothing would then use.
func TestAMessageClientDerivesBothServiceUrlsTheWayTheRestOfSdkDoes(t *testing.T) {
	for _, one := range []struct {
		name         string
		config       *MessageClientConfig
		wantPlatform string
		wantApi      string
	}{
		{
			"the deployed env",
			&MessageClientConfig{Host: "ur.io"},
			"wss://connect.ur.io", "https://api.ur.io",
		},
		{
			"main, spelled out",
			&MessageClientConfig{Host: "ur.io", Env: "main"},
			"wss://connect.ur.io", "https://api.ur.io",
		},
		{
			"any other env prefixes the service host",
			&MessageClientConfig{Host: "ur.io", Env: "staging"},
			"wss://staging-connect.ur.io", "https://staging-api.ur.io",
		},
		{
			"an env is normalised the way NetworkSpace normalises it",
			&MessageClientConfig{Host: "ur.io", Env: "STAGING"},
			"wss://staging-connect.ur.io", "https://staging-api.ur.io",
		},
		{
			"both urls given absolutely, and no host at all",
			&MessageClientConfig{PlatformUrl: "wss://one.example/", ApiUrl: "https://two.example/"},
			"wss://one.example", "https://two.example",
		},
		{
			"one url given, the other derived",
			&MessageClientConfig{Host: "ur.io", PlatformUrl: "wss://one.example"},
			"wss://one.example", "https://api.ur.io",
		},
	} {
		platformUrl, apiUrl, err := messageClientUrls(one.config)
		if err != nil {
			t.Errorf("%s: %v", one.name, err)
			continue
		}
		if platformUrl != one.wantPlatform {
			t.Errorf("%s: platform url %q, want %q", one.name, platformUrl, one.wantPlatform)
		}
		if apiUrl != one.wantApi {
			t.Errorf("%s: api url %q, want %q", one.name, apiUrl, one.wantApi)
		}
	}
}

// THE CLIENT ITSELF, STOOD UP AND IMMEDIATELY CLOSED, AND THE THREE THINGS THAT CAN BE READ OFF
// IT WITHOUT AN OPERATOR.
//
// THE HOST IS example.invalid ON PURPOSE. RFC 6761 reserves .invalid to never resolve, so this
// constructs and dials exactly as it would in production and the dial cannot reach anything. What
// is under test is what the constructor DID, not what the network answered.
//
//  1. THE PROVIDE MODES. The message server's replies are IT sending to a client it has no
//     contract with, which is return traffic; a client that has not enabled ProvideMode_Stream is
//     one the platform delivers NOTHING to, and the absence is invisible at every health signal --
//     the socket connects, frames are accepted and acked, and every Call times out. Measured on
//     the live deployment. Deleting the SetProvideModesWithReturnTraffic line in message_client.go
//     turns THIS assertion red and nothing else in this module.
//  2. THE client_id IS THE CREDENTIAL'S AND NOT A FRESH ONE. connect.NewClient will take any Id,
//     so a constructor that passed connect.NewId() would build a perfectly healthy client at an
//     identity the operator has never heard of.
//  3. IT REALLY IS WHAT THE TRANSPORT TAKES. The compile-time assertion in message_client.go says
//     the method set matches; this says a transport is actually built over it, which is the
//     property the C abi's handle registry resolves by.
func TestAMessageClientCarriesTheCredentialsIdentityAndCanTakeReturnTraffic(t *testing.T) {
	clientId := connect.NewId()
	client, err := NewMessageClient(context.Background(), &MessageClientConfig{
		ByClientJwt: unsignedJwt(t, map[string]any{
			"client_id":    clientId.String(),
			"network_name": "someone",
		}),
		Host:       "example.invalid",
		AppVersion: "message-client-test",
	})
	if err != nil {
		t.Fatalf("NewMessageClient over an unresolvable host: %v", err)
	}
	defer client.Close()

	if client.ClientId() != clientId {
		t.Errorf("the client dials as %s and the credential names %s", client.ClientId(), clientId)
	}
	if client.PlatformUrl() != "wss://connect.example.invalid" {
		t.Errorf("the client dialled %q", client.PlatformUrl())
	}
	modes := client.Client().ContractManager().GetProvideModes()
	if !modes[protocol.ProvideMode_Stream] {
		t.Errorf("this client has not enabled ProvideMode_Stream, so the platform would deliver it nothing and every health signal would still read green; provide modes are %v", modes)
	}

	transport, err := NewMessageTransport(&MessageTransportConfig{
		Client:          client,
		Server:          connect.NewId(),
		ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatalf("a transport over a platform-attached client: %v", err)
	}
	transport.Close()

	// AND CLOSING IS IDEMPOTENT, because a C caller's stop-then-release is not always in that
	// order and a second close must not panic.
	client.Close()
}
