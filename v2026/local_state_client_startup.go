// Startup selects one coherent client credential before API or provider
// publication. Admin credentials never participate in provider selection.
package sdk

import (
	"errors"
	"fmt"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
)

// An unverified identity is used only to compare two already persisted or
// supplied client credentials. The server remains the signature authority.
type startupClientJwt struct {
	clientId  connect.Id
	deviceId  connect.Id
	networkId connect.Id
	issuedAt  *gojwt.NumericDate
	expiresAt *gojwt.NumericDate
}

// Client/device identify a refreshable provider. The server can recover an
// omitted or zero network id; an unknown network is never filled from admin
// auth, and a present malformed/null claim is not a recoverable omission.
func parseStartupClientJwt(byJwt string) (startupClientJwt, error) {
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(byJwt, claims); err != nil {
		return startupClientJwt{}, errors.New("invalid startup client JWT")
	}
	parseId := func(key string) (connect.Id, error) {
		value, ok := claims[key].(string)
		if !ok || value == "" {
			return connect.Id{}, fmt.Errorf("startup client JWT is missing %s", key)
		}
		id, err := connect.ParseId(value)
		if err != nil {
			return connect.Id{}, fmt.Errorf("startup client JWT has invalid %s", key)
		}
		return id, nil
	}
	clientId, err := parseId("client_id")
	if err != nil {
		return startupClientJwt{}, err
	}
	deviceId, err := parseId("device_id")
	if err != nil {
		return startupClientJwt{}, err
	}
	var networkId connect.Id
	if value, present := claims["network_id"]; present {
		// Match the server Id JSON shape; null and compact UUID spellings
		// are not missing claims and do not reach server-side recovery.
		if networkString, ok := value.(string); !ok || len(networkString) != 36 {
			return startupClientJwt{}, errors.New("startup client JWT has invalid network_id")
		}
		networkId, err = parseId("network_id")
		if err != nil {
			return startupClientJwt{}, err
		}
	}
	issuedAt, err := claims.GetIssuedAt()
	if err != nil {
		return startupClientJwt{}, errors.New("startup client JWT has invalid issue time")
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil {
		return startupClientJwt{}, errors.New("startup client JWT has invalid expiry")
	}
	return startupClientJwt{
		clientId: clientId, deviceId: deviceId, networkId: networkId,
		issuedAt: issuedAt, expiresAt: expiresAt,
	}, nil
}

// Follows the existing Apple bootstrap preference: unexpired, then later
// issue time, then later expiry. Ties retain the durable token, never a guess.
func preferSuppliedStartupClient(supplied startupClientJwt, stored startupClientJwt, now time.Time) bool {
	suppliedExpired := supplied.expiresAt != nil && !now.Before(supplied.expiresAt.Time)
	storedExpired := stored.expiresAt != nil && !now.Before(stored.expiresAt.Time)
	if suppliedExpired != storedExpired {
		return !suppliedExpired
	}
	compareDate := func(a *gojwt.NumericDate, b *gojwt.NumericDate) int {
		if a == nil {
			if b == nil {
				return 0
			}
			return -1
		}
		if b == nil {
			return 1
		}
		return a.Time.Compare(b.Time)
	}
	if cmp := compareDate(supplied.issuedAt, stored.issuedAt); cmp != 0 {
		return cmp > 0
	}
	return compareDate(supplied.expiresAt, stored.expiresAt) > 0
}

// Selects a startup client without committing a proposed credential or owner.
// Constructors repeat this selection in their publication transaction; callers
// must use the successfully constructed device's client credential afterward.
func (self *LocalState) SelectClientJwtForInstance(byClientJwt string, instanceId *Id) (string, error) {
	return self.selectClientJwtForInstance(byClientJwt, instanceId, time.Now())
}

// Takes an explicit clock for deterministic expired/unexpired boundary tests.
func (self *LocalState) selectClientJwtForInstance(byClientJwt string, instanceId *Id, now time.Time) (string, error) {
	state, err := self.loadAuthState()
	if err != nil {
		return "", err
	}
	return selectStartupClientJwt(state, byClientJwt, instanceId, now)
}

// Preparation is pure: neither token/instance bytes nor publication authority
// changes until the complete constructor can commit.
func selectStartupClientJwt(state persistedLocalAuthState, byClientJwt string, instanceId *Id, now time.Time) (string, error) {
	if byClientJwt == "" || instanceId == nil {
		return "", errors.New("client startup requires a credential and stable instance")
	}
	supplied, err := parseStartupClientJwt(byClientJwt)
	if err != nil {
		return "", err
	}
	if state.ByClientJwt == "" && state.InstanceId != "" {
		// Shipped Apple extensions used ByJwt as a client marker. Restore
		// only an exact independently supplied client for the same instance;
		// never take this legacy field as a fallback or recovered admin.
		if state.InstanceId == instanceId.String() && state.ByJwt == byClientJwt {
			return byClientJwt, nil
		}
		return "", errors.New("persisted client auth is missing its unambiguous credential")
	}
	if state.InstanceId == "" && state.ByClientJwt != "" && state.ByClientJwt != byClientJwt {
		return "", errors.New("persisted client auth has ambiguous instance ownership")
	}
	if state.InstanceId != "" {
		if _, err := ParseId(state.InstanceId); err != nil {
			return "", errors.New("persisted client auth has an invalid instance")
		}
	}
	if state.InstanceId == instanceId.String() && state.ByClientJwt != "" && state.ByClientJwt != byClientJwt {
		stored, err := parseStartupClientJwt(state.ByClientJwt)
		if err != nil {
			return "", err
		}
		if !supplied.compatibleIdentity(stored) {
			return "", errors.New("client startup changed the established instance identity")
		}
		if !preferSuppliedStartupClient(supplied, stored, now) {
			return state.ByClientJwt, nil
		}
	}
	return byClientJwt, nil
}

// Client/device must agree. Unknown networks are compatible, not equal: every
// known pair in a selection must still be compared because this is not transitive.
func (self startupClientJwt) compatibleIdentity(other startupClientJwt) bool {
	return self.clientId == other.clientId && self.deviceId == other.deviceId &&
		(self.networkId == (connect.Id{}) || other.networkId == (connect.Id{}) ||
			self.networkId == other.networkId)
}
