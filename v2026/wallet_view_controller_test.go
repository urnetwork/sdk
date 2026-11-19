//go:build !ios_extension

// The wallet view controller against a fake wallet api. USDC provider payouts
// ended on 2026-11-12, so the controller has no USDC payout wallet surface, a
// removal refetches only the wallets, and a failed removal ends the removing
// state.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A minimal Device for the wallet view controller: it embeds the Device
// interface and answers only GetApi, the one method the removal path reaches.
// Any other method would panic if called.
type testing_walletDevice struct {
	Device
	api *Api
}

// The fake wallet api.
func (self *testing_walletDevice) GetApi() *Api {
	return self.api
}

// Hands each removing state change to the test.
type testing_isRemovingWalletListener struct {
	changed chan bool
}

// Sends the new removing state to the test.
func (self *testing_isRemovingWalletListener) StateChanged(isRemoving bool) {
	self.changed <- isRemoving
}

// A wallet view controller whose wallet api answers from the raw post and get
// functions, keyed by path.
func newTestWalletViewController(
	t *testing.T,
	post func(path string) ([]byte, error),
	get func(path string) ([]byte, error),
) *WalletViewController {
	t.Helper()
	ctx, api := newTestApi(t, http.NotFoundHandler())
	// the wallet routes administer the account: they need the network credential
	api.SetByJwt("test-network-jwt")
	pathOf := func(requestUrl string) string {
		u, err := url.Parse(requestUrl)
		if err != nil {
			return requestUrl
		}
		return u.Path
	}
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, _ []byte, _ string) ([]byte, error) {
		return post(pathOf(requestUrl))
	})
	api.setHttpGetRaw(func(_ context.Context, requestUrl string, _ string) ([]byte, error) {
		return get(pathOf(requestUrl))
	})
	vc := newWalletViewController(ctx, &testing_walletDevice{api: api})
	t.Cleanup(vc.Close)
	return vc
}

// USDC payouts ended on 2026-11-12, so no app can read, set, poll or schedule
// a USDC payout wallet through the sdk, or add one: the gomobile and c abi
// surfaces are the exported methods, and none of these remain. The pending
// USDC payout is not on the balance result either (TestSubscriptionBalanceDecode
// checks the wire side).
func TestWalletViewControllerHasNoUsdcPayoutWallet(t *testing.T) {
	retired := func(name string) bool {
		return strings.Contains(name, "PayoutWallet") ||
			strings.Contains(name, "NextPayoutDate") ||
			strings.Contains(name, "ExternalWallet")
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(&WalletViewController{}),
		reflect.TypeOf(&Api{}),
	} {
		for i := 0; i < typ.NumMethod(); i += 1 {
			if name := typ.Method(i).Name; retired(name) {
				t.Errorf("%s.%s is a USDC payout wallet surface", typ.Elem().Name(), name)
			}
		}
	}
	balance := reflect.TypeOf(SubscriptionBalanceResult{})
	for i := 0; i < balance.NumField(); i += 1 {
		field := balance.Field(i)
		if strings.Contains(field.Name, "PendingPayout") || strings.Contains(field.Tag.Get("json"), "pending_payout") {
			t.Errorf("SubscriptionBalanceResult.%s carries the pending USDC payout", field.Name)
		}
	}
}

// A successful removal refetches the wallets, which ends the removing state.
// It reads nothing else: there is no payout wallet to refetch since USDC
// payouts ended (before, the payout wallet was read again here).
func TestWalletViewControllerRemoveWalletRefetchesTheWallets(t *testing.T) {
	removed := NewId()
	remaining := NewId()
	var stateLock sync.Mutex
	var gets []string
	vc := newTestWalletViewController(
		t,
		func(path string) ([]byte, error) {
			if path != "/account/wallets/remove" {
				return nil, fmt.Errorf("unexpected post %s", path)
			}
			return []byte(`{"success":true}`), nil
		},
		func(path string) ([]byte, error) {
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				gets = append(gets, path)
			}()
			if path != "/account/wallets" {
				return nil, fmt.Errorf("unexpected get %s", path)
			}
			return []byte(fmt.Sprintf(`{"wallets":[{"wallet_id":%q,"blockchain":"SOL","wallet_address":"synthetic-remaining","active":true}]}`, remaining.IdStr)), nil
		},
	)
	listener := &testing_isRemovingWalletListener{changed: make(chan bool, 4)}
	vc.AddIsRemovingWalletListener(listener)

	vc.RemoveWallet(removed)

	for _, want := range []bool{true, false} {
		select {
		case isRemoving := <-listener.changed:
			if isRemoving != want {
				t.Fatalf("removing state %v, want %v", isRemoving, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("removing state did not change to %v", want)
		}
	}
	wallets := vc.GetWallets()
	if wallets.Len() != 1 || wallets.Get(0).WalletId.Cmp(remaining) != 0 {
		t.Fatalf("wallets after the removal: %d, want only %s", wallets.Len(), remaining.IdStr)
	}
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		for _, path := range gets {
			if path != "/account/wallets" {
				t.Errorf("the removal read %s", path)
			}
		}
	}()
}

// A removal that fails or answers no result ends the removing state. Before,
// a null result was dereferenced in the callback, so the removing state never
// cleared and every later removal was ignored.
func TestWalletViewControllerRemoveWalletWithoutResult(t *testing.T) {
	for _, c := range []struct {
		name     string
		response func() ([]byte, error)
	}{
		{
			name: "null result",
			response: func() ([]byte, error) {
				return []byte("null"), nil
			},
		},
		{
			name: "api error",
			response: func() ([]byte, error) {
				return nil, errors.New("synthetic failure")
			},
		},
		{
			name: "refused",
			response: func() ([]byte, error) {
				return []byte(`{"success":false,"error":{"message":"synthetic refusal"}}`), nil
			},
		},
	} {
		vc := newTestWalletViewController(
			t,
			func(path string) ([]byte, error) {
				return c.response()
			},
			func(path string) ([]byte, error) {
				return nil, fmt.Errorf("unexpected get %s after a failed removal", path)
			},
		)
		listener := &testing_isRemovingWalletListener{changed: make(chan bool, 4)}
		vc.AddIsRemovingWalletListener(listener)

		vc.RemoveWallet(NewId())

		for _, want := range []bool{true, false} {
			select {
			case isRemoving := <-listener.changed:
				if isRemoving != want {
					t.Fatalf("%s: removing state %v, want %v", c.name, isRemoving, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: removing state did not change to %v", c.name, want)
			}
		}
	}
}
