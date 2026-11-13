package sdk

// api_mail_locale_test.go -- the device's zone and languages ride on sign-up,
// sign-in and auth-client (api_mail_locale.go). A sign-up carried neither
// until 2026-11-13, so the server mailed a new reader at 10:00 UTC in
// English until the app's next auth-client.

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// An Api whose requests are answered at once with "{}" and whose bodies are
// kept by route.
func newMailLocaleApi(t *testing.T) (*Api, func(route string) map[string]any) {
	t.Helper()
	api := newApi(t.Context(), nil, "https://mail-locale.example")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.CloseAndWait(ctx); err != nil {
			t.Errorf("close mail locale API: %v", err)
		}
	})
	api.authNetworkClientLaunch = func(request func()) {
		request()
	}
	var stateLock sync.Mutex
	routeBodies := map[string]map[string]any{}
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, body []byte, _ string) ([]byte, error) {
		decoded := map[string]any{}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("%s body: %v", requestUrl, err)
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			routeBodies[requestUrl[len("https://mail-locale.example"):]] = decoded
		}()
		return []byte("{}"), nil
	})
	return api, func(route string) map[string]any {
		stateLock.Lock()
		defer stateLock.Unlock()
		return routeBodies[route]
	}
}

// awaitResult waits for one async api callback.
func awaitResult[R any](t *testing.T, call func(callback connect.ApiCallback[R])) {
	t.Helper()
	done := make(chan struct{})
	call(connect.NewApiCallback(func(_ R, err error) {
		if err != nil {
			t.Errorf("request: %v", err)
		}
		close(done)
	}))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("no callback")
	}
}

func stringList(values ...string) *StringList {
	list := NewStringList()
	for _, value := range values {
		list.Add(value)
	}
	return list
}

func bodyLocales(body map[string]any) []string {
	values, _ := body["locales"].([]any)
	locales := []string{}
	for _, value := range values {
		if locale, ok := value.(string); ok {
			locales = append(locales, locale)
		}
	}
	return locales
}

// Every request that signs up or signs in, and a device's auth-client, sends
// the zone and the languages the app set; blank, repeated and "_" forms are
// cleaned.
func TestApiSendsTheMailLocaleWithSignUpAndSignIn(t *testing.T) {
	api, body := newMailLocaleApi(t)
	api.SetMailLocale(" Asia/Tokyo ", stringList("ja-JP", "en_US", " ", "ja-JP"))

	awaitResult(t, func(callback connect.ApiCallback[*NetworkCreateResult]) {
		api.NetworkCreate(&NetworkCreateArgs{UserAuth: "reader@mail-locale.example"}, callback)
	})
	awaitResult(t, func(callback connect.ApiCallback[*AuthLoginResult]) {
		api.AuthLogin(&AuthLoginArgs{AuthJwt: "synthetic", AuthJwtType: "apple"}, callback)
	})
	awaitResult(t, func(callback connect.ApiCallback[*AuthLoginWithPasswordResult]) {
		api.AuthLoginWithPassword(&AuthLoginWithPasswordArgs{UserAuth: "reader@mail-locale.example"}, callback)
	})
	awaitResult(t, func(callback connect.ApiCallback[*AuthVerifyResult]) {
		api.AuthVerify(&AuthVerifyArgs{UserAuth: "reader@mail-locale.example", VerifyCode: "000000"}, callback)
	})
	awaitResult(t, func(callback connect.ApiCallback[*AuthNetworkClientResult]) {
		api.AuthNetworkClient(&AuthNetworkClientArgs{DeviceSpec: "synthetic device"}, callback)
	})
	for _, route := range []string{"/auth/network-create", "/auth/login", "/auth/login-with-password", "/auth/verify", "/network/auth-client"} {
		sent := body(route)
		if sent["time_zone"] != "Asia/Tokyo" || !slices.Equal(bodyLocales(sent), []string{"ja-JP", "en-US"}) {
			t.Errorf("%s sent %v", route, sent)
		}
	}
	if locale := body("/network/auth-client")["locale"]; locale != "ja-JP" {
		t.Errorf("auth-client locale %v", locale)
	}
	if _, err := api.AuthLoginWithPasswordSyncWithContext(t.Context(), &AuthLoginWithPasswordArgs{UserAuth: "sync@mail-locale.example"}); err != nil {
		t.Fatal(err)
	}
	if sent := body("/auth/login-with-password"); sent["user_auth"] != "sync@mail-locale.example" || sent["time_zone"] != "Asia/Tokyo" {
		t.Errorf("sync sign-in sent %v", sent)
	}
	if _, err := api.AuthNetworkClientSyncWithContext(t.Context(), &AuthNetworkClientArgs{DeviceSpec: "sync device"}); err != nil {
		t.Fatal(err)
	}
	if sent := body("/network/auth-client"); sent["device_spec"] != "sync device" || sent["time_zone"] != "Asia/Tokyo" {
		t.Errorf("sync auth-client sent %v", sent)
	}
}

// A value the caller set wins, the caller's args are not changed, a derived
// client's mint carries nothing, and an api without a mail locale sends
// none.
func TestApiKeepsTheCallersMailLocale(t *testing.T) {
	api, body := newMailLocaleApi(t)
	awaitResult(t, func(callback connect.ApiCallback[*NetworkCreateResult]) {
		api.NetworkCreate(&NetworkCreateArgs{UserAuth: "none@mail-locale.example"}, callback)
	})
	if sent := body("/auth/network-create"); sent["time_zone"] != nil || sent["locales"] != nil {
		t.Errorf("an unset mail locale sent %v", sent)
	}

	api.SetMailLocale("Asia/Tokyo", stringList("ja-JP"))
	args := &NetworkCreateArgs{UserAuth: "own@mail-locale.example", TimeZone: "Europe/Paris"}
	awaitResult(t, func(callback connect.ApiCallback[*NetworkCreateResult]) {
		api.NetworkCreate(args, callback)
	})
	if sent := body("/auth/network-create"); sent["time_zone"] != "Europe/Paris" || !slices.Equal(bodyLocales(sent), []string{"ja-JP"}) {
		t.Errorf("the caller's zone: sent %v", sent)
	}
	if args.TimeZone != "Europe/Paris" || args.Locales != nil {
		t.Errorf("the caller's args changed: %+v", args)
	}

	sourceClientId := NewId()
	awaitResult(t, func(callback connect.ApiCallback[*AuthNetworkClientResult]) {
		api.AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: sourceClientId}, callback)
	})
	if sent := body("/network/auth-client"); sent["time_zone"] != nil || sent["locales"] != nil || sent["locale"] != nil {
		t.Errorf("a derived client's mint sent %v", sent)
	}

	// a session of the api keeps the device's
	session := api.newSession(t.Context())
	defer session.Close()
	if session.GetMailLocaleTimeZone() != "Asia/Tokyo" || session.GetMailLocales().Len() != 1 {
		t.Errorf("session mail locale %q %d", session.GetMailLocaleTimeZone(), session.GetMailLocales().Len())
	}
}

// At most MaxLocales languages are kept, and a tag past MaxLocaleBytes is
// none.
func TestApiMailLocaleBounds(t *testing.T) {
	api, _ := newMailLocaleApi(t)
	settings := defaultApiMailLocaleSettings()
	locales := NewStringList()
	locales.Add("x-" + string(make([]byte, settings.MaxLocaleBytes)))
	for i := 0; i < settings.MaxLocales+3; i += 1 {
		locales.Add("l" + string(rune('a'+i)))
	}
	api.SetMailLocale("", locales)
	got := api.GetMailLocales()
	if got.Len() != settings.MaxLocales || got.Get(0) != "la" {
		t.Errorf("kept %d locales, first %q", got.Len(), got.Get(0))
	}
	api.SetMailLocale("UTC", nil)
	if api.GetMailLocales().Len() != 0 || api.GetMailLocaleTimeZone() != "UTC" {
		t.Error("clearing the languages kept them")
	}
}
