//go:build !ios_extension

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect"
)

type locationsRequestListener func(*FilteredLocations, FilterLocationsState)

func (f locationsRequestListener) FilteredLocationsChanged(v *FilteredLocations, s FilterLocationsState) {
	f(v, s)
}

func locationsRequestApi(t *testing.T, read func(context.Context, string) ([]byte, error)) *Api {
	t.Helper()
	api := newApi(t.Context(), nil, "https://picker.invalid")
	api.setHttpGetRaw(func(ctx context.Context, _ string, _ string) ([]byte, error) { return read(ctx, "") })
	api.setHttpPostRaw(func(ctx context.Context, _ string, body []byte, _ string) ([]byte, error) {
		var args FindLocationsArgs
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		return read(ctx, args.Query)
	})
	t.Cleanup(api.Close)
	return api
}
func locationsRequestBody(name string) []byte {
	return []byte(`{"groups":[],"devices":[],"locations":[{"location_id":"00000000-0000-0000-0000-000000000101","location_type":"country","name":"` + name + `","provider_count":1,"match_distance":1}]}`)
}
func TestLocationsApiPanicAlwaysCompletes(t *testing.T) {
	for _, search := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "search"}[search], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := locationsRequestApi(t, func(context.Context, string) ([]byte, error) { panic("synthetic private transport payload") })
				done := make(chan error, 2)
				callback := connect.NewApiCallback(func(_ *FindLocationsResult, err error) { done <- err })
				if search {
					api.FindProviderLocations(&FindLocationsArgs{Query: "query"}, callback)
				} else {
					api.GetProviderLocations(callback)
				}
				synctest.Wait()
				select {
				case err := <-done:
					if err == nil || strings.Contains(err.Error(), "private") {
						t.Fatalf("unsafe terminal result: %v", err)
					}
				default:
					t.Fatal("request panic left picker callback unresolved")
				}
				if len(done) != 0 {
					t.Fatal("callback delivered twice")
				}
			})
		})
	}
}
func TestLocationsNullResponseTerminatesLoading(t *testing.T) {
	for _, body := range []string{"null", `{`, `{"groups":[null]}`, `{"locations":[null]}`, `{"devices":[null]}`} {
		t.Run(body, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := locationsRequestApi(t, func(context.Context, string) ([]byte, error) { return []byte(body), nil })
				vc := NewLocationsViewControllerWithApi(t.Context(), api)
				defer vc.Close()
				vc.FilterLocations("")
				synctest.Wait()
				if got := vc.GetFilteredLocationState(); got != LocationsError {
					t.Fatalf("invalid HTTP200 left state=%s, want terminal error", got)
				}
			})
		})
	}
}

func TestLocationsSupersededRequestCancelsTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan context.Context, 1)
		api := locationsRequestApi(t, func(ctx context.Context, q string) ([]byte, error) {
			if q == "" {
				entered <- ctx
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return locationsRequestBody("New"), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		vc.FilterLocations("")
		oldCtx := <-entered
		vc.FilterLocations("New")
		synctest.Wait()
		if oldCtx.Err() != context.Canceled || vc.GetFilteredLocationState() != LocationsLoaded {
			t.Fatalf("superseded request cancellation=%v state=%s", oldCtx.Err(), vc.GetFilteredLocationState())
		}
	})
}

func TestLocationsEmptySectionsRemainSuccessful(t *testing.T) {
	for _, body := range []string{`{}`, `{"groups":null,"locations":null,"devices":null}`} {
		t.Run(body, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := locationsRequestApi(t, func(context.Context, string) ([]byte, error) { return []byte(body), nil })
				vc := NewLocationsViewControllerWithApi(t.Context(), api)
				defer vc.Close()
				vc.FilterLocations("")
				synctest.Wait()
				got := vc.GetFilteredLocations()
				if vc.GetFilteredLocationState() != LocationsLoaded || got == nil || got.Countries.Len() != 0 || got.Devices.Len() != 0 {
					t.Fatal("empty sections did not produce a loaded empty result")
				}
			})
		})
	}
}

func TestLocationsApiCloseTerminatesCurrentRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		api := locationsRequestApi(t, func(ctx context.Context, _ string) ([]byte, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		vc.FilterLocations("")
		<-entered
		api.Close()
		synctest.Wait()
		if vc.GetFilteredLocationState() != LocationsError {
			t.Fatal("API shutdown left live controller loading")
		}
	})
}

func TestLocationsForeignCallbackPanicIsDeliveredOnlyOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := locationsRequestApi(t, func(context.Context, string) ([]byte, error) { return locationsRequestBody("Valid"), nil })
		calls := 0
		api.GetProviderLocations(connect.NewApiCallback(func(*FindLocationsResult, error) {
			calls++
			panic("synthetic foreign callback")
		}))
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("callback invoked %d times", calls)
		}
	})
}
func TestLocationsSupersededRequestCannotReplacePendingQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldRelease, newRelease := make(chan struct{}), make(chan struct{})
		entered := make(chan string, 2)
		api := locationsRequestApi(t, func(ctx context.Context, q string) ([]byte, error) {
			entered <- q
			if q == "" {
				<-oldRelease
				return locationsRequestBody("Old"), nil
			}
			<-newRelease
			return locationsRequestBody("New"), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		vc.FilterLocations("")
		<-entered
		vc.FilterLocations("New")
		<-entered
		close(oldRelease)
		synctest.Wait()
		if got := vc.GetFilteredLocationState(); got != LocationsLoading || vc.GetFilteredLocations() != nil {
			t.Errorf("superseded result published while current query pending: %s", got)
		}
		close(newRelease)
		synctest.Wait()
		if got := vc.GetFilteredLocations(); vc.GetFilteredLocationState() != LocationsLoaded || got == nil || got.Countries.Get(0).Name != "New" {
			t.Fatal("latest query did not win")
		}
	})
}
func TestLocationsCloseCancelsRequestAndDiscardsLateResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requestCtx := make(chan context.Context, 1)
		release := make(chan struct{})
		api := locationsRequestApi(t, func(ctx context.Context, _ string) ([]byte, error) {
			requestCtx <- ctx
			<-release
			return locationsRequestBody("Late"), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		var terminal int
		vc.AddFilteredLocationsListener(locationsRequestListener(func(_ *FilteredLocations, s FilterLocationsState) {
			if s != LocationsLoading {
				terminal++
			}
		}))
		vc.FilterLocations("")
		ctx := <-requestCtx
		vc.Close()
		select {
		case <-ctx.Done():
		default:
			close(release)
			synctest.Wait()
			t.Fatal("controller close did not cancel picker transport")
		}
		close(release)
		synctest.Wait()
		if terminal != 0 || vc.GetFilteredLocations() != nil {
			t.Fatal("closed controller published late result")
		}
	})
}
func TestLocationsFailedRefreshPreservesRows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fail := false
		api := locationsRequestApi(t, func(context.Context, string) ([]byte, error) {
			if fail {
				return nil, errors.New("synthetic offline")
			}
			return locationsRequestBody("Saved"), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		vc.FilterLocations("")
		synctest.Wait()
		before := vc.GetFilteredLocations()
		fail = true
		vc.FilterLocations("unavailable")
		synctest.Wait()
		if vc.GetFilteredLocationState() != LocationsError || vc.GetFilteredLocations() != before || before == nil {
			t.Fatal("failed refresh erased prior successful rows")
		}
	})
}

func TestLocationsReentrantQueryDiscardsRemainingOldNotifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		api := locationsRequestApi(t, func(ctx context.Context, query string) ([]byte, error) {
			if query != "" {
				<-hold
			}
			return locationsRequestBody(query), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		started, stale := false, false
		vc.AddFilteredLocationsListener(locationsRequestListener(func(_ *FilteredLocations, state FilterLocationsState) {
			if state == LocationsLoaded && !started {
				started = true
				vc.FilterLocations("replacement")
			}
		}))
		vc.AddFilteredLocationsListener(locationsRequestListener(func(_ *FilteredLocations, state FilterLocationsState) {
			if state == LocationsLoaded && vc.GetFilteredLocationState() == LocationsLoading {
				stale = true
			}
		}))
		vc.FilterLocations("")
		synctest.Wait()
		close(hold)
		synctest.Wait()
		if stale {
			t.Fatal("old loaded notification followed replacement loading notification")
		}
	})
}

func TestLocationsConcurrentNotificationsFinishInRequestOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oldCallbackEntered, releaseOldCallback := make(chan struct{}), make(chan struct{})
		newRequestEntered, releaseNewRequest := make(chan struct{}), make(chan struct{})
		api := locationsRequestApi(t, func(ctx context.Context, query string) ([]byte, error) {
			if query != "" {
				close(newRequestEntered)
				<-releaseNewRequest
				return locationsRequestBody("New"), nil
			}
			return locationsRequestBody("Old"), nil
		})
		vc := NewLocationsViewControllerWithApi(t.Context(), api)
		defer vc.Close()
		applied := make(chan FilterLocationsState, 8)
		vc.AddFilteredLocationsListener(locationsRequestListener(func(rows *FilteredLocations, state FilterLocationsState) {
			if state == LocationsLoaded && rows.Countries.Get(0).Name == "Old" {
				close(oldCallbackEntered)
				<-releaseOldCallback
			}
			applied <- state
		}))
		vc.FilterLocations("")
		<-oldCallbackEntered
		vc.FilterLocations("New")
		<-newRequestEntered
		synctest.Wait()
		close(releaseOldCallback)
		synctest.Wait()
		var last FilterLocationsState
		for len(applied) != 0 {
			last = <-applied
		}
		if last != LocationsLoading {
			t.Errorf("old callback completed after the replacement loading notification: %s", last)
		}
		close(releaseNewRequest)
		synctest.Wait()
		if vc.GetFilteredLocationState() != LocationsLoaded || <-applied != LocationsLoaded {
			t.Fatal("replacement completion was not delivered")
		}
	})
}
