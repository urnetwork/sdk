//go:build !ios_extension

package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testing_walletDevice is a minimal Device for the wallet view controller: it
// embeds the Device interface and answers only GetApi, the one method the
// removal path reaches. Any other method would panic if called.
type testing_walletDevice struct {
	Device
	api *Api
}

func (self *testing_walletDevice) GetApi() *Api {
	return self.api
}

type testing_payoutWalletListener struct {
	changed chan *Id
}

func (self *testing_payoutWalletListener) PayoutWalletChanged(id *Id) {
	self.changed <- id
}

type testing_isRemovingWalletListener struct {
	changed chan bool
}

func (self *testing_isRemovingWalletListener) StateChanged(isRemoving bool) {
	self.changed <- isRemoving
}

// newTestWalletViewController answers the wallet api from the raw post and
// get functions, keyed by path.
func newTestWalletViewController(
	t *testing.T,
	post func(path string) ([]byte, error),
	get func(path string) ([]byte, error),
) *WalletViewController {
	t.Helper()
	ctx, api := newTestApi(t, http.NotFoundHandler())
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, _ []byte, _ string) ([]byte, error) {
		return post(requestUrl[strings.Index(requestUrl, "/account/"):])
	})
	api.setHttpGetRaw(func(_ context.Context, requestUrl string, _ string) ([]byte, error) {
		return get(requestUrl[strings.Index(requestUrl, "/account/"):])
	})
	vc := newWalletViewController(ctx, &testing_walletDevice{api: api})
	t.Cleanup(vc.Close)
	return vc
}

// Removing the payout wallet can make another wallet the payout wallet on
// the server, so a successful removal refetches the payout wallet along with
// the wallets. Before, only the wallets were refetched and the removed wallet
// stayed the payout wallet in the app.
func TestWalletViewControllerRemoveWalletRefetchesPayoutWallet(t *testing.T) {
	removed := NewId()
	promoted := NewId()
	vc := newTestWalletViewController(
		t,
		func(path string) ([]byte, error) {
			if path != "/account/wallets/remove" {
				return nil, fmt.Errorf("unexpected post %s", path)
			}
			return []byte(fmt.Sprintf(`{"success":true,"payout_wallet_id":%q}`, promoted.IdStr)), nil
		},
		func(path string) ([]byte, error) {
			switch path {
			case "/account/wallets":
				return []byte(fmt.Sprintf(`{"wallets":[{"wallet_id":%q,"blockchain":"SOL","wallet_address":"synthetic-promoted","active":true}]}`, promoted.IdStr)), nil
			case "/account/payout-wallet":
				return []byte(fmt.Sprintf(`{"wallet_id":%q}`, promoted.IdStr)), nil
			default:
				return nil, fmt.Errorf("unexpected get %s", path)
			}
		},
	)
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.payoutWalletId = removed
	}()
	listener := &testing_payoutWalletListener{changed: make(chan *Id, 4)}
	vc.AddPayoutWalletListener(listener)

	vc.RemoveWallet(removed)

	select {
	case id := <-listener.changed:
		if id == nil || id.Cmp(promoted) != 0 {
			t.Fatalf("payout wallet changed to %v, want %s", id, promoted.IdStr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("payout wallet not refetched after the removal; still %v", vc.GetPayoutWalletId())
	}
	if id := vc.GetPayoutWalletId(); id == nil || id.Cmp(promoted) != 0 {
		t.Fatalf("payout wallet %v, want %s", id, promoted.IdStr)
	}
}

// A removal that fails or answers no result ends the removing state. Before,
// a null result was dereferenced in the callback, so the removing state never
// cleared and every later removal was ignored.
func TestWalletViewControllerRemoveWalletWithoutResult(t *testing.T) {
	for name, response := range map[string]func() ([]byte, error){
		"null result": func() ([]byte, error) {
			return []byte("null"), nil
		},
		"api error": func() ([]byte, error) {
			return nil, errors.New("synthetic failure")
		},
		"refused": func() ([]byte, error) {
			return []byte(`{"success":false,"error":{"message":"synthetic refusal"}}`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			vc := newTestWalletViewController(
				t,
				func(path string) ([]byte, error) {
					return response()
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
						t.Fatalf("removing state %v, want %v", isRemoving, want)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("removing state did not change to %v", want)
				}
			}
		})
	}
}
