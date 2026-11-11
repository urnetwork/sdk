package sdk

// A client's capture the flag wins from GET /sn/ctf/client (sn
// api/operator.yml): the epochs as the route answers them, the wins they hold
// for the "flag captured" line, a refused or unreadable answer as unavailable,
// never as "no wins", and the client token as the only credential it is sent.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// sn api/operator.yml's CtfClientResult example: a provider in Iceland that won
// in both epochs, and was not an extender candidate.
const testCtfClientAnswer = `{
	"as_of": "2026-12-11T22:24:12Z",
	"unit": {"token": "alpha", "symbol": "α", "netuid": 25, "decimals": 9},
	"client_id": "3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64",
	"epochs": [
		{"epoch": 9, "drawn": true, "root_status": "pending",
			"providers": {"candidate": true, "reason": null, "country": "is", "unit_family": 4,
				"presence": {"present": 664, "required": 336, "samples": 672, "excused": 0}, "units_in_draw": 2,
				"won": true, "rank": 1, "slot": {"alpha": "0.048025660", "rao": 48025660}, "pot_share": {"alpha": "0.028815396", "rao": 28815396},
				"pending": false, "coldkey": "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619", "coldkey_ss58": "5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"},
			"extenders": {"candidate": false, "reason": "presence", "country": "is", "unit_family": 4,
				"presence": {"present": 201, "required": 336, "samples": 672, "excused": 0}, "units_in_draw": null,
				"won": false, "rank": null, "slot": null, "pot_share": null, "pending": false, "coldkey": null, "coldkey_ss58": null}},
		{"epoch": 8, "drawn": true, "root_status": "finalized",
			"providers": {"candidate": true, "reason": null, "country": "is", "unit_family": 4,
				"presence": {"present": 671, "required": 336, "samples": 672, "excused": 0}, "units_in_draw": 2,
				"won": true, "rank": 2, "slot": {"alpha": "0.048025660", "rao": 48025660}, "pot_share": {"alpha": "0.000000000", "rao": 0},
				"pending": false, "coldkey": "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619", "coldkey_ss58": "5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"},
			"extenders": {"candidate": false, "reason": "not_in_role", "country": null, "unit_family": null, "presence": null, "units_in_draw": null,
				"won": false, "rank": null, "slot": null, "pot_share": null, "pending": false, "coldkey": null, "coldkey_ss58": null}}
	]
}`

const testCtfColdkeySs58 = "5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd"

func testCtfClientDocument(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(testCtfClientAnswer), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// The route's example reads as its epochs, newest first, and its wins, the
// newest the line names.
func TestSnCtfClientOfReadsTheRoute(t *testing.T) {
	result := SnCtfClientOf(testCtfClientAnswer)
	if result.Error != nil {
		t.Fatalf("error %+v", result.Error)
	}
	if result.ClientId != "3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64" || result.AsOfMillis != time.Date(2026, 12, 11, 22, 24, 12, 0, time.UTC).UnixMilli() || result.Epochs.Len() != 2 {
		t.Fatalf("result %+v", result)
	}
	epoch9 := result.Epochs.Get(0)
	wantProvider := &SnCtfClientRole{Role: SnCtfRoleProvider, Candidate: true, Country: "is", UnitFamily: 4, PresencePresent: 664, PresenceRequired: 336, PresenceSamples: 672,
		UnitsInDraw: 2, Won: true, Rank: 1, SlotRao: 48_025_660, PotShareRao: 28_815_396, ColdkeySs58: testCtfColdkeySs58}
	wantExtender := &SnCtfClientRole{Role: SnCtfRoleExtender, Reason: SnCtfReasonPresence, Country: "is", UnitFamily: 4, PresencePresent: 201, PresenceRequired: 336, PresenceSamples: 672}
	if epoch9.Epoch != 9 || !epoch9.Drawn || epoch9.RootStatus != "pending" || !reflect.DeepEqual(epoch9.Provider, wantProvider) || !reflect.DeepEqual(epoch9.Extender, wantExtender) {
		t.Fatalf("epoch 9 %+v %+v %+v", epoch9, epoch9.Provider, epoch9.Extender)
	}
	if epoch8 := result.Epochs.Get(1); epoch8.Epoch != 8 || epoch8.RootStatus != "finalized" || epoch8.Extender.Reason != SnCtfReasonNotInRole || epoch8.Extender.Country != "" || epoch8.Extender.PresenceSamples != 0 {
		t.Fatalf("epoch 8 %+v %+v", epoch8, epoch8.Extender)
	}
	wantWins := []*SnCtfWin{
		{Epoch: 9, Role: SnCtfRoleProvider, Country: "is", Rank: 1, SlotRao: 48_025_660, PotShareRao: 28_815_396, AmountRao: 76_841_056, RootStatus: "pending", ColdkeySs58: testCtfColdkeySs58},
		{Epoch: 8, Role: SnCtfRoleProvider, Country: "is", Rank: 2, SlotRao: 48_025_660, AmountRao: 48_025_660, RootStatus: "finalized", ColdkeySs58: testCtfColdkeySs58},
	}
	if !reflect.DeepEqual(result.Wins.getAll(), wantWins) || result.LatestWin != result.Wins.Get(0) {
		t.Fatalf("wins %+v, latest %+v", result.Wins.getAll(), result.LatestWin)
	}

	// the epochs in any order read newest first; both roles' wins in an epoch,
	// providers first
	document := testCtfClientDocument(t)
	epochs := document["epochs"].([]any)
	epochs[0], epochs[1] = epochs[1], epochs[0]
	extender := epochs[1].(map[string]any)["extenders"].(map[string]any)
	extender["candidate"], extender["reason"], extender["won"], extender["rank"] = true, nil, true, 3
	extender["slot"] = map[string]any{"alpha": "0.048025660", "rao": 48025660}
	extender["coldkey_ss58"] = testCtfColdkeySs58
	result = SnCtfClientOf(string(testLedgerJson(t, document)))
	if result.Error != nil || result.Epochs.Get(0).Epoch != 9 || result.Wins.Len() != 3 || result.Wins.Get(1).Role != SnCtfRoleExtender || result.Wins.Get(1).Epoch != 9 || result.LatestWin.Role != SnCtfRoleProvider {
		t.Fatalf("reordered %+v (%+v)", result, result.Error)
	}

	// before the first root: no epochs and no wins, which is an answer
	result = SnCtfClientOf(`{"as_of":"2026-11-27T18:29:00Z","client_id":"c","epochs":[]}`)
	raw := string(testLedgerJson(t, result))
	if result.Error != nil || result.LatestWin != nil || !strings.Contains(raw, `"epochs":[],"wins":[]`) || strings.Contains(raw, "latest_win") {
		t.Fatalf("no epochs %s", raw)
	}
}

// Any part that does not parse refuses the whole answer: no line is drawn from
// it, and it is unavailable, never "no wins".
func TestSnCtfClientOfRefusesMalformedAnswers(t *testing.T) {
	role := func(d map[string]any, epoch int, name string) map[string]any {
		return d["epochs"].([]any)[epoch].(map[string]any)[name].(map[string]any)
	}
	for _, c := range []struct {
		name   string
		mutate func(d map[string]any)
		want   string
	}{
		{"an alpha that is not its rao", func(d map[string]any) { role(d, 0, "providers")["slot"].(map[string]any)["alpha"] = "0.048025661" }, `slot: alpha "0.048025661" is not rao 48025660`},
		{"an alpha without nine decimals", func(d map[string]any) { role(d, 0, "providers")["slot"].(map[string]any)["alpha"] = "0.04802566" }, "slot: alpha"},
		{"an amount without rao", func(d map[string]any) { delete(role(d, 0, "providers")["pot_share"].(map[string]any), "rao") }, "pot share: an amount without a rao value"},
		{"a negative rao", func(d map[string]any) {
			role(d, 0, "providers")["pot_share"] = map[string]any{"alpha": "-0.000000001", "rao": -1}
		}, "pot share: an amount without a rao value"},
		{"a win without a rank", func(d map[string]any) { role(d, 0, "providers")["rank"] = nil }, "a win without a rank of 1 to 5"},
		{"a rank of 6", func(d map[string]any) { role(d, 0, "providers")["rank"] = 6 }, "a win without a rank of 1 to 5"},
		{"a win without a slot", func(d map[string]any) { role(d, 0, "providers")["slot"] = nil }, "a win without its slot or country"},
		{"a win without a coldkey", func(d map[string]any) { role(d, 0, "providers")["coldkey_ss58"] = nil }, "a win paid to"},
		{"a win that was not a candidate", func(d map[string]any) {
			role(d, 0, "providers")["candidate"], role(d, 0, "providers")["reason"] = false, "presence"
		}, "a win that was not a candidate"},
		{"a candidate with a reason", func(d map[string]any) { role(d, 0, "providers")["reason"] = "presence" }, "a candidate with a reason"},
		{"no reason for no candidate", func(d map[string]any) { role(d, 0, "extenders")["reason"] = nil }, "a reason missing"},
		{"a country in upper case", func(d map[string]any) { role(d, 0, "providers")["country"] = "IS" }, `country "IS"`},
		{"a unit family of 5", func(d map[string]any) { role(d, 0, "providers")["unit_family"] = 5 }, "unit family 5"},
		{"no units in the draw", func(d map[string]any) { role(d, 0, "providers")["units_in_draw"] = 0 }, "0 units in the draw"},
		{"a negative presence", func(d map[string]any) { role(d, 0, "providers")["presence"].(map[string]any)["present"] = -1 }, "a negative presence count"},
		{"a role missing", func(d map[string]any) { delete(d["epochs"].([]any)[1].(map[string]any), "extenders") }, "epoch 1: an epoch without its number or roles"},
		{"an epoch without its number", func(d map[string]any) { delete(d["epochs"].([]any)[0].(map[string]any), "epoch") }, "epoch 0: an epoch without its number or roles"},
		{"a root status of none", func(d map[string]any) { d["epochs"].([]any)[0].(map[string]any)["root_status"] = "committed" }, `root status "committed"`},
		{"a win without a draw", func(d map[string]any) { d["epochs"].([]any)[0].(map[string]any)["drawn"] = false }, "epoch 9: a win without a draw"},
		{"an epoch twice", func(d map[string]any) { d["epochs"].([]any)[1].(map[string]any)["epoch"] = 9 }, "epoch 9 is answered twice"},
		{"no epochs list", func(d map[string]any) { delete(d, "epochs") }, "the answer has no epochs list"},
		{"an as_of that is not a time", func(d map[string]any) { d["as_of"] = "yesterday" }, "as_of: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			document := testCtfClientDocument(t)
			c.mutate(document)
			result := SnCtfClientOf(string(testLedgerJson(t, document)))
			if result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || !strings.Contains(result.Error.Message, c.want) {
				t.Fatalf("error %+v, want %q", result.Error, c.want)
			}
			if result.Epochs.Len() != 0 || result.Wins.Len() != 0 || result.LatestWin != nil {
				t.Fatalf("a refused answer drew %+v", result)
			}
		})
	}
	if result := SnCtfClientOf("<html>"); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable {
		t.Fatalf("not json %+v", result.Error)
	}
}

// testCtfClientService answers GET /sn/ctf/client, recording each request's
// credential.
type testCtfClientService struct {
	mutex      sync.Mutex
	status     int
	body       string
	authorized []string
}

func (self *testCtfClientService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if r.URL.Path != "/sn/ctf/client" {
		http.NotFound(w, r)
		return
	}
	self.authorized = append(self.authorized, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
	w.WriteHeader(self.status)
	w.Write([]byte(self.body))
}

// A device reads its own status with its client token. Any other credential is
// never sent: the route answers it 401, which the api would take as the
// credential's rejection.
func TestSnCtfClientOnADevice(t *testing.T) {
	service := &testCtfClientService{status: http.StatusOK, body: testCtfClientAnswer}
	server := httptest.NewServer(testApiHandler(service))
	defer server.Close()
	ctx, api := newTestApiForURL(t, server.URL)
	device := &snDevice{ctx: ctx, api: func() *Api { return api }, clientId: connect.NewId(), state: newDeviceSn()}
	read := func() *SnCtfClientResult {
		results := make(chan *SnCtfClientResult, 1)
		device.SnCtfClient(connect.NewApiCallback[*SnCtfClientResult](func(result *SnCtfClientResult, err error) {
			if err != nil {
				t.Error(err)
			}
			results <- result
		}))
		select {
		case result := <-results:
			return result
		case <-time.After(30 * time.Second):
			t.Fatal("the read did not call back")
			return nil
		}
	}

	clientJwt := testingJwt(map[string]any{"network_id": "00000000-0000-0000-0000-000000000001", "client_id": "3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64"})
	api.SetByJwt(clientJwt)
	result := read()
	if result.Error != nil || result.LatestWin == nil || result.LatestWin.Epoch != 9 || result.LatestWin.Country != "is" {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
	if len(service.authorized) != 1 || service.authorized[0] != "GET /sn/ctf/client Bearer "+clientJwt {
		t.Fatalf("requests %q", service.authorized)
	}

	// the route refuses it: unavailable, and the client token is kept
	service.status, service.body = http.StatusUnauthorized, `{"type":"about:blank","title":"Unauthorized","status":401,"code":"unauthorized"}`
	if result = read(); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.LatestWin != nil {
		t.Fatalf("refused %+v", result)
	}
	if api.GetByJwt() != clientJwt {
		t.Fatal("a refused read dropped the client token")
	}
	service.status, service.body = http.StatusOK, "{}"
	if result = read(); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable {
		t.Fatalf("malformed %+v", result.Error)
	}

	// a network token or none is never sent
	asked := len(service.authorized)
	for _, byJwt := range []string{testingNetworkAdminJwt("00000000-0000-0000-0000-000000000001"), "urn_apikey", ""} {
		api.SetByJwt(byJwt)
		if result = read(); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.Error.Message != "the session has no client credential" {
			t.Fatalf("credential %q: %+v", byJwt, result.Error)
		}
	}
	if len(service.authorized) != asked {
		t.Fatalf("sent %q", service.authorized[asked:])
	}
}

// snCtfClientResultOf: a read that failed is unavailable.
func TestSnCtfClientResultOfAFailedRead(t *testing.T) {
	result := snCtfClientResultOf(nil, errors.New("connection refused"))
	if result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.Error.Message != "connection refused" || result.Epochs == nil || result.Wins == nil {
		t.Fatalf("result %+v", result)
	}
}
