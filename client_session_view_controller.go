//go:build !ios_extension

// Shared session inventory/action state for native, web, and API-only hosts.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

type ClientSessionError struct {
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable"`
	SignInRequired bool   `json:"sign_in_required"`
	// with SignInRequired: the server confirmed this sign-in session was
	// revoked from another device (AuthLogoutCauseSessionRevoked). Never set
	// for a generic rejection or a sign-out of this session made here.
	SessionRevoked bool `json:"session_revoked"`
	Unsupported    bool `json:"unsupported"`
}
type ClientSessionAction struct {
	Status      string `json:"status"`
	State       string `json:"state"`
	target      networkRenewalTarget
	SessionId   *Id                 `json:"session_id"`
	OperationId *Id                 `json:"operation_id"`
	Loading     bool                `json:"loading"`
	Pending     bool                `json:"pending"`
	Error       *ClientSessionError `json:"error"`
}
type ClientSessionActionList struct {
	exportedList[*ClientSessionAction]
}

func NewClientSessionActionList() *ClientSessionActionList {
	return &ClientSessionActionList{exportedList: *newExportedList[*ClientSessionAction]()}
}

type ClientSessionSnapshot struct {
	Sessions         *NetworkSessionInfoList  `json:"sessions"`
	CurrentSessionId *Id                      `json:"current_session_id"`
	LegacyCoverage   string                   `json:"legacy_coverage"`
	Generation       string                   `json:"generation"`
	EventId          int64                    `json:"event_id"`
	Loaded           bool                     `json:"loaded"`
	Loading          bool                     `json:"loading"`
	Refreshing       bool                     `json:"refreshing"`
	Supported        bool                     `json:"supported"`
	BulkAction       *ClientSessionAction     `json:"bulk_action"`
	Actions          *ClientSessionActionList `json:"actions"`
	Error            *ClientSessionError      `json:"error"`
}
type ClientSessionListener interface {
	ClientSessionsChanged(snapshot *ClientSessionSnapshot)
}

// Snapshot copies own their nested typed metadata; callers cannot mutate state.
func cloneClientSessionSnapshot(value *ClientSessionSnapshot) *ClientSessionSnapshot {
	encoded, _ := json.Marshal(value)
	copy := &ClientSessionSnapshot{}
	_ = json.Unmarshal(encoded, copy)
	return copy
}

type ClientSessionViewController struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	api                      *Api
	device                   Device
	stateLock                sync.Mutex
	state                    *ClientSessionSnapshot
	listeners                *connect.CallbackList[ClientSessionListener]
	started, closed, visible bool
	credentialGeneration     uint64
	sequence                 uint64
	mutation                 uint64
	requests                 chan struct{}
	lifecycle                chan struct{}
	subscription             Sub
	testingBeforeSubmit      func()
	testingAfterAction       func()
}

func newClientSessionViewController(ctx context.Context, device Device) *ClientSessionViewController {
	return NewClientSessionViewControllerWithDevice(ctx, device)
}
func NewClientSessionViewControllerWithDevice(ctx context.Context, device Device) *ClientSessionViewController {
	vc := NewClientSessionViewControllerWithApi(ctx, device.GetApi())
	vc.device = device
	return vc
}
func NewClientSessionViewControllerWithApi(ctx context.Context, api *Api) *ClientSessionViewController {
	ctx, cancel := context.WithCancel(ctx)
	return &ClientSessionViewController{ctx: ctx, cancel: cancel, api: api, credentialGeneration: api.sessionCredentialGeneration(), state: emptyClientSessionSnapshot(), listeners: connect.NewCallbackList[ClientSessionListener](), requests: make(chan struct{}, 1), lifecycle: make(chan struct{}, 1)}
}
func emptyClientSessionSnapshot() *ClientSessionSnapshot {
	return &ClientSessionSnapshot{Sessions: NewNetworkSessionInfoList(), Actions: NewClientSessionActionList(), LegacyCoverage: "partial", Supported: true}
}
func (self *ClientSessionViewController) Start() {
	self.stateLock.Lock()
	if self.closed || self.started {
		self.stateLock.Unlock()
		return
	}
	self.started = true
	self.subscription = self.api.AddNetworkSessionsChangeListener(self)
	self.stateLock.Unlock()
	go self.run()
	self.Refresh()
}
func (self *ClientSessionViewController) Stop() { self.SetVisible(false) }
func (self *ClientSessionViewController) Close() {
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return
	}
	self.closed = true
	self.sequence++
	subscription := self.subscription
	self.subscription = nil
	self.stateLock.Unlock()
	self.cancel()
	if subscription != nil {
		subscription.Close()
	}
}
func (self *ClientSessionViewController) SetVisible(visible bool) {
	self.stateLock.Lock()
	changed := self.visible != visible
	self.visible = visible
	self.stateLock.Unlock()
	select {
	case self.lifecycle <- struct{}{}:
	default:
	}
	if visible && changed {
		self.Refresh()
	}
}
func (self *ClientSessionViewController) SetForeground(foreground bool) {
	if foreground {
		self.Refresh()
	}
}
func (self *ClientSessionViewController) Refresh() {
	self.stateLock.Lock()
	closed := self.closed
	self.stateLock.Unlock()
	if closed {
		return
	}
	select {
	case self.requests <- struct{}{}:
	default:
	}
}
func (self *ClientSessionViewController) GetSnapshot() *ClientSessionSnapshot {
	generation := self.api.sessionCredentialGeneration()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if generation != self.credentialGeneration {
		return emptyClientSessionSnapshot()
	}
	return cloneClientSessionSnapshot(self.state)
}
func (self *ClientSessionViewController) AddClientSessionListener(listener ClientSessionListener) Sub {
	id := self.listeners.Add(listener)
	return newSub(func() { self.listeners.Remove(id) })
}
func (self *ClientSessionViewController) publish() {
	snapshot := self.GetSnapshot()
	for _, listener := range self.listeners.Get() {
		copy := cloneClientSessionSnapshot(snapshot)
		connect.HandleError(func() { listener.ClientSessionsChanged(copy) })
	}
}
func (self *ClientSessionViewController) NetworkSessionsChanged(revision *NetworkSessionsRevision) {
	self.stateLock.Lock()
	refresh := revision == nil || revision.Generation == "" || revision.Generation != self.state.Generation || revision.EventId > self.state.EventId
	self.stateLock.Unlock()
	if refresh {
		self.Refresh()
	}
}
func (self *ClientSessionViewController) run() {
	recovery := time.NewTicker(2 * time.Second)
	defer recovery.Stop()
	var timer *time.Timer
	var tick <-chan time.Time
	reset := func() {
		if timer != nil {
			timer.Stop()
		}
		self.stateLock.Lock()
		visible := self.visible
		self.stateLock.Unlock()
		tick = nil
		if visible {
			timer = time.NewTimer(30 * time.Second)
			tick = timer.C
		}
	}
	reset()
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-recovery.C:
			self.recoverPending()
		case <-self.lifecycle:
			reset()
		case <-tick:
			self.Refresh()
			reset()
		case <-self.requests:
			self.refresh()
			self.recoverPending()
		}
	}
}

// The error of a request that sent the credential sentByJwt. Once a confirmed
// rejection has cleared the network credential, the controller's requests
// fail before they are sent; they report that rejection as sign-in required,
// with its trusted cause, until a new login.
func (self *ClientSessionViewController) clientSessionError(err error, listing bool, sentByJwt string) *ClientSessionError {
	if err == nil {
		return nil
	}
	result := &ClientSessionError{Message: "Session request could not be completed.", Retryable: true}
	var status *connect.HttpStatusError
	if errors.As(err, &status) {
		result.SignInRequired = status.StatusCode == 401
		result.SessionRevoked = result.SignInRequired && self.api.trustedRejectionCause(sentByJwt, err) == AuthLogoutCauseSessionRevoked
		result.Unsupported = listing && status.StatusCode == 404
		result.Retryable = status.StatusCode == 503 || status.StatusCode == 429 || status.StatusCode == 408 || status.StatusCode == 409
	} else if errors.Is(err, ErrNetworkCredentialRequired) {
		if rejected, cause := self.api.sessionSignInRejection(); rejected {
			result.SignInRequired = true
			result.SessionRevoked = cause == AuthLogoutCauseSessionRevoked
			result.Retryable = false
		}
	}
	if result.SessionRevoked {
		result.Message = "This session was signed out from another device."
	} else if result.SignInRequired {
		result.Message = "Sign-in required."
	}
	return result
}
func (self *ClientSessionViewController) refresh() {
	target := self.api.captureNetworkTarget()
	generation := target.generation
	self.stateLock.Lock()
	if self.closed {
		self.stateLock.Unlock()
		return
	}
	if generation != self.credentialGeneration {
		self.state = emptyClientSessionSnapshot()
		self.credentialGeneration = generation
		self.mutation++
	}
	self.sequence++
	sequence, mutation := self.sequence, self.mutation
	self.state.Loading = !self.state.Loaded
	self.state.Refreshing = self.state.Loaded
	self.stateLock.Unlock()
	self.publish()
	result, err := self.api.networkSessionsFor(self.ctx, target)
	requestError := self.clientSessionError(err, true, target.byJwt)
	current := self.api.sessionCredentialGeneration()
	self.stateLock.Lock()
	if self.closed || sequence != self.sequence || mutation != self.mutation || generation != current {
		closed := self.closed
		self.stateLock.Unlock()
		if !closed {
			self.Refresh()
		}
		return
	}
	self.state.Loading = false
	self.state.Refreshing = false
	self.state.Error = requestError
	if err == nil {
		self.state.Sessions = result.Sessions
		self.state.CurrentSessionId = result.CurrentSessionId
		self.state.LegacyCoverage = result.LegacyCoverage
		self.state.Generation = result.Generation
		self.state.EventId = result.EventId
		self.state.Loaded = true
		self.state.Supported = true
	} else if self.state.Error.Unsupported {
		self.state.Supported = false
	}
	self.stateLock.Unlock()
	self.publish()
}
func (self *ClientSessionViewController) RevokeSession(sessionId *Id) {
	if sessionId == nil {
		return
	}
	self.revoke(sessionId, false)
}
func (self *ClientSessionViewController) RevokeOtherSessions() { self.revoke(nil, true) }
func (self *ClientSessionViewController) revoke(sessionId *Id, bulk bool) {
	target := self.api.captureNetworkTarget()
	generation := target.generation
	tracked := target.byJwt != ""
	self.stateLock.Lock()
	if self.closed || generation != self.credentialGeneration || !tracked {
		self.stateLock.Unlock()
		self.Refresh()
		return
	}
	var action *ClientSessionAction
	if bulk {
		action = self.state.BulkAction
	} else {
		for _, value := range self.state.Actions.values {
			if value.SessionId != nil && value.SessionId.Cmp(sessionId) == 0 {
				action = value
				break
			}
		}
	}
	if action != nil && (action.Loading || action.Pending) {
		self.stateLock.Unlock()
		return
	}
	if action == nil {
		action = &ClientSessionAction{SessionId: sessionId, OperationId: newId(connect.NewId()), target: target}
		if bulk {
			self.state.BulkAction = action
		} else {
			self.state.Actions.Add(action)
		}
	}
	action.Loading = true
	action.Error = nil
	self.mutation++
	operationId := action.OperationId
	currentSession := self.state.CurrentSessionId
	self.stateLock.Unlock()
	self.publish()
	go func() {
		if self.testingAfterAction != nil {
			defer self.testingAfterAction()
		}
		if self.testingBeforeSubmit != nil {
			self.testingBeforeSubmit()
		}
		var result *SessionOperationResult
		var err error
		if bulk {
			result, err = self.api.sessionOperationFor(self.ctx, "/network/revoke-other-sessions", &RevokeOtherNetworkSessionsArgs{OperationId: operationId}, target)
		} else {
			result, err = self.api.sessionOperationFor(self.ctx, "/network/revoke-session", &RevokeNetworkSessionArgs{OperationId: operationId, SessionId: sessionId}, target)
		}
		requestError := self.clientSessionError(err, false, target.byJwt)
		current := self.api.sessionCredentialGeneration()
		self.stateLock.Lock()
		if self.closed || generation != current {
			self.stateLock.Unlock()
			return
		}
		action.Loading = false
		action.Error = requestError
		self.mutation++
		enforced := err == nil && (result.Status == "revoked" || result.Status == "already_revoked" || result.State == "enforced" || result.State == "complete")
		terminalFailure := err == nil && (result.State == "cancelled" || result.State == "failed")
		if result != nil {
			action.Status = result.Status
			action.State = result.State
		}
		if terminalFailure {
			action.Error = &ClientSessionError{Message: "Session revocation was not applied.", Retryable: false}
		}
		action.Pending = !enforced && !terminalFailure && (err == nil || action.Error.Retryable)
		if enforced {
			remaining := NewNetworkSessionInfoList()
			for _, item := range self.state.Sessions.values {
				if bulk && item.Current || !bulk && (item.SessionId == nil || item.SessionId.Cmp(sessionId) != 0) {
					remaining.Add(item)
				}
			}
			self.state.Sessions = remaining
		}
		self.stateLock.Unlock()
		self.publish()
		if enforced && !bulk && tracked && currentSession != nil && currentSession.Cmp(sessionId) == 0 {
			if current, ok := self.api.currentNetworkTarget(target); ok {
				// this session was signed out here: no cause
				self.api.rejectNetworkCredential(current, "")
			}
			return
		}
		self.Refresh()
	}()
}
func (self *ClientSessionViewController) recoverPending() {
	self.stateLock.Lock()
	actions := append([]*ClientSessionAction{}, self.state.Actions.values...)
	if self.state.BulkAction != nil {
		actions = append(actions, self.state.BulkAction)
	}
	generation := self.credentialGeneration
	self.stateLock.Unlock()
	for _, action := range actions {
		self.stateLock.Lock()
		pending := action.Pending
		self.stateLock.Unlock()
		if !pending {
			continue
		}
		result, err := self.api.sessionOperationFor(self.ctx, "/network/session-operations/"+action.OperationId.String(), nil, action.target)
		requestError := self.clientSessionError(err, false, action.target.byJwt)
		current := self.api.sessionCredentialGeneration()
		self.stateLock.Lock()
		if self.closed || generation != current {
			self.stateLock.Unlock()
			return
		}
		action.Error = requestError
		if result != nil {
			action.Status = result.Status
			action.State = result.State
		}
		if err == nil && (result.State == "cancelled" || result.State == "failed") {
			action.Error = &ClientSessionError{Message: "Session revocation was not applied."}
		}
		var status *connect.HttpStatusError
		if errors.As(err, &status) && status.StatusCode == 404 {
			action.Pending = false
			action.Error.Retryable = true
		}
		if err == nil && (result.State == "enforced" || result.State == "complete" || result.State == "cancelled" || result.State == "failed") {
			action.Pending = false
			self.mutation++
			self.stateLock.Unlock()
			self.Refresh()
			self.publish()
		} else {
			self.stateLock.Unlock()
		}
	}
}

// API-only binding entry point; the API owns cancellation and credentials.
func (self *Api) OpenClientSessionViewController() *ClientSessionViewController {
	return NewClientSessionViewControllerWithApi(self.ctx, self)
}
