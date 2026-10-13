// Network payout consent calls through the actual transport against a local
// fake operator only (MOCK-AUDIT.md): the range policy, the request shapes and
// credentials, the checks on every answer and the coded refusals (CONSENT.md
// §2, §3, §7 and §8).
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The sn protocol's own encoding of a challenge, written by
// sn/protocol/testdata/gen_network_consent_challenge.go from the sn repository.
type snConsentFixture struct {
	NetworkId    string `json:"network_id"`
	ColdkeySs58  string `json:"coldkey_ss58"`
	CurrentEpoch int64  `json:"current_epoch"`
	FromEpoch    int64  `json:"from_epoch"`
	ThroughEpoch int64  `json:"through_epoch"`
	ExpiresAt    int64  `json:"expires_at"`
	Message      string `json:"message"`
}

func readSnConsentFixture(t *testing.T) *snConsentFixture {
	t.Helper()
	raw, err := os.ReadFile("sn/protocol/testdata/network-consent-challenge-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &snConsentFixture{}
	if err := json.Unmarshal(raw, fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// the epoch the fake operator states unless a test says otherwise
const snConsentTestEpoch = 42

// The message the operator issues for a request: the fixture's statement with
// the requested coldkey and range, encoded the way the server encodes it.
func snConsentTestMessage(t *testing.T, coldkeySs58 string, fromEpoch int64, throughEpoch int64, mutate func(*snNetworkConsentStatement)) string {
	t.Helper()
	statement, err := decodeSnNetworkConsentStatement(readSnConsentFixture(t).Message)
	if err != nil {
		t.Fatal(err)
	}
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil {
		t.Fatal(err)
	}
	statement.Coldkey = coldkey
	statement.FromEpoch, statement.ThroughEpoch = uint64(fromEpoch), uint64(throughEpoch)
	if mutate != nil {
		mutate(statement)
	}
	raw, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	return snNetworkConsentMessagePrefix + string(raw)
}

func snConsentChallengeBody(message string) string {
	raw, _ := json.Marshal(map[string]string{"message": message})
	return string(raw)
}

func snConsentNetworkEntry(coldkeySs58 string, fromEpoch int64, throughEpoch int64) string {
	return fmt.Sprintf(`{"coldkey_ss58":%q,"set_at_millis":1791244800000,"consent_scope":"network","from_epoch":%d,"through_epoch":%d}`, coldkeySs58, fromEpoch, throughEpoch)
}

// A GET /sn/wallet answer that also lists the entries a network consent read
// passes over: the network side copy, and a provider's own consent with a
// later range.
func snConsentWallets(entries ...string) string {
	return `{"wallets":[` + strings.Join(append([]string{
		fmt.Sprintf(`{"coldkey_ss58":%q,"set_at_millis":1}`, bittensorTestBobSs58),
		fmt.Sprintf(`{"coldkey_ss58":%q,"client_id":%q,"set_at_millis":1,"consent_scope":"provider","from_epoch":900,"through_epoch":901}`, bittensorTestBobSs58, credentialTestClientId),
	}, entries...), ",") + `]}`
}

type snConsentRequest struct {
	// "METHOD /path"
	route string
	byJwt string
	body  string
}

// A fake operator for the consent calls. Each route answers its field, and the
// challenge route the canonical message for the request unless challenge says
// otherwise. A status other than 200 answers its text as the server's router
// does.
type snConsentServer struct {
	t         *testing.T
	epoch     func() (int, string)
	wallets   func() (int, string)
	challenge func(args *SnNetworkWalletMappingChallengeArgs) (int, string)
	submit    func() (int, string)

	mutex    sync.Mutex
	requests []snConsentRequest
	answered chan string
}

func answer(status int, body string) func() (int, string) {
	return func() (int, string) {
		return status, body
	}
}

func newSnConsentServer(t *testing.T) *snConsentServer {
	server := &snConsentServer{
		t:       t,
		epoch:   answer(http.StatusOK, fmt.Sprintf(`{"epoch":%d}`, snConsentTestEpoch)),
		wallets: answer(http.StatusOK, snConsentWallets()),
		submit:  answer(http.StatusOK, fmt.Sprintf(`{"mapping_hash":%q,"mapping_generation":1}`, strings.Repeat("ab", 32))),
		// buffered beyond any test's request count, so an answer never waits
		answered: make(chan string, 64),
	}
	server.challenge = func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
		return http.StatusOK, snConsentChallengeBody(snConsentTestMessage(t, args.ColdkeySs58, args.FromEpoch, args.ThroughEpoch, nil))
	}
	return server
}

func (self *snConsentServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		self.t.Errorf("read %s %s: %v", r.Method, r.URL, err)
	}
	route := r.Method + " " + r.URL.RequestURI()
	self.mutex.Lock()
	self.requests = append(self.requests, snConsentRequest{
		route: route,
		byJwt: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		body:  string(body),
	})
	self.mutex.Unlock()
	status, response := http.StatusNotFound, "no such route"
	switch route {
	case "GET /sn/epoch":
		status, response = self.epoch()
	case "GET /sn/wallet":
		status, response = self.wallets()
	case "POST /sn/wallet/network-consent":
		args := &SnNetworkWalletMappingChallengeArgs{}
		if err := json.Unmarshal(body, args); err != nil {
			self.t.Errorf("decode the challenge request: %v", err)
		}
		status, response = self.challenge(args)
	case "POST /sn/wallet":
		status, response = self.submit()
	default:
		self.t.Errorf("unexpected request %s", route)
	}
	if status == http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	} else {
		http.Error(w, response, status)
	}
	self.answered <- route
}

func (self *snConsentServer) takeRequests() []snConsentRequest {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

// Requires exactly these requests, in order, each with its bearer.
func (self *snConsentServer) requireRequests(t *testing.T, expected ...snConsentRequest) []snConsentRequest {
	t.Helper()
	requests := self.takeRequests()
	if len(requests) != len(expected) {
		t.Fatalf("requests = %+v, want %d requests %+v", requests, len(expected), expected)
	}
	for i, request := range requests {
		if request.route != expected[i].route || request.byJwt != expected[i].byJwt {
			t.Fatalf("request %d = %s with %q, want %s with %q", i, request.route, request.byJwt, expected[i].route, expected[i].byJwt)
		}
	}
	return requests
}

func newSnConsentTestApi(t *testing.T, server *snConsentServer) (context.Context, *Api) {
	t.Helper()
	return newTestApi(t, server)
}

// An api that holds the network credential, as the network owner's session.
func newSnConsentOwnerApi(t *testing.T, server *snConsentServer) (context.Context, *Api, string) {
	t.Helper()
	ctx, api := newSnConsentTestApi(t, server)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	api.SetByJwt(networkJwt)
	return ctx, api, networkJwt
}

// Runs one asynchronous consent call and returns its callback's result.
func awaitSnConsent[R any](t *testing.T, start func(callback connect.ApiCallback[R])) (R, error) {
	t.Helper()
	type outcome struct {
		result R
		err    error
	}
	done := make(chan outcome, 1)
	start(connect.NewApiCallback[R](func(result R, err error) {
		done <- outcome{result, err}
	}))
	select {
	case o := <-done:
		return o.result, o.err
	case <-time.After(15 * time.Second):
		t.Fatal("the consent call did not complete")
		var empty R
		return empty, nil
	}
}

func requireSnConsentErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), code+": ") {
		t.Fatalf("error = %v, want one starting %q", err, code+": ")
	}
}

// CONSENT §2, §8.1: the range starts after the current epoch and after the
// newest network consent's first epoch, passes over every other wallet entry,
// and spans SnNetworkConsentEpochSpan.
func TestSnNetworkConsentChallengeRangePolicy(t *testing.T) {
	connect.AssertEqual(t, SnNetworkConsentEpochSpan, 65535)
	fixture := readSnConsentFixture(t)
	for _, c := range []struct {
		name    string
		wallets string
		from    int64
	}{
		{"first consent", snConsentWallets(), snConsentTestEpoch + 1},
		{"consent from before the current epoch", snConsentWallets(snConsentNetworkEntry(bittensorTestBobSs58, 40, 40+65535)), snConsentTestEpoch + 1},
		{"consent from the current epoch", snConsentWallets(snConsentNetworkEntry(bittensorTestBobSs58, snConsentTestEpoch, snConsentTestEpoch+65535)), snConsentTestEpoch + 1},
		{"consent from after the current epoch", snConsentWallets(snConsentNetworkEntry(bittensorTestBobSs58, 50, 50+65535)), 51},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			server.wallets = answer(http.StatusOK, c.wallets)
			ctx, api, networkJwt := newSnConsentOwnerApi(t, server)
			result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, " "+bittensorTestAliceSs58+"\n")
			if err != nil || result == nil {
				t.Fatalf("challenge = %+v, %v", result, err)
			}
			through := c.from + SnNetworkConsentEpochSpan
			expected := SnNetworkConsentChallengeResult{
				Message:      snConsentTestMessage(t, bittensorTestAliceSs58, c.from, through, nil),
				ColdkeySs58:  bittensorTestAliceSs58,
				FromEpoch:    c.from,
				ThroughEpoch: through,
				ExpiresAt:    fixture.ExpiresAt,
			}
			if *result != expected {
				t.Fatalf("challenge = %+v, want %+v", *result, expected)
			}
			requests := server.requireRequests(t,
				snConsentRequest{route: "GET /sn/epoch", byJwt: networkJwt},
				snConsentRequest{route: "GET /sn/wallet", byJwt: networkJwt},
				snConsentRequest{route: "POST /sn/wallet/network-consent", byJwt: networkJwt},
			)
			requireSnWalletMappingBody(t, []byte(requests[2].body), fmt.Sprintf(`{"coldkey_ss58":%q,"from_epoch":%d,"through_epoch":%d}`, bittensorTestAliceSs58, c.from, through))
		})
	}
}

// CONSENT §2, §8.1: the latest first epoch whose span still fits int64 is
// requested through math.MaxInt64; one later is refused before the challenge.
func TestSnNetworkConsentChallengeSpanMaximum(t *testing.T) {
	server := newSnConsentServer(t)
	server.epoch = answer(http.StatusOK, fmt.Sprintf(`{"epoch":%d}`, int64(math.MaxInt64-SnNetworkConsentEpochSpan-1)))
	ctx, api, networkJwt := newSnConsentOwnerApi(t, server)
	result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
	if err != nil || result.FromEpoch != math.MaxInt64-SnNetworkConsentEpochSpan || result.ThroughEpoch != math.MaxInt64 {
		t.Fatalf("challenge at the last epoch = %+v, %v", result, err)
	}
	server.takeRequests()

	server.wallets = answer(http.StatusOK, snConsentWallets(snConsentNetworkEntry(bittensorTestBobSs58, math.MaxInt64-SnNetworkConsentEpochSpan, math.MaxInt64)))
	result, err = api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
	if err == nil || result != nil || !strings.Contains(err.Error(), "is out of range") {
		t.Fatalf("challenge after the last epoch = %+v, %v", result, err)
	}
	server.requireRequests(t,
		snConsentRequest{route: "GET /sn/epoch", byJwt: networkJwt},
		snConsentRequest{route: "GET /sn/wallet", byJwt: networkJwt},
	)
}

// CONSENT §3, §8.4: on a device that holds its client token beside the
// network credential, the challenge's reads carry the client token and the
// challenge request the network credential, with the coldkey and range only.
func TestSnNetworkConsentChallengeRequestShape(t *testing.T) {
	server := newSnConsentServer(t)
	_, api := newSnConsentTestApi(t, server)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)

	result, err := awaitSnConsent(t, func(callback connect.ApiCallback[*SnNetworkConsentChallengeResult]) {
		api.SnNetworkConsentChallenge(bittensorTestAliceSs58, SnNetworkConsentChallengeCallback(callback))
	})
	if err != nil || result == nil || result.FromEpoch != snConsentTestEpoch+1 || result.ThroughEpoch != snConsentTestEpoch+1+SnNetworkConsentEpochSpan {
		t.Fatalf("challenge = %+v, %v", result, err)
	}
	requests := server.requireRequests(t,
		snConsentRequest{route: "GET /sn/epoch", byJwt: clientJwt},
		snConsentRequest{route: "GET /sn/wallet", byJwt: clientJwt},
		snConsentRequest{route: "POST /sn/wallet/network-consent", byJwt: networkJwt},
	)
	requireSnWalletMappingBody(t, []byte(requests[2].body), fmt.Sprintf(`{"coldkey_ss58":%q,"from_epoch":43,"through_epoch":65578}`, bittensorTestAliceSs58))
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)

	// without a callback the call still runs, and answers no one
	api.SnNetworkConsentChallenge(bittensorTestAliceSs58, nil)
	for route := range server.answered {
		if route == "POST /sn/wallet/network-consent" {
			break
		}
	}
}

// CONSENT §3: the sn protocol's own encoding of a challenge passes every check
// and reaches the app byte for byte, with its expiry. An API key names no
// network, so only the statement's coldkey and range are compared.
func TestSnNetworkConsentChallengeKeepsTheServerMessage(t *testing.T) {
	fixture := readSnConsentFixture(t)
	for _, byJwt := range []string{credentialTestNetworkJwt(t, fixture.NetworkId), apiKeyPrefix + "synthetic-key"} {
		server := newSnConsentServer(t)
		server.epoch = answer(http.StatusOK, fmt.Sprintf(`{"epoch":%d}`, fixture.CurrentEpoch))
		server.challenge = func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
			return http.StatusOK, snConsentChallengeBody(fixture.Message)
		}
		ctx, api := newSnConsentTestApi(t, server)
		api.SetByJwt(byJwt)
		result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, fixture.ColdkeySs58)
		expected := SnNetworkConsentChallengeResult{
			Message:      fixture.Message,
			ColdkeySs58:  fixture.ColdkeySs58,
			FromEpoch:    fixture.FromEpoch,
			ThroughEpoch: fixture.ThroughEpoch,
			ExpiresAt:    fixture.ExpiresAt,
		}
		if err != nil || result == nil || *result != expected {
			t.Fatalf("challenge = %+v, %v, want %+v", result, err, expected)
		}
	}
}

// CONSENT §3, §8.4: the signed consent goes to POST /sn/wallet with the
// network credential, not the device's client token, and without a client id;
// the acceptance's place in the chain comes back.
func TestSnNetworkConsentSubmitSendsNoClientId(t *testing.T) {
	server := newSnConsentServer(t)
	mappingHash := strings.Repeat("cd", 32)
	server.submit = answer(http.StatusOK, fmt.Sprintf(`{"mapping_hash":%q,"mapping_generation":4096}`, mappingHash))
	_, api := newSnConsentTestApi(t, server)
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	api.SetByJwt(networkJwt)
	installTestDeviceByJwt(t, api, nil, clientJwt, nil)
	message := snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil)

	result, err := awaitSnConsent(t, func(callback connect.ApiCallback[*SnSetWalletResult]) {
		api.SnNetworkConsentSubmit(" "+bittensorTestAliceSs58+" ", message, bittensorTestSignature, SnSetWalletCallback(callback))
	})
	if err != nil || result == nil || result.Error != nil || result.MappingHash != mappingHash || result.MappingGeneration != 4096 {
		t.Fatalf("submission = %+v, %v", result, err)
	}
	requests := server.requireRequests(t, snConsentRequest{route: "POST /sn/wallet", byJwt: networkJwt})
	expectedBody, err := json.Marshal(map[string]string{"coldkey_ss58": bittensorTestAliceSs58, "signature": bittensorTestSignature, "message": message})
	if err != nil {
		t.Fatal(err)
	}
	requireSnWalletMappingBody(t, []byte(requests[0].body), string(expectedBody))

	// without a callback the call still runs, and answers no one
	api.SnNetworkConsentSubmit(bittensorTestAliceSs58, message, bittensorTestSignature, nil)
	<-server.answered
	<-server.answered
}

// CONSENT §8.2, §8.3: the status is the newest network consent. It is active
// while its through epoch is at or after the current epoch, including one that
// starts next epoch; the generation is 0 until the server reports it. Any
// session of the network reads it with its own credential.
func TestSnNetworkConsentStatus(t *testing.T) {
	for _, c := range []struct {
		name     string
		wallets  string
		expected SnNetworkConsentStatus
	}{
		{"no consent", snConsentWallets(), SnNetworkConsentStatus{}},
		{
			"consent in force",
			snConsentWallets(snConsentNetworkEntry(bittensorTestAliceSs58, 40, 40+65535)),
			SnNetworkConsentStatus{Active: true, ColdkeySs58: bittensorTestAliceSs58, FromEpoch: 40, ThroughEpoch: 40 + 65535},
		},
		{
			"consent that starts next epoch",
			snConsentWallets(snConsentNetworkEntry(bittensorTestAliceSs58, 43, 43+65535)),
			SnNetworkConsentStatus{Active: true, ColdkeySs58: bittensorTestAliceSs58, FromEpoch: 43, ThroughEpoch: 43 + 65535},
		},
		{
			"consent through the current epoch",
			snConsentWallets(snConsentNetworkEntry(bittensorTestAliceSs58, 30, snConsentTestEpoch)),
			SnNetworkConsentStatus{Active: true, ColdkeySs58: bittensorTestAliceSs58, FromEpoch: 30, ThroughEpoch: snConsentTestEpoch},
		},
		{
			"consent that ended",
			snConsentWallets(snConsentNetworkEntry(bittensorTestAliceSs58, 30, snConsentTestEpoch-1)),
			SnNetworkConsentStatus{ColdkeySs58: bittensorTestAliceSs58, FromEpoch: 30, ThroughEpoch: snConsentTestEpoch - 1},
		},
		{
			"consent with its generation",
			snConsentWallets(strings.TrimSuffix(snConsentNetworkEntry(bittensorTestAliceSs58, 40, 40+65535), "}") + `,"mapping_generation":3}`),
			SnNetworkConsentStatus{Active: true, ColdkeySs58: bittensorTestAliceSs58, FromEpoch: 40, ThroughEpoch: 40 + 65535, Generation: 3},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			server.wallets = answer(http.StatusOK, c.wallets)
			ctx, api := newSnConsentTestApi(t, server)
			clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
			installTestDeviceByJwt(t, api, nil, clientJwt, nil)
			connect.AssertEqual(t, api.HasNetworkCredential(), false)
			statuses := []func() (*SnNetworkConsentStatus, error){
				func() (*SnNetworkConsentStatus, error) { return api.SnNetworkConsentStatusSyncWithContext(ctx) },
				func() (*SnNetworkConsentStatus, error) {
					return awaitSnConsent(t, func(callback connect.ApiCallback[*SnNetworkConsentStatus]) {
						api.SnNetworkConsentStatus(SnNetworkConsentStatusCallback(callback))
					})
				},
			}
			for _, status := range statuses {
				result, err := status()
				if err != nil || result == nil || *result != c.expected {
					t.Fatalf("status = %+v, %v, want %+v", result, err, c.expected)
				}
				server.requireRequests(t,
					snConsentRequest{route: "GET /sn/epoch", byJwt: clientJwt},
					snConsentRequest{route: "GET /sn/wallet", byJwt: clientJwt},
				)
			}
		})
	}
}

// The status call without a callback still runs, and answers no one.
func TestSnNetworkConsentStatusWithoutCallback(t *testing.T) {
	server := newSnConsentServer(t)
	_, api, _ := newSnConsentOwnerApi(t, server)
	api.SnNetworkConsentStatus(nil)
	<-server.answered
	<-server.answered
}

// CONSENT §7, §8.4: without the network credential the challenge and the
// submission fail locally with network_owner_only and send nothing.
func TestSnNetworkConsentRefusesWithoutTheNetworkCredential(t *testing.T) {
	server := newSnConsentServer(t)
	ctx, api := newSnConsentTestApi(t, server)
	installTestDeviceByJwt(t, api, nil, credentialTestClientJwt(t, credentialTestNetworkId, "embed"), nil)
	connect.AssertEqual(t, api.HasNetworkCredential(), false)

	challenge, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
	requireSnConsentErrorCode(t, err, SnErrorCodeNetworkOwnerOnly)
	if challenge != nil || !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("challenge = %+v, %v", challenge, err)
	}
	submission, err := api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
	requireSnConsentErrorCode(t, err, SnErrorCodeNetworkOwnerOnly)
	if submission != nil || !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("submission = %+v, %v", submission, err)
	}
	server.requireRequests(t)
}

// CONSENT §7: a 403 on any request of a consent call means the session is not
// the network owner's.
func TestSnNetworkConsentMapsForbiddenToNetworkOwnerOnly(t *testing.T) {
	forbidden := answer(http.StatusForbidden, "synthetic client credential refusal")
	for _, c := range []struct {
		name   string
		set    func(server *snConsentServer)
		submit bool
	}{
		{"challenge request", func(server *snConsentServer) {
			server.challenge = func(*SnNetworkWalletMappingChallengeArgs) (int, string) { return forbidden() }
		}, false},
		{"epoch read", func(server *snConsentServer) { server.epoch = forbidden }, false},
		{"submission", func(server *snConsentServer) { server.submit = forbidden }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			c.set(server)
			ctx, api, _ := newSnConsentOwnerApi(t, server)
			var err error
			if c.submit {
				_, err = api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
			} else {
				_, err = api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
			}
			requireSnConsentErrorCode(t, err, SnErrorCodeNetworkOwnerOnly)
			var statusErr *connect.HttpStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusForbidden {
				t.Fatalf("error = %v, want the server's 403", err)
			}
		})
	}
}

// CONSENT §7: the server's refusal of an expired message is challenge_expired.
func TestSnNetworkConsentMapsTheExpiredRefusal(t *testing.T) {
	server := newSnConsentServer(t)
	server.submit = answer(http.StatusInternalServerError, snNetworkConsentExpiredRefusal)
	ctx, api, _ := newSnConsentOwnerApi(t, server)
	result, err := api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
	requireSnConsentErrorCode(t, err, SnErrorCodeChallengeExpired)
	var statusErr *connect.HttpStatusError
	if result != nil || !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("submission = %+v, %v", result, err)
	}
	connect.AssertEqual(t, SnErrorCodeChallengeExpired, BittensorWalletErrorExpired)
}

// CONSENT §1, §7: a signature from another account stays the result's coded
// refusal, not an error.
func TestSnNetworkConsentSubmitKeepsTheSignatureMismatch(t *testing.T) {
	server := newSnConsentServer(t)
	server.submit = answer(http.StatusOK, `{"error":{"code":"signature_mismatch","message":"The signature does not match this coldkey address. Sign the challenge with this address."}}`)
	ctx, api, _ := newSnConsentOwnerApi(t, server)
	result, err := api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
	if err != nil || result == nil || result.Error == nil || result.Error.Code != SnErrorCodeSignatureMismatch {
		t.Fatalf("submission = %+v, %v", result, err)
	}
}

// CONSENT §4: any other refusal keeps the app's existing error surface: the
// server's error passes through without a code.
func TestSnNetworkConsentKeepsOtherServerErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    func(server *snConsentServer)
		submit bool
	}{
		{"challenge request", func(server *snConsentServer) {
			server.challenge = func(*SnNetworkWalletMappingChallengeArgs) (int, string) {
				return http.StatusInternalServerError, "network wallet mapping challenge capacity is temporarily full"
			}
		}, false},
		{"submission", func(server *snConsentServer) {
			server.submit = answer(http.StatusInternalServerError, "original wallet mapping contradicts its approved identity")
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			c.set(server)
			ctx, api, _ := newSnConsentOwnerApi(t, server)
			var err error
			if c.submit {
				_, err = api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
			} else {
				_, err = api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
			}
			var statusErr *connect.HttpStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError ||
				strings.HasPrefix(err.Error(), SnErrorCodeNetworkOwnerOnly) || strings.HasPrefix(err.Error(), SnErrorCodeChallengeExpired) {
				t.Fatalf("error = %v, want the server's 500 unchanged", err)
			}
		})
	}
}

// The challenge refuses an address that is not a Bittensor coldkey before any
// request.
func TestSnNetworkConsentChallengeRefusesAnInvalidColdkey(t *testing.T) {
	server := newSnConsentServer(t)
	ctx, api, _ := newSnConsentOwnerApi(t, server)
	for _, coldkeySs58 := range []string{"", "not-an-address", bittensorTestAlicePolkadot} {
		result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, coldkeySs58)
		requireSnConsentErrorCode(t, err, SnErrorCodeInvalidAddress)
		if result != nil {
			t.Fatalf("challenge for %q = %+v", coldkeySs58, result)
		}
	}
	server.requireRequests(t)
}

// CONSENT §8.4: a submission refuses a message without the network consent
// prefix before any request, because the server would take it for another kind
// of wallet set (a login proof).
func TestSnNetworkConsentSubmitRefusesAnotherMessage(t *testing.T) {
	server := newSnConsentServer(t)
	ctx, api, _ := newSnConsentOwnerApi(t, server)
	for _, message := range []string{"", bittensorTestMessage, strings.TrimSuffix(snNetworkConsentMessagePrefix, "\n")} {
		result, err := api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, message, bittensorTestSignature)
		if err == nil || result != nil || !strings.Contains(err.Error(), "the message is not a network consent") {
			t.Fatalf("submission of %q = %+v, %v", message, result, err)
		}
	}
	server.requireRequests(t)
}

// Every answer is checked before an app shows or signs it: a malformed or
// oversized answer fails the call, a read that fails stops the call before the
// challenge is requested, and the status reads the same way.
func TestSnNetworkConsentRefusesMalformedAnswers(t *testing.T) {
	ok := func(body string) func() (int, string) { return answer(http.StatusOK, body) }
	statement := func(mutate func(statement *snNetworkConsentStatement)) func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
		return func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
			return http.StatusOK, snConsentChallengeBody(snConsentTestMessage(t, args.ColdkeySs58, args.FromEpoch, args.ThroughEpoch, mutate))
		}
	}
	message := func(change func(message string) string) func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
		return func(args *SnNetworkWalletMappingChallengeArgs) (int, string) {
			return http.StatusOK, snConsentChallengeBody(change(snConsentTestMessage(t, args.ColdkeySs58, args.FromEpoch, args.ThroughEpoch, nil)))
		}
	}
	entry := func(fields string) func() (int, string) {
		return ok(snConsentWallets(`{"coldkey_ss58":"` + bittensorTestAliceSs58 + `","consent_scope":"network",` + fields + `}`))
	}
	for _, c := range []struct {
		name      string
		epoch     func() (int, string)
		wallets   func() (int, string)
		challenge func(args *SnNetworkWalletMappingChallengeArgs) (int, string)
		// the requests the challenge sends before it refuses
		requests int
		failure  string
	}{
		{name: "epoch null", epoch: ok("null"), requests: 1, failure: "GET /sn/epoch returned no current epoch"},
		{name: "epoch before zero", epoch: ok(`{"epoch":-1}`), requests: 1, failure: "GET /sn/epoch returned no current epoch"},
		{name: "epoch not json", epoch: ok(`{"epoch":`), requests: 1, failure: "unexpected end of JSON input"},
		{name: "epoch beyond int64", epoch: ok(`{"epoch":9223372036854775808}`), requests: 1, failure: "cannot unmarshal number 9223372036854775808"},
		{name: "wallets null", wallets: ok("null"), requests: 2, failure: "GET /sn/wallet returned no wallets"},
		{name: "wallets absent", wallets: ok("{}"), requests: 2, failure: "GET /sn/wallet returned no wallets"},
		{name: "wallets refused", wallets: ok(`{"wallets":[],"error":{"message":"synthetic refusal"}}`), requests: 2, failure: "GET /sn/wallet: synthetic refusal"},
		{name: "empty wallet", wallets: ok(`{"wallets":[null]}`), requests: 2, failure: "GET /sn/wallet listed an empty wallet"},
		{name: "two network consents", wallets: ok(snConsentWallets(snConsentNetworkEntry(bittensorTestAliceSs58, 40, 41), snConsentNetworkEntry(bittensorTestBobSs58, 41, 42))), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent of a client", wallets: entry(`"client_id":"` + credentialTestClientId + `","from_epoch":40,"through_epoch":41`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent of no coldkey", wallets: ok(snConsentWallets(snConsentNetworkEntry("not-an-address", 40, 41))), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent before epoch zero", wallets: entry(`"from_epoch":-1,"through_epoch":41`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent through before from", wallets: entry(`"from_epoch":40,"through_epoch":39`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent beyond the span", wallets: entry(`"from_epoch":40,"through_epoch":65576`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent generation before one", wallets: entry(`"from_epoch":40,"through_epoch":41,"mapping_generation":-1`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "network consent generation beyond the history", wallets: entry(`"from_epoch":40,"through_epoch":41,"mapping_generation":4097`), requests: 2, failure: "GET /sn/wallet listed a malformed network consent"},
		{name: "challenge null", challenge: func(*SnNetworkWalletMappingChallengeArgs) (int, string) { return http.StatusOK, "null" }, requests: 3, failure: "returned no challenge"},
		{name: "challenge empty", challenge: message(func(string) string { return "" }), requests: 3, failure: "is not a network consent message"},
		{name: "challenge without the prefix", challenge: message(func(m string) string { return strings.TrimPrefix(m, snNetworkConsentMessagePrefix) }), requests: 3, failure: "is not a network consent message"},
		{name: "challenge oversized", challenge: message(func(m string) string { return m + strings.Repeat(" ", snNetworkConsentMaxMessageBytes) }), requests: 3, failure: "is not a network consent message"},
		{name: "challenge not json", challenge: message(func(string) string { return snNetworkConsentMessagePrefix + "{" }), requests: 3, failure: "does not decode"},
		{name: "challenge unknown field", challenge: message(func(m string) string { return strings.TrimSuffix(m, "}") + `,"client_id":[1]}` }), requests: 3, failure: "does not decode"},
		{name: "challenge trailing bytes", challenge: message(func(m string) string { return m + "\n" }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge spaced", challenge: message(func(m string) string { return strings.Replace(m, `"scope":`, `"scope": `, 1) }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge duplicate field", challenge: message(func(m string) string {
			return strings.Replace(m, `"scope":"network"`, `"scope":"provider","scope":"network"`, 1)
		}), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge provider schema", challenge: statement(func(s *snNetworkConsentStatement) { s.Schema = "urnetwork-wallet-mapping-consent-v2" }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge provider scope", challenge: statement(func(s *snNetworkConsentStatement) { s.Scope = "provider" }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge issued at zero", challenge: statement(func(s *snNetworkConsentStatement) { s.IssuedAt = 0 }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge expired at issue", challenge: statement(func(s *snNetworkConsentStatement) { s.ExpiresAt = s.IssuedAt }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge window beyond 300 s", challenge: statement(func(s *snNetworkConsentStatement) { s.ExpiresAt = s.IssuedAt + 301 }), requests: 3, failure: "is not a canonical network statement"},
		{name: "challenge for another coldkey", challenge: statement(func(s *snNetworkConsentStatement) { s.Coldkey[0] ^= 1 }), requests: 3, failure: "is for another coldkey, range or network"},
		{name: "challenge from another epoch", challenge: statement(func(s *snNetworkConsentStatement) { s.FromEpoch += 1 }), requests: 3, failure: "is for another coldkey, range or network"},
		{name: "challenge through another epoch", challenge: statement(func(s *snNetworkConsentStatement) { s.ThroughEpoch -= 1 }), requests: 3, failure: "is for another coldkey, range or network"},
		{name: "challenge for another network", challenge: statement(func(s *snNetworkConsentStatement) { s.NetworkId[15] ^= 1 }), requests: 3, failure: "is for another coldkey, range or network"},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			if c.epoch != nil {
				server.epoch = c.epoch
			}
			if c.wallets != nil {
				server.wallets = c.wallets
			}
			if c.challenge != nil {
				server.challenge = c.challenge
			}
			ctx, api, _ := newSnConsentOwnerApi(t, server)
			result, err := api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
			if err == nil || result != nil || !strings.Contains(err.Error(), c.failure) {
				t.Fatalf("challenge = %+v, %v, want a refusal containing %q", result, err, c.failure)
			}
			if requests := server.takeRequests(); len(requests) != c.requests {
				t.Fatalf("the challenge sent %+v, want %d requests", requests, c.requests)
			}
			if c.requests < 3 {
				status, err := api.SnNetworkConsentStatusSyncWithContext(ctx)
				if err == nil || status != nil || !strings.Contains(err.Error(), c.failure) {
					t.Fatalf("status = %+v, %v, want a refusal containing %q", status, err, c.failure)
				}
			}
		})
	}
}

// An answer beyond the transport's bound never becomes a result: the read
// fails, and the challenge is never requested. The transport retries such an
// answer, so the deadline only bounds the test.
func TestSnNetworkConsentRefusesAnOversizedAnswer(t *testing.T) {
	server := newSnConsentServer(t)
	server.wallets = answer(http.StatusOK, strings.Repeat(" ", int(connect.DefaultMaxHttpResponseBodyBytes))+snConsentWallets())
	ctx, api, _ := newSnConsentOwnerApi(t, server)
	for _, call := range []func(ctx context.Context) (any, error){
		func(ctx context.Context) (any, error) {
			return api.SnNetworkConsentChallengeSyncWithContext(ctx, bittensorTestAliceSs58)
		},
		func(ctx context.Context) (any, error) { return api.SnNetworkConsentStatusSyncWithContext(ctx) },
	} {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, err := call(callCtx)
		cancel()
		if err == nil || !reflect.ValueOf(result).IsNil() {
			t.Fatalf("call with an oversized answer = %+v, %v", result, err)
		}
	}
	for _, request := range server.takeRequests() {
		if request.route != "GET /sn/epoch" && request.route != "GET /sn/wallet" {
			t.Fatalf("an oversized read was followed by %s", request.route)
		}
	}
}

// An acceptance must name the consent's place in the chain: an answer without
// it, or out of the protocol's bounds, fails the submission.
func TestSnNetworkConsentSubmitRefusesMalformedAcceptances(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for _, c := range []struct {
		name    string
		body    string
		failure string
	}{
		{"null", "null", "POST /sn/wallet returned no result"},
		{"not json", `{"mapping_hash":`, "unexpected end of JSON input"},
		{"no place in the chain", "{}", "without its place in the chain"},
		{"hash not hex", fmt.Sprintf(`{"mapping_hash":%q,"mapping_generation":1}`, strings.Repeat("zz", 32)), "without its place in the chain"},
		{"hash short", fmt.Sprintf(`{"mapping_hash":%q,"mapping_generation":1}`, strings.Repeat("ab", 31)), "without its place in the chain"},
		{"generation zero", fmt.Sprintf(`{"mapping_hash":%q}`, hash), "without its place in the chain"},
		{"generation beyond the history", fmt.Sprintf(`{"mapping_hash":%q,"mapping_generation":4097}`, hash), "without its place in the chain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			server.submit = answer(http.StatusOK, c.body)
			ctx, api, _ := newSnConsentOwnerApi(t, server)
			result, err := api.SnNetworkConsentSubmitSyncWithContext(ctx, bittensorTestAliceSs58, snConsentTestMessage(t, bittensorTestAliceSs58, 43, 65578, nil), bittensorTestSignature)
			if err == nil || result != nil || !strings.Contains(err.Error(), c.failure) {
				t.Fatalf("submission = %+v, %v, want a refusal containing %q", result, err, c.failure)
			}
		})
	}
}
