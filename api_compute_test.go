package sdk

// The mirror of api's compute routes (CSRV-M12-04; DESIGN-COMPUTE §14.3): each method reaches its route's method and
// path with the client's jwt, a GET's one query value only when set, a POST's JSON object as its body; each answer's
// JSON comes back as text; a refusal is a *ComputeApiError with its code and wait; and a POST's body that is no JSON
// object is not sent.

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// One request a test api server saw.
type computeApiSeenRequest struct {
	method        string
	path          string
	rawQuery      string
	authorization string
	body          string
}

// An api server answering every compute route with answer, recording each request.
type computeApiTestServer struct {
	stateLock sync.Mutex
	requests  []computeApiSeenRequest
	answer    func(w http.ResponseWriter, r *http.Request)
}

func (self *computeApiTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	self.stateLock.Lock()
	self.requests = append(self.requests, computeApiSeenRequest{
		method:        r.Method,
		path:          r.URL.Path,
		rawQuery:      r.URL.RawQuery,
		authorization: r.Header.Get("Authorization"),
		body:          string(body),
	})
	answer := self.answer
	self.stateLock.Unlock()
	answer(w, r)
}

func (self *computeApiTestServer) seen() []computeApiSeenRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]computeApiSeenRequest{}, self.requests...)
}

// Each method reaches its route with the client's jwt, and each answer's JSON comes back as it was written.
func TestComputeApiCallsReachTheirRoutes(t *testing.T) {
	server := &computeApiTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"route":"` + r.Method + " " + r.URL.Path + `"}`))
	}}
	_, api := newTestApi(t, server)
	byJwt := dpopTestStrictToken(t)
	api.SetByJwt(byJwt)
	args := &ComputeApiArgs{Json: ` {"pack_credits":10} `}
	type call struct {
		method   string
		path     string
		rawQuery string
		body     string
		run      func() (*ComputeApiResult, error)
	}
	calls := []call{
		{method: "GET", path: "/compute/balance", run: func() (*ComputeApiResult, error) { return api.ComputeBalanceSync() }},
		{method: "POST", path: "/compute/checkout", body: `{"pack_credits":10}`, run: func() (*ComputeApiResult, error) { return api.ComputeCheckoutSync(args) }},
		{method: "GET", path: "/compute/auto-renew", run: func() (*ComputeApiResult, error) { return api.ComputeAutoRenewSync() }},
		{method: "POST", path: "/compute/auto-renew", body: `{"pack_credits":10}`, run: func() (*ComputeApiResult, error) { return api.ComputeSetAutoRenewSync(args) }},
		{method: "GET", path: "/compute/charges", run: func() (*ComputeApiResult, error) { return api.ComputeChargesSync("") }},
		{method: "GET", path: "/compute/charges", rawQuery: "cursor=page+2%2F3", run: func() (*ComputeApiResult, error) { return api.ComputeChargesSync("page 2/3") }},
		{method: "GET", path: "/compute/statement", rawQuery: "month=2026-11", run: func() (*ComputeApiResult, error) { return api.ComputeStatementSync("2026-11") }},
		{method: "GET", path: "/compute/spend", run: func() (*ComputeApiResult, error) { return api.ComputeSpendSync("") }},
		{method: "GET", path: "/compute/spend", rawQuery: "by=model", run: func() (*ComputeApiResult, error) { return api.ComputeSpendSync("model") }},
		{method: "POST", path: "/compute/refund", body: `{}`, run: func() (*ComputeApiResult, error) { return api.ComputeRefundSync(nil) }},
		{method: "POST", path: "/compute/promo/redeem", body: `{"pack_credits":10}`, run: func() (*ComputeApiResult, error) { return api.ComputePromoRedeemSync(args) }},
		{method: "GET", path: "/compute/terms", run: func() (*ComputeApiResult, error) { return api.ComputeTermsSync() }},
		{method: "POST", path: "/compute/terms", body: `{}`, run: func() (*ComputeApiResult, error) { return api.ComputeAcceptTermsSync(&ComputeApiArgs{}) }},
	}
	for _, c := range calls {
		result, err := c.run()
		if err != nil || result.Json != `{"route":"`+c.method+" "+c.path+`"}` {
			t.Errorf("%s %s: %+v %v", c.method, c.path, result, err)
		}
	}
	seen := server.seen()
	if len(seen) != len(calls) {
		t.Fatalf("the server saw %d requests, %d sent", len(seen), len(calls))
	}
	for i, c := range calls {
		request := seen[i]
		if request.method != c.method || request.path != c.path || request.rawQuery != c.rawQuery || request.body != c.body {
			t.Errorf("request %d is %+v, want %s %s ?%s %s", i, request, c.method, c.path, c.rawQuery, c.body)
		}
		if request.authorization != "Bearer "+byJwt {
			t.Errorf("%s %s did not carry the client's jwt", c.method, c.path)
		}
	}

	// and the callback form answers as the blocking one
	results := make(chan *ComputeApiResult, 1)
	api.ComputeBalance(connect.NewApiCallback[*ComputeApiResult](func(result *ComputeApiResult, err error) {
		if err != nil {
			t.Errorf("the callback's error: %v", err)
		}
		results <- result
	}))
	if result := <-results; result == nil || result.Json != `{"route":"GET /compute/balance"}` {
		t.Errorf("the callback's result: %+v", result)
	}
}

// A refusal whose body names a code is a *ComputeApiError with its status, code and wait, which the helpers read; a
// failure with no code stays the transport's error.
func TestComputeApiRefusalIsAComputeApiError(t *testing.T) {
	server := &computeApiTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compute/promo/redeem":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":"rate_limited","retry_after_ms":3600000}`))
		case "/compute/balance":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			w.Write([]byte(`{"code":"not_built","retry_after_ms":0}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("bad gateway"))
		}
	}}
	_, api := newTestApi(t, server)
	api.SetByJwt(dpopTestStrictToken(t))

	_, err := api.ComputePromoRedeemSync(&ComputeApiArgs{Json: `{"code":"TESTCODE1234"}`})
	var refusal *ComputeApiError
	if !errors.As(err, &refusal) || refusal.StatusCode != http.StatusTooManyRequests || refusal.Code != "rate_limited" || refusal.RetryAfterMs != 3600000 {
		t.Fatalf("a 429: %v", err)
	}
	if ComputeApiErrorCode(err) != "rate_limited" || ComputeApiErrorRetryAfterMs(err) != 3600000 {
		t.Errorf("the helpers read %q %d", ComputeApiErrorCode(err), ComputeApiErrorRetryAfterMs(err))
	}
	if _, err := api.ComputeBalanceSync(); ComputeApiErrorCode(err) != "not_built" {
		t.Errorf("an unbuilt route: %v", err)
	}
	_, err = api.ComputeTermsSync()
	if err == nil || errors.As(err, &refusal) || ComputeApiErrorCode(err) != "" || ComputeApiErrorRetryAfterMs(err) != 0 {
		t.Errorf("a failure with no code: %v", err)
	}
}

// A POST's body that is no JSON object is refused before anything is sent.
func TestComputeApiArgsThatAreNoObjectAreNotSent(t *testing.T) {
	server := &computeApiTestServer{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}}
	_, api := newTestApi(t, server)
	api.SetByJwt(dpopTestStrictToken(t))
	for _, text := range []string{`["TESTCODE1234"]`, `code=TESTCODE1234`, `{"code":`, `  `} {
		if _, err := api.ComputePromoRedeemSync(&ComputeApiArgs{Json: text}); !errors.Is(err, ErrComputeApiArgs) {
			t.Errorf("%q: %v", text, err)
		}
	}
	if seen := server.seen(); len(seen) != 0 {
		t.Fatalf("the server saw %d requests", len(seen))
	}
}
