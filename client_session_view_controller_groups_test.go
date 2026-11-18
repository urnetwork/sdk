//go:build !ios_extension

// The Sessions screen's layout of a long list: device sign-ins first, then
// collapsed client groups, a page of rows at a time, read over the list's
// pages. The controllers stay unstarted; each reload is called directly.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A synthetic list endpoint that answers in pages of pageSize rows, naming
// each next page by its offset.
type clientSessionPagedTestEndpoint struct {
	stateLock        sync.Mutex
	currentSessionId *Id
	sessions         []*NetworkSessionInfo
	pageSize         int
	cursors          []string
	// answers the page at this index with an error instead
	failPage  int
	failError error
	// answers every page with this cursor
	repeatCursor string
}

func (self *clientSessionPagedTestEndpoint) get(_ context.Context, requestUrl string, _ string) ([]byte, error) {
	parsed, err := url.Parse(requestUrl)
	if err != nil || parsed.Path != "/network/sessions" {
		return nil, fmt.Errorf("unexpected synthetic endpoint URL: %s", requestUrl)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	cursor := parsed.Query().Get("cursor")
	page := len(self.cursors)
	self.cursors = append(self.cursors, cursor)
	if self.failError != nil && page == self.failPage {
		return nil, self.failError
	}
	offset := 0
	if cursor != "" && cursor != self.repeatCursor {
		if _, err := fmt.Sscanf(cursor, "offset %d", &offset); err != nil {
			return nil, &connect.HttpStatusError{StatusCode: http.StatusBadRequest}
		}
	}
	end := min(offset+self.pageSize, len(self.sessions))
	result := &NetworkSessionsResult{
		Sessions:         NewNetworkSessionInfoList(),
		CurrentSessionId: self.currentSessionId,
		LegacyCoverage:   "partial",
		Generation:       "synthetic",
		EventId:          int64(1 + page),
	}
	result.Sessions.addAll(self.sessions[offset:end]...)
	if end < len(self.sessions) {
		// a reserved character, so the cursor must be escaped in the query
		result.NextCursor = fmt.Sprintf("offset %d", end)
	}
	if self.repeatCursor != "" {
		result.NextCursor = self.repeatCursor
	}
	return json.Marshal(result)
}

func (self *clientSessionPagedTestEndpoint) requestedCursors() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.cursors...)
}

func newClientSessionGroupsTestController(t *testing.T, endpoint *clientSessionPagedTestEndpoint) *ClientSessionViewController {
	t.Helper()
	api := newApi(t.Context(), nil, "https://api.example")
	api.SetByJwt(taggedSessionTestJwt(t, endpoint.currentSessionId.String(), ""))
	api.setHttpGetRaw(endpoint.get)
	api.setHttpPostRaw(func(context.Context, string, []byte, string) ([]byte, error) {
		return nil, errors.New("unexpected synthetic session POST")
	})
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	t.Cleanup(func() {
		vc.Close()
		api.Close()
	})
	return vc
}

// One row of each kind a server lists: an older server's ways a session began
// and a newer server's kinds.
func clientSessionGroupsTestSessions(currentSessionId *Id) []*NetworkSessionInfo {
	session := func(kind, clientKind string) *NetworkSessionInfo {
		return &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: kind, ClientKind: clientKind}
	}
	return []*NetworkSessionInfo{
		{SessionId: currentSessionId, Current: true, Kind: "password"},
		session("legacy_proxy", ""),
		session("google", ""),
		session("api_key_client", ""),
		session("client", "hosted_proxy"),
		session("auth_code", ""),
		session("device_adopt", ""),
		session("client", "legacy"),
		session("legacy", ""),
		session("hosted_proxy", "hosted_proxy"),
		session("code", ""),
		session("client", "future_kind"),
		session("signin", ""),
		session("client", "api_key"),
		// a server that names the kind in session_kind and keeps the way
		// the session began in kind
		{SessionId: newId(connect.NewId()), Kind: "hosted_proxy", SessionKind: "client", ClientKind: "hosted_proxy"},
		{SessionId: newId(connect.NewId()), Kind: "legacy", SessionKind: "client", ClientKind: "legacy"},
		{SessionId: newId(connect.NewId()), Kind: "legacy", SessionKind: "signin"},
		{SessionId: newId(connect.NewId()), Kind: "auth_code", SessionKind: "code"},
	}
}

func clientSessionTestKinds(list *NetworkSessionInfoList) []string {
	kinds := []string{}
	for _, info := range list.values {
		kinds = append(kinds, info.Kind+"/"+info.ClientKind)
	}
	return kinds
}

// Every client session moved out of the device sign-ins into a collapsed
// group of its kind. The screen listed every session in one list before, so
// a Pro network's thousands of proxy sessions buried its devices.
func TestClientSessionGroupsClientSessionsBelowTheDeviceSignIns(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 100}
	endpoint.sessions = clientSessionGroupsTestSessions(currentSessionId)
	vc := newClientSessionGroupsTestController(t, endpoint)
	vc.refresh()
	snapshot := vc.GetSnapshot()
	if !snapshot.Loaded || snapshot.Error != nil {
		t.Fatalf("not loaded: %+v", snapshot)
	}
	signIns := strings.Join(clientSessionTestKinds(snapshot.Sessions), " ")
	if signIns != "password/ google/ auth_code/ legacy/ code/ signin/ legacy/ auth_code/" {
		t.Fatalf("device sign-ins: %s", signIns)
	}
	if snapshot.SessionCount != 8 || snapshot.SessionsHasMore {
		t.Fatalf("sign-in count %d, more %v", snapshot.SessionCount, snapshot.SessionsHasMore)
	}
	groups := []string{}
	for _, group := range snapshot.Groups.values {
		if group.Expanded || group.HasMore || group.Sessions.Len() != 0 {
			t.Fatalf("group not collapsed: %+v", group)
		}
		groups = append(groups, fmt.Sprintf("%s:%d", group.Kind, group.Count))
	}
	if strings.Join(groups, " ") != "hosted_proxies:4 api_clients:2 paired_devices:1 other_clients:3" {
		t.Fatalf("groups: %v", groups)
	}
	// everything but the current session and the 4 hosted proxies
	if snapshot.OtherSessionCount != 18-1-4 || snapshot.Truncated {
		t.Fatalf("other sessions %d, truncated %v", snapshot.OtherSessionCount, snapshot.Truncated)
	}
}

// The current session stays among the device sign-ins whatever its kind.
func TestClientSessionCurrentSessionIsAlwaysASignIn(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 100}
	endpoint.sessions = []*NetworkSessionInfo{
		{SessionId: currentSessionId, Current: true, Kind: "client", ClientKind: "api_key"},
		{SessionId: newId(connect.NewId()), Kind: "client", ClientKind: "api_key"},
	}
	vc := newClientSessionGroupsTestController(t, endpoint)
	vc.refresh()
	snapshot := vc.GetSnapshot()
	if snapshot.Sessions.Len() != 1 || !snapshot.Sessions.Get(0).Current {
		t.Fatalf("current session not a sign-in: %v", clientSessionTestKinds(snapshot.Sessions))
	}
	if snapshot.Groups.Len() != 1 || snapshot.Groups.Get(0).Kind != ClientSessionGroupApiClients || snapshot.Groups.Get(0).Count != 1 {
		t.Fatalf("groups: %+v", snapshot.Groups.values)
	}
}

// A group opens to a page of rows and shows another page at a time; the
// device sign-ins show a page at a time too. Opening, closing and showing
// more read nothing.
func TestClientSessionGroupOpensAndShowsAPageAtATime(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 1000}
	endpoint.sessions = []*NetworkSessionInfo{{SessionId: currentSessionId, Current: true, Kind: "password"}}
	for i := 0; i < 5; i++ {
		endpoint.sessions = append(endpoint.sessions, &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "legacy_proxy"})
		endpoint.sessions = append(endpoint.sessions, &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "verify"})
	}
	vc := newClientSessionGroupsTestController(t, endpoint)
	vc.settings.pageSize = 2
	vc.refresh()
	requests := len(endpoint.requestedCursors())

	snapshot := vc.GetSnapshot()
	if snapshot.Sessions.Len() != 2 || !snapshot.Sessions.Get(0).Current || snapshot.SessionCount != 6 || !snapshot.SessionsHasMore {
		t.Fatalf("first page of sign-ins: %d of %d, more %v", snapshot.Sessions.Len(), snapshot.SessionCount, snapshot.SessionsHasMore)
	}
	vc.ShowMoreSessions(ClientSessionGroupSignIns)
	vc.ShowMoreSessions(ClientSessionGroupSignIns)
	if snapshot = vc.GetSnapshot(); snapshot.Sessions.Len() != 6 || snapshot.SessionsHasMore {
		t.Fatalf("every sign-in: %d, more %v", snapshot.Sessions.Len(), snapshot.SessionsHasMore)
	}

	// a closed group does not grow
	vc.ShowMoreSessions(ClientSessionGroupHostedProxies)
	if group := vc.GetSnapshot().Groups.Get(0); group.Expanded || group.Sessions.Len() != 0 {
		t.Fatalf("closed group grew: %+v", group)
	}
	vc.SetGroupExpanded(ClientSessionGroupHostedProxies, true)
	group := vc.GetSnapshot().Groups.Get(0)
	if group.Kind != ClientSessionGroupHostedProxies || !group.Expanded || group.Count != 5 || group.Sessions.Len() != 2 || !group.HasMore {
		t.Fatalf("opened group: %+v", group)
	}
	vc.ShowMoreSessions(ClientSessionGroupHostedProxies)
	vc.ShowMoreSessions(ClientSessionGroupHostedProxies)
	if group = vc.GetSnapshot().Groups.Get(0); group.Sessions.Len() != 5 || group.HasMore {
		t.Fatalf("every proxy: %+v", group)
	}
	// the open group and its rows outlive a reload
	vc.refresh()
	if group = vc.GetSnapshot().Groups.Get(0); !group.Expanded || group.Sessions.Len() != 5 {
		t.Fatalf("reload closed the group: %+v", group)
	}
	vc.SetGroupExpanded(ClientSessionGroupHostedProxies, false)
	if group = vc.GetSnapshot().Groups.Get(0); group.Expanded || group.Sessions.Len() != 0 || group.HasMore {
		t.Fatalf("closed group: %+v", group)
	}
	// unknown kinds change nothing
	vc.SetGroupExpanded("unknown", true)
	vc.SetGroupExpanded(ClientSessionGroupSignIns, false)
	vc.ShowMoreSessions("unknown")
	if snapshot = vc.GetSnapshot(); snapshot.Sessions.Len() != 6 || snapshot.Groups.Get(0).Expanded {
		t.Fatalf("an unknown kind changed the layout: %+v", snapshot)
	}
	if len(endpoint.requestedCursors()) != requests+1 {
		t.Fatalf("presentation read the list: %v", endpoint.requestedCursors())
	}
}

// A reload reads every page of the list, each by the cursor its previous page
// named, once per row. Before, the screen read the first page only.
func TestClientSessionReloadReadsEveryPage(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 3}
	endpoint.sessions = []*NetworkSessionInfo{{SessionId: currentSessionId, Current: true, Kind: "password"}}
	for i := 0; i < 7; i++ {
		endpoint.sessions = append(endpoint.sessions, &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "client", ClientKind: "hosted_proxy"})
	}
	// a later page repeats a row an earlier page had: it is read once
	endpoint.sessions = append(endpoint.sessions, endpoint.sessions[2])
	vc := newClientSessionGroupsTestController(t, endpoint)
	vc.refresh()
	if cursors := strings.Join(endpoint.requestedCursors(), "|"); cursors != "|offset 3|offset 6" {
		t.Fatalf("pages: %s", cursors)
	}
	snapshot := vc.GetSnapshot()
	if snapshot.Groups.Len() != 1 || snapshot.Groups.Get(0).Count != 7 || snapshot.SessionCount != 1 || snapshot.Truncated {
		t.Fatalf("whole list: %+v", snapshot.Groups.values)
	}
	// the revision is the first page's, so a hint newer than it reloads
	if snapshot.EventId != 1 {
		t.Fatalf("revision %d", snapshot.EventId)
	}
}

// A reload stops at its page and session bounds, and at a server that names
// the same page again; the counts are then lower bounds.
func TestClientSessionReloadStopsAtItsBounds(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	sessions := []*NetworkSessionInfo{{SessionId: currentSessionId, Current: true, Kind: "password"}}
	for i := 0; i < 20; i++ {
		sessions = append(sessions, &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "api_key_client"})
	}
	for _, c := range []struct {
		maxPages, maxSessions int
		repeatCursor          string
		pages                 int
	}{
		{maxPages: 3, maxSessions: 1000, pages: 3},
		{maxPages: 100, maxSessions: 4, pages: 2},
		{maxPages: 100, maxSessions: 1000, repeatCursor: "again", pages: 2},
	} {
		endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 2, sessions: sessions, repeatCursor: c.repeatCursor}
		vc := newClientSessionGroupsTestController(t, endpoint)
		vc.settings.maxPages = c.maxPages
		vc.settings.maxSessions = c.maxSessions
		vc.refresh()
		snapshot := vc.GetSnapshot()
		if len(endpoint.requestedCursors()) != c.pages || !snapshot.Truncated || !snapshot.Loaded {
			t.Fatalf("%+v: pages %v, truncated %v", c, endpoint.requestedCursors(), snapshot.Truncated)
		}
	}
}

// A later page's failure fails the reload but keeps the last list, and only
// the first page's 404 means a server without the list.
func TestClientSessionLaterPageFailureKeepsTheList(t *testing.T) {
	currentSessionId := newId(connect.NewId())
	endpoint := &clientSessionPagedTestEndpoint{currentSessionId: currentSessionId, pageSize: 2}
	endpoint.sessions = []*NetworkSessionInfo{{SessionId: currentSessionId, Current: true, Kind: "password"}}
	for i := 0; i < 3; i++ {
		endpoint.sessions = append(endpoint.sessions, &NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "legacy_proxy"})
	}
	vc := newClientSessionGroupsTestController(t, endpoint)
	vc.refresh()
	if snapshot := vc.GetSnapshot(); snapshot.Groups.Len() != 1 || snapshot.Groups.Get(0).Count != 3 {
		t.Fatalf("first reload: %+v", snapshot)
	}
	endpoint.stateLock.Lock()
	endpoint.failPage = len(endpoint.cursors) + 1
	endpoint.failError = &connect.HttpStatusError{StatusCode: http.StatusNotFound}
	endpoint.stateLock.Unlock()
	vc.refresh()
	snapshot := vc.GetSnapshot()
	if !snapshot.Supported || snapshot.Error == nil || snapshot.Error.Unsupported || !snapshot.Error.Retryable && snapshot.Error.SignInRequired {
		t.Fatalf("a later page's 404 hid the screen: %+v", snapshot.Error)
	}
	if snapshot.Groups.Len() != 1 || snapshot.Groups.Get(0).Count != 3 || snapshot.SessionCount != 1 {
		t.Fatalf("the failed reload lost the list: %+v", snapshot)
	}
	endpoint.stateLock.Lock()
	endpoint.failPage = len(endpoint.cursors)
	endpoint.stateLock.Unlock()
	vc.refresh()
	if snapshot = vc.GetSnapshot(); snapshot.Supported || !snapshot.Error.Unsupported {
		t.Fatalf("the first page's 404 is a server without the list: %+v", snapshot.Error)
	}
}

// A sign-out of other sessions keeps the hosted proxies (server owner
// decision 4), so they stay listed until the reload says otherwise. They
// left the list with every other session before.
func TestClientSessionSignOutOfOthersKeepsTheHostedProxies(t *testing.T) {
	fixture := newClientSessionOperationTestFixture(t)
	vc := fixture.controller
	endpoint := newClientSessionIdempotentTestEndpoint(fixture.currentSessionId, "revoked", "enforced")
	endpoint.sessions = append(endpoint.sessions,
		&NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "legacy_proxy"},
		&NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "client", ClientKind: "hosted_proxy"},
		&NetworkSessionInfo{SessionId: newId(connect.NewId()), Kind: "api_key_client"},
	)
	fixture.api.setHttpGetRaw(endpoint.get)
	// the reload after the action fails, so the snapshot is the controller's own
	posts := 0
	fixture.api.setHttpPostRaw(func(ctx context.Context, requestUrl string, body []byte, byJwt string) ([]byte, error) {
		posts++
		fixture.api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
			return nil, &connect.HttpStatusError{StatusCode: http.StatusServiceUnavailable}
		})
		return endpoint.post(ctx, requestUrl, body, byJwt)
	})
	vc.refresh()
	snapshot := vc.GetSnapshot()
	if snapshot.OtherSessionCount != 2 || snapshot.Groups.Len() != 2 {
		t.Fatalf("before: other sessions %d, groups %+v", snapshot.OtherSessionCount, snapshot.Groups.values)
	}
	vc.RevokeOtherSessions()
	awaitClientSessionOperationTestSignal(t, fixture.completed)
	snapshot = vc.GetSnapshot()
	if posts != 1 || snapshot.BulkAction == nil || snapshot.BulkAction.Pending || snapshot.BulkAction.Error != nil {
		t.Fatalf("action: %+v", snapshot.BulkAction)
	}
	if snapshot.SessionCount != 1 || snapshot.OtherSessionCount != 0 {
		t.Fatalf("sign-ins after: %d, other sessions %d", snapshot.SessionCount, snapshot.OtherSessionCount)
	}
	if snapshot.Groups.Len() != 1 || snapshot.Groups.Get(0).Kind != ClientSessionGroupHostedProxies || snapshot.Groups.Get(0).Count != 2 {
		t.Fatalf("hosted proxies left the list: %+v", snapshot.Groups.values)
	}
}

// A hint reloads at once after the hint spacing and waits out the rest of it
// before; the spacing is the settings'. A hint while the screen is hidden
// reads nothing: a controller kept open in the background (android, since
// W15) read the whole list again at every change.
func TestClientSessionHintWaitsOutTheHintSpacing(t *testing.T) {
	api := newApi(t.Context(), nil, "https://api.example")
	defer api.Close()
	vc := NewClientSessionViewControllerWithApi(t.Context(), api)
	defer vc.Close()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if delay := vc.hintRefreshDelay(now); delay != 0 {
		t.Fatalf("before any reload: %v", delay)
	}
	vc.settings.hintRefreshInterval = 10 * time.Second
	vc.lastRefreshTime = now
	if delay := vc.hintRefreshDelay(now.Add(3 * time.Second)); delay != 7*time.Second {
		t.Fatalf("within the spacing: %v", delay)
	}
	if delay := vc.hintRefreshDelay(now.Add(11 * time.Second)); delay != 0 {
		t.Fatalf("after the spacing: %v", delay)
	}
	vc.settings.hintRefreshInterval = 0
	if delay := vc.hintRefreshDelay(now.Add(time.Second)); delay != 0 {
		t.Fatalf("without spacing: %v", delay)
	}
	// a hint no newer than the list starts nothing
	vc.state.Generation = "g"
	vc.state.EventId = 5
	vc.NetworkSessionsChanged(&NetworkSessionsRevision{Generation: "g", EventId: 5})
	select {
	case <-vc.hints:
		t.Fatal("a current hint queued a reload")
	default:
	}
	// hidden: a newer hint is dropped, since showing the screen reloads
	vc.NetworkSessionsChanged(&NetworkSessionsRevision{Generation: "g", EventId: 6})
	select {
	case <-vc.hints:
		t.Fatal("a hint queued a reload while the screen was hidden")
	default:
	}
	vc.SetVisible(true)
	select {
	case <-vc.requests:
	default:
		t.Fatal("showing the screen did not reload the list")
	}
	vc.NetworkSessionsChanged(&NetworkSessionsRevision{Generation: "g", EventId: 6})
	select {
	case <-vc.hints:
	default:
		t.Fatal("a newer hint queued no reload")
	}
}
