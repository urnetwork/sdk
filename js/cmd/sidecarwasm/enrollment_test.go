//go:build js && wasm && sidecar_http

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall/js"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

type pageFixtureRoundTrip func(*http.Request) (*http.Response, error)

func (f pageFixtureRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrowserBindsMarkedClientAndWaitsForAdmission(t *testing.T) {
	networkId, clientId := sidecar.NewId(), sidecar.NewId()
	expires := time.Now().Add(7 * 24 * time.Hour).Unix()
	claims := fmt.Sprintf(`{"network_id":%q,"client_id":%q,"device_id":%q,"iat":%d,"exp":%d,"strict_exp":true,"admin_refused":true,"declares_no_egress":true}`,
		networkId, clientId, sidecar.NewId(), time.Now().Unix(), expires)
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	token := encode(`{"alg":"RS256"}`) + "." + encode(claims) + ".YQ"

	var binds atomic.Int32
	previousTransport := http.DefaultTransport
	http.DefaultTransport = pageFixtureRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "api.pagetest.example" {
			return nil, fmt.Errorf("unexpected fixture service")
		}
		status, body := 404, "{}"
		if r.URL.Path == "/sidecar/bind" {
			var wire struct{ Binding []byte }
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				return nil, err
			}
			binding, err := sidecar.VerifySidecarIdentityBinding(wire.Binding)
			if err != nil || binding.NetworkId != networkId || binding.ClientId != clientId || r.Header.Get("Authorization") != "Bearer "+token {
				return nil, fmt.Errorf("invalid fixture binding")
			}
			binds.Add(1)
			status, body = 200, `{"generation":1}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	var mints atomic.Int32
	mint := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].String() != "one-time" {
			return js.Global().Get("Promise").Call("resolve", js.Null())
		}
		mints.Add(1)
		return js.Global().Get("Promise").Call("resolve", map[string]any{
			"client_id": clientId.String(), "by_client_jwt": token, "admin_refused": true, "strict_exp": true, "declares_no_egress": true,
		})
	})
	js.Global().Set("urnetworkSidecarMint", mint)
	t.Cleanup(func() {
		js.Global().Delete("urnetworkSidecarMint")
		mint.Release()
	})
	worker := testPageWorker(t)
	testOpenNetwork(t, worker, networkId)
	first := testPageReply(t, worker, `{"op":"admission_status","id":"1"}`)
	if !first.Ok || first.Admission == nil || first.Admission.State != "unenrolled" {
		t.Fatalf("initial admission: %+v", first)
	}
	reply := testPageReply(t, worker, `{"op":"enroll","id":"2","code":"one-time"}`)
	if !reply.Ok || reply.Admission == nil || reply.Admission.State != "waiting" || binds.Load() < 1 || mints.Load() != 1 {
		t.Fatalf("bound browser: %+v, binds %d, mints %d", reply, binds.Load(), mints.Load())
	}
	raw := worker.handle(t.Context(), []byte(`{"op":"admission_status","id":"3"}`))
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "one-time") {
		t.Fatal("credential in page reply")
	}
	records := worker.current().instance.Networks()
	if len(records) != 1 || records[0].Child || records[0].ClientId != clientId {
		t.Fatal("incorrect persisted page identity")
	}
}
