// The payout wallet disconnect calls through the actual transport against a
// local fake server, and the rule the apps use to offer it.
package sdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Both calls carry the network credential, the code goes as the server reads
// it, and the answers read as the server sends them. An api without the
// network credential sends nothing.
func TestSnWalletDisconnectCallsSendTheNetworkCredential(t *testing.T) {
	server := newAccountTestServer(t, map[string]func(string) (int, string){
		"POST /sn/wallet/disconnect-code": accountAnswer(`{"sent_to":["j…@example.com","o…@example.org"],"expires_at_millis":1791244800000}`),
		"POST /sn/wallet/disconnect":      accountAnswer(`{"disconnected":true}`),
	})
	ctx, api, networkJwt, _ := newAccountTestApi(t, server)

	sent, err := api.SnWalletDisconnectCodeSyncWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, sent.SentTo.Len(), 2)
	connect.AssertEqual(t, sent.SentTo.Get(0), "j…@example.com")
	connect.AssertEqual(t, sent.ExpiresAtMillis, int64(1791244800000))

	disconnected, err := awaitAccountCall(t, func(cb connect.ApiCallback[*SnWalletDisconnectResult]) { api.SnWalletDisconnect(" 12-34 56 ", cb) })
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, disconnected.Disconnected, true)

	requests := server.takeRequests()
	connect.AssertEqual(t, len(requests), 2)
	for _, request := range requests {
		if request.byJwt != networkJwt {
			t.Fatalf("%s carried %q, want the network credential", request.route, request.byJwt)
		}
	}
	body := map[string]string{}
	if err := json.Unmarshal([]byte(requests[1].body), &body); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, body, map[string]string{"code": "123456"})

	_, clientOnly := newTestApi(t, server)
	clientOnly.SetByJwt(credentialTestClientJwt(t, credentialTestNetworkId, "device"))
	if _, err := clientOnly.SnWalletDisconnectSyncWithContext(ctx, "123456"); !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("err = %v, want ErrNetworkCredentialRequired", err)
	}
	connect.AssertEqual(t, len(server.takeRequests()), 0)
}

func TestSnWalletDisconnectRefusalsRead(t *testing.T) {
	server := newAccountTestServer(t, map[string]func(string) (int, string){
		"POST /sn/wallet/disconnect-code": accountAnswer(`{"error":{"code":"consent_in_force","message":"Your signed payout consent pays your earnings."}}`),
		"POST /sn/wallet/disconnect":      accountAnswer(`{"disconnected":false,"error":{"code":"rate_limited","message":"Too many codes.","retry_after_seconds":600}}`),
	})
	ctx, api, _, _ := newAccountTestApi(t, server)

	sent, err := api.SnWalletDisconnectCodeSyncWithContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, sent.Error.Code, SnWalletDisconnectErrorCodeConsentInForce)

	disconnected, err := api.SnWalletDisconnectSyncWithContext(ctx, "123456")
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, disconnected.Disconnected, false)
	connect.AssertEqual(t, disconnected.Error.Code, SnWalletDisconnectErrorCodeRateLimited)
	connect.AssertEqual(t, disconnected.Error.RetryAfterSeconds, int64(600))
}

func snDisconnectTestWallets(t *testing.T, entries ...string) *SnGetWalletResult {
	t.Helper()
	result := &SnGetWalletResult{}
	raw := `{"wallets":[` + strings.Join(entries, ",") + `]}`
	if err := json.Unmarshal([]byte(raw), result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Disconnect is offered for a wallet connected without a consent, and never
// while a signed network consent or hotkey delegation pays the network (only
// a signed replacement ends one). A provider's own consent neither offers nor
// blocks it.
func TestSnGetWalletResultDisconnectable(t *testing.T) {
	sideCopy := fmt.Sprintf(`{"coldkey_ss58":%q,"set_at_millis":1}`, bittensorTestBobSs58)
	unsignedProvider := fmt.Sprintf(`{"coldkey_ss58":%q,"client_id":%q,"set_at_millis":1}`, bittensorTestBobSs58, credentialTestClientId)
	providerConsent := fmt.Sprintf(`{"coldkey_ss58":%q,"client_id":%q,"set_at_millis":1,"consent_scope":"provider","from_epoch":9,"through_epoch":65544}`, bittensorTestBobSs58, credentialTestClientId)
	networkConsent := fmt.Sprintf(`{"coldkey_ss58":%q,"set_at_millis":1,"consent_scope":"network","from_epoch":9,"through_epoch":65544}`, bittensorTestBobSs58)
	hotkey := fmt.Sprintf(`{"coldkey_ss58":%q,"set_at_millis":1,"consent_scope":"hotkey","from_epoch":9,"through_epoch":65544,"hotkey_ss58":%q}`, bittensorTestBobSs58, bittensorTestBobSs58)

	for _, c := range []struct {
		name           string
		entries        []string
		disconnectable bool
		hasWallet      bool
	}{
		{"no wallet", nil, false, false},
		{"the connected wallet", []string{sideCopy}, true, true},
		{"a provider's wallet without a consent", []string{unsignedProvider}, true, true},
		{"a provider's own consent only", []string{providerConsent}, false, true},
		{"the connected wallet beside a provider's own consent", []string{sideCopy, providerConsent}, true, true},
		{"a network consent", []string{networkConsent}, false, true},
		{"a network consent before the connected wallet", []string{networkConsent, sideCopy}, false, true},
		{"a hotkey delegation with the connected wallet", []string{hotkey, sideCopy}, false, true},
	} {
		result := snDisconnectTestWallets(t, c.entries...)
		if disconnectable := result.Disconnectable(); disconnectable != c.disconnectable {
			t.Errorf("%s: Disconnectable = %t, want %t", c.name, disconnectable, c.disconnectable)
		}
		if hasWallet := result.HasWallet(); hasWallet != c.hasWallet {
			t.Errorf("%s: HasWallet = %t, want %t", c.name, hasWallet, c.hasWallet)
		}
	}
	var none *SnGetWalletResult
	connect.AssertEqual(t, none.Disconnectable(), false)
	connect.AssertEqual(t, none.HasWallet(), false)
}
