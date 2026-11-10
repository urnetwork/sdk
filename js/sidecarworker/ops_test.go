//go:build !sidecar_http

package sidecarworker

// Each operation's view (SDK-M4-04): what a page reads of a listing, a file and its history, and a record log, built
// from what the root verified, with ids and versions as strings and names that are not UTF-8 carried as base64; and
// each refusal by its code alone.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// A reply as the page reads it: through JSON.
func testReplyOf(t *testing.T, worker *Worker, request *Request) *Reply {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	reply := &Reply{}
	if err := json.Unmarshal(worker.HandleJson(context.Background(), encoded), reply); err != nil {
		t.Fatal(err)
	}
	if reply.Id != request.Id {
		t.Fatalf("the reply names %q for %q", reply.Id, request.Id)
	}
	return reply
}

// list answers each child with its kind, a name that is not UTF-8 as text and base64 both, and marks a listing over
// its bound truncated.
func TestListView(t *testing.T) {
	world := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Settings.ListMaxEntryCount = 3
	})
	refId := sidecar.NewId()
	raw := []byte{'n', 0xff, 'x'}
	world.repo.listing = &sidecar.EvsListing{RefId: refId, Path: "notes", HeadVersion: math.MaxUint64, Entries: []*sidecar.EvsListEntry{
		{Name: []byte("a.txt"), Kind: protocol.EvsEntryKind_EVS_ENTRY_FILE},
		{Name: []byte("sub"), Kind: protocol.EvsEntryKind_EVS_ENTRY_DIRECTORY},
		{Name: raw, Kind: protocol.EvsEntryKind_EVS_ENTRY_FILE},
		{Name: []byte("d.txt"), Kind: protocol.EvsEntryKind_EVS_ENTRY_FILE},
	}}
	request := world.request("list", OperationList)
	request.Path = "notes"
	reply := testReplyOf(t, world.worker, request)
	listing := reply.Listing
	if !reply.Ok || listing == nil || listing.RepoId != world.repoId.String() || listing.RefId != refId.String() || listing.Path.Text != "notes" {
		t.Fatalf("the listing: %+v", reply)
	}
	// a version past 2^53 crosses whole, as a decimal string
	if listing.HeadVersion != strconv.FormatUint(math.MaxUint64, 10) {
		t.Fatalf("the head version %s", listing.HeadVersion)
	}
	if len(listing.Entries) != 3 || !listing.Truncated {
		t.Fatalf("a listing over its bound: %d entries, truncated %v", len(listing.Entries), listing.Truncated)
	}
	if listing.Entries[0].Name.Text != "a.txt" || listing.Entries[0].Kind != "file" || listing.Entries[0].Name.Base64 != "" || listing.Entries[1].Kind != "directory" {
		t.Fatalf("the entries: %+v %+v", listing.Entries[0], listing.Entries[1])
	}
	if name := listing.Entries[2].Name; name.Text != "n�x" || name.Base64 != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("a name that is not UTF-8: %+v", name)
	}
}

// read_view answers the content and what its push signed, and with history each version; a path named by its bytes
// reads as the bytes; a file over the bound answers too_large and carries no content.
func TestReadViewWithHistory(t *testing.T) {
	world := newTestWorkerWorld(t, nil)
	pushId, signerId, clientId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	signedAt := time.UnixMilli(1_700_000_123_456)
	world.repo.info = &sidecar.EvsFileInfo{
		RefId: sidecar.NewId(), Path: "notes/a.txt", Version: 4, CommitVersion: 9, PushId: pushId, SignerSidecarId: signerId,
		ClientId: clientId, Time: signedAt, Kind: protocol.EvsEntryKind_EVS_ENTRY_FILE, Mode: 0o100644, Size: 11, HeadVersion: 12,
	}
	world.repo.content = []byte("hello world")
	world.repo.chunkCount = 3
	world.repo.history = &sidecar.EvsHistory{Versions: []*sidecar.EvsHistoryVersion{
		{Version: 4, CommitVersion: 9, PushId: pushId, SignerSidecarId: signerId, ClientId: clientId, Time: signedAt, State: "kept"},
		{Version: 3, CommitVersion: 7, Removed: true, State: "kept"},
	}}
	request := world.request("read", OperationReadView)
	request.History = true
	reply := testReplyOf(t, world.worker, request)
	file := reply.File
	if !reply.Ok || file == nil {
		t.Fatalf("the read: %+v", reply)
	}
	content, err := base64.StdEncoding.DecodeString(file.ContentBase64)
	if err != nil || string(content) != "hello world" {
		t.Fatalf("the content %q: %v", content, err)
	}
	if file.Version != "4" || file.CommitVersion != "9" || file.HeadVersion != "12" || file.PushId != pushId.String() ||
		file.SignerSidecarId != signerId.String() || file.ClientId != clientId.String() || file.TimeMillis != signedAt.UnixMilli() ||
		file.Kind != "file" || file.Mode != 0o100644 || file.Size != "11" || file.Path.Text != "notes/a.txt" {
		t.Fatalf("the file: %+v", file)
	}
	if len(file.Versions) != 2 || file.Versions[0].Version != "4" || file.Versions[0].State != "kept" || !file.Versions[1].Removed {
		t.Fatalf("the versions: %+v", file.Versions)
	}

	// without history, no versions and no History call
	world.repo.calls = nil
	if reply := testReplyOf(t, world.worker, world.request("plain", OperationReadView)); !reply.Ok || reply.File.Versions != nil {
		t.Fatalf("a read without history: %+v", reply)
	}
	for _, call := range world.repo.calls {
		if call == "History" {
			t.Fatal("a read without history asked for it")
		}
	}

	// a path named by its bytes
	byBytes := world.request("bytes", OperationReadView)
	byBytes.Path = ""
	byBytes.PathBase64 = base64.StdEncoding.EncodeToString([]byte{'a', 0xfe})
	byBytes.Version = "3"
	if reply := testReplyOf(t, world.worker, byBytes); !reply.Ok {
		t.Fatalf("a read by bytes: %+v", reply)
	}

	small := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Settings.ReadMaxByteCount = 10
	})
	small.repo.info = world.repo.info
	small.repo.content = world.repo.content
	if reply := testReplyOf(t, small.worker, small.request("big", OperationReadView)); reply.Ok || reply.Code != CodeTooLarge || reply.File != nil {
		t.Fatalf("a file over the bound: %+v", reply)
	}
}

// record_view answers a page of verified entries with their headers' fields, an untrusted entry flagged, an invalid
// body withheld with its header standing, and where the next page starts.
func TestRecordViewPages(t *testing.T) {
	world := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Settings.RecordMaxEntryCount = 2
	})
	runId := sidecar.NewId()
	first := testRecordEntry(world.logId, 1, sidecar.RecordKindChange, []byte("first body"), []byte("a salt of sixteen"), nil)
	first.Header.Header.RunId = runId.Bytes()
	first.Header.Header.ActionId = make([]byte, 32)
	second := testRecordEntry(world.logId, 2, sidecar.RecordKindNote, []byte("ignored"), nil, nil)
	second.BodyInvalid = true
	second.Flagged = true
	third := testRecordEntry(world.logId, 3, sidecar.RecordKind(9999), nil, nil, nil)
	third.State = sidecar.RecordEntryErased
	world.records.entries = []*sidecar.RecordReadEntry{first, second, third}

	reply := testReplyOf(t, world.worker, world.request("records", OperationRecordView))
	record := reply.Record
	if !reply.Ok || record == nil || record.LogId != world.logId.String() || record.EntryCount != 3 || len(record.Entries) != 2 || record.Next != "2" {
		t.Fatalf("the first page: %+v", reply)
	}
	if world.records.syncs != 1 {
		t.Fatalf("the reader synced %d times", world.records.syncs)
	}
	entry := record.Entries[0]
	body, _ := base64.StdEncoding.DecodeString(entry.BodyBase64)
	if entry.EntryId != first.EntryId.String() || entry.Kind != "change" || entry.KindNumber != uint32(sidecar.RecordKindChange) ||
		entry.Seq != "1" || entry.Source != "observed" || entry.RunId != runId.String() || entry.AgentId != "" ||
		len(entry.ActionId) != 64 || entry.State != "live" || string(body) != "first body" || entry.Flagged {
		t.Fatalf("the first entry: %+v", entry)
	}
	if invalid := record.Entries[1]; !invalid.Flagged || !invalid.BodyInvalid || invalid.BodyBase64 != "" || invalid.Kind != "note" {
		t.Fatalf("an invalid, flagged entry: %+v", invalid)
	}

	next := world.request("next", OperationRecordView)
	next.After = record.Next
	reply = testReplyOf(t, world.worker, next)
	if !reply.Ok || len(reply.Record.Entries) != 1 || reply.Record.Next != "" {
		t.Fatalf("the last page: %+v", reply)
	}
	if unknown := reply.Record.Entries[0]; unknown.Kind != "kind 9999" || unknown.State != "erased" || unknown.BodyBase64 != "" {
		t.Fatalf("an erased entry of a kind the registry does not hold: %+v", unknown)
	}
}

// Each refusal answers its code and that code's fixed message, whatever the error's text: a page learns what kind of
// refusal it was and nothing an error wrapped.
func TestErrorsAnswerTheirCodeAlone(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{err: sidecar.ErrSidecarNoIdentity, code: CodeNotEnrolled},
		{err: fmt.Errorf("open: %w", sidecar.ErrSidecarStoreLocked), code: CodeLocked},
		{err: sidecar.ErrEvsPathAbsent, code: CodeNotFound},
		{err: sidecar.ErrEvsUnauthorized, code: CodeRefused},
		{err: sidecar.ErrRecordNotReader, code: CodeRefused},
		{err: sidecar.ErrEvsPinFork, code: CodeUnverified},
		{err: sidecar.ErrEvsChainBroken, code: CodeUnverified},
		{err: sidecar.ErrEvsFormatUnsupported, code: CodeUnsupported},
		{err: sidecar.ErrGkmUnavailable, code: CodeUnavailable},
		{err: sidecar.ErrEvsNoConfig, code: CodeUnavailable},
		{err: sidecar.ErrRecordStreamsUnavailable, code: CodeUnavailable},
		{err: &sidecar.EvsError{Status: http.StatusNotFound}, code: CodeNotFound},
		{err: &sidecar.EvsError{Status: http.StatusForbidden}, code: CodeRefused},
		{err: &sidecar.EvsError{Status: http.StatusServiceUnavailable}, code: CodeUnavailable},
		{err: context.Canceled, code: CodeCancelled},
		{err: errors.New("anything else /secret/path"), code: CodeFailed},
	} {
		world := newTestWorkerWorld(t, nil)
		world.repo.err = c.err
		reply := testReplyOf(t, world.worker, world.request("list", OperationList))
		if reply.Ok || reply.Code != c.code || reply.Message != codeMessages[c.code] {
			t.Errorf("%v: %+v", c.err, reply)
		}
	}
	// a seam that cannot answer at all answers by its error too
	world := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Records = func() (WorkerRecords, error) {
			return nil, sidecar.ErrSidecarNoIdentity
		}
		args.Repo = func(sidecar.Id) (WorkerRepo, error) {
			return nil, sidecar.ErrSidecarStoreLocked
		}
	})
	if reply := testReplyOf(t, world.worker, world.request("records", OperationRecordView)); reply.Code != CodeNotEnrolled {
		t.Errorf("records before enrollment: %+v", reply)
	}
	if reply := testReplyOf(t, world.worker, world.request("list", OperationList)); reply.Code != CodeLocked {
		t.Errorf("a repo while locked: %+v", reply)
	}
}

// enroll and admission_status answer unavailable and reach no seam, while plan-SDK-key-origin-credential's credential
// is not built: no page Worker enrolls.
func TestEnrollAndAdmissionStatusUnavailable(t *testing.T) {
	world := newTestWorkerWorld(t, nil)
	enroll := world.request("enroll", OperationEnroll)
	enroll.Code = "a-one-time-code"
	for _, request := range []*Request{enroll, world.request("enroll-bare", OperationEnroll), world.request("status", OperationAdmissionStatus)} {
		if reply := testReplyOf(t, world.worker, request); reply.Ok || reply.Code != CodeUnavailable {
			t.Errorf("%s: %+v", request.Operation, reply)
		}
	}
	if len(world.repo.calls) != 0 || world.records.syncs != 0 || world.sidecar.logouts() != 0 {
		t.Fatal("an unavailable operation reached a seam")
	}
}
