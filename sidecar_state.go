//go:build !ios_extension && !js

// §18.5's state calls (D-SDK-14): GetState and GetStateReason read the core's lifecycle on the local backend, the
// daemon's on the remote one, and Unlock opens the keychain on the local backend alone.
//
//	state       locked | unenrolled | offline | online | revoked
//	reason      unenrolled: expired | refused | parent_lost | session_ended | keystore_lost; revoked: signed
//
// The core decides each: a confirmed 401 reads unenrolled only when the host found it current, a revocation hint or
// a bind refused sidecar_revoked reads revoked only once a SidecarRevoke verifies, else unenrolled(refused) with an
// alarm, and an older generation's event is dropped (sidecar_state.go, identity_revoke_self.go in the core). The sdk
// only names what the core decided; it never writes a state of its own.

package sdk

import (
	"errors"

	"github.com/urnetwork/sidecar"
)

// The remote backend unlocks nothing: the daemon opens its own keychain (§5.9).
var errSidecarRemoteUnlock = errors.New("a remote-mode sidecar's daemon unlocks its own keychain")

// The state now (§18.5's State): the core's on the local backend, the daemon's on the remote one, and unenrolled
// once closed.
func (self *Sidecar) GetState() string {
	state, _ := self.stateAndReason()
	return state
}

// The state's reason, empty where the state has none.
func (self *Sidecar) GetStateReason() string {
	_, reason := self.stateAndReason()
	return reason
}

// The state and its reason.
func (self *Sidecar) stateAndReason() (string, string) {
	type stateAndReason struct {
		state  string
		reason string
	}
	answer, err := traceWithReturn(func() stateAndReason {
		local, remote := self.backends()
		switch {
		case local != nil:
			status := local.Status()
			return stateAndReason{state: string(status.State), reason: string(status.Reason)}
		case remote != nil:
			state, reason := remote.state()
			return stateAndReason{state: state, reason: reason}
		}
		return stateAndReason{state: string(sidecar.SidecarStateUnenrolled)}
	})
	if err != nil {
		return string(sidecar.SidecarStateUnenrolled), ""
	}
	return answer.state, answer.reason
}

// Opens the keychain (§5.9), local only: an empty passphrase takes the key store's wrap of U, which an app calls once
// its store can answer, as after a device unlock; a passphrase opens the pass wrap. A store that answers Locked or
// Failed creates nothing and leaves the sidecar Locked (D-SDK-15). The remote backend refuses: the daemon unlocks.
// The result's Value is the state after.
func (self *Sidecar) Unlock(passphrase string, callback SidecarResultCallback) {
	trace(func() {
		local, remote := self.backends()
		go func() {
			result, err := traceWithReturnError(func() (*SidecarResult, error) {
				switch {
				case remote != nil:
					return sidecarResultOf("", errSidecarRemoteUnlock), nil
				case local == nil:
					return sidecarResultOf("", sidecar.ErrSidecarClosed), nil
				}
				var pass []byte
				if passphrase != "" {
					pass = []byte(passphrase)
				}
				err := local.Unlock(pass)
				clear(pass)
				if err != nil {
					return sidecarResultOf("", err), nil
				}
				return sidecarResultOf(string(local.Status().State), nil), nil
			})
			if callback != nil {
				callback.Result(result, err)
			}
		}()
	})
}
