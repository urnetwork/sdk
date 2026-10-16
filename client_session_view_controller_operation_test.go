//go:build !ios_extension

// Operation identities are exercised through the controller and its real JSON
// API path. The endpoint retains receipts just as the server's idempotency path
// does; no test starts the controller's polling loop or contacts a service.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A synthetic login owns each controller; unconfigured requests cannot escape
// to the network. Action completion has an explicit callback barrier.
type clientSessionOperationTestFixture struct {
	api              *Api
	controller       *ClientSessionViewController
	currentSessionId *Id
	completed        chan struct{}
}

// Controllers remain unstarted so recovery and inventory ordering are explicit.
func newClientSessionOperationTestFixture(t *testing.T) *clientSessionOperationTestFixture {
	t.Helper()
	currentSessionId := newId(connect.NewId())
	api := newApi(t.Context(), nil, "https://api.example")
	api.SetByJwt(taggedSessionTestJwt(t, currentSessionId.String(), ""))
	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("unexpected synthetic session GET")
	})
	api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		return nil, errors.New("unexpected synthetic session POST")
	})
	controller := NewClientSessionViewControllerWithApi(t.Context(), api)
	completed := make(chan struct{}, 8)
	controller.testingAfterAction = func() { completed <- struct{}{} }
	t.Cleanup(func() {
		controller.Close()
		api.Close()
	})
	return &clientSessionOperationTestFixture{
		api: api, controller: controller, currentSessionId: currentSessionId, completed: completed,
	}
}

// The timeout is only a deadlock fail-safe, never the ordering or success oracle.
func awaitClientSessionOperationTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("session operation did not reach its explicit completion barrier")
	}
}

// Receipts are keyed by operation id for one fixed bulk-action/kept-session
// fingerprint. Refreshing inventory does not change that fingerprint.
type clientSessionIdempotentTestEndpoint struct {
	stateLock          sync.Mutex
	currentSessionId   *Id
	sessions           []*NetworkSessionInfo
	operationIdResults map[string]*SessionOperationResult
	postedOperationIds []*Id
	status             string
	state              string
	eventId            int64
}

// Only generated test identities enter the endpoint's inventory.
func newClientSessionIdempotentTestEndpoint(currentSessionId *Id, status, state string) *clientSessionIdempotentTestEndpoint {
	return &clientSessionIdempotentTestEndpoint{
		currentSessionId: currentSessionId,
		sessions: []*NetworkSessionInfo{
			{SessionId: currentSessionId, Current: true},
			{SessionId: newId(connect.NewId())},
		},
		operationIdResults: map[string]*SessionOperationResult{},
		status:             status, state: state, eventId: 1,
	}
}

// Inventory and operation lookup use the same endpoint-owned receipts.
func (self *clientSessionIdempotentTestEndpoint) get(_ context.Context, url, _ string) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if strings.HasSuffix(url, "/network/sessions") {
		sessions := NewNetworkSessionInfoList()
		for _, session := range self.sessions {
			sessions.Add(session)
		}
		return json.Marshal(&NetworkSessionsResult{
			Sessions: sessions, CurrentSessionId: self.currentSessionId,
			Generation: "synthetic-inventory", EventId: self.eventId,
		})
	}
	const prefix = "https://api.example/network/session-operations/"
	if strings.HasPrefix(url, prefix) {
		result := self.operationIdResults[strings.TrimPrefix(url, prefix)]
		if result == nil {
			return nil, &connect.HttpStatusError{StatusCode: http.StatusNotFound}
		}
		return json.Marshal(result)
	}
	return nil, fmt.Errorf("unexpected synthetic endpoint URL: %s", url)
}

// Replays return the saved receipt without re-evaluating the live inventory.
func (self *clientSessionIdempotentTestEndpoint) post(_ context.Context, url string, body []byte, _ string) ([]byte, error) {
	if !strings.HasSuffix(url, "/network/revoke-other-sessions") {
		return nil, fmt.Errorf("unexpected synthetic endpoint URL: %s", url)
	}
	var args RevokeOtherNetworkSessionsArgs
	if err := json.Unmarshal(body, &args); err != nil {
		return nil, err
	}
	if args.OperationId == nil {
		return nil, errors.New("missing synthetic operation id")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postedOperationIds = append(self.postedOperationIds, args.OperationId)
	if result := self.operationIdResults[args.OperationId.String()]; result != nil {
		return json.Marshal(result)
	}
	revokedCount := len(self.sessions) - 1
	self.sessions = []*NetworkSessionInfo{{SessionId: self.currentSessionId, Current: true}}
	self.eventId++
	result := &SessionOperationResult{
		OperationId: args.OperationId, Status: self.status, State: self.state,
		KeptSessionId: self.currentSessionId, RevokedCount: revokedCount,
		Generation: "synthetic-inventory", EventId: self.eventId,
	}
	self.operationIdResults[args.OperationId.String()] = result
	return json.Marshal(result)
}

// A new login becomes visible to a subsequent real controller refresh.
func (self *clientSessionIdempotentTestEndpoint) addSession() *Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sessionId := newId(connect.NewId())
	self.sessions = append(self.sessions, &NetworkSessionInfo{SessionId: sessionId})
	self.eventId++
	return sessionId
}

// M01: the server must actually revoke the newly created session, not merely
// replay an old success while the controller hides that session locally.
func TestClientSessionBulkActionCreatesFreshOperationAfterCompletion(t *testing.T) {
	for _, outcome := range []struct{ status, state string }{
		{status: "revoked", state: "enforced"},
		{status: "revoked"},
		{status: "already_revoked"},
		{state: "enforced"},
		{state: "complete"},
	} {
		fixture := newClientSessionOperationTestFixture(t)
		vc := fixture.controller
		endpoint := newClientSessionIdempotentTestEndpoint(fixture.currentSessionId, outcome.status, outcome.state)
		fixture.api.setHttpGetRaw(endpoint.get)
		fixture.api.setHttpPostRaw(endpoint.post)
		vc.refresh()
		if snapshot := vc.GetSnapshot(); !snapshot.Loaded || snapshot.Sessions.Len() != 2 {
			t.Fatalf("initial inventory was not loaded: %+v", snapshot)
		}
		vc.RevokeOtherSessions()
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		first := vc.GetSnapshot().BulkAction
		if first == nil || first.Loading || first.Pending || first.Error != nil {
			t.Fatalf("first action did not complete (%+v): %+v", outcome, first)
		}
		newSessionId := endpoint.addSession()
		vc.refresh()
		if vc.GetSnapshot().Sessions.Len() != 2 {
			t.Fatal("refresh did not expose the new session")
		}
		vc.RevokeOtherSessions()
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		endpoint.stateLock.Lock()
		postedOperationIds := append([]*Id{}, endpoint.postedOperationIds...)
		sessions := append([]*NetworkSessionInfo{}, endpoint.sessions...)
		endpoint.stateLock.Unlock()
		if len(postedOperationIds) != 2 || postedOperationIds[0].Cmp(postedOperationIds[1]) == 0 {
			t.Fatalf("M01: new bulk action reused a completed operation (%+v); new session %s remains in endpoint inventory of size %d", outcome, newSessionId, len(sessions))
		}
		if len(sessions) != 1 || !sessions[0].Current {
			t.Fatalf("new bulk action did not revoke the endpoint's new session: %+v", sessions)
		}
		vc.refresh()
		if snapshot := vc.GetSnapshot(); snapshot.Sessions.Len() != 1 || snapshot.BulkAction.OperationId.Cmp(postedOperationIds[1]) != 0 {
			t.Fatalf("controller and endpoint disagree after second action: %+v", snapshot)
		}
	}
}

// Every known terminal outcome ends its identity, through both POST completion
// and recovery. Repeated single-session actions replace their one existing slot.
func TestClientSessionKnownTerminalOutcomesStartNewOperations(t *testing.T) {
	for _, outcome := range []struct{ status, state string }{
		{status: "revoked"}, {status: "already_revoked"},
		{state: "enforced"}, {state: "complete"},
		{status: "cancelled", state: "cancelled"}, {status: "failed", state: "failed"},
	} {
		for _, bulk := range []bool{true, false} {
			for _, recovery := range []bool{false, true} {
				fixture := newClientSessionOperationTestFixture(t)
				vc := fixture.controller
				sessionId := newId(connect.NewId())
				var postedOperationIds []*Id
				fixture.api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
					var args RevokeNetworkSessionArgs
					if err := json.Unmarshal(body, &args); err != nil {
						return nil, err
					}
					postedOperationIds = append(postedOperationIds, args.OperationId)
					result := &SessionOperationResult{OperationId: args.OperationId, Status: outcome.status, State: outcome.state}
					if recovery && len(postedOperationIds) == 1 {
						result.Status, result.State = "pending", "prepared"
					}
					return json.Marshal(result)
				})
				fixture.api.setHttpGetRaw(func(_ context.Context, url, _ string) ([]byte, error) {
					operationId := postedOperationIds[0]
					if !strings.HasSuffix(url, "/network/session-operations/"+operationId.String()) {
						return nil, fmt.Errorf("wrong recovery operation: %s", url)
					}
					return json.Marshal(&SessionOperationResult{OperationId: operationId, Status: outcome.status, State: outcome.state})
				})
				revoke := func() {
					if bulk {
						vc.RevokeOtherSessions()
					} else {
						vc.RevokeSession(sessionId)
					}
					awaitClientSessionOperationTestSignal(t, fixture.completed)
				}
				action := func() *ClientSessionAction {
					snapshot := vc.GetSnapshot()
					if bulk {
						return snapshot.BulkAction
					}
					if snapshot.Actions.Len() != 1 {
						t.Fatalf("single-session action list grew: %d", snapshot.Actions.Len())
					}
					return snapshot.Actions.Get(0)
				}
				revoke()
				if recovery {
					vc.recoverPending()
				}
				first := action()
				if first == nil || first.Loading || first.Pending {
					t.Fatalf("terminal outcome stayed pending (%+v, bulk=%v, recovery=%v): %+v", outcome, bulk, recovery, first)
				}
				failed := outcome.state == "failed" || outcome.state == "cancelled"
				if failed && (first.Error == nil || first.Error.Retryable) || !failed && first.Error != nil {
					t.Fatalf("terminal outcome classification changed (%+v): %+v", outcome, first)
				}
				revoke()
				second := action()
				if len(postedOperationIds) != 2 || first.OperationId.Cmp(second.OperationId) == 0 {
					t.Fatalf("terminal action retained its old operation id (%+v, bulk=%v, recovery=%v)", outcome, bulk, recovery)
				}
			}
		}
	}
}

// Missing receipts and unconfirmed request failures are retries of the same
// operation, including a nonretryable HTTP error without a terminal receipt.
func TestClientSessionUncertainActionKeepsOperationIdentity(t *testing.T) {
	for _, response := range []string{"lost-response", "accepted", "request-rejected"} {
		fixture := newClientSessionOperationTestFixture(t)
		vc := fixture.controller
		var postedOperationIds []*Id
		fixture.api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
			var args RevokeOtherNetworkSessionsArgs
			if err := json.Unmarshal(body, &args); err != nil {
				return nil, err
			}
			postedOperationIds = append(postedOperationIds, args.OperationId)
			if len(postedOperationIds) == 1 {
				switch response {
				case "lost-response":
					return nil, errors.New("synthetic response unavailable")
				case "accepted":
					body, err := json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: "pending", State: "prepared"})
					if err != nil {
						return nil, err
					}
					return nil, &connect.HttpStatusError{StatusCode: http.StatusAccepted, Body: body}
				default:
					return nil, &connect.HttpStatusError{StatusCode: http.StatusBadRequest}
				}
			}
			return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: "revoked", State: "enforced"})
		})
		fixture.api.setHttpGetRaw(func(_ context.Context, url, _ string) ([]byte, error) {
			if !strings.HasSuffix(url, "/network/session-operations/"+postedOperationIds[0].String()) {
				return nil, fmt.Errorf("wrong recovery operation: %s", url)
			}
			return nil, &connect.HttpStatusError{StatusCode: http.StatusNotFound}
		})
		vc.RevokeOtherSessions()
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		first := vc.GetSnapshot().BulkAction
		if first == nil || first.Loading || first.Pending != (response != "request-rejected") {
			t.Fatalf("unexpected initial uncertainty (%s): %+v", response, first)
		}
		if first.Pending {
			vc.RevokeOtherSessions()
			pending := vc.GetSnapshot().BulkAction
			if pending == nil || pending.Loading || !pending.Pending || pending.OperationId.Cmp(first.OperationId) != 0 {
				t.Fatalf("pending operation admitted another action: %+v", pending)
			}
			vc.recoverPending()
			retry := vc.GetSnapshot().BulkAction
			if retry.Pending || retry.Error == nil || !retry.Error.Retryable || retry.OperationId.Cmp(first.OperationId) != 0 {
				t.Fatalf("missing receipt lost its retry identity: %+v", retry)
			}
		}
		vc.RevokeOtherSessions()
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		if len(postedOperationIds) != 2 || postedOperationIds[0].Cmp(postedOperationIds[1]) != 0 {
			t.Fatalf("uncertain retry minted a new operation (%s)", response)
		}
		vc.RevokeOtherSessions()
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		if len(postedOperationIds) != 3 || postedOperationIds[1].Cmp(postedOperationIds[2]) == 0 {
			t.Fatalf("completed retry prevented a fresh action (%s)", response)
		}
	}
}

// Losing the POST response does not lose a committed endpoint receipt.
func TestClientSessionPendingRecoveryKeepsCommittedOperationIdentity(t *testing.T) {
	fixture := newClientSessionOperationTestFixture(t)
	vc := fixture.controller
	endpoint := newClientSessionIdempotentTestEndpoint(fixture.currentSessionId, "revoked", "")
	fixture.api.setHttpGetRaw(endpoint.get)
	fixture.api.setHttpPostRaw(func(ctx context.Context, url string, body []byte, token string) ([]byte, error) {
		if _, err := endpoint.post(ctx, url, body, token); err != nil {
			return nil, err
		}
		return nil, errors.New("synthetic committed response lost")
	})
	vc.refresh()
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	first := vc.GetSnapshot().BulkAction
	if first == nil || !first.Pending {
		t.Fatalf("lost response did not enter recovery: %+v", first)
	}
	vc.recoverPending()
	recovered := vc.GetSnapshot().BulkAction
	if recovered.Pending || recovered.Error != nil || recovered.OperationId.Cmp(first.OperationId) != 0 {
		t.Fatalf("committed receipt did not complete the same operation: %+v", recovered)
	}
	endpoint.stateLock.Lock()
	postCount, sessionCount := len(endpoint.postedOperationIds), len(endpoint.sessions)
	endpoint.stateLock.Unlock()
	if postCount != 1 || sessionCount != 1 {
		t.Fatalf("recovery submitted another action: posts=%d sessions=%d", postCount, sessionCount)
	}
}

// A listener sees the exact synchronous recovery transition, without a timer
// tick, inventory refresh, or a second user action.
type clientSessionOperationTestListener func(*ClientSessionSnapshot)

// Listener dispatch deliberately remains the controller's production path.
func (self clientSessionOperationTestListener) ClientSessionsChanged(snapshot *ClientSessionSnapshot) {
	self(snapshot)
}

// M30: both bulk and single-session missing receipts must unblock listener-driven UI.
func TestClientSessionPendingRecoveryPublishesNotFoundRetry(t *testing.T) {
	for _, bulk := range []bool{true, false} {
		fixture := newClientSessionOperationTestFixture(t)
		vc := fixture.controller
		fixture.api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
			return nil, errors.New("synthetic response unavailable")
		})
		var getCount int
		fixture.api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
			getCount++
			return nil, &connect.HttpStatusError{StatusCode: http.StatusNotFound}
		})
		if bulk {
			vc.RevokeOtherSessions()
		} else {
			vc.RevokeSession(newId(connect.NewId()))
		}
		awaitClientSessionOperationTestSignal(t, fixture.completed)
		before := vc.GetSnapshot()
		beforeAction := before.BulkAction
		if !bulk {
			beforeAction = before.Actions.Get(0)
		}
		if beforeAction == nil || !beforeAction.Pending {
			t.Fatal("fixture did not enter pending recovery")
		}
		var notifications []*ClientSessionSnapshot
		sub := vc.AddClientSessionListener(clientSessionOperationTestListener(func(snapshot *ClientSessionSnapshot) {
			notifications = append(notifications, snapshot)
		}))
		t.Cleanup(sub.Close)
		vc.stateLock.Lock()
		mutation := vc.mutation
		vc.stateLock.Unlock()
		vc.recoverPending()
		if len(notifications) != 1 {
			t.Fatalf("M30: missing-operation recovery published %d transitions, want 1 (bulk=%v)", len(notifications), bulk)
		}
		action := notifications[0].BulkAction
		if !bulk {
			action = notifications[0].Actions.Get(0)
		}
		if action == nil || action.Pending || action.Loading || action.Error == nil || !action.Error.Retryable || action.OperationId.Cmp(beforeAction.OperationId) != 0 {
			t.Fatalf("listener did not receive the same retryable operation: %+v", action)
		}
		vc.stateLock.Lock()
		afterMutation, refreshCount := vc.mutation, vc.sequence
		vc.stateLock.Unlock()
		if getCount != 1 || refreshCount != 0 || afterMutation != mutation+1 {
			t.Fatalf("recovery depended on refresh or did not fence old inventory: gets=%d refreshes=%d mutations=%d->%d", getCount, refreshCount, mutation, afterMutation)
		}
	}
}

// M30's adjacent path publishes changed pending errors but does not emit a new
// snapshot for identical visible values on the next synchronous recovery call.
func TestClientSessionPendingRecoveryPublishesOnlyVisibleChanges(t *testing.T) {
	fixture := newClientSessionOperationTestFixture(t)
	vc := fixture.controller
	var operationId *Id
	fixture.api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
		var args RevokeOtherNetworkSessionsArgs
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		operationId = args.OperationId
		return json.Marshal(&SessionOperationResult{OperationId: operationId, Status: "pending", State: "prepared"})
	})
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	var statusCode int
	fixture.api.setHttpGetRaw(func(_ context.Context, url, _ string) ([]byte, error) {
		if !strings.HasSuffix(url, "/network/session-operations/"+operationId.String()) {
			return nil, fmt.Errorf("wrong recovery operation: %s", url)
		}
		if statusCode != 0 {
			return nil, &connect.HttpStatusError{StatusCode: statusCode}
		}
		return json.Marshal(&SessionOperationResult{OperationId: operationId, Status: "pending", State: "prepared"})
	})
	var notifications []*ClientSessionSnapshot
	sub := vc.AddClientSessionListener(clientSessionOperationTestListener(func(snapshot *ClientSessionSnapshot) {
		notifications = append(notifications, snapshot)
	}))
	t.Cleanup(sub.Close)
	for _, response := range []struct {
		statusCode int
		changed    bool
	}{
		{statusCode: http.StatusServiceUnavailable, changed: true},
		{statusCode: http.StatusServiceUnavailable},
		{statusCode: http.StatusTooManyRequests},
		{changed: true},
		{},
		{statusCode: http.StatusForbidden, changed: true},
		{statusCode: http.StatusForbidden},
		{statusCode: http.StatusServiceUnavailable, changed: true},
	} {
		statusCode = response.statusCode
		beforeNotifications := len(notifications)
		vc.stateLock.Lock()
		beforeMutation := vc.mutation
		vc.stateLock.Unlock()
		vc.recoverPending()
		expectedChange := 0
		if response.changed {
			expectedChange = 1
		}
		vc.stateLock.Lock()
		afterMutation := vc.mutation
		vc.stateLock.Unlock()
		if len(notifications)-beforeNotifications != expectedChange || afterMutation-beforeMutation != uint64(expectedChange) {
			t.Fatalf("pending recovery did not compare visible values (%+v): notifications=%d mutations=%d", response, len(notifications)-beforeNotifications, afterMutation-beforeMutation)
		}
		action := vc.GetSnapshot().BulkAction
		if action == nil || !action.Pending || action.OperationId.Cmp(operationId) != 0 || action.Status != "pending" || action.State != "prepared" {
			t.Fatalf("error publication changed the uncertain operation: %+v", action)
		}
		if response.changed {
			published := notifications[len(notifications)-1].BulkAction
			if published == nil || !published.Pending || published.OperationId.Cmp(operationId) != 0 || published.Status != action.Status || published.State != action.State || (published.Error == nil) != (action.Error == nil) || published.Error != nil && *published.Error != *action.Error {
				t.Fatalf("listener received a stale pending transition: published=%+v current=%+v", published, action)
			}
		}
		if statusCode == 0 {
			if action.Error != nil {
				t.Fatalf("successful pending lookup retained an error: %+v", action.Error)
			}
		} else if action.Error == nil || action.Error.SignInRequired || action.Error.Retryable != (statusCode != http.StatusForbidden) {
			t.Fatalf("pending error was not preserved: status=%d action=%+v", statusCode, action)
		}
	}
}

// A real unauthorized response goes through the credential wrapper before
// recovery resumes. It invalidates the login instead of publishing its old action.
func TestClientSessionUnauthorizedRecoveryInvalidatesCredentialWithoutPublication(t *testing.T) {
	fixture := newClientSessionOperationTestFixture(t)
	vc := fixture.controller
	fixture.api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
		var args RevokeOtherNetworkSessionsArgs
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: "pending", State: "prepared"})
	})
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	before := vc.GetSnapshot().BulkAction
	if before == nil || !before.Pending {
		t.Fatalf("fixture did not enter pending recovery: %+v", before)
	}
	target := fixture.api.captureNetworkTarget()
	entered := make(chan struct{})
	release := make(chan struct{})
	recoveryDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	var getCount atomic.Int32
	fixture.api.setHttpGetRaw(func(ctx context.Context, url, token string) ([]byte, error) {
		getCount.Add(1)
		if token != target.byJwt || !strings.HasSuffix(url, "/network/session-operations/"+before.OperationId.String()) {
			return nil, fmt.Errorf("wrong credential or operation reached unauthorized recovery")
		}
		close(entered)
		select {
		case <-release:
			return nil, &connect.HttpStatusError{StatusCode: http.StatusUnauthorized}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	var notifications atomic.Int32
	sub := vc.AddClientSessionListener(clientSessionOperationTestListener(func(*ClientSessionSnapshot) {
		notifications.Add(1)
	}))
	t.Cleanup(sub.Close)
	vc.stateLock.Lock()
	mutation := vc.mutation
	vc.stateLock.Unlock()
	go func() {
		defer close(recoveryDone)
		vc.recoverPending()
	}()
	t.Cleanup(func() {
		releaseRequest()
		awaitClientSessionOperationTestSignal(t, recoveryDone)
	})
	awaitClientSessionOperationTestSignal(t, entered)
	if !fixture.api.HasNetworkCredential() || fixture.api.sessionCredentialGeneration() != target.generation {
		t.Fatal("credential changed before the unauthorized response was released")
	}
	releaseRequest()
	awaitClientSessionOperationTestSignal(t, recoveryDone)
	if getCount.Load() != 1 || fixture.api.HasNetworkCredential() || fixture.api.sessionCredentialGeneration() == target.generation {
		t.Fatal("real unauthorized recovery did not invalidate the captured credential")
	}
	if _, current := fixture.api.currentNetworkTarget(target); current {
		t.Fatal("invalidated recovery target is still admissible")
	}
	vc.stateLock.Lock()
	afterMutation := vc.mutation
	vc.stateLock.Unlock()
	if notifications.Load() != 0 || afterMutation != mutation {
		t.Fatalf("old action published after credential invalidation: notifications=%d mutations=%d->%d", notifications.Load(), mutation, afterMutation)
	}
	if snapshot := vc.GetSnapshot(); snapshot.BulkAction != nil || snapshot.Actions.Len() != 0 || snapshot.Sessions.Len() != 0 {
		t.Fatalf("invalidated login exposed its old action: %+v", snapshot)
	}
}

// A delayed recovery response cannot mutate or notify a newer action after
// another receipt has already completed the old operation.
func TestClientSessionStaleRecoveryDoesNotPublishIntoReplacement(t *testing.T) {
	fixture := newClientSessionOperationTestFixture(t)
	vc := fixture.controller
	var postedOperationIds []*Id
	fixture.api.setHttpPostRaw(func(_ context.Context, _ string, body []byte, _ string) ([]byte, error) {
		var args RevokeOtherNetworkSessionsArgs
		if err := json.Unmarshal(body, &args); err != nil {
			return nil, err
		}
		postedOperationIds = append(postedOperationIds, args.OperationId)
		result := &SessionOperationResult{OperationId: args.OperationId, Status: "revoked", State: "enforced"}
		if len(postedOperationIds) == 1 {
			result.Status, result.State = "pending", "prepared"
		}
		return json.Marshal(result)
	})
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	oldOperationId := postedOperationIds[0]
	entered := make(chan struct{})
	release := make(chan struct{})
	recoveryDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	var getCount atomic.Int32
	fixture.api.setHttpGetRaw(func(ctx context.Context, url, _ string) ([]byte, error) {
		if !strings.HasSuffix(url, "/network/session-operations/"+oldOperationId.String()) {
			return nil, fmt.Errorf("wrong recovery operation: %s", url)
		}
		if getCount.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, &connect.HttpStatusError{StatusCode: http.StatusNotFound}
		}
		return json.Marshal(&SessionOperationResult{OperationId: oldOperationId, Status: "revoked", State: "enforced"})
	})
	go func() {
		defer close(recoveryDone)
		vc.recoverPending()
	}()
	t.Cleanup(func() {
		releaseRequest()
		awaitClientSessionOperationTestSignal(t, recoveryDone)
	})
	awaitClientSessionOperationTestSignal(t, entered)
	vc.recoverPending()
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	before := vc.GetSnapshot().BulkAction
	if before == nil || before.Pending || before.OperationId.Cmp(oldOperationId) == 0 {
		t.Fatalf("fixture did not replace the completed operation: %+v", before)
	}
	var notifications atomic.Int32
	sub := vc.AddClientSessionListener(clientSessionOperationTestListener(func(*ClientSessionSnapshot) {
		notifications.Add(1)
	}))
	t.Cleanup(sub.Close)
	vc.stateLock.Lock()
	mutation := vc.mutation
	vc.stateLock.Unlock()
	releaseRequest()
	awaitClientSessionOperationTestSignal(t, recoveryDone)
	after := vc.GetSnapshot().BulkAction
	vc.stateLock.Lock()
	afterMutation := vc.mutation
	vc.stateLock.Unlock()
	if getCount.Load() != 2 || notifications.Load() != 0 || afterMutation != mutation || after.OperationId.Cmp(before.OperationId) != 0 || after.Pending || after.Error != nil {
		t.Fatalf("detached recovery changed the new action: gets=%d notifications=%d mutations=%d->%d action=%+v", getCount.Load(), notifications.Load(), mutation, afterMutation, after)
	}
}

// Both POST completion and the newly published missing-receipt transition are
// fenced when their owning controller closes or moves to another login.
func TestClientSessionLateResultCannotPublishAfterLifecycleChange(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, lifecycle := range []string{"new-login", "close"} {
			fixture := newClientSessionOperationTestFixture(t)
			vc := fixture.controller
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
			holdRequest := func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			fixture.api.setHttpPostRaw(func(ctx context.Context, _ string, body []byte, _ string) ([]byte, error) {
				var args RevokeOtherNetworkSessionsArgs
				if err := json.Unmarshal(body, &args); err != nil {
					return nil, err
				}
				if recovery {
					return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: "pending", State: "prepared"})
				}
				if err := holdRequest(ctx); err != nil {
					return nil, err
				}
				return json.Marshal(&SessionOperationResult{OperationId: args.OperationId, Status: "revoked", State: "enforced"})
			})
			newSessionId := newId(connect.NewId())
			fixture.api.setHttpGetRaw(func(ctx context.Context, url, _ string) ([]byte, error) {
				if strings.HasSuffix(url, "/network/sessions") {
					sessions := NewNetworkSessionInfoList()
					sessions.Add(&NetworkSessionInfo{SessionId: newSessionId, Current: true})
					return json.Marshal(&NetworkSessionsResult{
						Sessions: sessions, CurrentSessionId: newSessionId,
						Generation: "new-synthetic-login", EventId: 1,
					})
				}
				if err := holdRequest(ctx); err != nil {
					return nil, err
				}
				return nil, &connect.HttpStatusError{StatusCode: http.StatusNotFound}
			})
			vc.RevokeOtherSessions()
			var done <-chan struct{} = fixture.completed
			if recovery {
				awaitClientSessionOperationTestSignal(t, fixture.completed)
				recoveryDone := make(chan struct{})
				go func() {
					defer close(recoveryDone)
					vc.recoverPending()
				}()
				done = recoveryDone
			}
			joined := false
			t.Cleanup(func() {
				releaseRequest()
				if !joined {
					vc.Close()
					awaitClientSessionOperationTestSignal(t, done)
				}
			})
			awaitClientSessionOperationTestSignal(t, entered)
			var notifications atomic.Int32
			sub := vc.AddClientSessionListener(clientSessionOperationTestListener(func(*ClientSessionSnapshot) {
				notifications.Add(1)
			}))
			t.Cleanup(sub.Close)
			var expectedNotifications int32
			if lifecycle == "new-login" {
				fixture.api.SetByJwt(taggedSessionTestJwt(t, newSessionId.String(), ""))
				vc.refresh()
				expectedNotifications = notifications.Load()
				if snapshot := vc.GetSnapshot(); snapshot.BulkAction != nil || !snapshot.Loaded || snapshot.CurrentSessionId.Cmp(newSessionId) != 0 {
					t.Fatalf("new login did not replace old controller state: %+v", snapshot)
				}
			} else {
				vc.Close()
			}
			vc.stateLock.Lock()
			mutation := vc.mutation
			vc.stateLock.Unlock()
			releaseRequest()
			awaitClientSessionOperationTestSignal(t, done)
			joined = true
			vc.stateLock.Lock()
			afterMutation := vc.mutation
			vc.stateLock.Unlock()
			if notifications.Load() != expectedNotifications || afterMutation != mutation {
				t.Fatalf("late result crossed lifecycle boundary (recovery=%v, lifecycle=%s): notifications=%d want=%d mutations=%d->%d", recovery, lifecycle, notifications.Load(), expectedNotifications, mutation, afterMutation)
			}
			if lifecycle == "new-login" {
				if snapshot := vc.GetSnapshot(); snapshot.BulkAction != nil || snapshot.CurrentSessionId.Cmp(newSessionId) != 0 {
					t.Fatalf("late result changed the new login: %+v", snapshot)
				}
			}
		}
	}
}
