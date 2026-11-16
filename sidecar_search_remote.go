//go:build !ios_extension && !js

// A remote-mode Sidecar's search (§18.7's control row, §5.8, §16.10's wire row; SDK-M10-01): Search, GetStatus and
// SetPolicy forward to `ur daemon`'s control mode, so the desktop app uses the daemon's cache and policy. It never asks
// an indexer, its daemon holding all, and asks for untrusted hits' text, since a person reads them (§5.8: a control-mode
// search returns untrusted text only when the call asks for it).
//
//	search         the control method search, with §16.10's search tool's arguments (query, scope, repos, filters'
//	               since and until, latest, more, untrusted, fuzzy, limit, wait_ms, cursor) and the paired network;
//	               each call answers by wait_ms with what landed, and the next, naming the reply's next_cursor,
//	               answers what landed meanwhile, so one search is a stream of calls until a reply names no cursor.
//	               A search keeps a control session of its own, since a cursor is the session's, and a waiting call
//	               holds no other call of the app's behind it
//	search.status  a repo's status, or with no repo every repo's together, as §18.5's SidecarSearchStatus
//	search.policy  this device's policy for all, for a repo or every repo; "" restores the daemon's default
//
// The daemon's methods are SEARCH-M10-29's, not on sidecar yet: their names beyond search, and the reply's shape, are
// this file's reading of §16.10's MCP result (hits with repo_id, version, time, match, by, correction, partition,
// fields and snippet; status with complete and next_cursor), which that task's method is to match. A daemon without
// them answers method not found, which reads SEARCH_UNAVAILABLE.
//
// Only the fields named here are read, and a reply's text reaches no message: each refusal answers its code's fixed
// text (I-13, I-19). A remote-mode Sidecar reports no conditions, foreground or background window: its daemon
// reports none and runs its own (§18.7), so those calls do nothing or answer SEARCH_REFUSED, and DropCache, whose
// daemon's cache is `ur search sync --evict`'s, answers SEARCH_REFUSED.

package sdk

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
)

// The control methods a remote search calls (the file's comment).
const (
	sidecarControlMethodSearch       = "search"
	sidecarControlMethodSearchStatus = "search.status"
	sidecarControlMethodSearchPolicy = "search.policy"
)

// §16.10's untrusted argument that returns untrusted text, for a person to read.
const sidecarControlSearchUntrustedInclude = "include"

// §16.10's breadths a remote search names.
const (
	sidecarControlSearchScopeRepo    = "repo"
	sidecarControlSearchScopeNetwork = "network"
)

// §16.10's repo argument for a repo named by its id.
const sidecarControlSearchRepoIdPrefix = "id:"

// §16.10's hit match and confirmation, and partitions.
const (
	sidecarControlSearchMatchConfirmed = "confirmed"
	sidecarControlSearchMatchPossible  = "possible"
	sidecarControlSearchMatchNear      = "near"
	sidecarControlSearchByText         = "text"
	sidecarControlSearchPartitionAll   = "all"
)

// §16.10's display fields a hit's fields name.
const (
	sidecarControlSearchFieldPath    = "path"
	sidecarControlSearchFieldTitle   = "title"
	sidecarControlSearchFieldSubject = "subject"
	sidecarControlSearchFieldFrom    = "from"
	sidecarControlSearchFieldAgent   = "agent_label"
)

// JSON-RPC's code for params a method does not take.
const sidecarControlCodeInvalidParams = -32602

// A call a remote-mode Sidecar does not forward.
var errSidecarSearchRemoteRefused = errors.New("a remote-mode sidecar's daemon runs its own search windows and cache")

// search's params: §16.10's search tool's arguments, as far as an app asks them.
type sidecarControlSearchParams struct {
	Network   string                       `json:"network,omitempty"`
	Query     string                       `json:"query,omitempty"`
	Scope     string                       `json:"scope,omitempty"`
	Repos     []string                     `json:"repos,omitempty"`
	Filters   *sidecarControlSearchFilters `json:"filters,omitempty"`
	Latest    bool                         `json:"latest,omitempty"`
	More      bool                         `json:"more,omitempty"`
	Untrusted string                       `json:"untrusted,omitempty"`
	Fuzzy     bool                         `json:"fuzzy,omitempty"`
	Limit     int                          `json:"limit,omitempty"`
	WaitMs    int64                        `json:"wait_ms,omitempty"`
	Cursor    string                       `json:"cursor,omitempty"`
}

// search's filters an app names: RFC 3339 times.
type sidecarControlSearchFilters struct {
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
}

// search's reply, as far as this file reads it.
type sidecarControlSearchReply struct {
	Hits   []*sidecarControlSearchHit  `json:"hits"`
	Status *sidecarControlSearchStatus `json:"status"`
}

// One hit of search's reply.
type sidecarControlSearchHit struct {
	RepoId     *Id               `json:"repo_id"`
	Version    uint64            `json:"version"`
	Time       string            `json:"time"`
	Match      string            `json:"match"`
	By         string            `json:"by"`
	Correction string            `json:"correction,omitempty"`
	Partition  string            `json:"partition"`
	Fields     map[string]string `json:"fields,omitempty"`
	Snippet    string            `json:"snippet,omitempty"`
}

// search's status.
type sidecarControlSearchStatus struct {
	Complete   bool   `json:"complete"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// search.status's params.
type sidecarControlSearchStatusParams struct {
	Network string `json:"network,omitempty"`
	RepoId  *Id    `json:"repo_id,omitempty"`
}

// search.policy's params.
type sidecarControlSearchPolicyParams struct {
	Network string `json:"network,omitempty"`
	RepoId  *Id    `json:"repo_id,omitempty"`
	Policy  string `json:"policy"`
}

// sidecarSearchBackend over a remote backend.
type sidecarSearchRemoteBackend struct {
	remote   sidecarBackend
	settings *sidecarSearchSettings
}

// The network the pairing bound, as a call's network argument.
func (self *sidecarSearchRemoteBackend) network() string {
	if named, ok := self.remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// A control session of the search's own, nil where the backend opens none, whose calls then take the shared one.
func (self *sidecarSearchRemoteBackend) openSession(ctx context.Context) (sidecarControlChannel, error) {
	remote, ok := self.remote.(*sidecarRemoteBackend)
	if !ok {
		return nil, nil
	}
	if remote.ctx.Err() != nil {
		return nil, errSidecarRemoteClosed
	}
	channel, err := remote.dial(ctx)
	if err != nil {
		return nil, errors.Join(errSidecarRemoteOffline, err)
	}
	return channel, nil
}

// See sidecarSearchBackend. The first call goes at the first pull, so a search that asks nothing more of the daemon
// costs one call.
func (self *sidecarSearchRemoteBackend) search(ctx context.Context, request *sidecarSearchRequest) (sidecarSearchSource, error) {
	params := &sidecarControlSearchParams{
		Network:   self.network(),
		Query:     request.text,
		Scope:     sidecarControlSearchScopeNetwork,
		Untrusted: sidecarControlSearchUntrustedInclude,
		Fuzzy:     request.fuzzy,
	}
	if 0 < len(request.repoIds) {
		params.Scope = sidecarControlSearchScopeRepo
		for _, repoId := range request.repoIds {
			params.Repos = append(params.Repos, sidecarControlSearchRepoIdPrefix+repoId.String())
		}
	}
	switch request.scope {
	case SidecarSearchScopeLatest:
		params.Latest = true
	case SidecarSearchScopeAll:
		params.More = true
	}
	if !request.since.IsZero() || !request.until.IsZero() {
		params.Filters = &sidecarControlSearchFilters{}
		if !request.since.IsZero() {
			params.Filters.Since = request.since.UTC().Format(time.RFC3339Nano)
		}
		if !request.until.IsZero() {
			params.Filters.Until = request.until.UTC().Format(time.RFC3339Nano)
		}
	}
	return &sidecarSearchRemoteSource{backend: self, params: params, pageSize: request.pageSize}, nil
}

// See sidecarSearchBackend.
func (self *sidecarSearchRemoteBackend) status(ctx context.Context, repoId *sidecar.Id) (*SidecarSearchStatus, error) {
	params := &sidecarControlSearchStatusParams{Network: self.network()}
	if repoId != nil {
		params.RepoId = newId([16]byte(*repoId))
	}
	reply := &SidecarSearchStatus{}
	if err := self.remote.call(ctx, sidecarControlMethodSearchStatus, params, false, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// See sidecarSearchBackend: the daemon's cache is the daemon's.
func (self *sidecarSearchRemoteBackend) dropCache(ctx context.Context, repoId *sidecar.Id) error {
	return errSidecarSearchRemoteRefused
}

// See sidecarSearchBackend.
func (self *sidecarSearchRemoteBackend) setPolicy(ctx context.Context, repoId *sidecar.Id, policy string) error {
	params := &sidecarControlSearchPolicyParams{Network: self.network(), Policy: policy}
	if repoId != nil {
		params.RepoId = newId([16]byte(*repoId))
	}
	return self.remote.call(ctx, sidecarControlMethodSearchPolicy, params, false, nil)
}

// See sidecarSearchBackend: the daemon reports none (§18.7).
func (self *sidecarSearchRemoteBackend) setConditions(unmetered bool, charging bool, lowPower bool) {
}

// See sidecarSearchBackend: daemon hosts never report one (§18.7).
func (self *sidecarSearchRemoteBackend) setForeground(foreground bool) {
}

// See sidecarSearchBackend: the daemon runs its own background.
func (self *sidecarSearchRemoteBackend) runBackgroundSync(ctx context.Context) error {
	return errSidecarSearchRemoteRefused
}

// A remote search's pages: one control call a pull, on the search's own session.
type sidecarSearchRemoteSource struct {
	backend  *sidecarSearchRemoteBackend
	params   *sidecarControlSearchParams
	pageSize int
	// the session, opened at the first pull; nil while the shared one serves
	session sidecarControlChannel
	opened  bool
	// the cursor the next call names; empty before the first reply
	cursor string
}

// See sidecarSearchSource.
func (self *sidecarSearchRemoteSource) next(ctx context.Context) ([]*SidecarSearchHit, *SidecarSearchProgress, bool, error) {
	settings := self.backend.settings
	if !self.opened {
		session, err := self.backend.openSession(ctx)
		if err != nil {
			return nil, nil, false, err
		}
		self.session, self.opened = session, true
	}
	params := self.params
	if self.cursor != "" {
		// a cursor goes on from where its search stands: its query stays in the daemon's session
		params = &sidecarControlSearchParams{
			Network:   self.params.Network,
			Untrusted: self.params.Untrusted,
			Cursor:    self.cursor,
		}
	}
	params.Limit = max(1, min(self.pageSize, settings.RemoteLimitMaxCount))
	params.WaitMs = settings.RemoteWaitTimeout.Milliseconds()
	reply := &sidecarControlSearchReply{}
	callCtx, cancel := context.WithTimeout(ctx, settings.RemoteWaitTimeout+settings.RemoteCallTimeout)
	defer cancel()
	var err error
	if self.session != nil {
		err = self.session.call(callCtx, sidecarControlMethodSearch, params, false, reply)
	} else {
		err = self.backend.remote.call(callCtx, sidecarControlMethodSearch, params, false, reply)
	}
	if err != nil {
		var refusal *sidecarControlError
		if !errors.As(err, &refusal) && self.session != nil {
			// the session is gone, and its cursor with it
			self.session.close()
			self.session = nil
			err = errors.Join(errSidecarRemoteOffline, err)
		}
		return nil, nil, false, err
	}
	hits := make([]*SidecarSearchHit, 0, len(reply.Hits))
	for _, hit := range reply.Hits {
		if bound := sidecarSearchRemoteHitOf(hit); bound != nil {
			hits = append(hits, bound)
		}
	}
	done := reply.Status == nil || reply.Status.NextCursor == ""
	if !done {
		self.cursor = reply.Status.NextCursor
	}
	return hits, &SidecarSearchProgress{}, done, nil
}

// See sidecarSearchSource: the session closes, and with it the daemon's cursor. Nothing else is sent.
func (self *sidecarSearchRemoteSource) close() {
	if self.session != nil {
		self.session.close()
		self.session = nil
	}
}

// The bound hit of one of search's reply, nil for one that names no repo.
func sidecarSearchRemoteHitOf(hit *sidecarControlSearchHit) *SidecarSearchHit {
	if hit == nil || hit.RepoId == nil {
		return nil
	}
	bound := &SidecarSearchHit{
		RepoId:     newId(hit.RepoId.id),
		Path:       hit.Fields[sidecarControlSearchFieldPath],
		Version:    int64(hit.Version),
		Snippet:    hit.Snippet,
		Correction: hit.Correction,
		Partition:  SidecarSearchPartitionLatest,
	}
	if parsed, err := time.Parse(time.RFC3339Nano, hit.Time); err == nil {
		bound.TimeMillis = parsed.UnixMilli()
	}
	if hit.Partition == sidecarControlSearchPartitionAll {
		bound.Partition = SidecarSearchPartitionAll
	}
	switch hit.Match {
	case sidecarControlSearchMatchConfirmed:
		// confirmed by the device's own plaintext is §18.5's verified; by a doc-table field, confirmed
		if hit.By == sidecarControlSearchByText {
			bound.State = SidecarSearchHitVerified
		} else {
			bound.State = SidecarSearchHitConfirmed
		}
	case sidecarControlSearchMatchNear:
		bound.State = SidecarSearchHitNear
	default:
		bound.State = SidecarSearchHitPossible
	}
	bound.Title = hit.Fields[sidecarControlSearchFieldSubject]
	if bound.Title == "" {
		bound.Title = hit.Fields[sidecarControlSearchFieldTitle]
	}
	bound.From = hit.Fields[sidecarControlSearchFieldFrom]
	if bound.From == "" {
		bound.From = hit.Fields[sidecarControlSearchFieldAgent]
	}
	return bound
}

// The code a daemon's refusal stands for, by its JSON-RPC code and, for a handler's failure, the core's error it
// names. Its text goes nowhere else.
func sidecarSearchControlCodeOf(refusal *sidecarControlError) string {
	switch refusal.Code {
	case sidecarControlCodeMethodNotFound:
		return SidecarSearchCodeUnavailable
	case sidecarControlCodeInvalidParams:
		return SidecarSearchCodeInvalid
	case int(sidecar.DaemonRpcTimeout):
		return SidecarSearchCodeBusy
	case int(sidecar.DaemonRpcFailedPrecondition):
		return SidecarSearchCodeUnavailable
	}
	for _, named := range []struct {
		err  error
		code string
	}{
		{sidecar.ErrSearchBusy, SidecarSearchCodeBusy},
		{sidecar.ErrSearchCursorExpired, SidecarSearchCodeExpired},
		{sidecar.ErrSearchCursorInvalid, SidecarSearchCodeInvalid},
		{sidecar.ErrSidecarSearchUnavailable, SidecarSearchCodeUnavailable},
		{sidecar.ErrSidecarSearchRefused, SidecarSearchCodeRefused},
		{sidecar.ErrSearchRequest, SidecarSearchCodeInvalid},
		{sidecar.ErrSearchQueryTooLong, SidecarSearchCodeInvalid},
		{sidecar.ErrSearchStateArgs, SidecarSearchCodeInvalid},
	} {
		if strings.Contains(refusal.Message, named.err.Error()) {
			return named.code
		}
	}
	return SidecarSearchCodeFailed
}
