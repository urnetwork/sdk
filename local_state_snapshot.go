// Immutable auth observations carry their originating in-process owner as
// well as the complete durable envelope. No claim accessor rereads storage.
package sdk

import (
	"errors"
	"fmt"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// Fixed gomobile-visible discriminator for a bounded same-owner observation
// recapture. Other read/write failures must not be retried as supersession.
const localAuthSnapshotSupersededMessage = "auth snapshot was superseded or is not settled"

// Captures bytes and ownership under the same LocalState lock. Disk-only
// snapshots remain useful observations but cannot authorize a paired reset.
func (self *LocalState) authStateSnapshotWithLock() (*LocalAuthStateSnapshot, error) {
	if self.ctx.Err() != nil {
		return nil, errors.New("auth state owner is closed")
	}
	state, err := self.loadAuthStateLocked()
	if err != nil {
		return nil, localStorageStageError("read auth snapshot", err)
	}
	if self.testingAfterAuthSnapshotRead != nil {
		self.testingAfterAuthSnapshotRead()
	}
	var instanceId *Id
	if state.InstanceId != "" {
		instanceId, err = ParseId(state.InstanceId)
		if err != nil {
			return nil, errors.New("invalid auth state instance")
		}
	}
	return &LocalAuthStateSnapshot{
		instanceId:      instanceId,
		empty:           state.ByJwt == "" && state.ByClientJwt == "" && state.InstanceId == "",
		localState:      self,
		state:           state,
		localGeneration: self.deviceAuthGeneration,
		localOwner:      self.deviceAuthOwner,
	}, nil
}

// Returns only the separately captured admin credential.
func (self *LocalAuthStateSnapshot) GetByJwt() string {
	return self.state.ByJwt
}

// Returns only the captured provider credential, without an admin fallback.
func (self *LocalAuthStateSnapshot) GetByClientJwt() string {
	return self.state.ByClientJwt
}

// Parses the captured admin claims without another storage read or signature
// validation. The server remains the authority for authentication.
func (self *LocalAuthStateSnapshot) ParseByJwt() (*ByJwt, error) {
	return parseLocalByJwt(self.state.ByJwt)
}

// Checks the captured pair before reading the current saved destination.
// Supersession is an error, not a missing destination. Native owners must
// still revalidate their own lifecycle before publishing the returned value.
func (self *LocalAuthStateSnapshot) LoadConnectLocation() (*ConnectLocation, error) {
	return self.loadLocation(localConnectLocationFileName)
}

// Binds the default-location observation to the same captured auth pair.
func (self *LocalAuthStateSnapshot) LoadDefaultLocation() (*ConnectLocation, error) {
	return self.loadLocation(localDefaultLocationFileName)
}

// Persists a destination only while this exact auth pair remains settled and
// current. A late callback must not retry an error with the unguarded setter.
// nil is an explicit disconnect, not a recovery action for a failed read.
func (self *LocalAuthStateSnapshot) SetConnectLocation(location *ConnectLocation) error {
	return self.setLocation(localConnectLocationFileName, location)
}

// Gives an owner-bound default selection the same conditional commit boundary.
func (self *LocalAuthStateSnapshot) SetDefaultLocation(location *ConnectLocation) error {
	return self.setLocation(localDefaultLocationFileName, location)
}

// Snapshot validation and atomic location commit use the reset lock order.
// Callers remain responsible for destination-generation/selection ownership;
// a valid auth snapshot does not order two choices within the same session.
func (self *LocalAuthStateSnapshot) setLocation(name string, location *ConnectLocation) error {
	if self == nil || !self.networkSpace.ownsAuthSnapshot(self) {
		return errors.New("location write requires a paired auth snapshot")
	}
	self.api.authMutationLock.Lock()
	defer self.api.authMutationLock.Unlock()
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	current, err := self.networkSpace.authSnapshotCurrentWithLock(self)
	if err != nil {
		return err
	}
	if !current {
		return errors.New(localAuthSnapshotSupersededMessage)
	}
	return self.localState.setLocationWithLock(name, location)
}

// A disk-only snapshot cannot prove API ownership. Location writes and reset
// share the LocalState lock; the result describes this read, not a lease over
// future native work or a snapshot of other independently persisted settings.
func (self *LocalAuthStateSnapshot) loadLocation(name string) (*ConnectLocation, error) {
	if self == nil || !self.networkSpace.ownsAuthSnapshot(self) {
		return nil, errors.New("location read requires a paired auth snapshot")
	}
	self.api.authMutationLock.Lock()
	defer self.api.authMutationLock.Unlock()
	self.localState.authStateLock.Lock()
	defer self.localState.authStateLock.Unlock()
	current, err := self.networkSpace.authSnapshotCurrentWithLock(self)
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, errors.New(localAuthSnapshotSupersededMessage)
	}
	return self.localState.loadLocationWithLock(name)
}

// Retains optional claims and tolerant invalid-string IDs. Present values of
// the wrong type produce a controlled error instead of an assertion panic.
func parseLocalByJwt(byJwtString string) (*ByJwt, error) {
	if byJwtString == "" {
		return nil, errors.New("Not found.")
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwtString, claims); err != nil {
		return nil, errors.New("decode admin JWT")
	}
	byJwt := &ByJwt{}
	for _, claim := range []struct {
		name string
		id   **Id
	}{
		{name: "user_id", id: &byJwt.UserId},
		{name: "network_id", id: &byJwt.NetworkId},
	} {
		if value, present := claims[claim.name]; present {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("admin JWT has invalid %s type", claim.name)
			}
			if id, err := ParseId(text); err == nil {
				*claim.id = id
			}
		}
	}
	if value, present := claims["network_name"]; present {
		name, ok := value.(string)
		if !ok {
			return nil, errors.New("admin JWT has invalid network_name type")
		}
		byJwt.NetworkName = name
	}
	for _, claim := range []struct {
		name string
		flag *bool
	}{
		{name: "guest_mode", flag: &byJwt.GuestMode},
		{name: "pro", flag: &byJwt.Pro},
	} {
		if value, present := claims[claim.name]; present {
			flag, ok := value.(bool)
			if !ok {
				return nil, fmt.Errorf("admin JWT has invalid %s type", claim.name)
			}
			*claim.flag = flag
		}
	}
	return byJwt, nil
}
