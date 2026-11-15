//go:build !js

// DeviceLocal as §17.10's BridgeHost on app hosts (SDK-M8-08; D-SDK-3, D-SDK-13, D-SDK-32, I-3, I-5, R-CTX-5;
// DESIGN-DETAILS 4.7's SDK-M8-08): the VPN component builds its links itself, on the forwarding client F of the link's
// network, so no link waits on the app and links build with the app's process stopped.
//
//	link       NewBridgeLink: one multi client per target (Q-BRIDGE-2) on device.BridgeLinkMultiClientSettings, a
//	           fixed window of one Required window client in ProvideMode_Network, or for a share link FriendsAndFamily
//	           with the link's aliases as C18's source half; its generator is connect's, wrapped by
//	           device.NewBridgeLinkGenerator, so every window client is signed for and held to the target's key before
//	           it can send, and a client whose signing fails is retired before it opens a contract
//	parent     the generator mints each window client through F (ForwardingClient.AuthWindowClient), F's token as
//	           bearer with F's proof and F's client as parent, so the child inherits forward_only and strict_exp and C13
//	           seals S's client as its subject (D-ACL-3). An answer that does not confirm all three marks, or a reissue
//	           of an existing client, is refused, a minted one removed
//	sign       each role's LinkSign is its Forwarder's ForwardingSigner.SignWindowDelegation, in process: subject S, the
//	           forwarding Delegation as signer, not_after the window client's strict_exp, which the signer never lets
//	           pass its own bound. With F lapsed, no Forwarder running, no current key set or no compiled state in
//	           force, it errors and the link opens no contract
//	binding    each role's LinkTargetKey reads a target's current binding through F (ForwardingClient.Binding), which
//	           the link verifies against the identity key the rules pin before it trusts the client key it names; an
//	           app host's pinned client key needs none (device/bridge_link.go)
//	resign     resignShares signs each live window client of a share's links again (Bridge.ResignShares), for when a
//	           compile brings the share a new BridgeShareSigner (§17.10). The Forwarder carries no share signer yet and
//	           its signer refuses share links (BRIDGE-M6-04), so nothing calls it and share links fail closed until it
//	           does
//	which      the role is the network whose sidecar is the link's subject, one sidecar per network; the device binds
//	           itself as every role's host once it is whole (deviceBridgeLinkHost), so a link asked for before that is
//	           refused
//
// This replaces QD-184's mint over the device channel on app hosts. No token, key or delegation reaches a log line
// (I-7). Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// What a link's window clients are minted as. Identifiers, as the device's own spec is.
const (
	deviceForwardLinkDescription = "forwarding link"
	deviceForwardLinkDeviceSpec  = "ur-forward-link"
)

var (
	// No forwarding role's sidecar is the link's subject.
	errDeviceForwardLinkNoRole = errors.New("no forwarding role holds the link's subject")
	// The role runs no forwarding client or Forwarder yet: no hand-off was kept, or the device is not built.
	errDeviceForwardLinkNoClient = errors.New("the forwarding role runs no forwarding client")
	// F's token is past its exp: the role's links sign and mint nothing until a new hand-off.
	errDeviceForwardLinkLapsed = errors.New("the forwarding client lapsed")
	// api minted a window client without forward_only, strict_exp and admin_refused, or the generator asked to reissue
	// one: a window client is always a new child of F.
	errDeviceForwardLinkMint = errors.New("a window client must be a new child of F with forward_only, strict_exp and admin_refused")
	// A delegation that names another network, or ids that do not read.
	errDeviceForwardLinkDelegation = errors.New("the link's delegation does not name this role's network and ids")
)

// What a role's links are minted under: F's client, its token and its client id, the role's network and its sidecar.
type deviceForwardLinkSource struct {
	networkId       connect.Id
	sidecarId       connect.Id
	client          *sidecar.ForwardingClient
	byJwt           string
	forwardClientId connect.Id
}

// device.BridgeHost: one link to target for the forwarding role whose sidecar is subject (the file's comment). sign is
// the link's, which the bridge hands every window client's delegation through: on app hosts the role's LinkSign.
func (self *DeviceLocal) NewBridgeLink(ctx context.Context, target connect.Id, subject connect.Id, shareAliases []netip.Prefix,
	sign func(windowClientId connect.Id, windowClientPublicKey []byte, notAfter time.Time) ([]byte, error),
) (connect.UserNatClient, error) {
	forwarding := self.forwardingHost()
	if forwarding == nil {
		return nil, fmt.Errorf("%w: the device runs no forwarding role", sidecardevice.ErrBridgeLinkRefused)
	}
	role, err := forwarding.linkRole(subject)
	if err != nil {
		return nil, errors.Join(sidecardevice.ErrBridgeLinkRefused, err)
	}
	source, err := role.linkSource()
	if err != nil {
		return nil, errors.Join(sidecardevice.ErrBridgeLinkRefused, err)
	}
	provideMode, multiClientSettings, err := deviceBridgeLinkSettings(forwarding.bridgeSettings, shareAliases)
	if err != nil {
		return nil, err
	}
	generator := forwarding.linkGenerator(ctx, self, source, target)
	return connect.NewRemoteUserNatMultiClient(ctx, sidecardevice.NewBridgeLinkGenerator(ctx, generator, sign), self.receive,
		provideMode, multiClientSettings), nil
}

var _ sidecardevice.BridgeHost = (*DeviceLocal)(nil)

// A link's provide mode and multi client settings: device.BridgeLinkMultiClientSettings, held to the links' rules, in
// ProvideMode_Network; or for a share link, whose shareAliases are not nil, ProvideMode_FriendsAndFamily with the
// aliases as SharePrivateDestinations, C18's source half, so no other private destination leaves through it.
func deviceBridgeLinkSettings(settings *sidecardevice.BridgeSettings, shareAliases []netip.Prefix) (protocol.ProvideMode, *connect.MultiClientSettings, error) {
	if settings == nil {
		settings = sidecardevice.DefaultBridgeSettings()
	}
	multiClientSettings := sidecardevice.BridgeLinkMultiClientSettings(settings)
	if err := sidecardevice.CheckBridgeLinkMultiClientSettings(multiClientSettings, settings.LinkAllowDirect); err != nil {
		return protocol.ProvideMode_None, nil, err
	}
	if shareAliases == nil {
		return protocol.ProvideMode_Network, multiClientSettings, nil
	}
	multiClientSettings.SharePrivateDestinations = append([]netip.Prefix{}, shareAliases...)
	return protocol.ProvideMode_FriendsAndFamily, multiClientSettings, nil
}

// The role whose sidecar is subject: one sidecar per network.
func (self *deviceLocalForwardingHost) linkRole(subject connect.Id) (*deviceLocalForwarding, error) {
	roles := func() []*deviceLocalForwarding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		roles := []*deviceLocalForwarding{}
		for _, role := range self.roles {
			roles = append(roles, role)
		}
		return roles
	}()
	for _, role := range roles {
		if sidecarId, ok := role.subject(); ok && sidecarId == subject {
			return role, nil
		}
	}
	return nil, errDeviceForwardLinkNoRole
}

// The connect generator a link's window clients come from: target alone, minted as children of F through F's api with
// no identity reuse (device.BridgeLinkApiMultiClientGeneratorSettings), on the device's client strategy. A test's deps
// put their own in its place.
func (self *deviceLocalForwardingHost) linkGenerator(ctx context.Context, device *DeviceLocal, source *deviceForwardLinkSource, target connect.Id) connect.MultiClientGenerator {
	credentials := &deviceForwardLinkCredentials{client: source.client, forwardClientId: source.forwardClientId}
	self.stateLock.Lock()
	deps := self.deps
	self.stateLock.Unlock()
	apiUrl, platformUrl := device.networkSpace.apiUrl, device.networkSpace.platformUrl
	if deps != nil {
		if deps.linkGenerator != nil {
			return deps.linkGenerator(ctx, source, credentials, target)
		}
		apiUrl, platformUrl = deps.apiUrl, deps.platformUrl
	}
	settings := sidecardevice.BridgeLinkApiMultiClientGeneratorSettings(nil)
	settings.ClientCredentials = credentials
	forwardClientId := source.forwardClientId
	clientStrategy := device.clientStrategy
	return connect.NewApiMultiClientGenerator(
		ctx,
		sidecardevice.BridgeLinkProviderSpecs(target),
		clientStrategy,
		nil,
		apiUrl,
		source.byJwt,
		platformUrl,
		deviceForwardLinkDescription,
		deviceForwardLinkDeviceSpec,
		device.appVersion,
		&forwardClientId,
		func() *connect.ClientSettings {
			return newDeviceClientSettings(connect.DefaultClientSettings(), apiUrl, clientStrategy)
		},
		settings,
	)
}

// The role's sidecar, false before a hand-off is kept.
func (self *deviceLocalForwarding) subject() (connect.Id, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.setup == nil {
		return connect.Id{}, false
	}
	sidecarId, err := connect.IdFromBytes(self.setup.GetPending().GetSidecarId())
	if err != nil {
		return connect.Id{}, false
	}
	return sidecarId, true
}

// What the role's links mint under now: F's client and token, refused while none runs or F lapsed.
func (self *deviceLocalForwarding) linkSource() (*deviceForwardLinkSource, error) {
	self.stateLock.Lock()
	client, setup := self.client, self.setup
	self.stateLock.Unlock()
	if client == nil || setup == nil {
		return nil, errDeviceForwardLinkNoClient
	}
	if self.lapsedNow() {
		return nil, errDeviceForwardLinkLapsed
	}
	byJwt, err := self.keys.token()
	if err != nil {
		return nil, err
	}
	if byJwt == "" {
		return nil, errDeviceForwardLinkNoClient
	}
	sidecarId, sidecarErr := connect.IdFromBytes(setup.GetPending().GetSidecarId())
	// F's own client, as the hand-off names it: a refresh keeps it
	forwardClientId, clientErr := connect.IdFromBytes(setup.GetForwardClientId())
	if sidecarErr != nil || clientErr != nil {
		return nil, errDeviceForwardLinkNoClient
	}
	return &deviceForwardLinkSource{
		networkId:       self.networkId,
		sidecarId:       sidecarId,
		client:          client,
		byJwt:           byJwt,
		forwardClientId: forwardClientId,
	}, nil
}

// The role's LinkSign (the file's sign row): the Forwarder's signer over the delegation the link asks for, refused while
// F lapsed, while no Forwarder runs, and wherever the signer refuses.
func (self *deviceLocalForwarding) signWindowDelegation(ctx context.Context, delegation *bridge.WindowDelegation) ([]byte, error) {
	if self.lapsedNow() {
		return nil, errDeviceForwardLinkLapsed
	}
	forwarder := self.currentForwarder()
	if forwarder == nil {
		return nil, errDeviceForwardLinkNoClient
	}
	signer, err := forwarder.NewSigner(nil)
	if err != nil {
		return nil, err
	}
	return deviceForwardSignWindowDelegation(signer, self.networkId, delegation)
}

// delegation as the forwarding signer's request: this role's network, its ids, and not_after the window client's
// strict_exp, which the signer refuses past its own bound rather than signing it.
func deviceForwardSignWindowDelegation(signer *sidecar.ForwardingSigner, networkId connect.Id, delegation *bridge.WindowDelegation) ([]byte, error) {
	if delegation == nil {
		return nil, errDeviceForwardLinkDelegation
	}
	delegationNetworkId, networkErr := sidecar.IdFromBytes(delegation.GetNetworkId())
	subjectId, subjectErr := sidecar.IdFromBytes(delegation.GetSubjectSidecarId())
	windowClientId, windowClientErr := sidecar.IdFromBytes(delegation.GetWindowClientId())
	targetId, targetErr := sidecar.IdFromBytes(delegation.GetTargetSidecarId())
	if networkErr != nil || subjectErr != nil || windowClientErr != nil || targetErr != nil || delegationNetworkId != sidecar.Id(networkId) {
		return nil, errDeviceForwardLinkDelegation
	}
	request := &sidecar.ForwardingSignRequest{
		NetworkId:             delegationNetworkId,
		SubjectSidecarId:      subjectId,
		WindowClientId:        windowClientId,
		WindowClientPublicKey: delegation.GetWindowClientPublicKey(),
		TargetSidecarId:       targetId,
		NotAfter:              time.UnixMilli(delegation.GetNotAfterMs()),
	}
	if shareId := delegation.GetShareId(); 0 < len(shareId) {
		parsed, err := sidecar.IdFromBytes(shareId)
		if err != nil {
			return nil, errDeviceForwardLinkDelegation
		}
		request.ShareId = parsed
	}
	return signer.SignWindowDelegation(request)
}

// The role's LinkTargetKey (the file's binding row): the target's current binding through F.
func (self *deviceLocalForwarding) targetBinding(ctx context.Context, targetSidecarId connect.Id, targetClientId connect.Id) ([]byte, error) {
	self.stateLock.Lock()
	client := self.client
	self.stateLock.Unlock()
	if client == nil {
		return nil, errDeviceForwardLinkNoClient
	}
	return client.Binding(ctx, sidecar.Id(targetSidecarId))
}

// Signs each live window client of the shares' links again (the file's resign row), through the role's bridge.
func (self *deviceLocalForwarding) resignShares(shareIds []connect.Id) error {
	return self.sink.onRole(false, func(bridge deviceBridgeOwned) error {
		return bridge.ResignShares(shareIds)
	})
}

// connect.NetworkClientCredentials over F (the file's parent row).
type deviceForwardLinkCredentials struct {
	client          *sidecar.ForwardingClient
	forwardClientId connect.Id
}

var _ connect.NetworkClientCredentials = (*deviceForwardLinkCredentials)(nil)

// A new window client, F's child: F as parent whatever the generator names, a reissue refused, and an answer that does
// not confirm forward_only, strict_exp and admin_refused removed and refused.
func (self *deviceForwardLinkCredentials) AuthNetworkClient(ctx context.Context, args *connect.AuthNetworkClientArgs) (*connect.AuthNetworkClientResult, error) {
	if args == nil || args.ClientId != nil {
		return nil, errDeviceForwardLinkMint
	}
	if args.SourceClientId != nil && *args.SourceClientId != self.forwardClientId {
		return nil, errDeviceForwardLinkMint
	}
	parentId := sidecar.Id(self.forwardClientId)
	result, err := self.client.AuthWindowClient(ctx, &sidecar.SidecarAuthClientArgs{
		SourceClientId:   &parentId,
		Description:      args.Description,
		DeviceSpec:       args.DeviceSpec,
		DeclaresNoEgress: true,
	})
	if err != nil {
		apiError := &sidecar.SidecarApiError{}
		if errors.As(err, &apiError) && apiError.Status == 200 {
			switch apiError.Code {
			case "client_limit_exceeded":
				return &connect.AuthNetworkClientResult{Error: &connect.AuthNetworkClientError{ClientLimitExceeded: true, Message: apiError.Code}}, nil
			case "upgrade_required":
				return &connect.AuthNetworkClientResult{Error: &connect.AuthNetworkClientError{UpgradeRequired: true, Message: apiError.Code}}, nil
			}
		}
		return nil, err
	}
	if !result.ForwardOnly || !result.StrictExp || !result.AdminRefused {
		if result.ClientId != (sidecar.Id{}) {
			// best effort: an unremoved child ends at its strict_exp
			self.client.RemoveWindowClient(ctx, result.ClientId)
		}
		return nil, errDeviceForwardLinkMint
	}
	return &connect.AuthNetworkClientResult{ByClientJwt: result.ByClientJwt}, nil
}

// Removes a window client a link retired.
func (self *deviceForwardLinkCredentials) RemoveNetworkClient(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
	if args == nil {
		return nil, errDeviceForwardLinkMint
	}
	if err := self.client.RemoveWindowClient(ctx, sidecar.Id(args.ClientId)); err != nil {
		return nil, err
	}
	return &connect.RemoveNetworkClientResult{}, nil
}
