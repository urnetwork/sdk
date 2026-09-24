// Device startup prepares without changing the serving session. Final commit
// serializes auth mutations only; API request reads never wait for disk I/O.
package sdk

import (
	"errors"
	"time"
)

// Immutable preparation captures both token bytes and in-process authority.
// A rejected commit leaves its old serving owner and durable state untouched.
type deviceAuthStartup struct {
	localState      *LocalState
	state           persistedLocalAuthState
	localGeneration uint64
	apiGeneration   uint64
	byJwt           string
	instanceId      string
}

// Reads one coherent preparation under auth-mutation -> LocalState -> Api.
// A serving API refresh may precede its persistence callback; that exact owned
// client participates in selection, but an admin/API login never does.
func (self *Api) prepareDeviceAuth(
	localState *LocalState, byJwt string, instanceId *Id, now time.Time,
	owner *deviceAuthPublicationGate,
) (*deviceAuthStartup, error) {
	prepared := &deviceAuthStartup{localState: localState, byJwt: byJwt}
	err := func() error {
		self.authMutationLock.Lock()
		defer self.authMutationLock.Unlock()
		if localState != nil {
			localState.authStateLock.Lock()
			defer localState.authStateLock.Unlock()
			state, err := localState.loadAuthStateLocked()
			if err != nil {
				return err
			}
			selected, err := selectStartupClientJwt(state, byJwt, instanceId, now)
			if err != nil {
				return err
			}
			prepared.state = state
			prepared.byJwt = selected
			prepared.instanceId = instanceId.String()
			prepared.localGeneration = localState.deviceAuthGeneration
		}
		self.mutex.Lock()
		apiGeneration := self.deviceAuthGeneration
		apiOwner := self.deviceAuthOwner
		apiJwt := self.byJwt
		rejectedJwt := self.rejectedByJwt
		self.mutex.Unlock()
		prepared.apiGeneration = apiGeneration
		if localState == nil || apiOwner == nil || apiOwner != localState.deviceAuthOwner ||
			prepared.state.InstanceId != prepared.instanceId {
			return nil
		}
		if apiJwt == "" && rejectedJwt != "" {
			return errors.New("startup client was rejected by its serving API")
		}
		if apiJwt == "" || apiJwt == prepared.byJwt {
			return nil
		}
		current, err := parseStartupClientJwt(apiJwt)
		if err != nil {
			return err
		}
		selected, err := parseStartupClientJwt(prepared.byJwt)
		if err != nil {
			return err
		}
		// Selection can prefer a token with an unknown network. Compare the
		// API against both original candidates so that unknown cannot bridge
		// conflicting known networks that are still available in this snapshot.
		for _, candidateJwt := range []string{byJwt, prepared.state.ByClientJwt} {
			if candidateJwt == "" {
				continue
			}
			candidate, err := parseStartupClientJwt(candidateJwt)
			if err != nil {
				return err
			}
			if !candidate.compatibleIdentity(current) {
				return errors.New("serving API client differs from the established startup identity")
			}
		}
		// The API already committed this refresh. Equal/unknown dates cannot
		// authorize restoring the older durable copy over it.
		if preferSuppliedStartupClient(selected, current, now) {
			if prepared.byJwt == prepared.state.ByClientJwt {
				// Either persistence is ahead of an explicit API update, or
				// a refresh has conflicting dates. Neither permits reverting
				// an already-published API token from the durable snapshot.
				return errors.New("startup client freshness is ambiguous between API and storage")
			}
		} else {
			prepared.byJwt = apiJwt
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if localState != nil && localState.testingAfterDeviceAuthPrepare != nil {
		localState.testingAfterDeviceAuthPrepare(owner)
	}
	return prepared, nil
}

// Conditional commit holds the narrow auth-mutation mutex while the existing
// durable transaction runs, never Api.mutex. No observer callbacks, provider
// work, token notifications, or joins run under either transaction/state lock.
func (self *Api) publishDeviceOwner(
	prepared *deviceAuthStartup, owner *deviceAuthPublicationGate, publishWithLock func(),
) error {
	self.authMutationLock.Lock()
	defer self.authMutationLock.Unlock()
	localState := prepared.localState
	if localState != nil {
		localState.authStateLock.Lock()
		defer localState.authStateLock.Unlock()
		state, err := localState.loadAuthStateLocked()
		if err != nil {
			return err
		}
		if state != prepared.state || localState.deviceAuthGeneration != prepared.localGeneration {
			return errors.New("device auth startup was superseded in local storage")
		}
	}
	self.mutex.Lock()
	current := self.deviceAuthGeneration == prepared.apiGeneration
	self.mutex.Unlock()
	if !current {
		return errors.New("device auth startup was superseded at the API")
	}
	if localState != nil {
		state := prepared.state
		if state.ByClientJwt != prepared.byJwt || state.InstanceId != prepared.instanceId {
			state.ByClientJwt = prepared.byJwt
			state.InstanceId = prepared.instanceId
			state.Generation += 1
			if err := localState.writeAuthStateLocked(state); err != nil {
				return err
			}
		}
	}
	// All fallible work finished. The auth-mutation mutex still excludes
	// API setters; LocalState remains locked through both owner publications.
	self.mutex.Lock()
	publishWithLock()
	self.deviceAuthGeneration += 1
	if localState != nil {
		localState.deviceAuthOwner = owner
		localState.deviceAuthGeneration += 1
	}
	self.mutex.Unlock()
	return nil
}
