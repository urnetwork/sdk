//go:build !ios_extension && !js

// EnrollWithDevice (§18.5, §5.6's sdk host row, §7.4; CORE-M8-01): an app's sidecar enrolls as a child of its device
// client, admin_refused and strict_exp, so the sidecar role never holds the device's token or a network credential
// (I-6). The core runs Enroll's order, failing closed: its keys in memory, then the mint, which this file makes on the
// device's own Api with the device's token, naming nothing but the marks, then the binding, and only once the mint's
// answer confirmed admin_refused and strict_exp the keychain and sidecar.json; without both marks it keeps nothing and
// removes the child. A re-pair, a later EnrollWithDevice on an enrolled sidecar, keeps the sidecar_id and identity key
// and binds generation n+1 on a new client (identity_enroll_child.go).
//
// The child's token goes into a new Api session, a session of the device's Api with none of its credentials, which
// becomes the host's TokenSource before the enrollment runs, so the generation that enrollment starts finds it; a
// refused enrollment drops it and keeps the session that was there. The device's Api never holds the child's token.
//
// The remote backend pairs instead (§5.8's pairing, SDK-M8-02), and forwarding's setup for the app's VPN component
// after an enrollment is SDK-M8-07's wiring (DESIGN-DETAILS 4.7).

package sdk

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The remote backend pairs with its daemon rather than enroll a child here.
var errSidecarRemoteEnroll = errors.New("a remote-mode sidecar pairs with its daemon (§5.8)")

// The child client's description and spec, as api lists them beside the device's.
const (
	sidecarChildDescription = "sidecar"
	sidecarChildDeviceSpec  = "sdk"
)

// POST /network/auth-client's body for an app's sidecar: a child of the bearer's client, with §7.4's marks and §20.8's
// declaration.
type sidecarChildMintArgs struct {
	Description      string `json:"description"`
	DeviceSpec       string `json:"device_spec"`
	AdminRefused     bool   `json:"admin_refused"`
	StrictExp        bool   `json:"strict_exp"`
	DeclaresNoEgress bool   `json:"declares_no_egress"`
}

// Its answer.
type sidecarChildMintResult struct {
	ByClientJwt  string                  `json:"by_client_jwt,omitempty"`
	ClientId     *Id                     `json:"client_id,omitempty"`
	AdminRefused bool                    `json:"admin_refused,omitempty"`
	StrictExp    bool                    `json:"strict_exp,omitempty"`
	Error        *AuthNetworkClientError `json:"error,omitempty"`
}

// Enrolls this sidecar as a child of device's client, and answers with its sidecar_id. A refusal's result carries
// the core's message; nothing is kept.
func (self *Sidecar) EnrollWithDevice(device Device, callback SidecarResultCallback) {
	trace(func() {
		go func() {
			result, err := traceWithReturnError(func() (*SidecarResult, error) {
				if device == nil {
					return sidecarResultOf("", errors.New("an enrollment needs the app's device")), nil
				}
				return sidecarResultOf(self.enrollWithDeviceApi(self.ctx, device.GetApi())), nil
			})
			if callback != nil {
				callback.Result(result, err)
			}
		}()
	})
}

// EnrollWithDevice on the device's Api.
func (self *Sidecar) enrollWithDeviceApi(ctx context.Context, deviceApi *Api) (string, error) {
	local, remote := self.backends()
	switch {
	case remote != nil:
		return "", errSidecarRemoteEnroll
	case local == nil || self.host == nil:
		return "", sidecar.ErrSidecarClosed
	case deviceApi == nil || deviceApi.GetByJwt() == "":
		return "", errors.New("the device holds no client token to mint the sidecar's client with")
	}
	session := newSidecarApiSession(deviceApi.newSession(self.ctx))
	previous := self.host.swapSession(session)
	err := local.Enroll(ctx, &sidecar.SidecarEnrollCredential{
		MintChild: func(ctx context.Context, clientPublicKey []byte) (*sidecar.SidecarAuthClientResult, error) {
			result, err := mintSidecarChild(ctx, deviceApi)
			if err != nil {
				return nil, err
			}
			// only a child the core will keep is ever presented by the session
			if result.AdminRefused && result.StrictExp {
				session.install(result.ByClientJwt)
			}
			return result, nil
		},
	})
	if err != nil {
		self.host.swapSession(previous)
		session.close()
		return "", err
	}
	if previous != nil {
		previous.close()
	}
	identity, err := awaitSidecarIdentity(ctx, local)
	if err != nil {
		return "", err
	}
	return identity.SidecarId.String(), nil
}

// The identity an enrollment made, once the core's run loop has adopted it, which it does before the new
// generation's first state event.
func awaitSidecarIdentity(ctx context.Context, local *sidecar.Sidecar) (*sidecar.SidecarIdentity, error) {
	changed := make(chan struct{}, 1)
	remove := local.AddEventCallback(func(event sidecar.SidecarEvent) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer remove()
	for {
		if identity := local.Identity(); identity != nil {
			return identity, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// POST /network/auth-client on the device's token: a new child of the device's client, admin_refused, strict_exp and
// declaring no egress. Only the child's answer comes back; the device's token stays its Api's.
func mintSidecarChild(ctx context.Context, deviceApi *Api) (*sidecar.SidecarAuthClientResult, error) {
	reply, err := connect.HttpPostWithRawFunction(
		ctx,
		deviceApi.getHttpPostRaw(),
		fmt.Sprintf("%s/network/auth-client", deviceApi.apiUrl),
		&sidecarChildMintArgs{
			Description:      sidecarChildDescription,
			DeviceSpec:       sidecarChildDeviceSpec,
			AdminRefused:     true,
			StrictExp:        true,
			DeclaresNoEgress: true,
		},
		deviceApi.GetByJwt(),
		&sidecarChildMintResult{},
		connect.NewNoopApiCallback[*sidecarChildMintResult](),
	)
	if err != nil {
		return nil, err
	}
	switch {
	case reply.Error != nil && reply.Error.UpgradeRequired:
		return nil, errors.New("the network is at its plan's client limit")
	case reply.Error != nil && reply.Error.ClientLimitExceeded:
		return nil, errors.New("the network has no room for another client")
	case reply.Error != nil:
		return nil, errors.New("api refused the sidecar's client")
	case reply.ByClientJwt == "":
		return nil, errors.New("api answered no token for the sidecar's client")
	}
	result := &sidecar.SidecarAuthClientResult{
		ByClientJwt:  reply.ByClientJwt,
		AdminRefused: reply.AdminRefused,
		StrictExp:    reply.StrictExp,
	}
	if reply.ClientId != nil {
		result.ClientId = sidecar.Id(reply.ClientId.id)
	}
	return result, nil
}
