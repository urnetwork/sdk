package sdk

import (
	"context"
	"sync"
	"testing"
)

type testingAuthLogoutEvent struct {
	generation int64
	cause      string
}

type testingAuthLogoutEventListener func(int64, string)

func (self testingAuthLogoutEventListener) AuthLogoutEvent(generation int64, cause string) {
	self(generation, cause)
}

// Exercise both production rejection commits without a transport or clock.
// The first legacy callback parks dispatch before the new listener list is
// read, so a listener added by the replacement login also sees the old event.
func TestAuthLogoutEventRetainsOriginBeforeListenerSnapshot(t *testing.T) {
	for _, network := range []bool{false, true} {
		for _, equalBytes := range []bool{false, true} {
			func() {
				api := newApi(t.Context(), nil, "https://logout-event.example")
				defer func() { _ = api.CloseAndWait(context.Background()) }()
				oldJwt := taggedSessionTestJwt(t, NewId().String(), "")
				newJwt := oldJwt
				if !equalBytes {
					newJwt = taggedSessionTestJwt(t, NewId().String(), "")
				}
				api.SetByJwt(oldJwt)
				target, ok := api.networkRequestTarget(oldJwt)
				if !ok {
					t.Fatal("synthetic login did not install its network credential")
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				legacy := api.AddAuthLogoutListener(authLogoutListenerFunc(func() {
					close(entered)
					<-release
				}))
				defer legacy.Close()
				done := make(chan bool, 1)
				go func() {
					if network {
						done <- api.rejectNetworkCredential(target, AuthLogoutCauseSessionRevoked)
					} else {
						done <- api.rejectByJwt(oldJwt, AuthLogoutCauseSessionRevoked)
					}
				}()
				<-entered
				api.SetByJwt(newJwt)
				observed := make(chan testingAuthLogoutEvent, 1)
				sub := api.AddAuthLogoutEventListener(testingAuthLogoutEventListener(func(generation int64, cause string) {
					observed <- testingAuthLogoutEvent{generation: generation, cause: cause}
				}))
				defer sub.Close()
				unblock()
				if !<-done {
					t.Fatal("the original current rejection did not commit")
				}
				event := <-observed
				if event.generation == 0 || event.cause != AuthLogoutCauseSessionRevoked {
					t.Fatalf("network %t equal bytes %t: event lost its committed origin/cause", network, equalBytes)
				}
				if api.IsAuthLogoutCurrent(event.generation) || api.GetByJwt() != newJwt {
					t.Fatalf("network %t equal bytes %t: delayed event owns the replacement login", network, equalBytes)
				}
				if api.GetAuthLogoutCause() != "" {
					t.Fatal("the event cause was incorrectly read from the replacement API state")
				}
			}()
		}
	}
}

// Disconnecting a rejected device before UI delivery is not a new sign-in.
// The event remains actionable even though device retirement advances the
// request generation used to reject stale refresh completions.
func TestAuthLogoutEventSurvivesDeviceRetirement(t *testing.T) {
	api := newApi(t.Context(), nil, "https://logout-event.example")
	defer func() { _ = api.CloseAndWait(context.Background()) }()
	api.SetByJwt("synthetic-device-credential")
	owner := newDeviceAuthPublicationGate()
	api.mutex.Lock()
	api.deviceAuthOwner = owner
	api.mutex.Unlock()
	var event testingAuthLogoutEvent
	sub := api.AddAuthLogoutEventListener(testingAuthLogoutEventListener(func(generation int64, cause string) {
		event = testingAuthLogoutEvent{generation: generation, cause: cause}
	}))
	defer sub.Close()
	if !api.rejectByJwt("synthetic-device-credential", AuthLogoutCauseSessionRevoked) {
		t.Fatal("current device rejection did not commit")
	}
	api.closeDeviceOwner(owner)
	if !api.IsAuthLogoutCurrent(event.generation) || event.cause != AuthLogoutCauseSessionRevoked {
		t.Fatal("ordinary device retirement suppressed the current rejection")
	}
	api.SetByJwt("")
	if api.IsAuthLogoutCurrent(event.generation) {
		t.Fatal("explicit logout did not retire the already handled rejection")
	}
}

// The device's explicit setter uses replaceDeviceByJwt, not Api.SetByJwt.
// Clearing through either public surface retires the old rejection.
func TestAuthLogoutEventRetiresOnExplicitDeviceClear(t *testing.T) {
	api := newApi(t.Context(), nil, "https://logout-event.example")
	defer func() { _ = api.CloseAndWait(context.Background()) }()
	api.SetByJwt("synthetic-device-credential")
	owner := newDeviceAuthPublicationGate()
	api.mutex.Lock()
	api.deviceAuthOwner = owner
	api.mutex.Unlock()
	var generation int64
	sub := api.AddAuthLogoutEventListener(testingAuthLogoutEventListener(func(value int64, _ string) {
		generation = value
	}))
	defer sub.Close()
	if !api.rejectByJwt("synthetic-device-credential", "") || !api.IsAuthLogoutCurrent(generation) {
		t.Fatal("current device rejection did not commit")
	}
	accepted, err := api.replaceDeviceByJwt(owner, "", nil, nil)
	if err != nil || !accepted || api.IsAuthLogoutCurrent(generation) {
		t.Fatal("the explicit device clear left a rejection event actionable")
	}
}

// A real paired reset may follow ordinary retirement, which cleared the
// rejected token but deliberately retained the event. Reset is an explicit
// auth cleanup and retires the event along with the observed pair.
func TestAuthLogoutEventRetiresOnPairedReset(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	api := fixture.api
	api.SetByJwt("synthetic-reset-credential")
	owner := newDeviceAuthPublicationGate()
	api.mutex.Lock()
	api.deviceAuthOwner = owner
	api.mutex.Unlock()
	var generation int64
	sub := api.AddAuthLogoutEventListener(testingAuthLogoutEventListener(func(value int64, _ string) {
		generation = value
	}))
	defer sub.Close()
	if !api.rejectByJwt("synthetic-reset-credential", "") {
		t.Fatal("current rejection did not commit")
	}
	api.closeDeviceOwner(owner)
	if !api.IsAuthLogoutCurrent(generation) {
		t.Fatal("ordinary retirement suppressed the rejection before reset")
	}
	snapshot := testingPairedAuthSnapshot(t, fixture)
	result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || result == nil || !result.GetReset() || api.IsAuthLogoutCurrent(generation) {
		t.Fatal("the paired reset did not retire the original rejection")
	}
}

// The current rejection still reaches the application with its original
// trusted or generic cause. Explicit logout emits neither listener kind.
func TestAuthLogoutEventPreservesCurrentAndExplicitLogout(t *testing.T) {
	for _, network := range []bool{false, true} {
		for _, c := range []struct {
			cause       string
			selfRevoked bool
			want        string
		}{
			{cause: AuthLogoutCauseSessionRevoked, want: AuthLogoutCauseSessionRevoked},
			{cause: "", want: ""},
			{cause: AuthLogoutCauseSessionRevoked, selfRevoked: true, want: ""},
		} {
			func() {
				api := newApi(t.Context(), nil, "https://logout-event.example")
				defer func() { _ = api.CloseAndWait(context.Background()) }()
				var event testingAuthLogoutEvent
				events, legacyEvents := 0, 0
				sub := api.AddAuthLogoutEventListener(testingAuthLogoutEventListener(func(generation int64, cause string) {
					events++
					event = testingAuthLogoutEvent{generation: generation, cause: cause}
				}))
				defer sub.Close()
				legacy := api.AddAuthLogoutListener(authLogoutListenerFunc(func() { legacyEvents++ }))
				defer legacy.Close()
				sessionId := NewId()
				jwt := taggedSessionTestJwt(t, sessionId.String(), "")
				api.SetByJwt(jwt)
				if c.selfRevoked {
					api.noteSelfRevocation(jwt, sessionId)
				}
				rejected := false
				if network {
					target, ok := api.networkRequestTarget(jwt)
					if !ok {
						t.Fatal("synthetic login has no network target")
					}
					rejected = api.rejectNetworkCredential(target, c.cause)
				} else {
					rejected = api.rejectByJwt(jwt, c.cause)
				}
				if !rejected || !api.IsAuthLogoutCurrent(event.generation) {
					t.Fatal("current rejection lost its cleanup authority")
				}
				if events != 1 || legacyEvents != 1 || event.cause != c.want {
					t.Fatalf("network %t self-revoked %t: current rejection changed its trusted cause or compatibility delivery", network, c.selfRevoked)
				}
				api.SetByJwt("synthetic-next-login")
				api.SetByJwt("")
				if events != 1 || legacyEvents != 1 || api.IsAuthLogoutCurrent(event.generation) {
					t.Fatal("explicit logout emitted an event or revived the old rejection")
				}
			}()
		}
	}
}
