package sdk

// A client's or a network's capture the flag wins from GET /sn/ctf/client (sn
// api/operator.yml): the epochs as the route answers them, its wins with their
// claim state and the unclaimed ones the "you won a slot" line shows, the wins
// of an older server's epochs, a refused or unreadable answer as unavailable,
// never as "no wins", and a network token that an older server refuses read as
// no wins, never as a reason to sign out.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// an answer without wins, from a server before them, has its epochs' wins:
	// one in a pending root is unclaimed, and one in a finalized root unknown
	wantWins := []*SnCtfWin{
		{Epoch: 9, Role: SnCtfRoleProvider, Country: "is", Rank: 1, SlotRao: 48_025_660, PotShareRao: 28_815_396, AmountRao: 76_841_056, RootStatus: "pending", ColdkeySs58: testCtfColdkeySs58,
			ClientId: testCtfClientId, ClaimState: SnCtfWinClaimStateUnclaimed},
		{Epoch: 8, Role: SnCtfRoleProvider, Country: "is", Rank: 2, SlotRao: 48_025_660, AmountRao: 48_025_660, RootStatus: "finalized", ColdkeySs58: testCtfColdkeySs58,
			ClientId: testCtfClientId, ClaimState: SnCtfWinClaimStateUnknown},
	}
	if !reflect.DeepEqual(result.Wins.getAll(), wantWins) || result.LatestWin != result.Wins.Get(0) {
		t.Fatalf("wins %+v, latest %+v", result.Wins.getAll(), result.LatestWin)
	}
	if result.UnclaimedWins.Len() != 1 || result.UnclaimedWins.Get(0) != result.Wins.Get(0) {
		t.Fatalf("unclaimed %+v", result.UnclaimedWins.getAll())
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
	if result.Error != nil || result.LatestWin != nil || !strings.Contains(raw, `"epochs":[],"wins":[],"unclaimed_wins":[]`) || strings.Contains(raw, "latest_win") {
		t.Fatalf("no epochs %s", raw)
	}
}

const testCtfClientId = "3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64"

// testCtfWin is a CtfClientWin as the route answers it.
func testCtfWin(epoch int, role string, country string, rank int, clientId string, slot int64, potShare int64, rootStatus string, claimed any) map[string]any {
	amount := func(rao int64) map[string]any {
		return map[string]any{"alpha": fmt.Sprintf("%d.%09d", rao/SnRaoPerAlpha, rao%SnRaoPerAlpha), "rao": rao}
	}
	return map[string]any{
		"epoch": epoch, "role": role, "country": country, "rank": rank, "client_id": clientId,
		"amount": amount(slot + potShare), "slot": amount(slot), "pot_share": amount(potShare),
		"pending": false, "root_status": rootStatus,
		"coldkey":      "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619",
		"coldkey_ss58": testCtfColdkeySs58, "claimed": claimed,
	}
}

// The route's wins (ctf-client-route): for a client token beside its epochs,
// for a network token alone with a null client_id and no epochs. Each win has
// its claim state; the unclaimed ones are the line's, newest first whatever
// the answer's order, and an unknown claim state is neither claimed nor
// unclaimed.
func TestSnCtfClientOfReadsWins(t *testing.T) {
	other := "7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6"
	document := testCtfClientDocument(t)
	document["wins"] = []any{
		testCtfWin(8, "provider", "is", 2, testCtfClientId, 48_025_660, 0, "finalized", true),
		testCtfWin(9, "provider", "is", 1, testCtfClientId, 48_025_660, 28_815_396, "pending", false),
		// an older win, outside the epochs listed, not claimed yet
		testCtfWin(3, "extender", "no", 4, testCtfClientId, 40_000_000, 0, "finalized", false),
		testCtfWin(5, "provider", "fo", 5, testCtfClientId, 40_000_000, 1, "finalized", nil),
	}
	result := SnCtfClientOf(string(testLedgerJson(t, document)))
	if result.Error != nil || result.ClientId != testCtfClientId || result.Epochs.Len() != 2 || result.Wins.Len() != 4 {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
	var order []int64
	var states []string
	for _, win := range result.Wins.getAll() {
		order, states = append(order, win.Epoch), append(states, win.ClaimState)
	}
	if !reflect.DeepEqual(order, []int64{9, 8, 5, 3}) || !reflect.DeepEqual(states, []string{"unclaimed", "claimed", "unknown", "unclaimed"}) {
		t.Fatalf("wins %v %v", order, states)
	}
	want3 := &SnCtfWin{Epoch: 3, Role: SnCtfRoleExtender, Country: "no", Rank: 4, SlotRao: 40_000_000, AmountRao: 40_000_000, RootStatus: "finalized", ColdkeySs58: testCtfColdkeySs58,
		ClientId: testCtfClientId, ClaimState: SnCtfWinClaimStateUnclaimed}
	if result.UnclaimedWins.Len() != 2 || result.UnclaimedWins.Get(0).Epoch != 9 || !reflect.DeepEqual(result.UnclaimedWins.Get(1), want3) || result.LatestWin.Epoch != 9 {
		t.Fatalf("unclaimed %+v", result.UnclaimedWins.getAll())
	}
	raw := string(testLedgerJson(t, result.UnclaimedWins.Get(1)))
	if raw != `{"epoch":3,"role":"extender","country":"no","rank":4,"slot_rao":40000000,"pot_share_rao":0,"amount_rao":40000000,"pending":false,"root_status":"finalized","coldkey_ss58":"`+testCtfColdkeySs58+`","client_id":"`+testCtfClientId+`","claim_state":"unclaimed"}` {
		t.Fatalf("json %s", raw)
	}

	// a network's answer: no client, no epochs, the wins of two of its clients;
	// in one epoch providers before extenders, then by country and rank
	network := map[string]any{
		"as_of": "2026-12-11T22:24:12Z", "unit": map[string]any{"token": "alpha"}, "client_id": nil, "epochs": []any{},
		"wins": []any{
			testCtfWin(9, "extender", "is", 1, other, 48_025_660, 0, "pending", false),
			testCtfWin(9, "provider", "is", 3, other, 48_025_660, 0, "pending", false),
			testCtfWin(9, "provider", "fo", 1, testCtfClientId, 48_025_660, 0, "pending", false),
			testCtfWin(9, "provider", "is", 1, testCtfClientId, 48_025_660, 0, "pending", false),
			// a later server's role
			testCtfWin(9, "head", "is", 1, testCtfClientId, 48_025_660, 0, "pending", false),
		},
	}
	// a client wins one provider slot in an epoch: fo's winner is a third client
	network["wins"].([]any)[2].(map[string]any)["client_id"] = "11111111-2222-4333-8444-555555555555"
	result = SnCtfClientOf(string(testLedgerJson(t, network)))
	if result.Error != nil || result.ClientId != "" || result.Epochs.Len() != 0 || result.UnclaimedWins.Len() != 5 {
		t.Fatalf("network %+v (%+v)", result, result.Error)
	}
	var seen []string
	for _, win := range result.Wins.getAll() {
		seen = append(seen, win.Role+" "+win.Country+" "+win.ClientId[:4])
	}
	if !reflect.DeepEqual(seen, []string{"provider fo 1111", "provider is 3f6c", "provider is 7d1e", "extender is 7d1e", "head is 3f6c"}) {
		t.Fatalf("network order %q", seen)
	}

	// an empty list is an answer without a win
	network["wins"] = []any{}
	if result = SnCtfClientOf(string(testLedgerJson(t, network))); result.Error != nil || result.Wins.Len() != 0 || result.UnclaimedWins.Len() != 0 || result.LatestWin != nil {
		t.Fatalf("no wins %+v", result)
	}
}

// A win that does not parse refuses the whole answer, as a malformed epoch does.
func TestSnCtfClientOfRefusesMalformedWins(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(win map[string]any)
		want   string
	}{
		{"no epoch", func(win map[string]any) { delete(win, "epoch") }, "a win without its epoch"},
		{"a role that is not a word", func(win map[string]any) { win["role"] = "Provider" }, `role "Provider"`},
		{"a country in upper case", func(win map[string]any) { win["country"] = "IS" }, `country "IS"`},
		{"a rank of 0", func(win map[string]any) { win["rank"] = 0 }, "a win without a rank of 1 to 5"},
		{"a root status of none", func(win map[string]any) { win["root_status"] = "committed" }, `root status "committed"`},
		{"no amount", func(win map[string]any) { delete(win, "amount") }, "a win without its amount, slot or pot share"},
		{"an amount that is not the slot and pot share", func(win map[string]any) {
			win["amount"] = map[string]any{"alpha": "0.048025661", "rao": 48025661}
		}, "an amount of 48025661 rao that is not its slot and pot share"},
		{"an alpha that is not its rao", func(win map[string]any) {
			win["slot"] = map[string]any{"alpha": "0.048025661", "rao": 48025660}
		}, `slot: alpha "0.048025661" is not rao 48025660`},
		{"a coldkey that is not one", func(win map[string]any) { win["coldkey_ss58"] = "5Hq5" }, `a win paid to "5Hq5"`},
		{"a coldkey that is another's", func(win map[string]any) {
			win["coldkey"] = "0x" + strings.Repeat("ab", 32)
		}, "is not " + testCtfColdkeySs58},
		{"no client", func(win map[string]any) { delete(win, "client_id") }, `client ""`},
		{"another client's win", func(win map[string]any) { win["client_id"] = "7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6" }, "a win of client 7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6 in the answer for " + testCtfClientId},
		{"claimed as text", func(win map[string]any) { win["claimed"] = "no" }, `claimed "no"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			document := testCtfClientDocument(t)
			win := testCtfWin(9, "provider", "is", 1, testCtfClientId, 48_025_660, 28_815_396, "pending", false)
			c.mutate(win)
			document["wins"] = []any{win}
			result := SnCtfClientOf(string(testLedgerJson(t, document)))
			if result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || !strings.Contains(result.Error.Message, c.want) {
				t.Fatalf("error %+v, want %q", result.Error, c.want)
			}
			if result.Wins.Len() != 0 || result.UnclaimedWins.Len() != 0 || result.LatestWin != nil {
				t.Fatalf("a refused answer drew %+v", result)
			}
		})
	}
	document := testCtfClientDocument(t)
	document["wins"] = []any{
		testCtfWin(9, "provider", "is", 1, testCtfClientId, 48_025_660, 0, "pending", false),
		testCtfWin(9, "provider", "fo", 2, testCtfClientId, 48_025_660, 0, "pending", false),
	}
	if result := SnCtfClientOf(string(testLedgerJson(t, document))); result.Error == nil || !strings.Contains(result.Error.Message, "won epoch 9's provider draw twice") {
		t.Fatalf("a win twice %+v", result.Error)
	}
}

// A page's own read with its status: 200 reads the body, 401 (an older server
// refusing a network token) is no wins, and anything else is unavailable.
func TestSnCtfClientOfResponse(t *testing.T) {
	if result := SnCtfClientOfResponse(http.StatusOK, testCtfClientAnswer); result.Error != nil || result.Wins.Len() != 2 {
		t.Fatalf("200 %+v", result)
	}
	result := SnCtfClientOfResponse(http.StatusUnauthorized, `{"type":"about:blank","title":"Unauthorized","status":401,"code":"unauthorized"}`)
	if raw := string(testLedgerJson(t, result)); result.Error != nil || raw != `{"client_id":"","as_of_millis":0,"epochs":[],"wins":[],"unclaimed_wins":[]}` {
		t.Fatalf("401 %s", raw)
	}
	for _, status := range []int{http.StatusNotModified, http.StatusBadRequest, http.StatusServiceUnavailable, 0} {
		if result := SnCtfClientOfResponse(status, testCtfClientAnswer); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.Wins.Len() != 0 {
			t.Fatalf("%d %+v", status, result)
		}
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
// credential: status and body, or answer's for the credential.
type testCtfClientService struct {
	mutex      sync.Mutex
	status     int
	body       string
	answer     func(byJwt string) (int, string)
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
	status, body := self.status, self.body
	if self.answer != nil {
		status, body = self.answer(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func (self *testCtfClientService) take() []string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	requests := self.authorized
	self.authorized = nil
	return requests
}

const testCtfUnauthorized = `{"type":"about:blank","title":"Unauthorized","status":401,"code":"unauthorized"}`

// a network's answer: one unclaimed win of another of its clients
const testCtfNetworkAnswer = `{"as_of":"2026-12-11T22:24:12Z","client_id":null,"epochs":[],"wins":[
	{"epoch":9,"role":"extender","country":"fo","rank":2,"client_id":"7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6",
		"amount":{"alpha":"0.048025660","rao":48025660},"slot":{"alpha":"0.048025660","rao":48025660},"pot_share":{"alpha":"0.000000000","rao":0},
		"pending":false,"root_status":"pending","coldkey":"0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619",
		"coldkey_ss58":"5Hq5oMjMnNDPJyRUScRVkWoFxYRdNqkHsjMBCxhPtYAEcnTd","claimed":false}]}`

type testSignInRequired struct{ count *int }

func (self testSignInRequired) AccountSignInRequired() { *self.count += 1 }

// A device asks with its network credential, which the route answers with the
// wins of all of the network's clients, as ur.io reads them, and else with its
// client token. A server before the network answer refuses the network
// credential 401: the device asks again with its client token, and without one
// reads no wins. No answer of the route signs anyone out or drops a credential.
func TestSnCtfClientOnADevice(t *testing.T) {
	service := &testCtfClientService{status: http.StatusOK, body: testCtfClientAnswer}
	server := httptest.NewServer(testApiHandler(service))
	defer server.Close()
	ctx, api := newTestApiForURL(t, server.URL)
	logouts, signInsRequired := 0, 0
	api.AddAuthLogoutListener(authLogoutListenerFunc(func() { logouts += 1 }))
	api.AddAccountSignInRequiredListener(testSignInRequired{&signInsRequired})
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
	networkJwt := testingNetworkAdminJwt("00000000-0000-0000-0000-000000000001")
	// a server before the network answer: a client token's epochs, a network
	// credential refused
	olderServer := func(byJwt string) (int, string) {
		if credentialNamesClient(byJwt) {
			return http.StatusOK, testCtfClientAnswer
		}
		return http.StatusUnauthorized, testCtfUnauthorized
	}
	newServer := func(byJwt string) (int, string) {
		if credentialNamesClient(byJwt) {
			return http.StatusOK, testCtfClientAnswer
		}
		return http.StatusOK, testCtfNetworkAnswer
	}

	// a client token alone: that client's wins
	api.SetByJwt(clientJwt)
	result := read()
	if result.Error != nil || result.LatestWin == nil || result.LatestWin.Epoch != 9 || result.LatestWin.Country != "is" || result.UnclaimedWins.Len() != 1 {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
	if requests := service.take(); len(requests) != 1 || requests[0] != "GET /sn/ctf/client Bearer "+clientJwt {
		t.Fatalf("requests %q", requests)
	}

	// the route refuses it: unavailable, and the client token is kept
	service.status, service.body = http.StatusUnauthorized, testCtfUnauthorized
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
	service.take()

	// a network credential alone, the session's own (ur.io's kind of session),
	// or an API key: the network's wins
	service.answer = newServer
	for _, byJwt := range []string{networkJwt, "urn_apikey"} {
		api.SetByJwt(byJwt)
		result = read()
		if result.Error != nil || result.ClientId != "" || result.UnclaimedWins.Len() != 1 || result.UnclaimedWins.Get(0).ClientId != "7d1e9a40-1b2c-4d3e-8f56-a1b2c3d4e5f6" {
			t.Fatalf("network %q: %+v (%+v)", byJwt, result, result.Error)
		}
		if requests := service.take(); len(requests) != 1 || requests[0] != "GET /sn/ctf/client Bearer "+byJwt {
			t.Fatalf("network %q requests %q", byJwt, requests)
		}
	}
	// an older server refuses it: no wins, not unavailable, and the network
	// credential stays
	service.answer = olderServer
	for _, byJwt := range []string{networkJwt, "urn_apikey"} {
		api.SetByJwt(byJwt)
		if result = read(); result.Error != nil || result.Wins.Len() != 0 || result.UnclaimedWins.Len() != 0 || result.LatestWin != nil {
			t.Fatalf("older server %q: %+v", byJwt, result)
		}
		if api.GetByJwt() != byJwt || api.networkCredential() != byJwt || len(service.take()) != 1 {
			t.Fatalf("older server %q dropped the credential", byJwt)
		}
	}

	// the client token with the network credential kept beside it, as the apps
	// hold them: the network's wins, from one request
	withBoth := func() {
		api.SetByJwt(clientJwt)
		api.mutex.Lock()
		api.networkByJwt = networkJwt
		api.mutex.Unlock()
	}
	withBoth()
	service.answer = newServer
	if result = read(); result.Error != nil || result.ClientId != "" || result.UnclaimedWins.Len() != 1 {
		t.Fatalf("both %+v (%+v)", result, result.Error)
	}
	if requests := service.take(); len(requests) != 1 || requests[0] != "GET /sn/ctf/client Bearer "+networkJwt {
		t.Fatalf("both requests %q", requests)
	}
	// an older server: the network credential refused, then the client's wins
	service.answer = olderServer
	if result = read(); result.Error != nil || result.ClientId != testCtfClientId || result.Wins.Len() != 2 {
		t.Fatalf("both, older server %+v (%+v)", result, result.Error)
	}
	if requests := service.take(); len(requests) != 2 || requests[0] != "GET /sn/ctf/client Bearer "+networkJwt || requests[1] != "GET /sn/ctf/client Bearer "+clientJwt {
		t.Fatalf("both, older server requests %q", requests)
	}
	// both refused: unavailable
	service.answer = func(string) (int, string) { return http.StatusUnauthorized, testCtfUnauthorized }
	if result = read(); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || len(service.take()) != 2 {
		t.Fatalf("both refused %+v", result)
	}
	if api.GetByJwt() != clientJwt || api.networkCredential() != networkJwt {
		t.Fatal("a refused read dropped a credential")
	}

	// no credential: nothing is sent
	api.SetByJwt("")
	if result = read(); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.Error.Message != "the session has no credential" || len(service.take()) != 0 {
		t.Fatalf("no credential %+v", result.Error)
	}
	if logouts != 0 || signInsRequired != 0 {
		t.Fatalf("%d sign-outs and %d sign-ins required from capture the flag reads", logouts, signInsRequired)
	}

	// the control: the same 401 through the api's request seam would have
	// signed the network credential out
	api.SetByJwt(networkJwt)
	api.getHttpGetRaw()(ctx, server.URL+"/sn/ctf/client", networkJwt)
	if logouts != 1 || api.networkCredential() != "" {
		t.Fatalf("the control did not sign out: %d, %q", logouts, api.networkCredential())
	}
}

// snCtfClientResultOf: a read that failed is unavailable.
func TestSnCtfClientResultOfAFailedRead(t *testing.T) {
	result := snCtfClientResultOf(nil, errors.New("connection refused"))
	if result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || result.Error.Message != "connection refused" || result.Epochs == nil || result.Wins == nil || result.UnclaimedWins == nil {
		t.Fatalf("result %+v", result)
	}
}

// Through an installed transport, as a DeviceRemote's forwards a request to
// its device and hands back the server's 401 as the same typed status: the
// network credential refused, the client token asked next, and no credential
// dropped.
func TestSnCtfClientThroughAnInstalledTransport(t *testing.T) {
	ctx, api := newTestApiForURL(t, "https://api.test")
	clientJwt := testingJwt(map[string]any{"network_id": "00000000-0000-0000-0000-000000000001", "client_id": testCtfClientId})
	networkJwt := testingNetworkAdminJwt("00000000-0000-0000-0000-000000000001")
	var asked []string
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		asked = append(asked, requestUrl+" "+byJwt)
		if !credentialNamesClient(byJwt) {
			return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte(testCtfUnauthorized)}
		}
		return []byte(testCtfClientAnswer), nil
	})
	api.SetByJwt(clientJwt)
	api.mutex.Lock()
	api.networkByJwt = networkJwt
	api.mutex.Unlock()
	result := snCtfClient(ctx, api)
	if result.Error != nil || result.ClientId != testCtfClientId || result.UnclaimedWins.Len() != 1 {
		t.Fatalf("result %+v (%+v)", result, result.Error)
	}
	if len(asked) != 2 || asked[0] != "https://api.test/sn/ctf/client "+networkJwt || asked[1] != "https://api.test/sn/ctf/client "+clientJwt {
		t.Fatalf("asked %q", asked)
	}
	if api.GetByJwt() != clientJwt || api.networkCredential() != networkJwt {
		t.Fatal("a refused read dropped a credential")
	}
	// a failure that is not the server's 401 ends the read: unavailable
	asked = nil
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		asked = append(asked, "")
		return nil, errors.New("device rpc http concurrency limit reached")
	})
	if result = snCtfClient(ctx, api); result.Error == nil || result.Error.Code != SnErrorCodeCtfUnavailable || len(asked) != 1 {
		t.Fatalf("a failed read %+v, asked %d", result, len(asked))
	}
}
