//go:build sidecar_http || !js

// What a page Worker answers (§18.8's messages row): a Reply with a view, or a refusal's code and that code's fixed
// message. Every view is this package's own struct, filled field by field from what the root verified, so nothing a
// root struct holds beside them, a record's salt and Signed bytes, a token, a key, reaches a page by being passed
// through. An error's text never reaches a reply either: a reply carries its code alone, mapped from the error's
// class, so nothing an error wraps, a path, an url or a value, can leak through it (I-7, I-13).
//
// Ids are uuid strings, versions decimal strings, times milliseconds since the epoch, names text and, when their bytes
// are not UTF-8, base64 as well, and contents base64.

package sidecarworker

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// A refusal's code, which a page shows by.
const (
	// The request does not parse or its fields do not fit its operation.
	CodeInvalid = "invalid"
	// No such operation.
	CodeUnknownOperation = "unknown_operation"
	// Not on this Worker, or not reachable now: enroll and admission_status, and a service that refused for now.
	CodeUnavailable = "unavailable"
	// This Worker holds no enrolled identity.
	CodeNotEnrolled = "not_enrolled"
	// The keychain is not open.
	CodeLocked = "locked"
	// No such repo, path or entry.
	CodeNotFound = "not_found"
	// The server or the log refused this sidecar the read.
	CodeRefused = "refused"
	// What came back did not verify: a head below its pin or forking from it, a broken chain, a bad seal or
	// statement. The page shows nothing of it and says so.
	CodeUnverified = "unverified"
	// A construct of a format this Worker does not read.
	CodeUnsupported = "unsupported"
	// The request or the content is over its bound.
	CodeTooLarge = "too_large"
	// This Worker signed out: nothing more is served.
	CodeSignedOut = "signed_out"
	// The operation was cancelled.
	CodeCancelled = "cancelled"
	// Anything else.
	CodeFailed = "failed"
)

// Each code's message, the only text a refusal carries.
var codeMessages = map[string]string{
	CodeInvalid:          "the request is not one this Worker takes",
	CodeUnknownOperation: "no such operation",
	CodeUnavailable:      "not available now",
	CodeNotEnrolled:      "this browser is not enrolled in the network",
	CodeLocked:           "this browser's keychain is not open",
	CodeNotFound:         "not found",
	CodeRefused:          "the network refused this read",
	CodeUnverified:       "what the server answered did not verify, so nothing of it is shown",
	CodeUnsupported:      "this is stored in a format this browser does not read yet",
	CodeTooLarge:         "too large for a browser view",
	CodeSignedOut:        "this browser signed out of the network",
	CodeCancelled:        "cancelled",
	CodeFailed:           "the operation failed",
}

// Content too large for a view, which ends a read.
var errWorkerTooLarge = errors.New("content over the view's bound")

// One request's answer.
type Reply struct {
	// The request's correlation id; empty only for a request whose id could not be read.
	Id      string `json:"id"`
	Ok      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// One view, as the operation gives.
	Listing *ListingView `json:"listing,omitempty"`
	File    *FileView    `json:"file,omitempty"`
	Record  *RecordView  `json:"record,omitempty"`
	SignOut *SignOutView `json:"sign_out,omitempty"`
}

// The reply's JSON. A view marshals by construction, so this never fails; if it did, the page gets a refusal.
func (self *Reply) json() []byte {
	encoded, err := json.Marshal(self)
	if err != nil {
		encoded, _ = json.Marshal(refusalReply(self.Id, CodeFailed))
	}
	return encoded
}

// A refusal with code under id.
func refusalReply(id string, code string) *Reply {
	return &Reply{Id: id, Code: code, Message: codeMessages[code]}
}

// The refusal err stands for, by its class alone.
func errorReply(err error) *Reply {
	return refusalReply("", codeOf(err))
}

// err's code. Its text goes nowhere.
func codeOf(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeCancelled
	case errors.Is(err, errWorkerTooLarge):
		return CodeTooLarge
	case errors.Is(err, sidecar.ErrSidecarNoIdentity):
		return CodeNotEnrolled
	case errors.Is(err, sidecar.ErrSidecarStoreLocked):
		return CodeLocked
	case errors.Is(err, sidecar.ErrEvsPathAbsent), errors.Is(err, sidecar.ErrSidecarStoreAbsent):
		return CodeNotFound
	case errors.Is(err, sidecar.ErrEvsUnauthorized), errors.Is(err, sidecar.ErrRecordNotReader):
		return CodeRefused
	case errors.Is(err, sidecar.ErrEvsPinLower), errors.Is(err, sidecar.ErrEvsPinFork), errors.Is(err, sidecar.ErrEvsChainBroken),
		errors.Is(err, sidecar.ErrEvsHeadForked), errors.Is(err, sidecar.ErrEvsStatementRefused), errors.Is(err, sidecar.ErrEvsEntryRefused),
		errors.Is(err, sidecar.ErrEvsSealInvalid), errors.Is(err, sidecar.ErrEvsVersionRule), errors.Is(err, sidecar.ErrEvsReplyInvalid):
		return CodeUnverified
	case errors.Is(err, sidecar.ErrEvsFormatUnsupported):
		return CodeUnsupported
	case errors.Is(err, sidecar.ErrGkmUnavailable), errors.Is(err, sidecar.ErrEvsNoConfig), errors.Is(err, sidecar.ErrRecordStreamsUnavailable),
		errors.Is(err, sidecar.ErrEvsReceiptGate), errors.Is(err, sidecar.ErrSidecarClosed):
		return CodeUnavailable
	}
	// evs's answer by its status: its code and paths stay behind
	var evsError *sidecar.EvsError
	if errors.As(err, &evsError) {
		switch evsError.Status {
		case http.StatusNotFound:
			return CodeNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			return CodeRefused
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			return CodeUnavailable
		case http.StatusRequestEntityTooLarge:
			return CodeTooLarge
		}
	}
	return CodeFailed
}

// A name or path as a page shows it: its text, with each byte that is not UTF-8 as U+FFFD, and then its bytes as
// base64 too, which a request names it back by (path_base64).
type NameView struct {
	Text   string `json:"text"`
	Base64 string `json:"base64,omitempty"`
}

// name's view.
func nameViewOf(name []byte) NameView {
	if utf8.Valid(name) {
		return NameView{Text: string(name)}
	}
	return NameView{Text: strings.ToValidUTF8(string(name), "�"), Base64: base64.StdEncoding.EncodeToString(name)}
}

// A directory's children, as list answers them.
type ListingView struct {
	RepoId      string              `json:"repo_id"`
	RefId       string              `json:"ref_id"`
	Path        NameView            `json:"path"`
	HeadVersion string              `json:"head_version"`
	Entries     []*ListingEntryView `json:"entries"`
	// More children than ListMaxEntryCount: the rest are not shown.
	Truncated bool `json:"truncated,omitempty"`
}

// One child.
type ListingEntryView struct {
	Name NameView `json:"name"`
	// file, directory or unknown.
	Kind string `json:"kind"`
}

// A kind's name in a view.
func evsEntryKindName(kind protocol.EvsEntryKind) string {
	switch kind {
	case protocol.EvsEntryKind_EVS_ENTRY_FILE:
		return "file"
	case protocol.EvsEntryKind_EVS_ENTRY_DIRECTORY:
		return "directory"
	}
	return "unknown"
}

// A file version as read_view answers it: what its push signed, and its content.
type FileView struct {
	RepoId        string   `json:"repo_id"`
	RefId         string   `json:"ref_id"`
	Path          NameView `json:"path"`
	Version       string   `json:"version"`
	CommitVersion string   `json:"commit_version"`
	HeadVersion   string   `json:"head_version"`
	PushId        string   `json:"push_id"`
	// The push's signer, its client then, and its signed time.
	SignerSidecarId string `json:"signer_sidecar_id"`
	ClientId        string `json:"client_id"`
	TimeMillis      int64  `json:"time_millis"`
	Kind            string `json:"kind"`
	Mode            uint32 `json:"mode"`
	// The plaintext size the entry sealed, decimal.
	Size          string `json:"size"`
	ContentBase64 string `json:"content_base64"`
	// The path's versions, newest first, with history.
	Versions []*VersionView `json:"versions,omitempty"`
}

// info's view, with content.
func fileViewOf(repoId sidecar.Id, info *sidecar.EvsFileInfo, content []byte) *FileView {
	return &FileView{
		RepoId:          repoId.String(),
		RefId:           info.RefId.String(),
		Path:            nameViewOf([]byte(info.Path)),
		Version:         strconv.FormatUint(info.Version, 10),
		CommitVersion:   strconv.FormatUint(info.CommitVersion, 10),
		HeadVersion:     strconv.FormatUint(info.HeadVersion, 10),
		PushId:          info.PushId.String(),
		SignerSidecarId: info.SignerSidecarId.String(),
		ClientId:        info.ClientId.String(),
		TimeMillis:      info.Time.UnixMilli(),
		Kind:            evsEntryKindName(info.Kind),
		Mode:            info.Mode,
		Size:            strconv.FormatUint(info.Size, 10),
		ContentBase64:   base64.StdEncoding.EncodeToString(content),
	}
}

// One version of a path (§11.7's history row).
type VersionView struct {
	Version         string `json:"version"`
	CommitVersion   string `json:"commit_version"`
	PushId          string `json:"push_id"`
	SignerSidecarId string `json:"signer_sidecar_id"`
	ClientId        string `json:"client_id"`
	TimeMillis      int64  `json:"time_millis"`
	// The removal that ended the path here.
	Removed bool `json:"removed,omitempty"`
	// kept, collected, drop_pending or dropped (§11.9).
	State string `json:"state"`
}

// version's view.
func versionViewOf(version *sidecar.EvsHistoryVersion) *VersionView {
	return &VersionView{
		Version:         strconv.FormatUint(version.Version, 10),
		CommitVersion:   strconv.FormatUint(version.CommitVersion, 10),
		PushId:          version.PushId.String(),
		SignerSidecarId: version.SignerSidecarId.String(),
		ClientId:        version.ClientId.String(),
		TimeMillis:      version.Time.UnixMilli(),
		Removed:         version.Removed,
		State:           version.State,
	}
}

// A page of a record log's verified entries, in the reader's order.
type RecordView struct {
	LogId string `json:"log_id"`
	// How many entries the reader holds for the log.
	EntryCount int                `json:"entry_count"`
	Entries    []*RecordEntryView `json:"entries"`
	// The index of the next page's first entry, as record_view's after takes it; empty when this page is the last.
	Next string `json:"next,omitempty"`
}

// One entry (RECORD.md section 5.6's rows): its header's fields, its state and its opened body.
type RecordEntryView struct {
	EntryId         string `json:"entry_id"`
	WriterSidecarId string `json:"writer_sidecar_id"`
	Seq             string `json:"seq"`
	// The registry's name, or "kind <n>" for one it does not hold, shown generically (section 6.1).
	Kind       string `json:"kind"`
	KindNumber uint32 `json:"kind_number"`
	// observed, hook, agent, approver or unknown (section 4.6): hook means any same-user process could have written it.
	Source   string `json:"source"`
	AgentId  string `json:"agent_id,omitempty"`
	RunId    string `json:"run_id,omitempty"`
	ActionId string `json:"action_id,omitempty"`
	// The writer's own time, and the push's signed time, which evs bounds.
	TimeMillis     int64 `json:"time_millis"`
	PushTimeMillis int64 `json:"push_time_millis"`
	// The writer's time is more than the skew allowed past the push's.
	Skewed bool `json:"skewed,omitempty"`
	// Untrusted text (§16.6, I-9): the page shows it labeled as such.
	Flagged bool `json:"flagged,omitempty"`
	// live, erased, expired, archived, forked or lost (section 5.5).
	State string `json:"state"`
	// The body did not open, did not commit or broke its kind's caps: shown as invalid, the header standing.
	BodyInvalid bool `json:"body_invalid,omitempty"`
	// The body as stored, the kind's one message, when the reader holds it opened.
	BodyBase64 string `json:"body_base64,omitempty"`
	CopyCount  int    `json:"copy_count"`
}

// A record source's name.
func recordSourceName(source uint32) string {
	switch sidecar.RecordSource(source) {
	case sidecar.RecordSourceObserved:
		return "observed"
	case sidecar.RecordSourceHook:
		return "hook"
	case sidecar.RecordSourceAgent:
		return "agent"
	case sidecar.RecordSourceApprover:
		return "approver"
	}
	return "unknown"
}

// An optional 16-byte id as a view's string, empty when absent or of another length.
func optionalIdString(idBytes []byte) string {
	id, err := sidecar.IdFromBytes(idBytes)
	if err != nil {
		return ""
	}
	return id.String()
}

// entry's view. Its salt and Signed bytes stay in the reader.
func recordEntryViewOf(entry *sidecar.RecordReadEntry) *RecordEntryView {
	view := &RecordEntryView{
		EntryId:        entry.EntryId.String(),
		State:          string(entry.State),
		Flagged:        entry.Flagged,
		BodyInvalid:    entry.BodyInvalid,
		PushTimeMillis: entry.PushTimeMillis,
		Skewed:         entry.Skewed,
		CopyCount:      len(entry.Copies),
	}
	if entry.Header != nil && entry.Header.Header != nil {
		header := entry.Header.Header
		view.WriterSidecarId = optionalIdString(header.GetWriterSidecarId())
		view.Seq = strconv.FormatUint(header.GetSeq(), 10)
		view.Kind = sidecar.RecordKind(header.GetKind()).String()
		view.KindNumber = header.GetKind()
		view.Source = recordSourceName(header.GetSource())
		view.AgentId = optionalIdString(header.GetAgentId())
		view.RunId = optionalIdString(header.GetRunId())
		if actionId := header.GetActionId(); 0 < len(actionId) {
			view.ActionId = hex.EncodeToString(actionId)
		}
		view.TimeMillis = header.GetTimeMillis()
	}
	if entry.Body != nil && !entry.BodyInvalid {
		view.BodyBase64 = base64.StdEncoding.EncodeToString(entry.Body)
	}
	return view
}

// What sign_out did.
type SignOutView struct {
	// The sidecar that signed out, empty when none was enrolled.
	SidecarId string `json:"sidecar_id,omitempty"`
}
