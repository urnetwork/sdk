//go:build ios || android || js

package sdk

import (
	"context"
)

// device_local_extender_mobile.go — the builds that carry no extender role
// (EXTENDER.md G1, H).
//
// ios, android and js binaries carry neither `connect/extender` nor the
// activation loop: a phone is not a host with spare capacity and a public
// address, and the js build has no gossip node at all. The type exists so the
// provider and the status surface need no build tag of their own, and
// everything it answers is the disabled status of F3.

// This build carries no role (G1).
const extenderProvideSupported = false

type deviceLocalExtender struct{}

func newDeviceLocalExtender(
	ctx context.Context,
	settings *deviceLocalExtenderSettings,
) *deviceLocalExtender {
	return nil
}

func (self *deviceLocalExtender) status() *ExtenderProvideStatus {
	return disabledExtenderProvideStatus()
}

func (self *deviceLocalExtender) statusUpdate() chan struct{} {
	return nil
}

func (self *deviceLocalExtender) Close() {
}
