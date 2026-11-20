//go:build js && wasm && sidecar_http

// The team console's operations on the page Worker's own leg (SDK-M6-02, SDK-M10-05), in Node under
// test/sidecarwasm_harness.mjs with the fake IndexedDB: before open and on an unenrolled network directory_view,
// config_view and drop_states answer as every read does, a malformed console request answers invalid, and the views
// themselves, the root's stubs among them, build and answer through JSON on js/wasm, prose crossing only as untrusted
// text. test/sidecarwasm_ops.test.ts runs them.

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// Before open the console's operations answer unavailable, as every read does; once a network opens unenrolled they
// answer not_enrolled; and a malformed console request answers invalid either way, reaching nothing.
func TestConsoleOperationsBeforeOpenAndUnenrolled(t *testing.T) {
	worker := testPageWorker(t)
	repoId := sidecar.NewId().String()
	malformed := []string{
		`{"id":"m1","op":"directory_view","kind":"team"}`,
		`{"id":"m2","op":"directory_view","writer_sidecar_id":"` + sidecar.NewId().String() + `"}`,
		`{"id":"m3","op":"config_view","query":"members"}`,
		`{"id":"m4","op":"drop_states"}`,
		`{"id":"m5","op":"drop_states","repo_id":"` + repoId + `","confirm_path":"notes/a.txt"}`,
	}
	for _, request := range []string{`{"id":"d","op":"directory_view"}`, `{"id":"c","op":"config_view"}`, `{"id":"s","op":"drop_states","repo_id":"` + repoId + `"}`} {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeUnavailable {
			t.Errorf("before open, %s: %+v", request, reply)
		}
	}
	for _, request := range malformed {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeInvalid {
			t.Errorf("before open, %s: %+v", request, reply)
		}
	}
	testOpenNetwork(t, worker, sidecar.NewId())
	for _, request := range []string{
		`{"id":"d","op":"directory_view","kind":"run","active_only":true,"query":"settings"}`,
		`{"id":"g","op":"directory_view","announcement_id":"` + sidecar.NewId().String() + `"}`,
		`{"id":"c","op":"config_view"}`,
		`{"id":"s","op":"drop_states","repo_id":"` + repoId + `","ref":"release"}`,
	} {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeNotEnrolled {
			t.Errorf("unenrolled, %s: %+v", request, reply)
		}
	}
	for _, request := range malformed {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeInvalid {
			t.Errorf("unenrolled, %s: %+v", request, reply)
		}
	}
}

// A WorkerDirectory over items a test made.
type testConsoleDirectory struct {
	directoryId sidecar.Id
	items       []*sidecar.DirectoryItem
	syncs       int
}

func (self *testConsoleDirectory) DirectoryId() (sidecar.Id, bool) {
	return self.directoryId, true
}

func (self *testConsoleDirectory) Sync(ctx context.Context) error {
	self.syncs += 1
	return nil
}

func (self *testConsoleDirectory) List(filter *sidecar.DirectoryFilter) ([]*sidecar.DirectoryItem, error) {
	return self.items, nil
}

func (self *testConsoleDirectory) Get(announcementId sidecar.Id, writerSidecarId *sidecar.Id) (*sidecar.DirectoryItem, error) {
	for _, item := range self.items {
		if item.Key.AnnouncementId == announcementId {
			return item, nil
		}
	}
	return nil, sidecar.ErrDirectoryItemNone
}

// On this leg the views build as natively: a member's run crosses with its identifiers trusted and its title as
// untrusted text, the root's stub splitting them, ids as strings and the version decimal, and config_view with no
// verified state answers unavailable.
func TestConsoleViewsAnswerOnThisLeg(t *testing.T) {
	writerId, memberId, announcementId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	directory := &testConsoleDirectory{directoryId: sidecar.NewId(), items: []*sidecar.DirectoryItem{{
		Key: sidecar.DirectoryItemKey{Kind: sidecar.DirectoryAnnouncementKindRun, WriterSidecarId: writerId, AnnouncementId: announcementId},
		Announcement: &protocol.Announcement{
			AnnouncementId: announcementId.Bytes(),
			Kind:           uint32(sidecar.DirectoryAnnouncementKindRun),
			Version:        7,
			State:          uint32(sidecar.DirectoryAnnouncementStateActive),
			Who:            &protocol.AnnounceWho{SidecarId: writerId.Bytes(), MemberId: memberId.Bytes(), AgentLabel: "builder", Harness: "codex"},
			What:           &protocol.AnnounceWhat{Title: "a wasm leg title", RunId: sidecar.NewId().Bytes()},
			Where:          &protocol.AnnounceWhere{Branch: "main"},
		},
		Display:         sidecar.DirectoryItemActive,
		SignerSidecarId: writerId,
		MemberId:        &memberId,
		MemberDevice:    true,
	}}}
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar: pageUnopenedSidecar{},
		Repo: func(sidecar.Id) (sidecarworker.WorkerRepo, error) {
			return nil, sidecar.ErrSidecarClosed
		},
		Records: func() (sidecarworker.WorkerRecords, error) {
			return nil, sidecar.ErrSidecarClosed
		},
		Directory: func() (sidecarworker.WorkerDirectory, error) {
			return directory, nil
		},
		Config: func() (*sidecarworker.WorkerConfigState, error) {
			return &sidecarworker.WorkerConfigState{}, nil
		},
		Yield: yieldToEventLoop,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := worker.HandleJson(context.Background(), []byte(`{"id":"view","op":"directory_view"}`))
	reply := &sidecarworker.Reply{}
	if err := json.Unmarshal(encoded, reply); err != nil {
		t.Fatal(err)
	}
	if !reply.Ok || reply.Id != "view" || reply.Directory == nil || len(reply.Directory.Items) != 1 || directory.syncs != 1 {
		t.Fatalf("the view: %s", encoded)
	}
	item := reply.Directory.Items[0]
	if item.AnnouncementId != announcementId.String() || item.Version != "7" || item.MemberId != memberId.String() || item.IdentifierTrust != "member" ||
		item.Identifiers["who.agent_label"] != "builder" || item.Identifiers["where.branch"] != "main" || item.UntrustedText["what.title"] != "a wasm leg title" {
		t.Errorf("the item: %+v", item)
	}
	if strings.Count(string(encoded), "a wasm leg title") != 1 {
		t.Errorf("the title crosses more than once: %s", encoded)
	}
	config := &sidecarworker.Reply{}
	if err := json.Unmarshal(worker.HandleJson(context.Background(), []byte(`{"id":"config","op":"config_view"}`)), config); err != nil {
		t.Fatal(err)
	}
	if config.Ok || config.Code != sidecarworker.CodeUnavailable {
		t.Errorf("config_view with no verified state: %+v", config)
	}
}
