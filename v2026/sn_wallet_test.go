package sdk

// ConnectSnWallet against a scripted api: what reaches the apps when
// POST /sn/wallet refuses the coldkey.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// POST /sn/wallet refuses a coldkey signature made by another account than
// the typed address with error.code signature_mismatch. ConnectSnWallet keeps
// that code in SnError.Code so the apps can say what went wrong in the user's
// language. A refusal without a code, or with one the apps have no words for,
// stays server_error, as before. Nothing is cached for a refusal.
func TestConnectSnWalletKeepsTheSignatureMismatchCode(t *testing.T) {
	// the well-known substrate dev account (//Alice), a test-only key
	const address = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	const mismatchMessage = "The signature does not match this coldkey address. Sign the challenge with this address."

	cases := []struct {
		name        string
		setResponse string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "signature from another account",
			setResponse: fmt.Sprintf(`{"error":{"code":"signature_mismatch","message":%q}}`, mismatchMessage),
			wantCode:    SnErrorCodeSignatureMismatch,
			wantMessage: mismatchMessage,
		},
		{
			name:        "no code",
			setResponse: `{"error":{"message":"400 invalid signature encoding"}}`,
			wantCode:    SnErrorCodeServer,
			wantMessage: "400 invalid signature encoding",
		},
		{
			name:        "a code the apps have no words for",
			setResponse: `{"error":{"code":"something_new","message":"Something new."}}`,
			wantCode:    SnErrorCodeServer,
			wantMessage: "Something new.",
		},
	}
	for _, c := range cases {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/sn/wallet/validate":
				fmt.Fprint(w, `{"valid_syntax":true,"exists_on_chain":true,"banned":false}`)
			case "/sn/wallet":
				fmt.Fprint(w, c.setResponse)
			default:
				http.Error(w, "unexpected route", http.StatusNotFound)
			}
		})
		ctx, api := newTestApi(t, handler)
		api.SetByJwt("subnet-bearer")
		device := &snDevice{
			ctx:      ctx,
			api:      func() *Api { return api },
			clientId: connect.NewId(),
			state:    newDeviceSn(),
		}

		result, err := device.connectSnWallet(ctx, address, "0x"+fmt.Sprintf("%0128x", 1), "Sign in to URnetwork\nChallenge: abc\nTimestamp: 1")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if result.Error == nil {
			t.Fatalf("%s: the refused set connected: %+v", c.name, result)
		}
		if result.Error.Code != c.wantCode || result.Error.Message != c.wantMessage {
			t.Errorf("%s: error = %+v, want code %q message %q", c.name, result.Error, c.wantCode, c.wantMessage)
		}
		if result.Wallet != nil || device.GetSnWallet() != nil {
			t.Errorf("%s: a refused set cached a wallet", c.name)
		}
	}
}

// The wallet a device shows follows settlement: this client's own provider
// consent, then the network consent, then the network's hotkey entry, then
// this client's other wallet, then the network-level copy, whatever order
// they are listed in. A server without consent scopes keeps the earlier
// order. The decoded server json is the input, as SyncSnWallet receives it.
func TestSnPickWalletFollowsSettlementPrecedence(t *testing.T) {
	const clientId = "00000000-0000-0000-0000-000000000001"
	const other = "00000000-0000-0000-0000-000000000002"
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "own provider consent wins over the network consent",
			json: `{"wallets":[{"coldkey_ss58":"network-consent","consent_scope":"network","set_at_millis":1},{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"own-consent","client_id":"` + clientId + `","consent_scope":"provider","set_at_millis":3}]}`,
			want: "own-consent",
		},
		{
			name: "the network consent wins over this client's login-proof wallet",
			json: `{"wallets":[{"coldkey_ss58":"network-consent","consent_scope":"network","set_at_millis":1},{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"own-login","client_id":"` + clientId + `","set_at_millis":3}]}`,
			want: "network-consent",
		},
		{
			name: "own provider consent wins over the hotkey entry",
			json: `{"wallets":[{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":1},{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"own-consent","client_id":"` + clientId + `","consent_scope":"provider","set_at_millis":3}]}`,
			want: "own-consent",
		},
		{
			name: "the network consent wins over the hotkey entry listed before it",
			json: `{"wallets":[{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":1},{"coldkey_ss58":"network-consent","consent_scope":"network","set_at_millis":2},{"coldkey_ss58":"side-copy","set_at_millis":3}]}`,
			want: "network-consent",
		},
		{
			name: "the hotkey entry wins over this client's login-proof wallet",
			json: `{"wallets":[{"coldkey_ss58":"side-copy","set_at_millis":1},{"coldkey_ss58":"own-login","client_id":"` + clientId + `","set_at_millis":2},{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":3}]}`,
			want: "hotkey-entry",
		},
		{
			name: "the hotkey entry wins over the side copy listed before it",
			json: `{"wallets":[{"coldkey_ss58":"side-copy","set_at_millis":1},{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":2}]}`,
			want: "hotkey-entry",
		},
		{
			name: "only the effective hotkey entry",
			json: `{"wallets":[],"wallet":{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":1}}`,
			want: "hotkey-entry",
		},
		{
			name: "another client's consent is not this client's",
			json: `{"wallets":[{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"other-consent","client_id":"` + other + `","consent_scope":"provider","set_at_millis":3}]}`,
			want: "side-copy",
		},
		{
			name: "a server without scopes keeps this client's wallet first",
			json: `{"wallets":[{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"own-wallet","client_id":"` + clientId + `","set_at_millis":3}],"wallet":{"coldkey_ss58":"own-wallet","client_id":"` + clientId + `","set_at_millis":3}}`,
			want: "own-wallet",
		},
		{
			name: "only the effective wallet",
			json: `{"wallets":[],"wallet":{"coldkey_ss58":"network-consent","consent_scope":"network","set_at_millis":1}}`,
			want: "network-consent",
		},
		{
			name: "no wallet",
			json: `{"wallets":[]}`,
			want: "",
		},
	}
	for _, c := range cases {
		var result SnGetWalletResult
		if err := json.Unmarshal([]byte(c.json), &result); err != nil {
			t.Fatal(c.name, err)
		}
		wallet := snPickWallet(&result, clientId)
		got := ""
		if wallet != nil {
			got = wallet.ColdkeySs58
		}
		if got != c.want {
			t.Errorf("%s: picked %q, want %q", c.name, got, c.want)
		}
	}
}

// A network session (no client id) has no wallet of its own client: the
// network consent, then the hotkey entry, then the network-level copy.
func TestSnPickWalletForANetworkSession(t *testing.T) {
	const clientId = "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "the network consent wins over the hotkey entry",
			json: `{"wallets":[{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":1},{"coldkey_ss58":"side-copy","set_at_millis":2},{"coldkey_ss58":"network-consent","consent_scope":"network","set_at_millis":3}]}`,
			want: "network-consent",
		},
		{
			name: "the hotkey entry wins over the side copy listed before it",
			json: `{"wallets":[{"coldkey_ss58":"side-copy","set_at_millis":1},{"coldkey_ss58":"hotkey-entry","consent_scope":"hotkey","set_at_millis":2}]}`,
			want: "hotkey-entry",
		},
		{
			name: "a client's wallets are not the network's",
			json: `{"wallets":[{"coldkey_ss58":"client-consent","client_id":"` + clientId + `","consent_scope":"provider","set_at_millis":1},{"coldkey_ss58":"client-login","client_id":"` + clientId + `","set_at_millis":2},{"coldkey_ss58":"side-copy","set_at_millis":3}]}`,
			want: "side-copy",
		},
	}
	for _, c := range cases {
		var result SnGetWalletResult
		if err := json.Unmarshal([]byte(c.json), &result); err != nil {
			t.Fatal(c.name, err)
		}
		wallet := snPickWallet(&result, "")
		got := ""
		if wallet != nil {
			got = wallet.ColdkeySs58
		}
		if got != c.want {
			t.Errorf("%s: picked %q, want %q", c.name, got, c.want)
		}
	}
}

// The picked hotkey entry keeps the delegation and wallet consent heads that
// GET /sn/wallet lists with it. The other scopes encode none of them, so their
// cached json is unchanged.
func TestSnPickWalletKeepsTheHotkeyEntryFields(t *testing.T) {
	const clientId = "00000000-0000-0000-0000-000000000001"
	consentHeadHash := "0x" + strings.Repeat("ab", 32)
	mappingHash := "0x" + strings.Repeat("cd", 32)
	body := `{"wallets":[{"coldkey_ss58":"synthetic-coldkey","set_at_millis":5,"consent_scope":"hotkey","hotkey_ss58":"synthetic-hotkey",` +
		`"from_epoch":7,"through_epoch":107,"consent_head_hash":"` + consentHeadHash + `","consent_generation":2,` +
		`"mapping_hash":"` + mappingHash + `","mapping_generation":3}]}`
	var result SnGetWalletResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	want := SnWallet{
		ColdkeySs58:       "synthetic-coldkey",
		SetAtMillis:       5,
		FromEpoch:         7,
		ConsentScope:      SnWalletConsentScopeHotkey,
		ThroughEpoch:      107,
		HotkeySs58:        "synthetic-hotkey",
		ConsentHeadHash:   consentHeadHash,
		ConsentGeneration: 2,
		MappingHash:       mappingHash,
		MappingGeneration: 3,
	}
	if wallet := snPickWallet(&result, clientId); wallet == nil || *wallet != want {
		t.Fatalf("picked %+v, want %+v", wallet, want)
	}

	encoded, err := json.Marshal(&SnWallet{ColdkeySs58: "synthetic-coldkey", SetAtMillis: 5, ConsentScope: SnWalletConsentScopeNetwork})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"coldkey_ss58":"synthetic-coldkey","set_at_millis":5,"consent_scope":"network"}` {
		t.Errorf("network consent encoded as %s", encoded)
	}
}
