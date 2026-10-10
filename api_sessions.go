package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/urnetwork/connect"
)

type SessionLastUsed struct {
	UnixTime    int64  `json:"unix_time"`
	City        string `json:"city"`
	Region      string `json:"region"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	DeviceType  string `json:"device_type"`
	AppVersion  string `json:"app_version"`
}
type NetworkSessionInfo struct {
	SessionId       *Id              `json:"session_id"`
	Current         bool             `json:"current"`
	Kind            string           `json:"kind"`
	CreateTime      *Time            `json:"create_time"`
	LastMintTime    *Time            `json:"last_mint_time"`
	TokenExpireTime *Time            `json:"token_expire_time"`
	AcceptUntil     *Time            `json:"accept_until"`
	OriginSessionId *Id              `json:"origin_session_id"`
	LastUsed        *SessionLastUsed `json:"last_used"`
}
type NetworkSessionInfoList struct {
	exportedList[*NetworkSessionInfo]
}

func NewNetworkSessionInfoList() *NetworkSessionInfoList {
	return &NetworkSessionInfoList{exportedList: *newExportedList[*NetworkSessionInfo]()}
}

type NetworkSessionsResult struct {
	Sessions         *NetworkSessionInfoList `json:"sessions"`
	Generation       string                  `json:"generation"`
	EventId          int64                   `json:"event_id"`
	CurrentSessionId *Id                     `json:"current_session_id"`
	LegacyCoverage   string                  `json:"legacy_coverage"`
}
type SessionOperationResult struct {
	OperationId    *Id    `json:"operation_id"`
	Status         string `json:"status"`
	State          string `json:"state"`
	SessionId      *Id    `json:"session_id"`
	KeptSessionId  *Id    `json:"kept_session_id"`
	RevokedCount   int    `json:"revoked_count"`
	CleanupPending bool   `json:"cleanup_pending"`
	Generation     string `json:"generation"`
	EventId        int64  `json:"event_id"`
}
type RevokeNetworkSessionArgs struct {
	SessionId   *Id `json:"session_id"`
	OperationId *Id `json:"operation_id"`
}
type RevokeOtherNetworkSessionsArgs struct {
	OperationId *Id `json:"operation_id"`
}
type GetNetworkSessionsCallback connect.ApiCallback[*NetworkSessionsResult]
type SessionOperationCallback connect.ApiCallback[*SessionOperationResult]

type NetworkSessionsRevision struct {
	Generation string `json:"generation"`
	EventId    int64  `json:"event_id"`
}
type NetworkSessionsChangeListener interface {
	NetworkSessionsChanged(revision *NetworkSessionsRevision)
}

func (self *Api) AddNetworkSessionsChangeListener(listener NetworkSessionsChangeListener) Sub {
	id := self.networkSessionsListeners.Add(listener)
	return newSub(func() { self.networkSessionsListeners.Remove(id) })
}
func (self *Api) networkSessionsChanged(revision *NetworkSessionsRevision) {
	for _, listener := range self.networkSessionsListeners.Get() {
		value := *revision
		connect.HandleError(func() { listener.NetworkSessionsChanged(&value) })
	}
}
func (self *Api) sessionCredentialGeneration() uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.networkByJwtGeneration
}

func (self *Api) GetNetworkSessions(callback GetNetworkSessionsCallback) {
	runAsyncApiRequest[*NetworkSessionsResult](callback, func(cb connect.ApiCallback[*NetworkSessionsResult]) {
		value, err := self.networkSessions(self.ctx)
		cb.Result(value, err)
	})
}
func (self *Api) networkSessions(ctx context.Context) (*NetworkSessionsResult, error) {
	return self.networkSessionsFor(ctx, self.captureNetworkTarget())
}
func (self *Api) networkSessionsFor(ctx context.Context, target networkRenewalTarget) (*NetworkSessionsResult, error) {
	var current bool
	target, current = self.currentNetworkTarget(target)
	if !current {
		return nil, fmt.Errorf("%w: GET /network/sessions", ErrNetworkCredentialRequired)
	}
	raw, err := self.getHttpGetRaw()(ctx, self.apiUrl+"/network/sessions", target.byJwt)
	if err != nil {
		return nil, err
	}
	var result NetworkSessionsResult
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Sessions == nil {
		result.Sessions = NewNetworkSessionInfoList()
	}
	if result.Generation == "" || result.EventId < 1 {
		return nil, errApiRequestFailed
	}
	return &result, nil
}
func (self *Api) captureNetworkTarget() networkRenewalTarget {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return networkRenewalTarget{byJwt: self.networkByJwt, generation: self.networkByJwtGeneration, store: self.networkByJwtStore}
}
func (self *Api) networkTargetCurrent(target networkRenewalTarget) bool {
	_, ok := self.currentNetworkTarget(target)
	return ok
}
func (self *Api) currentNetworkTarget(target networkRenewalTarget) (networkRenewalTarget, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if target.byJwt == "" || target.generation != self.networkByJwtGeneration || target.store != self.networkByJwtStore {
		return target, false
	}
	if target.byJwt == self.networkByJwt {
		return target, true
	}
	// Renewal retains the login generation. Reuse the new token only with
	// positive account/session proof; an unrelated login always changes epoch.
	if sameTaggedSession(target.byJwt, self.networkByJwt) {
		target.byJwt = self.networkByJwt
		return target, true
	}
	return target, false
}

func (self *Api) sessionOperation(ctx context.Context, path string, args any) (*SessionOperationResult, error) {
	return self.sessionOperationFor(ctx, path, args, self.captureNetworkTarget())
}
func (self *Api) sessionOperationFor(ctx context.Context, path string, args any, target networkRenewalTarget) (*SessionOperationResult, error) {
	var current bool
	target, current = self.currentNetworkTarget(target)
	if !current {
		method := "POST"
		if args == nil {
			method = "GET"
		}
		return nil, fmt.Errorf("%w: %s %s", ErrNetworkCredentialRequired, method, path)
	}
	var raw []byte
	var err error
	if args == nil {
		raw, err = self.getHttpGetRaw()(ctx, self.apiUrl+path, target.byJwt)
	} else {
		var body []byte
		body, err = json.Marshal(args)
		if err == nil {
			raw, err = self.getHttpPostRaw()(ctx, self.apiUrl+path, body, target.byJwt)
		}
	}
	if err != nil {
		var status *connect.HttpStatusError
		if errors.As(err, &status) && status.StatusCode == 202 {
			raw = status.Body
		} else {
			return nil, err
		}
	}
	var result SessionOperationResult
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.OperationId == nil {
		return nil, errApiRequestFailed
	}
	return &result, nil
}
func (self *Api) RevokeNetworkSession(args *RevokeNetworkSessionArgs, callback SessionOperationCallback) {
	target := self.captureNetworkTarget()
	if args != nil {
		copy := *args
		if copy.OperationId == nil {
			copy.OperationId = newId(connect.NewId())
		}
		args = &copy
	}
	runAsyncApiRequest[*SessionOperationResult](callback, func(cb connect.ApiCallback[*SessionOperationResult]) {
		value, err := self.sessionOperationFor(self.ctx, "/network/revoke-session", args, target)
		cb.Result(value, err)
	})
}
func (self *Api) RevokeOtherNetworkSessions(args *RevokeOtherNetworkSessionsArgs, callback SessionOperationCallback) {
	target := self.captureNetworkTarget()
	if args == nil {
		args = &RevokeOtherNetworkSessionsArgs{}
	}
	copy := *args
	if copy.OperationId == nil {
		copy.OperationId = newId(connect.NewId())
	}
	runAsyncApiRequest[*SessionOperationResult](callback, func(cb connect.ApiCallback[*SessionOperationResult]) {
		value, err := self.sessionOperationFor(self.ctx, "/network/revoke-other-sessions", &copy, target)
		cb.Result(value, err)
	})
}
func (self *Api) GetNetworkSessionOperation(operationId *Id, callback SessionOperationCallback) {
	target := self.captureNetworkTarget()
	runAsyncApiRequest[*SessionOperationResult](callback, func(cb connect.ApiCallback[*SessionOperationResult]) {
		if operationId == nil {
			cb.Result(nil, errApiRequestFailed)
			return
		}
		value, err := self.sessionOperationFor(self.ctx, fmt.Sprintf("/network/session-operations/%s", operationId.String()), nil, target)
		cb.Result(value, err)
	})
}
