//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

// The remote backend's agents calls held to ur daemon's agents methods (the root's agents_daemon.go) on the legs that
// build both: each method's name, and each method's params decoding strictly as the daemon's.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/urnetwork/sidecar"
)

func TestAgentsControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodAgentsList:  sidecar.DaemonMethodAgentsList,
		sidecarControlMethodAgentsSend:  sidecar.DaemonMethodAgentsSend,
		sidecarControlMethodAgentsInbox: sidecar.DaemonMethodAgentsInbox,
		sidecarControlMethodAgentsGraph: sidecar.DaemonMethodAgentsGraph,
		sidecarControlMethodAgentsWatch: sidecar.DaemonMethodAgentsWatch,
	} {
		if own != daemon {
			t.Errorf("the sdk calls %s where the daemon serves %s", own, daemon)
		}
	}
	served := map[string]bool{}
	for _, method := range sidecar.DaemonAgentsMethods(nil) {
		served[method.Name] = true
		if method.Confirm {
			// agents.send reads Confirmed itself: a harness run's message is no person's act to confirm
			t.Errorf("%s is a Confirm method", method.Name)
		}
	}
	for _, name := range []string{sidecarControlMethodAgentsList, sidecarControlMethodAgentsSend, sidecarControlMethodAgentsInbox,
		sidecarControlMethodAgentsGraph, sidecarControlMethodAgentsWatch} {
		if !served[name] {
			t.Errorf("the daemon serves no %s", name)
		}
	}
	strict := func(label string, params any, into any) {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			t.Errorf("%s's params %s do not decode as the daemon's: %v", label, data, err)
		}
	}
	strict("list", &sidecarControlAgentsListParams{Network: "n", Retired: true, Untrusted: "include"}, &sidecar.DaemonAgentsListParams{})
	send := &sidecar.DaemonAgentsSendParams{}
	strict("send", &sidecarControlAgentsSendParams{Network: "n", To: "fix", Subject: "s", Body: "b", Thread: "t", Broadcast: true, Close: true}, send)
	if send.To != "fix" || send.Body != "b" || send.Thread != "t" || !send.Broadcast || !send.Close {
		t.Errorf("send's params read %+v", send)
	}
	strict("inbox", &sidecarControlAgentsInboxParams{Network: "n", Thread: "t", Untrusted: "include"}, &sidecar.DaemonAgentsInboxParams{})
	strict("graph", &sidecarControlAgentsGraphParams{Network: "n", Retired: true, Agent: "fix", Untrusted: "include"}, &sidecar.DaemonAgentsGraphParams{})
	strict("watch", &sidecarControlAgentsWatchParams{Network: "n", Cursor: "3", Untrusted: "include"}, &sidecar.DaemonAgentsWatchParams{})
	// the daemon's answers read as the sdk's
	reply := &sidecar.DaemonAgentsWatchReply{Cursor: "4", Changed: true, Messages: []*sidecar.DaemonAgentsWatchMessage{{
		Message: &sidecar.AgentThreadMessage{ThreadHex: "bb"}, Notification: &sidecar.AgentNotification{Sender: "fix"}}}}
	strict("watch's answer", reply, &sidecarControlAgentsWatchReply{})
	strict("send's answer", &sidecar.DaemonAgentsSendReply{AgentSendToolResult: &sidecar.AgentSendToolResult{To: "fix"}, Person: true}, &sidecarAgentsSendReply{})
}
