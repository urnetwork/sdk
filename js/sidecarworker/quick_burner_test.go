//go:build sidecar_http || !js

package sidecarworker

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A burner's sidecar: unenrolled until its enrollment, then the state a test sets.
type testBurnerSidecar struct {
	stateLock sync.Mutex
	identity  *sidecar.SidecarIdentity
	state     sidecar.SidecarState
}

func (self *testBurnerSidecar) Identity() *sidecar.SidecarIdentity {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identity
}

func (self *testBurnerSidecar) Status() sidecar.SidecarStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return sidecar.SidecarStatus{State: self.state}
}

func (self *testBurnerSidecar) Logout(ctx context.Context) error {
	return sidecar.ErrSidecarNoIdentity
}

func (self *testBurnerSidecar) set(identity *sidecar.SidecarIdentity, state sidecar.SidecarState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.identity, self.state = identity, state
}

// A burner over a test sidecar whose enrollment answers closeCapability, and what reached the document.
type testBurner struct {
	burner  *QuickBurner
	sidecar *testBurnerSidecar
	closes  []string
	shared  *sidecar.Id
}

func newTestBurner(t *testing.T, closeCapability string, enroll bool) *testBurner {
	t.Helper()
	self := &testBurner{sidecar: &testBurnerSidecar{state: sidecar.SidecarStateUnenrolled}}
	args := &QuickBurnerArgs{
		HandClose: func(capability string) {
			self.closes = append(self.closes, capability)
		},
		SharedMailbox: func() (sidecar.Id, bool) {
			if self.shared == nil {
				return sidecar.Id{}, false
			}
			return *self.shared, true
		},
	}
	if enroll {
		args.Enroll = func(ctx context.Context, code string) (string, error) {
			if code != "synthetic-one-time-code" {
				return "", errors.New("synthetic refusal")
			}
			self.sidecar.set(&sidecar.SidecarIdentity{NetworkId: sidecar.NewId(), SidecarId: sidecar.NewId(), Generation: 1}, sidecar.SidecarStateOnline)
			return closeCapability, nil
		}
	}
	burner, err := NewQuickBurner(&WorkerArgs{
		Sidecar: self.sidecar,
		Repo:    func(repoId sidecar.Id) (WorkerRepo, error) { return nil, errors.New("no repo") },
		Records: func() (WorkerRecords, error) { return nil, errors.New("no records") },
	}, args)
	if err != nil {
		t.Fatal(err)
	}
	self.burner = burner
	return self
}

// The burner's reply to quick_burner with code, as the frame passes it.
func (self *testBurner) ask(t *testing.T, code string) ([]byte, *Reply) {
	t.Helper()
	request := `{"id":"burner-1","op":"quick_burner"`
	if code != "" {
		request += `,"code":"` + code + `"`
	}
	request += `}`
	replyJson := self.burner.HandleJson(context.Background(), []byte(request))
	reply := self.burner.Handle(context.Background(), &Request{Id: "burner-2", Operation: OperationQuickBurner})
	return replyJson, reply
}

// The mint's close capability goes to the document alone: no reply carries it, the document gets it once, and the
// burner keeps no copy.
func TestCloseNeverReachesPage(t *testing.T) {
	const closeCapability = "synthetic-close-capability-0123456789abcdef"
	burner := newTestBurner(t, closeCapability, true)
	replyJson, reply := burner.ask(t, "synthetic-one-time-code")
	if bytes.Contains(replyJson, []byte(closeCapability)) || bytes.Contains(reply.json(), []byte(closeCapability)) {
		t.Fatal("a reply to the page carries the close capability")
	}
	if len(burner.closes) != 1 || burner.closes[0] != closeCapability {
		t.Errorf("the document got %v", burner.closes)
	}
	if !reply.Ok || reply.QuickBurner == nil || !reply.QuickBurner.Ephemeral {
		t.Errorf("the reply %+v is not labeled ephemeral", reply)
	}
	// a second ask enrolls nothing again and hands nothing again
	burner.ask(t, "synthetic-one-time-code")
	if len(burner.closes) != 1 {
		t.Errorf("the close capability was handed %d times", len(burner.closes))
	}
}

// With no consented device of its member online to admit it and share the mailbox, the burner waits; once the share
// opens the mailbox, it is admitted and names it.
func TestWaitsWithNoConsentedDevice(t *testing.T) {
	burner := newTestBurner(t, "synthetic-close", true)
	_, reply := burner.ask(t, "synthetic-one-time-code")
	if reply.QuickBurner == nil || reply.QuickBurner.State != QuickBurnerStateWaiting || reply.QuickBurner.MailboxRepoId != "" {
		t.Fatalf("with no consented device the burner reads %+v", reply.QuickBurner)
	}
	mailboxId := sidecar.NewId()
	burner.shared = &mailboxId
	_, reply = burner.ask(t, "")
	if reply.QuickBurner.State != QuickBurnerStateAdmitted || reply.QuickBurner.MailboxRepoId != mailboxId.String() {
		t.Errorf("once shared the burner reads %+v", reply.QuickBurner)
	}
}

func TestBurnerReadsOnlySharedMailbox(t *testing.T) {
	world := newTestBurner(t, "fixture-close", true)
	world.ask(t, "synthetic-one-time-code")
	mailbox, other := sidecar.NewId(), sidecar.NewId()
	calls := 0
	world.burner.worker.args.Repo = func(id sidecar.Id) (WorkerRepo, error) {
		calls++
		if id != mailbox {
			t.Fatal("opened a repo outside the shared mailbox")
		}
		return nil, sidecar.ErrEvsNoConfig
	}
	ask := func(op string, repoId sidecar.Id) *Reply {
		return world.burner.Handle(t.Context(), &Request{Id: "mailbox", Operation: op, RepoId: repoId.String()})
	}
	if reply := ask(OperationList, mailbox); reply.Code != CodeRefused {
		t.Fatal("a mailbox read before its share was published was accepted")
	}
	world.shared = &mailbox
	if reply := ask(OperationList, other); reply.Code != CodeRefused {
		t.Fatal("a different repo was accepted")
	}
	if reply := ask(OperationRecordView, other); reply.Code != CodeUnavailable {
		t.Fatal("a record request reached the ordinary Worker")
	}
	ask(OperationList, mailbox)
	if calls != 1 {
		t.Fatalf("the shared mailbox was opened %d times", calls)
	}
	world.shared = nil
	if reply := ask(OperationList, mailbox); reply.Code != CodeRefused || calls != 1 {
		t.Fatal("a read after revocation reached the repo")
	}
}

// A tab resumed past the reap finds its client gone: the core reads unenrolled, and the burner answers reaped and
// offers re-enrollment, since a memory-only keychain cannot rejoin.
func TestResumeAfterReapOffersReenrollment(t *testing.T) {
	burner := newTestBurner(t, "synthetic-close", true)
	burner.ask(t, "synthetic-one-time-code")
	identity := burner.sidecar.Identity()
	burner.sidecar.set(identity, sidecar.SidecarStateUnenrolled)
	_, reply := burner.ask(t, "")
	if reply.QuickBurner == nil || reply.QuickBurner.State != QuickBurnerStateReaped || !reply.QuickBurner.Reenroll {
		t.Errorf("after the reap the burner reads %+v", reply.QuickBurner)
	}
}

// With no ephemeral mint from the session's code the burner answers unavailable and enrolls nothing; a request that
// names fields quick_burner does not take, or no code before an enrollment, is invalid.
func TestQuickBurnerFailsClosedWithoutAMint(t *testing.T) {
	burner := newTestBurner(t, "synthetic-close", false)
	_, reply := burner.ask(t, "synthetic-one-time-code")
	if reply.Ok || reply.Code != CodeInvalid {
		// the second ask, with no code, is invalid: nothing is enrolled
		t.Errorf("an ask with no code before an enrollment answered %+v", reply)
	}
	replyJson := burner.burner.HandleJson(context.Background(), []byte(`{"id":"b","op":"quick_burner","code":"synthetic-one-time-code"}`))
	if !bytes.Contains(replyJson, []byte(`"code":"unavailable"`)) {
		t.Errorf("with no mint the burner answered %s", replyJson)
	}
	if len(burner.closes) != 0 {
		t.Error("a capability went to the document with nothing minted")
	}
	replyJson = burner.burner.HandleJson(context.Background(), []byte(`{"id":"c","op":"quick_burner","repo_id":"00000000-0000-0000-0000-000000000001"}`))
	if !bytes.Contains(replyJson, []byte(`"code":"invalid"`)) {
		t.Errorf("a field quick_burner does not take answered %s", replyJson)
	}
	// the page Worker's operations still answer through the burner
	replyJson = burner.burner.HandleJson(context.Background(), []byte(`{"id":"d","op":"enroll"}`))
	if !bytes.Contains(replyJson, []byte(`"code":"unavailable"`)) {
		t.Errorf("the Worker's enroll answered %s through the burner", replyJson)
	}
	if _, err := NewQuickBurner(&WorkerArgs{}, &QuickBurnerArgs{}); err == nil {
		t.Error("a burner with nowhere to hand its close capability was built")
	}
}
