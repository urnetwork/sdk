//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

// The remote backend's model wire against ur daemon's own (the root's infer_daemon.go, which builds on desktop legs
// alone, as this file does): the method names are the daemon's, every params Models() sends decodes whole into the
// daemon's, which reads them strictly, and SetSettings reaches the daemon only as its Confirm method sent confirmed,
// which refuses an unconfirmed call before it reads or writes anything.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// Each method name is the daemon's, each params decodes into the daemon's with no field left over, and models.set alone
// is a Confirm method.
func TestModelsControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodModelsList:     sidecar.DaemonMethodModelsList,
		sidecarControlMethodModelsPull:     sidecar.DaemonMethodModelsPull,
		sidecarControlMethodModelsRemove:   sidecar.DaemonMethodModelsRemove,
		sidecarControlMethodModelsSettings: sidecar.DaemonMethodModelsSettings,
		sidecarControlMethodModelsSet:      sidecar.DaemonMethodModelsSet,
	} {
		if own != daemon {
			t.Errorf("the sdk calls %s where the daemon serves %s", own, daemon)
		}
	}
	confirms := map[string]bool{}
	for _, method := range sidecar.DaemonInferMethods(nil) {
		confirms[method.Name] = method.Confirm
	}
	if !confirms[sidecarControlMethodModelsSet] || confirms[sidecarControlMethodModelsPull] || confirms[sidecarControlMethodModelsRemove] ||
		confirms[sidecarControlMethodModelsList] || confirms[sidecarControlMethodModelsSettings] {
		t.Errorf("the daemon's Confirm methods are %v", confirms)
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
	list := &sidecar.DaemonModelsListParams{}
	strict("list", &sidecarControlModelsParams{Network: "n"}, list)
	pull := &sidecar.DaemonModelsPullParams{}
	strict("pull", &sidecarControlModelsNameParams{Network: "n", Name: "local-gemma-4-e2b-off"}, pull)
	remove := &sidecar.DaemonModelsRemoveParams{}
	strict("remove", &sidecarControlModelsNameParams{Network: "n", Name: "local-gemma-4-e2b-off"}, remove)
	settings := &sidecar.DaemonModelsParams{}
	strict("settings", &sidecarControlModelsParams{Network: "n"}, settings)
	teeOnly := true
	set := &sidecar.DaemonModelsSetParams{}
	strict("set", &sidecarControlModelsSetParams{Network: "n", InferModelsChange: &sidecar.InferModelsChange{TeeOnly: &teeOnly}}, set)
	if list.Network != "n" || pull.Name != "local-gemma-4-e2b-off" || pull.Follow || remove.Name != "local-gemma-4-e2b-off" ||
		settings.Network != "n" || set.TeeOnly == nil || !*set.TeeOnly || set.Network != "n" {
		t.Errorf("the daemon reads %+v %+v %+v %+v %+v", list, pull, remove, settings, set)
	}
	// the reply a poll reads is the daemon's own
	reply, _ := json.Marshal(&sidecar.DaemonModelsPullReply{Name: "local-gemma-4-e2b-off", State: sidecar.DaemonModelsPullRunning})
	status := &sidecar.InferPullStatus{}
	strict("models.pull's reply", json.RawMessage(reply), status)
}

// SetSettings goes to the daemon as models.set sent confirmed, the person's act in the app's UI, and the daemon's
// handler refuses the same call unconfirmed before it reads its params or the models key, so an unconfirmed call
// records nothing; an empty or unknown change never leaves the app.
func TestModelsSetSettingsRemoteNeedsConfirm(t *testing.T) {
	daemon := newTestModelsDaemon(nil)
	models := testModelsRemote(t, daemon)
	if err := models.SetSettings(`{"tee_only": true}`); err != nil {
		t.Fatal(err)
	}
	calls := daemon.taken(sidecarControlMethodModelsSet)
	if len(calls) != 1 || !calls[0].confirmed {
		t.Fatalf("models.set was sent %+v", calls)
	}
	set := &sidecar.DaemonModelsSetParams{}
	decoder := json.NewDecoder(bytes.NewReader(calls[0].params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(set); err != nil || set.TeeOnly == nil || !*set.TeeOnly || set.Network != "the paired network" {
		t.Errorf("models.set had %s: %v", calls[0].params, err)
	}
	if !daemon.key.TeeOnly {
		t.Error("the confirmed change was not written")
	}

	// the daemon's own handler, unconfirmed: refused before it reads anything, with no Sidecar to write to
	var handler func(ctx context.Context, call *sidecar.DaemonCall) (any, error)
	for _, method := range sidecar.DaemonInferMethods(nil) {
		if method.Name == sidecar.DaemonMethodModelsSet {
			handler = method.Handler
		}
	}
	if handler == nil {
		t.Fatal("the daemon serves no models.set")
	}
	if answer, err := handler(context.Background(), &sidecar.DaemonCall{}); err == nil || answer != nil || !strings.Contains(err.Error(), "confirmed") {
		t.Errorf("an unconfirmed models.set answered %v, %v", answer, err)
	}

	// nothing the app sends unconfirmed or malformed changes the key
	daemon.key.TeeOnly = false
	if err := daemon.call(context.Background(), sidecarControlMethodModelsSet, &sidecarControlModelsSetParams{InferModelsChange: &sidecar.InferModelsChange{TeeOnly: &[]bool{true}[0]}}, false, &json.RawMessage{}); err == nil || daemon.key.TeeOnly {
		t.Errorf("an unconfirmed call answered %v, the key tee_only %v", err, daemon.key.TeeOnly)
	}
	for _, change := range []string{``, `{}`, `{"tee_only": "yes"}`, `{"teeonly": true}`} {
		before := len(daemon.taken(sidecarControlMethodModelsSet))
		if err := models.SetSettings(change); err == nil {
			t.Errorf("the change %q was taken", change)
		}
		if after := len(daemon.taken(sidecarControlMethodModelsSet)); after != before {
			t.Errorf("the change %q reached the daemon", change)
		}
	}
}
