//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

// The remote backend's disclosure wire against ur daemon's own (the root's disclosure_daemon.go, which builds on
// desktop legs alone, as this file does): the method names are the daemon's, the acts are its Confirm methods and the
// list is not, every params the sdk sends decodes whole into the daemon's, which it reads strictly, and the daemon's
// act reply decodes into the sdk's.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/urnetwork/sidecar"
)

// Each method name is the daemon's, each params decodes into the daemon's with no field left over, and each reply the
// daemon writes reads whole here.
func TestDisclosureControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodDisclosureList: sidecar.DaemonMethodComputeDisclosureList,
		sidecarControlMethodDisclosureAck:  sidecar.DaemonMethodComputeDisclosureAck,
		sidecarControlMethodDisclosureOpen: sidecar.DaemonMethodComputeDisclosureOpen,
	} {
		if own != daemon {
			t.Errorf("the sdk calls %s where the daemon serves %s", own, daemon)
		}
	}
	confirms := map[string]bool{}
	for _, method := range sidecar.DaemonComputeDisclosureMethods(nil) {
		confirms[method.Name] = method.Confirm
	}
	if confirms[sidecarControlMethodDisclosureList] || !confirms[sidecarControlMethodDisclosureAck] || !confirms[sidecarControlMethodDisclosureOpen] {
		t.Errorf("the daemon's Confirm methods are %v", confirms)
	}
	strict := func(data []byte, into any) error {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		return decoder.Decode(into)
	}
	listParams, _ := json.Marshal(&sidecarControlDisclosureListParams{Network: "acme", Locale: "de-AT"})
	if err := strict(listParams, &sidecar.DaemonComputeDisclosureListParams{}); err != nil {
		t.Errorf("the list's params: %v", err)
	}
	actParams, _ := json.Marshal(&sidecarControlDisclosureActParams{Network: "acme", Id: "inference:engy", Version: 2})
	daemonParams := &sidecar.DaemonComputeDisclosureActParams{}
	if err := strict(actParams, daemonParams); err != nil || daemonParams.Id != "inference:engy" || daemonParams.Version != 2 || daemonParams.Network != "acme" {
		t.Errorf("the act's params: %+v, %v", daemonParams, err)
	}
	for _, reply := range []*sidecar.DaemonComputeDisclosureActReply{
		{Id: "inference:engy", Version: 2},
		{Id: "inference:engy", Refusal: sidecar.ComputeDisclosureCodeChanged},
	} {
		data, _ := json.Marshal(reply)
		own := &sidecarControlDisclosureActReply{}
		if err := strict(data, own); err != nil || own.Id != reply.Id || own.Version != reply.Version || own.Refusal != reply.Refusal {
			t.Errorf("the reply %s: %+v, %v", data, own, err)
		}
	}
}
