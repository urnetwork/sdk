//go:build js && wasm && sidecar_http

package main

import (
	"context"
	"syscall/js"

	"github.com/urnetwork/sdk/js/sidecarworker"
	"github.com/urnetwork/sidecar"
)

// Unlocking a fresh instance wakes the core's asynchronous lifecycle. Wait for that
// transition before accepting an enrollment on the independently scheduled Worker bridge.
func (self *pageNetwork) unlock() {
	if self.local.NetworkId() != nil {
		self.local.Unlock(nil)
		return
	}
	changed := make(chan struct{}, 1)
	unsubscribe := self.local.AddEventCallback(func(sidecar.SidecarEvent) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer unsubscribe()
	if err := self.local.Unlock(nil); err != nil {
		return
	}
	for self.local.Status().State == sidecar.SidecarStateLocked {
		select {
		case <-changed:
		case <-self.ctx.Done():
			return
		}
	}
}

// The callback is private to the Worker and its creating frame. Neither the transient
// network bearer nor a burner's close capability crosses it.
func (self *pageNetwork) enroll(ctx context.Context, code string) error {
	return self.local.EnrollPageClient(ctx, self.networkId, self.ephemeral, func(ctx context.Context) (*sidecar.SidecarAuthClientResult, error) {
		mint := js.Global().Get("urnetworkSidecarMint")
		if mint.Type() != js.TypeFunction {
			return nil, sidecar.ErrSidecarClosed
		}
		reply, err := awaitPromise(mint.Invoke(code))
		if err != nil || reply.Type() != js.TypeObject {
			return nil, sidecar.ErrSidecarApiReply
		}
		token, id := reply.Get("by_client_jwt"), reply.Get("client_id")
		if token.Type() != js.TypeString || id.Type() != js.TypeString {
			return nil, sidecar.ErrSidecarApiReply
		}
		clientId, err := sidecar.ParseId(id.String())
		if err != nil {
			return nil, sidecar.ErrSidecarApiReply
		}
		marked := func(key string) bool {
			value := reply.Get(key)
			return value.Type() == js.TypeBoolean && value.Bool()
		}
		return &sidecar.SidecarAuthClientResult{
			ClientId: clientId, ByClientJwt: token.String(),
			AdminRefused: marked("admin_refused"), StrictExp: marked("strict_exp"), DeclaresNoEgress: marked("declares_no_egress"),
		}, nil
	})
}

func (self *pageNetwork) admission() *sidecarworker.AdmissionView {
	status := self.local.Status()
	view := &sidecarworker.AdmissionView{State: "unenrolled"}
	switch {
	case status.State == sidecar.SidecarStateRevoked, status.State == sidecar.SidecarStateLocked:
		view.State = string(status.State)
		return view
	case self.local.NetworkId() == nil, status.State == sidecar.SidecarStateUnenrolled && status.Reason != "":
		return view
	}
	view.State = "waiting"
	if identity := self.local.Identity(); identity != nil {
		view.SidecarId = identity.SidecarId.String()
		if number, err := self.local.SafetyNumber(); err == nil {
			view.SafetyNumber = sidecar.FormatSafetyNumber(number)
		}
		client, _, err := self.standingReplica()
		if err == nil {
			state := client.State()
			if own := self.admittedMember(state); own != nil && !self.ephemeral {
				view.State = "admitted"
				auth := sidecar.NewRecordAuthView(state)
				if vault, found := auth.Vault(own.MemberId); found && auth.Consented(vault, identity.SidecarId) && auth.Reads(vault, identity.SidecarId) {
					view.VaultId = vault.String()
				}
			}
		}
	}
	return view
}
