//go:build ios || android || js

package sdk

import (
	"context"
	"errors"
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

// Never reached: the provider forces the role off on this build before it
// builds one (G1). The error is what it would report if it were.
func newDeviceLocalExtender(
	ctx context.Context,
	settings *deviceLocalExtenderSettings,
) (*deviceLocalExtender, error) {
	return nil, errors.New("this build carries no extender role")
}

func (self *deviceLocalExtender) status() *ExtenderProvideStatus {
	return disabledExtenderProvideStatus()
}

// No role, so nothing is relayed and there is no series to show (O2).
func (self *deviceLocalExtender) stats() *ExtenderStats {
	return nil
}

func (self *deviceLocalExtender) statusUpdate() chan struct{} {
	return nil
}

func (self *deviceLocalExtender) Close() {
}
