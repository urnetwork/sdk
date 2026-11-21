//go:build !ios_extension && !js

// The public SDK submits to the core's gate and reports success only for accepted recipients.
package sdk

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A real, presently unbound core answers its own refusal of a draft out of shape, which it holds the draft's subject,
// text and reply id to before it looks at its binding, and its own state for every other message. It judges the
// recipients only once bound, under the network's zone, since a recipient's delivery form folds a local part of the
// network's own to the zone's form (§15.8's local parts) and keeps an outside one as typed: so a recipient with no
// domain reads unbound here too, as a valid one does.
func TestSidecarMailSubmitsToTheLocalCore(t *testing.T) {
	client, err := sidecar.NewMailClient(&sidecar.MailClientArgs{NetworkId: sidecar.NewId(), SidecarId: sidecar.NewId(), Store: testEmptySidecarStore{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	mail := newTestSidecarMail(t, &sidecarMailCoreBackend{client: client})
	for _, test := range []struct{ to, subject, code, why string }{
		{to: "member@net.example", subject: "Plans", code: sidecarMailCodeUnavailable, why: sidecar.ErrMailUnbound.Error()},
		{to: "no-domain", subject: "Plans", code: sidecarMailCodeUnavailable, why: sidecar.ErrMailUnbound.Error()},
		{to: "member@net.example", subject: "Plans\r\nBcc: other@outside.example", code: sidecarMailCodeInvalid, why: sidecar.ErrMailSendInvalid.Error()},
	} {
		draft := &SidecarDraft{From: "admin@net.example", To: NewStringList(), Subject: test.subject, Body: "body"}
		draft.To.Add(test.to)
		callback := newTestEvsCallback[*SidecarResult]()
		mail.Send(draft, callback)
		result, err := callback.await(t)
		if err != nil || result.Ok || result.Value != "" || result.Code != test.code || !strings.Contains(result.Message, test.why) {
			t.Errorf("%s, %q: %+v, %v", test.to, test.subject, result, err)
		}
	}
}

// The daemon may return individual failures, partial success, or just the id of a previously committed approval. A
// send whose every recipient waits, internet mail on a daemon with no outbox for it, queued nothing (§15.10's step 4
// queues at o/) and so names no send to follow.
func TestSidecarMailRemoteSubmissionRequiresAnAcceptedRecipient(t *testing.T) {
	path := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, path)
	replies := make(chan *sidecar.DaemonMailSendReply, 1)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method == sidecarControlMethodMailSend {
			return <-replies, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, path)
	id, zero := sidecar.NewId(), sidecar.Id{}
	accepted := &sidecar.DaemonMailSendRecipient{Outcome: string(sidecar.MailSendQueued)}
	refused := &sidecar.DaemonMailSendRecipient{Outcome: string(sidecar.MailSendInternetRefused)}
	waits := &sidecar.DaemonMailSendRecipient{Outcome: string(sidecar.MailSendInternetWaits)}
	for _, test := range []struct {
		id         *sidecar.Id
		recipients []*sidecar.DaemonMailSendRecipient
		want       bool
	}{
		{id: &zero, recipients: nil, want: false},
		{id: &id, recipients: []*sidecar.DaemonMailSendRecipient{refused}, want: false},
		{id: &id, recipients: []*sidecar.DaemonMailSendRecipient{waits}, want: false},
		{id: &id, recipients: []*sidecar.DaemonMailSendRecipient{waits, accepted}, want: true},
		{id: &id, recipients: []*sidecar.DaemonMailSendRecipient{refused, accepted}, want: true},
		{id: &id, recipients: nil, want: true},
	} {
		replies <- &sidecar.DaemonMailSendReply{SendId: test.id, Recipients: test.recipients}
		callback := newTestEvsCallback[*SidecarResult]()
		remote.GetMail().Send(testSendDraft("outside.example"), callback)
		result, err := callback.await(t)
		if err != nil || result.Ok != test.want || test.want && result.Value != id.String() || !test.want && result.Value != "" {
			t.Errorf("send: %+v, %v", result, err)
		}
	}
}
