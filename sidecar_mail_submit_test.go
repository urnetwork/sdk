//go:build !ios_extension && !js

// The public SDK submits to the core's gate and reports success only for accepted recipients.
package sdk

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A real, presently unbound core returns its own state for a valid message, and its own syntax refusal first.
func TestSidecarMailSubmitsToTheLocalCore(t *testing.T) {
	client, err := sidecar.NewMailClient(&sidecar.MailClientArgs{NetworkId: sidecar.NewId(), SidecarId: sidecar.NewId(), Store: testEmptySidecarStore{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	mail := newTestSidecarMail(t, &sidecarMailCoreBackend{client: client})
	for _, test := range []struct{ to, code, why string }{
		{"member@net.example", sidecarMailCodeUnavailable, sidecar.ErrMailUnbound.Error()},
		{"no-domain", sidecarMailCodeInvalid, sidecar.ErrMailSendInvalid.Error()},
	} {
		draft := &SidecarDraft{From: "admin@net.example", To: NewStringList(), Body: "body"}
		draft.To.Add(test.to)
		callback := newTestEvsCallback[*SidecarResult]()
		mail.Send(draft, callback)
		result, err := callback.await(t)
		if err != nil || result.Ok || result.Value != "" || result.Code != test.code || !strings.Contains(result.Message, test.why) {
			t.Errorf("%s: %+v, %v", test.to, result, err)
		}
	}
}

// The daemon may return individual failures, partial success, or just the id of a previously committed approval.
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
	for _, test := range []struct {
		id         *sidecar.Id
		recipients []*sidecar.DaemonMailSendRecipient
		want       bool
	}{
		{&zero, nil, false},
		{&id, []*sidecar.DaemonMailSendRecipient{refused}, false},
		{&id, []*sidecar.DaemonMailSendRecipient{refused, accepted}, true},
		{&id, nil, true},
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
