//go:build !ios_extension && !js

// NewSidecarRemote (§18.5, D-SDK-2, R-SDK-9): a desktop app's Sidecar, whose core runs in `ur daemon` and is reached
// over §5.8's control mode. The app holds no sidecar key, token or state: every call forwards, and the state and the
// sidecar_id it names are the daemon's (sidecar_backend.go).
//
//	no daemon  the Sidecar reads offline and starts nothing: it embeds no local core and spawns no daemon
//	           (plan-SDK-small-readings #9). It dials again at each status read and each call
//	pairing    EnrollWithDevice: the app's device token mints the child, admin_refused and strict_exp, the app gives
//	           it to the daemon through pair, and the daemon binds generation n+1 under the sidecar_id it has (§5.8).
//	           The daemon never holds the device's token, and the app keeps no copy of the child's. A child the
//	           daemon did not take is removed with the device's token
//	Unlock     refused: the daemon unlocks its own keychain (§5.9), so no pass ever crosses this backend
//	push       refused: the daemon registers no app wake token (sidecar_push.go)
//	events     state alone, from the status reads, each naming its generation; §5.8 has no method that carries the
//	           core's other events yet (question sdk-m8-02-remote-events)
//	forwarding the paired app relays its VPN component's bridge.ForwardingKeys out and the daemon's
//	           bridge.ForwardingHandOff back (§18.6; BRIDGE-M8-06), public halves and the hand-off alone, never a key.
//	           SDK-M8-07's setup run calls these
//	confirm    control mode grants nothing: a call §5.8 signs only on a person's confirmation goes with confirmed
//	           set by a wrapper whose bound call is that person's act in the app's UI (D-CORE-13), never from here
//
// Components name wrappers over the daemon's engines, and none is bound on this backend yet, so a Sidecar named with
// one is refused rather than opened without it.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A child api minted without the marks a daemon's client carries (§7.4): nothing is paired and the child is removed.
var errSidecarRemoteChildMarks = errors.New("api minted the daemon's client without admin_refused and strict_exp")

// A forwarding call on a Sidecar whose core runs in this process: the local backend sets forwarding up itself.
var errSidecarNotRemote = errors.New("this sidecar's core runs in this process, not in a daemon")

// What a test adjusts in a remote Sidecar. Production passes the zero value.
type sidecarRemoteOptions struct {
	// Nil: the instance's control socket (sidecarDaemonControlDial).
	dial sidecarControlDial
	// Nil: defaultSidecarRemoteSettings.
	remoteSettings *sidecarRemoteSettings
	// Nil: defaultSidecarEventSettings.
	eventSettings *sidecarEventSettings
	// The test reads the daemon's status itself: no read runs on its own.
	noStatusPoll bool
}

// A Sidecar on the remote backend: the daemon of settings' Instance, reached on §5.9's socket or, with
// DaemonSocketDir, the one in a sandboxed app's app-group container. presenceSigner may be nil, in which case another
// device approves. With no daemon the Sidecar is offline, and nothing is started.
func NewSidecarRemote(presenceSigner SidecarPresenceSigner, settings *SidecarSettings) (*Sidecar, error) {
	return traceWithReturnError(func() (*Sidecar, error) {
		return newSidecarRemote(context.Background(), presenceSigner, settings, &sidecarRemoteOptions{})
	})
}

// NewSidecarRemote with a test's options.
func newSidecarRemote(ctx context.Context, presenceSigner SidecarPresenceSigner, settings *SidecarSettings, options *sidecarRemoteOptions) (*Sidecar, error) {
	if settings == nil {
		return nil, errors.New("a remote sidecar needs settings")
	}
	// no wrapper is bound over the daemon yet, so every component name refuses
	noFactory := func(name string) (sidecarComponentFactory, bool) {
		return nil, false
	}
	settings, err := checkedSidecarSettingsWith(settings, false, noFactory)
	if err != nil {
		return nil, err
	}
	if settings.DaemonSocketDir != "" && !filepath.IsAbs(settings.DaemonSocketDir) {
		return nil, errors.New("a remote sidecar's DaemonSocketDir must be an absolute directory")
	}
	dial := options.dial
	if dial == nil {
		dial, err = sidecarDaemonControlDial(settings)
		if err != nil {
			return nil, err
		}
	}
	remoteSettings := options.remoteSettings
	if remoteSettings == nil {
		remoteSettings = defaultSidecarRemoteSettings()
	}
	eventSettings := options.eventSettings
	if eventSettings == nil {
		eventSettings = defaultSidecarEventSettings()
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	events := newSidecarEventDispatch(cancelCtx, eventSettings)
	backend := newSidecarRemoteBackend(cancelCtx, dial, remoteSettings, func(generation int64, state string, reason string) {
		events.offer(sidecar.SidecarEvent{
			Kind:       sidecar.SidecarEventState,
			Generation: generation,
			State:      sidecar.SidecarState(state),
			Reason:     sidecar.SidecarStateReason(reason),
		})
	})
	self := &Sidecar{
		ctx:                cancelCtx,
		cancel:             cancel,
		settings:           settings,
		presenceSigner:     presenceSigner,
		events:             events,
		closeDone:          make(chan struct{}),
		remote:             backend,
		components:         map[string]sidecar.SidecarComponent{},
		componentFactoryOf: noFactory,
	}
	if !options.noStatusPoll {
		go backend.run()
	}
	return self, nil
}

// EnrollWithDevice on the remote backend (§5.8's pair row): the device's Api mints the child on its own token, the
// daemon takes it through pair and binds the next generation under its sidecar_id, and the answer is that sidecar_id.
// The daemon must answer before anything is minted, so a daemon that is not there leaves no client behind, and a child
// the daemon did not take is removed.
func (self *Sidecar) pairWithDeviceApi(ctx context.Context, remote sidecarBackend, deviceApi *Api) (string, error) {
	if deviceApi == nil || deviceApi.GetByJwt() == "" {
		return "", errors.New("the device holds no client token to mint the daemon's client with")
	}
	if err := remote.refresh(ctx); err != nil {
		return "", err
	}
	child, err := mintSidecarChild(ctx, deviceApi)
	if err != nil {
		return "", err
	}
	removeChild := func() {
		if child.ClientId == (sidecar.Id{}) {
			return
		}
		// best effort: an unbound strict_exp child that stays ends at its exp (§7.4)
		removeSidecarChild(ctx, deviceApi, newId([16]byte(child.ClientId)))
	}
	if !child.AdminRefused || !child.StrictExp {
		removeChild()
		return "", errSidecarRemoteChildMarks
	}
	_, err = remote.pair(ctx, child.ByClientJwt)
	// the daemon holds the child's token now, or nobody does
	child.ByClientJwt = ""
	if err != nil {
		removeChild()
		return "", err
	}
	if err := remote.refresh(ctx); err != nil {
		return "", err
	}
	sidecarId := remote.sidecarId()
	if sidecarId == nil {
		return "", errors.New("the daemon paired and names no sidecar_id")
	}
	return sidecarId.String(), nil
}

// POST /network/remove-client on the device's token, for a child the daemon did not take.
func removeSidecarChild(ctx context.Context, deviceApi *Api, clientId *Id) error {
	_, err := connect.HttpPostWithRawFunction(
		ctx,
		deviceApi.getHttpPostRaw(),
		fmt.Sprintf("%s/network/remove-client", deviceApi.apiUrl),
		&RemoveNetworkClientArgs{ClientId: clientId},
		deviceApi.GetByJwt(),
		&RemoveNetworkClientResult{},
		connect.NewNoopApiCallback[*RemoveNetworkClientResult](),
	)
	return err
}

// The remote backend's forwarding relay, or errSidecarNotRemote on the local backend.
func (self *Sidecar) remoteForwarding() (*sidecarRemoteBackend, error) {
	_, remote := self.backends()
	backend, ok := remote.(*sidecarRemoteBackend)
	if !ok || backend == nil {
		return nil, errSidecarNotRemote
	}
	return backend, nil
}

// Relays forwarding's setup for a paired desktop app's VPN component to the daemon (§18.6; bridge.forwarding.setup):
// the component's keys out, public halves only, and the daemon's hand-off back, for that component alone. Go only:
// SDK-M8-07's setup run calls it. confirmAfterRevocation is the person's confirmation, in the app's UI, of setting
// forwarding up again after another signer's revocation; nothing else sets it.
func (self *Sidecar) relayForwardingSetup(ctx context.Context, keys *bridge.ForwardingKeys, confirmAfterRevocation bool) (*bridge.ForwardingHandOff, error) {
	backend, err := self.remoteForwarding()
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, errors.New("forwarding's setup takes the component's keys")
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(keys)
	if err != nil {
		return nil, err
	}
	reply, err := backend.forwardingSetup(ctx, encoded, confirmAfterRevocation)
	if err != nil {
		return nil, err
	}
	handOff := &bridge.ForwardingHandOff{}
	if err := proto.Unmarshal(reply, handOff); err != nil {
		// the bytes may hold F's token: the error names nothing of them (I-7)
		return nil, errors.New("the daemon's hand-off does not read")
	}
	return handOff, nil
}

// What the daemon's sidecar witnessed for the app's VPN component (§18.6; bridge.forwarding.witness).
type sidecarForwardingWitness struct {
	Pin       *bridge.AuthPin
	Witnessed bool
	Fork      bool
	// Why a setup is due, a sidecar.ForwardingSetupReason's text; empty when none is.
	SetupDue string
}

// Relays forwarding's witness hint from the daemon. Go only: SDK-M8-07 hands it to the VPN component.
func (self *Sidecar) relayForwardingWitness(ctx context.Context) (*sidecarForwardingWitness, error) {
	backend, err := self.remoteForwarding()
	if err != nil {
		return nil, err
	}
	reply, err := backend.forwardingWitness(ctx)
	if err != nil {
		return nil, err
	}
	pin := &bridge.AuthPin{}
	if err := proto.Unmarshal(reply.Pin, pin); err != nil {
		return nil, errors.New("the daemon's witnessed pin does not read")
	}
	return &sidecarForwardingWitness{Pin: pin, Witnessed: reply.Witnessed, Fork: reply.Fork, SetupDue: reply.SetupDue}, nil
}
