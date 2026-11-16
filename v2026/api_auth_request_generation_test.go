// Real HTTP refresh responses carry the request's auth generation, so a new
// explicit login with identical bytes cannot inherit the old request's result.
package sdk

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// A held server response is released only after the explicit equality login.
func testingApiRefreshReplyAfterEqualLogin(t *testing.T, reject bool, replace bool) {
	t.Helper()
	initialJwt := testingRefreshableJwtWithMarker(t, "request-owner")
	refreshedJwt := testingRefreshableJwtWithMarker(t, "request-result")
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(resume)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		if reject {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("{}"))
		} else {
			_, _ = fmt.Fprintf(w, "{\"by_jwt\":%q}", refreshedJwt)
		}
	})
	_, api := newTestApi(t, handler)
	api.SetByJwt(initialJwt)
	type result struct {
		loggedOut bool
		stale     bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		loggedOut, stale, err := api.tokenManager.refreshToken(initialJwt)
		done <- result{loggedOut: loggedOut, stale: stale, err: err}
	}()
	testingAwaitAuthBoundary(t, entered)
	if replace {
		api.SetByJwt(initialJwt)
	}
	resume()
	got := <-done
	if got.err != nil {
		t.Fatal("refresh response failed for an unrelated reason")
	}
	if replace {
		if !got.stale || got.loggedOut || api.GetByJwt() != initialJwt {
			t.Error("old response crossed an explicit equality login boundary")
		}
	} else if reject {
		if got.stale || !got.loggedOut || api.GetByJwt() != "" {
			t.Error("current-owner rejection was not applied")
		}
	} else if got.stale || got.loggedOut || api.GetByJwt() != refreshedJwt {
		t.Error("current-owner refresh was not applied")
	}
}

// An old successful response cannot rotate the newly installed same-byte JWT.
func TestApiRefreshReplyCannotCrossEqualLogin(t *testing.T) {
	testingApiRefreshReplyAfterEqualLogin(t, false, true)
}

// An old unauthorized response must not clear the new same-byte login.
func TestApiRejectReplyCannotCrossEqualLogin(t *testing.T) {
	testingApiRefreshReplyAfterEqualLogin(t, true, true)
}

// The generation guard must not suppress an unchanged owner's valid refresh.
func TestApiRefreshReplyKeepsCurrentOwner(t *testing.T) {
	testingApiRefreshReplyAfterEqualLogin(t, false, false)
}

// Healthy current rejection retains existing logout behavior.
func TestApiRejectReplyKeepsCurrentOwner(t *testing.T) {
	testingApiRefreshReplyAfterEqualLogin(t, true, false)
}
