package sdk

// The v2 balances the apps show (VERSION2-FINAL.md §11, CL-2): the accounts
// from GET /sn/ledger/account in every state, beside the v1 entitlements read
// as before, and "unavailable" rather than zero when the ledger cannot be
// read or answers with something malformed.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026/sn/evm"
	"github.com/urnetwork/sdk/v2026/sn/ss58"
)

// A ledger service answering GET /sn/ledger/account with the status and body
// it holds, recording each request.
type testLedgerService struct {
	mutex    sync.Mutex
	status   int
	body     []byte
	requests []string
}

func (self *testLedgerService) answer(status int, body []byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.status = status
	self.body = body
}

func (self *testLedgerService) takeRequests() []string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	requests := self.requests
	self.requests = nil
	return requests
}

func (self *testLedgerService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if r.URL.Path != "/sn/ledger/account" {
		http.NotFound(w, r)
		return
	}
	// the read is public: it never carries a credential
	self.requests = append(self.requests, r.Method+" "+r.RequestURI+" "+r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(self.status)
	w.Write(self.body)
}

func newTestLedgerService(t *testing.T, status int, body []byte) (*testLedgerService, *httptest.Server) {
	t.Helper()
	ledger := &testLedgerService{}
	ledger.answer(status, body)
	server := httptest.NewServer(ledger)
	t.Cleanup(server.Close)
	return ledger, server
}

const testLedgerVault = "0xabababababababababababababababababababab"

// One pool in each state the apps show: pool 1 paid in part, with a pending
// root above it; pool 2 paid in full, with no pending root; pool 3 only in the
// pending root; pool 4 never paid, at depth 0, above 2^53.
func testLedgerDocument(coldkey [32]byte) map[string]any {
	return map[string]any{
		"coldkey": "0x" + hex.EncodeToString(coldkey[:]),
		"accounts": []any{
			map[string]any{
				"vault": testLedgerVault, "no_id": 1, "index": 42, "root_index": 3,
				"cumulative": "123456789",
				"proof": []any{
					map[string]any{"hash": "0x" + strings.Repeat("01", 32), "sum": "5"},
					map[string]any{"hash": "0x" + strings.Repeat("2e", 32), "sum": "900000000"},
				},
				"claimed": "100000000", "pending_cumulative": "130000000",
			},
			map[string]any{
				"vault": testLedgerVault, "no_id": 2, "index": 0, "root_index": 0,
				"cumulative": "5000000000",
				"proof":      []any{map[string]any{"hash": "0x" + strings.Repeat("03", 32), "sum": "0"}},
				"claimed":    "5000000000", "pending_cumulative": nil,
			},
			map[string]any{
				"vault": testLedgerVault, "no_id": 3, "index": 7, "root_index": nil,
				"cumulative": "0", "proof": nil,
				"claimed": "0", "pending_cumulative": "777",
			},
			map[string]any{
				"vault": "0x" + strings.Repeat("cd", 20), "no_id": 4, "index": 1, "root_index": 0,
				"cumulative": "9007199254740993", "proof": []any{},
				"claimed": "0", "pending_cumulative": nil,
			},
		},
	}
}

// What testLedgerDocument shows.
func testLedgerAccounts() []*SnLedgerAccount {
	return []*SnLedgerAccount{
		{Vault: testLedgerVault, NoId: 1, Index: 42, FinalizedRao: 123_456_789, ClaimableRao: 23_456_789, PaidRao: 100_000_000, Pending: true, PendingRao: 130_000_000},
		{Vault: testLedgerVault, NoId: 2, Index: 0, FinalizedRao: 5_000_000_000, ClaimableRao: 0, PaidRao: 5_000_000_000},
		{Vault: testLedgerVault, NoId: 3, Index: 7, Pending: true, PendingRao: 777},
		{Vault: "0x" + strings.Repeat("cd", 20), NoId: 4, Index: 1, FinalizedRao: 9_007_199_254_740_993, ClaimableRao: 9_007_199_254_740_993},
	}
}

func testLedgerJson(t *testing.T, document any) []byte {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testAliceSs58(t *testing.T) ([32]byte, string) {
	t.Helper()
	alice := testColdkey(1)
	address, err := ss58.Encode(alice, SnSs58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	return alice, address
}

// testReadLedger waits for one v2 accounts read.
func testReadLedger(t *testing.T, read func(SnLedgerAccountsCallback)) *SnLedgerAccountsResult {
	t.Helper()
	results := make(chan *SnLedgerAccountsResult, 1)
	read(connect.NewApiCallback(func(result *SnLedgerAccountsResult, err error) {
		if err != nil {
			t.Errorf("ledger read error: %v", err)
		}
		results <- result
	}))
	select {
	case result := <-results:
		return result
	case <-time.After(30 * time.Second):
		t.Fatal("the ledger read did not call back")
		return nil
	}
}

// testReadLedgerAt reads as a host without a DeviceLocal does.
func testReadLedgerAt(t *testing.T, artifactBaseUrl string, coldkeySs58 string) *SnLedgerAccountsResult {
	t.Helper()
	settings := NewSnChainSettings()
	settings.ArtifactBaseUrl = artifactBaseUrl
	return testReadLedger(t, func(callback SnLedgerAccountsCallback) {
		SnLedgerAccountsFor(settings, coldkeySs58, callback)
	})
}

// testReadClaims waits for one v1 claims read.
func testReadClaims(t *testing.T, device *snDevice) *SnClaimsResult {
	t.Helper()
	results := make(chan *SnClaimsResult, 1)
	device.SnClaims(connect.NewApiCallback(func(result *SnClaimsResult, err error) {
		if err != nil {
			t.Errorf("claims read error: %v", err)
		}
		results <- result
	}))
	select {
	case result := <-results:
		return result
	case <-time.After(60 * time.Second):
		t.Fatal("the claims read did not call back")
		return nil
	}
}

func requireLedgerAccounts(t *testing.T, result *SnLedgerAccountsResult, want []*SnLedgerAccount) {
	t.Helper()
	if result.Error != nil {
		t.Fatalf("ledger error %+v", result.Error)
	}
	if got := result.Accounts.getAll(); !reflect.DeepEqual(got, want) {
		gotJson, _ := json.Marshal(got)
		wantJson, _ := json.Marshal(want)
		t.Fatalf("accounts\n%s\nwant\n%s", gotJson, wantJson)
	}
}

// requireLedgerUnavailable checks a read that shows no balance at all, only
// that the balances are unavailable.
func requireLedgerUnavailable(t *testing.T, result *SnLedgerAccountsResult, want string) {
	t.Helper()
	if result.Error == nil || result.Error.Code != SnErrorCodeLedgerUnavailable || !strings.Contains(result.Error.Message, want) {
		t.Fatalf("error %+v, want %s with %q", result.Error, SnErrorCodeLedgerUnavailable, want)
	}
	if result.Accounts == nil || result.Accounts.Len() != 0 {
		t.Fatalf("an unavailable ledger showed accounts: %+v", result.Accounts)
	}
}

// CL-2: every account state across several pools. Finalized is the
// cumulative, claimable the cumulative less claimed, paid the claimed
// watermark, and a pending root's cumulative is shown as pending, including
// for an account that is only in the pending root.
func TestSnLedgerAccountsShowEveryState(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	ledger, server := newTestLedgerService(t, http.StatusOK, testLedgerJson(t, testLedgerDocument(alice)))

	// a trailing slash on the host is dropped
	result := testReadLedgerAt(t, server.URL+"/", " "+aliceSs58+" ")
	requireLedgerAccounts(t, result, testLedgerAccounts())
	if result.ColdkeySs58 != aliceSs58 {
		t.Fatalf("coldkey %q", result.ColdkeySs58)
	}
	if requests := ledger.takeRequests(); len(requests) != 1 || requests[0] != "GET /sn/ledger/account?coldkey="+aliceSs58+" " {
		t.Fatalf("requests %q", requests)
	}
}

// CL-2: a coldkey the ledger has no account for shows no account, and no
// error: an empty list is an answer, unlike an unavailable ledger.
func TestSnLedgerAccountsUnknownColdkey(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	_, server := newTestLedgerService(t, http.StatusOK, []byte(`{"coldkey":"0x`+hex.EncodeToString(alice[:])+`","accounts":[]}`))

	result := testReadLedgerAt(t, server.URL, aliceSs58)
	requireLedgerAccounts(t, result, nil)
	if raw, _ := json.Marshal(result); !strings.Contains(string(raw), `"accounts":[]`) {
		t.Fatalf("json %s", raw)
	}
}

// CL-2: a ledger that cannot be reached, or answers with an http error or
// with something other than json, leaves the balances unavailable, never
// zero.
func TestSnLedgerAccountsUnavailableIsNeverZero(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	requireLedgerUnavailable(t, testReadLedgerAt(t, closed.URL, aliceSs58), "connection refused")

	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusInternalServerError, "ledger down", "http 500 fetching the ledger account"},
		{http.StatusServiceUnavailable, "", "http 503 fetching the ledger account"},
		// a server without the v2 ledger
		{http.StatusNotFound, "404 page not found", "http 404 fetching the ledger account"},
		{http.StatusBadRequest, "bad coldkey", "http 400 fetching the ledger account"},
		{http.StatusOK, "<html>maintenance</html>", "refused the ledger answer: invalid character"},
		{http.StatusOK, "", "refused the ledger answer: unexpected end of JSON input"},
	} {
		_, server := newTestLedgerService(t, c.status, []byte(c.body))
		requireLedgerUnavailable(t, testReadLedgerAt(t, server.URL, aliceSs58), c.want)
	}
}

// CL-2: an answer with any malformed part is refused whole, so none of its
// balances is shown. Each case changes one field of a well-formed answer.
func TestSnLedgerAccountsRefuseMalformedAnswers(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	bob := testColdkey(0x40)
	account := func(document map[string]any, i int) map[string]any {
		return document["accounts"].([]any)[i].(map[string]any)
	}
	proofNode := func(document map[string]any, i int, j int) map[string]any {
		return account(document, i)["proof"].([]any)[j].(map[string]any)
	}
	cases := []struct {
		name   string
		mutate func(document map[string]any)
		want   string
	}{
		// bad decimals
		{"a cumulative with a letter", func(d map[string]any) { account(d, 0)["cumulative"] = "12a" }, `account 0: cumulative: "12a" is not a decimal amount of rao`},
		{"a negative cumulative", func(d map[string]any) { account(d, 0)["cumulative"] = "-5" }, `account 0: cumulative: "-5" is not a decimal amount of rao`},
		{"a signed cumulative", func(d map[string]any) { account(d, 0)["cumulative"] = "+5" }, `account 0: cumulative: "+5" is not a decimal amount of rao`},
		{"a cumulative with a leading zero", func(d map[string]any) { account(d, 0)["cumulative"] = "0123456789" }, `account 0: cumulative: "0123456789" is not a decimal amount of rao`},
		{"a fractional cumulative", func(d map[string]any) { account(d, 0)["cumulative"] = "1.5" }, `account 0: cumulative: "1.5" is not a decimal amount of rao`},
		{"a cumulative beyond int64", func(d map[string]any) { account(d, 0)["cumulative"] = "9223372036854775808" }, `account 0: cumulative: "9223372036854775808" is not a decimal amount of rao`},
		{"a cumulative as a json number", func(d map[string]any) { account(d, 0)["cumulative"] = 123456789 }, "cumulative of type string"},
		{"a missing claimed", func(d map[string]any) { delete(account(d, 1), "claimed") }, `account 1: claimed: "" is not a decimal amount of rao`},
		{"a pending cumulative in exponent form", func(d map[string]any) { account(d, 0)["pending_cumulative"] = "1e3" }, `account 0: pending_cumulative: "1e3" is not a decimal amount of rao`},
		{"a hex proof sum", func(d map[string]any) { proofNode(d, 0, 1)["sum"] = "0x10" }, `account 0: proof 1 sum: "0x10" is not a decimal amount of rao`},
		// bad hex
		{"a coldkey in upper case", func(d map[string]any) { d["coldkey"] = "0x" + strings.ToUpper(hex.EncodeToString(alice[:])) }, "coldkey: "},
		{"a coldkey without 0x", func(d map[string]any) { d["coldkey"] = hex.EncodeToString(alice[:]) }, "coldkey: "},
		{"a coldkey of 31 bytes", func(d map[string]any) { d["coldkey"] = "0x" + hex.EncodeToString(alice[:31]) }, "coldkey: "},
		{"a vault in upper case", func(d map[string]any) { account(d, 1)["vault"] = "0x" + strings.Repeat("AB", 20) }, "account 1: vault: "},
		{"a vault of 19 bytes", func(d map[string]any) { account(d, 1)["vault"] = "0x" + strings.Repeat("ab", 19) }, "account 1: vault: "},
		{"a vault with a digit that is not hex", func(d map[string]any) { account(d, 1)["vault"] = "0x" + strings.Repeat("ab", 19) + "ag" }, "account 1: vault: encoding/hex: invalid byte"},
		{"a proof hash of 31 bytes", func(d map[string]any) { proofNode(d, 1, 0)["hash"] = "0x" + strings.Repeat("03", 31) }, "account 1: proof 0 hash: "},
		{"a proof hash with a digit that is not hex", func(d map[string]any) { proofNode(d, 0, 0)["hash"] = "0x" + strings.Repeat("0g", 32) }, "account 0: proof 0 hash: encoding/hex: invalid byte"},
		// negative claimable
		{"claimed above the cumulative", func(d map[string]any) { account(d, 0)["claimed"] = "123456790" }, "account 0: claimed 123456790 exceeds the finalized cumulative 123456789"},
		{"claimed on an account only in the pending root", func(d map[string]any) { account(d, 2)["claimed"] = "1" }, "account 2: claimed 1 exceeds the finalized cumulative 0"},
		// the answer's shape
		{"the answer for another coldkey", func(d map[string]any) { d["coldkey"] = "0x" + hex.EncodeToString(bob[:]) }, "the answer is for another coldkey"},
		{"no accounts list", func(d map[string]any) { delete(d, "accounts") }, "the answer has no accounts list"},
		{"a null accounts list", func(d map[string]any) { d["accounts"] = nil }, "the answer has no accounts list"},
		{"a zero no_id", func(d map[string]any) { account(d, 0)["no_id"] = 0 }, "account 0: no_id 0 or index 42 out of range"},
		{"a negative index", func(d map[string]any) { account(d, 0)["index"] = -1 }, "account 0: no_id 1 or index -1 out of range"},
		{"a fractional index", func(d map[string]any) { account(d, 0)["index"] = 1.5 }, "cannot unmarshal number 1.5"},
		{"a negative root index", func(d map[string]any) { account(d, 1)["root_index"] = -1 }, "account 1: an account in a finalized root needs its root index and proof"},
		{"a finalized account without a proof", func(d map[string]any) { account(d, 3)["proof"] = nil }, "account 3: an account in a finalized root needs its root index and proof"},
		{"a cumulative on an account only in the pending root", func(d map[string]any) { account(d, 2)["cumulative"] = "5" }, "account 2: an account in no finalized root must be pending, with a zero cumulative and no proof"},
		{"a proof on an account only in the pending root", func(d map[string]any) { account(d, 2)["proof"] = []any{} }, "account 2: an account in no finalized root must be pending, with a zero cumulative and no proof"},
		{"an account in neither root", func(d map[string]any) { account(d, 2)["pending_cumulative"] = nil }, "account 2: an account in no finalized root must be pending, with a zero cumulative and no proof"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			document := testLedgerDocument(alice)
			c.mutate(document)
			_, server := newTestLedgerService(t, http.StatusOK, testLedgerJson(t, document))
			requireLedgerUnavailable(t, testReadLedgerAt(t, server.URL, aliceSs58), c.want)
		})
	}
}

// CL-2: the ledger is read from the default artifact host when none is
// configured, and a coldkey that is not a Bittensor address is refused
// before any request.
func TestSnLedgerAccountsRequestTarget(t *testing.T) {
	_, aliceSs58 := testAliceSs58(t)
	var requested []string
	fetch := func(ctx context.Context, requestUrl string) ([]byte, error) {
		requested = append(requested, requestUrl)
		return nil, errors.New("offline")
	}
	result := snLedgerAccounts(context.Background(), fetch, "", aliceSs58)
	requireLedgerUnavailable(t, result, "offline")
	if len(requested) != 1 || requested[0] != "https://api.bringyour.com/sn/ledger/account?coldkey="+aliceSs58 {
		t.Fatalf("requested %q", requested)
	}

	requested = nil
	result = snLedgerAccounts(context.Background(), fetch, "", "0x"+strings.Repeat("01", 32))
	if result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress || result.Accounts.Len() != 0 || len(requested) != 0 {
		t.Fatalf("hex coldkey: %+v, requested %q", result.Error, requested)
	}
	// without settings, as a host may call it
	result = testReadLedger(t, func(callback SnLedgerAccountsCallback) {
		SnLedgerAccountsFor(nil, "not an address", callback)
	})
	if result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress || result.Accounts.Len() != 0 {
		t.Fatalf("invalid address: %+v", result.Error)
	}
}

// CL-2: the apps read the v2 accounts through either device. Without a
// connected wallet the DeviceLocal asks for one, and the app-side DeviceRemote
// reads the accounts while its tunnel process is down.
func TestSnLedgerAccountsOnBothDevices(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	result := testReadLedger(t, deviceLocal.SnLedgerAccounts)
	if result.Error == nil || result.Error.Code != SnErrorCodeConnectWalletFirst || result.Accounts.Len() != 0 {
		t.Fatalf("device local without a wallet: %+v", result.Error)
	}

	ledger := &testLedgerService{}
	ledger.answer(http.StatusOK, testLedgerJson(t, testLedgerDocument(alice)))
	server := httptest.NewServer(testApiHandler(ledger))
	defer server.Close()
	space, localState := testingLogoutCauseSpace(t, server.URL)
	if err := localState.SetSnWallet(&SnWallet{ColdkeySs58: aliceSs58}); err != nil {
		t.Fatal(err)
	}
	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, space, "")
	requireLedgerAccounts(t, testReadLedger(t, deviceRemote.SnLedgerAccounts), testLedgerAccounts())
}

// CL-2: on a device the v1 entitlements are read from the v1 vault as before
// and shown separately from the v2 accounts. Neither read changes the other:
// the ledger failing leaves the v1 claims as they were, and the chain failing
// leaves the v2 accounts shown.
func TestSnLedgerAccountsBesideV1Claims(t *testing.T) {
	alice, aliceSs58 := testAliceSs58(t)
	raw7, hash7, root7 := buildTestArtifact(t, 7, 3, [][32]byte{alice, testColdkey(0x40)}, []uint64{1234, 8766})
	ledgerAnswer := testLedgerJson(t, testLedgerDocument(alice))
	ledger := &testLedgerService{}
	ledger.answer(http.StatusOK, ledgerAnswer)
	// the operator's api host serves the v1 artifacts and the v2 ledger
	mux := http.NewServeMux()
	mux.Handle("/sn/ledger/account", ledger)
	mux.HandleFunc("/sn/artifact", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hash") != "sha256:"+hex.EncodeToString(hash7[:]) {
			http.NotFound(w, r)
			return
		}
		w.Write(raw7)
	})
	operator := httptest.NewServer(testApiHandler(mux))
	defer operator.Close()

	vault, _ := evm.ParseAddress("0x2222222222222222222222222222222222222222")
	coordinator, _ := evm.ParseAddress("0x1111111111111111111111111111111111111111")
	chain := &fakeChain{
		t: t, block: 1000, currentEpoch: 9, vault: vault, coordinator: coordinator,
		entitlements: map[int64]*evm.Entitlement{
			7: {PayoutRoot: root7, ArtifactHash: hash7, Funded: big.NewInt(1_000_000_000), Total: big.NewInt(1_000_000_000), Claimed: big.NewInt(0), ExpiryBlock: 5000, Status: evm.StatusFinalized},
		},
		claimed:  map[string]bool{},
		receipts: map[string]bool{},
		headTime: 1_791_244_800,
	}
	chainServer := httptest.NewServer(chain)
	defer chainServer.Close()

	space, localState := testingLogoutCauseSpace(t, operator.URL)
	settings := SnTestnetChainSettings()
	settings.SetRpcUrls(nil)
	settings.AddRpcUrl(chainServer.URL)
	settings.VaultAddress = "0x2222222222222222222222222222222222222222"
	settings.CoordinatorAddress = "0x1111111111111111111111111111111111111111"
	settings.NoId = "3"
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, api := newTestApiForURL(t, operator.URL)
	device := &snDevice{
		ctx:          ctx,
		networkSpace: space,
		// one direct dialer, so that each read is one request
		api:      func() *Api { return api },
		clientId: connect.NewId(),
		state:    newDeviceSn(),
	}

	// no account is read without a connected wallet
	result := testReadLedger(t, device.SnLedgerAccounts)
	if result.Error == nil || result.Error.Code != SnErrorCodeConnectWalletFirst || result.Accounts.Len() != 0 {
		t.Fatalf("without a wallet: %+v", result.Error)
	}
	device.setSnWallet(&SnWallet{ColdkeySs58: aliceSs58})

	// v1 as before: epoch 7's share of the v1 vault, and nothing of the ledger
	claims := testReadClaims(t, device)
	if claims.Error != nil || claims.TotalClaimableRao != 123_400_000 || claims.Claims.Len() != 3 {
		t.Fatalf("v1 claims %+v", claims)
	}
	if c := claims.Claims.Get(2); c.Epoch != 7 || c.Status != SnClaimStatusClaimable || c.AmountRao != 123_400_000 {
		t.Fatalf("v1 epoch 7 %+v", c)
	}
	v1Json := testLedgerJson(t, claims)

	// v2 beside it, from the api host, without a credential
	requireLedgerAccounts(t, testReadLedger(t, device.SnLedgerAccounts), testLedgerAccounts())
	if requests := ledger.takeRequests(); len(requests) != 1 || requests[0] != "GET /sn/ledger/account?coldkey="+aliceSs58+" " {
		t.Fatalf("requests %q", requests)
	}
	if again := testLedgerJson(t, testReadClaims(t, device)); string(again) != string(v1Json) {
		t.Fatalf("v1 changed after the v2 read\n%s\nwant\n%s", again, v1Json)
	}

	// the ledger fails: v2 is unavailable and v1 is as it was
	ledger.answer(http.StatusInternalServerError, []byte("ledger down"))
	requireLedgerUnavailable(t, testReadLedger(t, device.SnLedgerAccounts), "500 Internal Server Error: ledger down")
	if again := testLedgerJson(t, testReadClaims(t, device)); string(again) != string(v1Json) {
		t.Fatalf("v1 changed with the ledger down\n%s\nwant\n%s", again, v1Json)
	}

	// the chain fails: v1 has an error and v2 is shown
	ledger.answer(http.StatusOK, ledgerAnswer)
	chainServer.Close()
	if claims := testReadClaims(t, device); claims.Error == nil || claims.Error.Code != SnErrorCodeChainRpcUnreachable {
		t.Fatalf("v1 with the chain down: %+v", claims.Error)
	}
	requireLedgerAccounts(t, testReadLedger(t, device.SnLedgerAccounts), testLedgerAccounts())

	// a configured artifact host serves the ledger in place of the api host
	other, otherServer := newTestLedgerService(t, http.StatusOK, ledgerAnswer)
	settings.ArtifactBaseUrl = otherServer.URL
	if err := localState.SetSnChainSettings(settings); err != nil {
		t.Fatal(err)
	}
	ledger.takeRequests()
	requireLedgerAccounts(t, testReadLedger(t, device.SnLedgerAccounts), testLedgerAccounts())
	if requests := other.takeRequests(); len(requests) != 1 || len(ledger.takeRequests()) != 0 {
		t.Fatalf("artifact host requests %q", requests)
	}
}
