//go:build !ios_extension

package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The profile's NetworkUser carries the account's email and phone password
// sign-ins, the server's `user_auths`, so an app can offer Change password at
// the address of the sign-in it changes.
//
// Root cause (inbox 7962, 7969): the sdk modeled only `user_auth`, the identity
// the account was created with. An account created with a wallet, a seed
// phrase or as a guest that later added an email and password has none, so no
// app offered it Change password, and a Google or Apple account that added one
// was offered the reset at the provider's address. The apps read this
// NetworkUser as json: the web through the wasm view controller, windows and
// linux through the c abi. So the json the sdk hands them must keep
// `user_auths`.

// GET /network/user for an account created with a wallet that added an email
// and password later (server/model/network_user_model.go GetNetworkUser).
func networkUserAddedEmailResponse(userId *Id) string {
	return fmt.Sprintf(`{
		"network_user": {
			"user_id": "%s",
			"verified": false,
			"auth_type": "solana",
			"network_name": "wallet-network",
			"wallet_address": "wallet-address-example",
			"user_auths": [{"user_auth": "added@example.com", "auth_type": "email"}],
			"wallet_auths": [{"wallet_address": "wallet-address-example", "blockchain": "SOL"}],
			"auth_types": ["email", "solana"]
		}
	}`, userId.String())
}

type networkUserTestListener func()

func (self networkUserTestListener) StateChanged() {
	self()
}

// The password sign-ins as the json of a NetworkUser lists them.
func jsonUserAuths(t *testing.T, value any) []map[string]string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		UserAuths []map[string]string `json:"user_auths"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.UserAuths
}

func TestNetworkUserViewControllerKeepsUserAuths(t *testing.T) {
	ctx, api := newTestApiForURL(t, "http://unused.invalid")
	// GET /network/user administers the account, so it carries the network
	// credential
	api.SetByJwt("synthetic-network-credential")
	api.setHttpGetRaw(func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		if !strings.HasSuffix(requestUrl, "/network/user") {
			return nil, fmt.Errorf("unexpected request %s", requestUrl)
		}
		return []byte(networkUserAddedEmailResponse(NewId())), nil
	})

	vc := NewNetworkUserViewControllerWithApi(ctx, api)
	t.Cleanup(vc.Close)
	changed := make(chan struct{}, 1)
	sub := vc.AddNetworkUserListener(networkUserTestListener(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(sub.Close)

	vc.FetchNetworkUser()
	select {
	case <-changed:
	case <-time.After(30 * time.Second):
		t.Fatal("the network user was never fetched")
	}

	userAuths := jsonUserAuths(t, vc.GetNetworkUser())
	if len(userAuths) != 1 || userAuths[0]["user_auth"] != "added@example.com" || userAuths[0]["auth_type"] != "email" {
		t.Fatalf("the network user lost its password sign-ins: user_auths = %v", userAuths)
	}
}

// The result the api hands the c abi keeps them too.
func TestGetNetworkUserResultKeepsUserAuths(t *testing.T) {
	result := &GetNetworkUserResult{}
	if err := json.Unmarshal([]byte(networkUserAddedEmailResponse(NewId())), result); err != nil {
		t.Fatal(err)
	}
	if result.NetworkUser == nil {
		t.Fatal("no network user")
	}
	userAuths := jsonUserAuths(t, result.NetworkUser)
	if len(userAuths) != 1 || userAuths[0]["user_auth"] != "added@example.com" || userAuths[0]["auth_type"] != "email" {
		t.Fatalf("the network user lost its password sign-ins: user_auths = %v", userAuths)
	}
}
