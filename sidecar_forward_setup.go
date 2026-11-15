//go:build !ios_extension && !js

// Forwarding's setup from the sidecar role (§18.6's Setup, DESIGN-DETAILS 4.2.6's S steps; D-SDK-32, I-7, owner
// decision 197; SDK-M8-07): the sidecar role's one act for its VPN component. After EnrollWithDevice, and whenever the
// role finds a setup due while it knows that device, it asks the component for its keys' public halves, has its core
// run Sidecar.SetUpForwarding for them (the forwarding Delegation, its key at gkm, its wrap, F's mint), and hands the
// component the answer; on the remote backend the paired daemon runs the setup (bridge.forwarding.setup) and the app
// relays the hand-off. No key of the sidecar's crosses and none of the component's: the keys are public halves and the
// hand-off holds F's token, which goes to the component alone and reaches no log (I-7).
//
//	when      the core's ForwardingSetupDue: a new client generation, the 90-day rotation, no live Delegation; after
//	          another signer revoked the Delegation (foreign_revocation) only once the person confirms in the app, which
//	          the role offers by raising bridge_alarm forwarding_revoked, and ConfirmForwardingSetup runs it
//	witness   every WitnessInterval while the role knows the device, its witnessed pin and fork go to the component as
//	          a hint (ForwardingWitness) when either moved; a due setup found then runs too
//	device    the app's Device given to EnrollWithDevice, which carries the VPN component (DeviceLocal in process on
//	          Android, DeviceRemote over the device RPC to the Apple extensions): a role that has none since its start,
//	          as after a restart before the app enrolls again, runs nothing, since no other bound call names the device
//	          (DESIGN-DETAILS 4.7: setup has no other app-facing API; question sdk-m8-07-setup-device-at-start)
//
// The core's setup refuses where its host installed no setup sources (ErrForwardingSetupUnavailable), which an app
// host's core does not yet compose (its config follower, gkm and statement poster, as for evs): the run reports it and
// nothing is handed off. Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The bridge_alarm reason that offers ConfirmForwardingSetup: another signer revoked this sidecar's forwarding
// Delegation, and setting it up again waits for the person (DESIGN-DETAILS 4.2.6's when row). A format value.
const SidecarBridgeAlarmForwardingRevoked = "forwarding_revoked"

// What forwarding's setup asks of the sidecar role's core: *sidecar.Sidecar, or a test's.
type sidecarForwardingSetupCore interface {
	ForwardingSetupDue(networkId sidecar.Id) (sidecar.ForwardingSetupReason, error)
	ConfirmForwardingSetup(networkId sidecar.Id) error
	SetUpForwarding(ctx context.Context, networkId sidecar.Id, keys *bridge.ForwardingKeys) (*bridge.ForwardingHandOff, error)
	ForwardingWitness(networkId sidecar.Id) (*sidecar.ForwardingWitness, error)
}

var _ sidecarForwardingSetupCore = (*sidecar.Sidecar)(nil)

// What the setup run is held to.
type sidecarForwardingSetupSettings struct {
	// The bound of one setup, its Delegation's landing and F's mint included: 3 min.
	SetupTimeout time.Duration
	// How often the witness hint is read and sent while the role knows the device: 5 min.
	WitnessInterval time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarForwardingSetupSettings() *sidecarForwardingSetupSettings {
	return &sidecarForwardingSetupSettings{
		SetupTimeout:    3 * time.Minute,
		WitnessInterval: 5 * time.Minute,
	}
}

// A Sidecar's forwarding setup state: the device, the run in flight and the last witness sent.
type sidecarForwarding struct {
	settings *sidecarForwardingSetupSettings
	// a test's core in place of the local backend's, nil otherwise
	core sidecarForwardingSetupCore

	// one setup at a time
	runLock sync.Mutex

	stateLock sync.Mutex
	device    deviceForwardingChannel
	cancel    context.CancelFunc
	// the pin and fork last sent, so a witness goes only when either moved
	sentPinVersion uint64
	sentFork       bool
	sentOnce       bool
	// a hand-off the component did not take, held in memory alone and handed again at each witness until it is: the
	// device RPC was down or the component's process restarting (§18.6's after row)
	pendingHandOff *bridge.ForwardingHandOff
}

// Hands handOff to device, keeping it for the next witness when the component cannot be reached; a refusal of the
// component's own drops it, since the same bytes would be refused again.
func (self *sidecarForwarding) deliver(device deviceForwardingChannel, handOff *bridge.ForwardingHandOff) error {
	err := device.forwardingHandOff(handOff)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case err == nil:
		self.pendingHandOff = nil
	case errors.Is(err, errDeviceForwardingUnreachable):
		self.pendingHandOff = handOff
	default:
		self.pendingHandOff = nil
	}
	return err
}

// Hands a kept hand-off again, answering whether the component took it.
func (self *sidecarForwarding) deliverPending(device deviceForwardingChannel) bool {
	self.stateLock.Lock()
	handOff := self.pendingHandOff
	self.stateLock.Unlock()
	if handOff == nil {
		return false
	}
	return self.deliver(device, handOff) == nil
}

// The Sidecar's forwarding state, made at the first use.
func (self *Sidecar) forwardingState() *sidecarForwarding {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.forwarding == nil {
		self.forwarding = &sidecarForwarding{settings: defaultSidecarForwardingSetupSettings()}
	}
	return self.forwarding
}

// Keeps device as the one the VPN component runs in and starts the witness loop, once, then runs a setup: after an
// enrollment or a pairing a new client generation makes one due. A device with no forwarding channel is ignored.
func (self *Sidecar) attachForwardingDevice(device Device) {
	channel, ok := device.(deviceForwardingChannel)
	if !ok || channel == nil {
		return
	}
	forwarding := self.forwardingState()
	started := func() bool {
		forwarding.stateLock.Lock()
		defer forwarding.stateLock.Unlock()
		forwarding.device = channel
		if forwarding.cancel != nil {
			return false
		}
		ctx, cancel := context.WithCancel(self.ctx)
		forwarding.cancel = cancel
		go self.runForwardingWitness(ctx)
		return true
	}()
	if !started {
		go self.runForwardingSetup(false)
	}
}

// The network the setup is for, and the core: the local backend's, or the test's; on the remote backend the paired
// network and no core, since the daemon runs the setup.
func (self *Sidecar) forwardingNetwork() (sidecar.Id, sidecarForwardingSetupCore, error) {
	local, remote := self.backends()
	core := self.forwardingState().core
	if core == nil && local != nil {
		core = local
	}
	if core == nil {
		backend, ok := remote.(*sidecarRemoteBackend)
		if !ok {
			return sidecar.Id{}, nil, sidecar.ErrSidecarClosed
		}
		networkId, err := sidecar.ParseId(backend.network())
		if err != nil {
			return sidecar.Id{}, nil, errors.New("the daemon has paired no network yet")
		}
		return networkId, nil, nil
	}
	named, ok := core.(interface{ NetworkId() *sidecar.Id })
	if !ok || named.NetworkId() == nil {
		return sidecar.Id{}, nil, errors.New("the sidecar has enrolled in no network yet")
	}
	return *named.NetworkId(), core, nil
}

// One setup run (the file's when row): the component's keys, the core's or the daemon's setup, and the hand-off to
// the component. confirmed carries the person's confirmation after another signer's revocation. Answers why nothing
// was handed off, nil where nothing was due or the hand-off was taken.
func (self *Sidecar) runForwardingSetup(confirmed bool) error {
	forwarding := self.forwardingState()
	forwarding.runLock.Lock()
	defer forwarding.runLock.Unlock()
	forwarding.stateLock.Lock()
	device := forwarding.device
	forwarding.stateLock.Unlock()
	if device == nil {
		return errors.New("no device of the app's VPN component is known")
	}
	ctx, cancel := context.WithTimeout(self.ctx, forwarding.settings.SetupTimeout)
	defer cancel()
	networkId, core, err := self.forwardingNetwork()
	if err != nil {
		return err
	}
	if core == nil {
		return self.runRemoteForwardingSetup(ctx, device, networkId, confirmed)
	}
	reason, err := core.ForwardingSetupDue(networkId)
	if err != nil {
		return err
	}
	switch {
	case reason == sidecar.ForwardingSetupForeignRevocation && !confirmed:
		self.raiseForwardingRevoked(networkId)
		return sidecar.ErrForwardingSetupNeedsConfirm
	case reason == sidecar.ForwardingSetupNotDue && !confirmed:
		return nil
	}
	keys, err := device.forwardingKeys(connect.Id(networkId))
	if err != nil {
		return fmt.Errorf("the VPN component's keys: %w", err)
	}
	handOff, err := core.SetUpForwarding(ctx, networkId, keys)
	if err != nil {
		if errors.Is(err, sidecar.ErrForwardingSetupNeedsConfirm) {
			self.raiseForwardingRevoked(networkId)
		}
		return err
	}
	return forwarding.deliver(device, handOff)
}

// The remote backend's run: the daemon says what is due in its witness, runs the setup, and the app relays the
// hand-off to its VPN component.
func (self *Sidecar) runRemoteForwardingSetup(ctx context.Context, device deviceForwardingChannel, networkId sidecar.Id, confirmed bool) error {
	witness, err := self.relayForwardingWitness(ctx)
	if err != nil {
		return err
	}
	switch {
	case witness.SetupDue == string(sidecar.ForwardingSetupForeignRevocation) && !confirmed:
		self.raiseForwardingRevoked(networkId)
		return sidecar.ErrForwardingSetupNeedsConfirm
	case witness.SetupDue == "" && !confirmed:
		return nil
	}
	keys, err := device.forwardingKeys(connect.Id(networkId))
	if err != nil {
		return fmt.Errorf("the VPN component's keys: %w", err)
	}
	handOff, err := self.relayForwardingSetup(ctx, keys, confirmed)
	if err != nil {
		return err
	}
	return self.forwardingState().deliver(device, handOff)
}

// Offers ConfirmForwardingSetup: bridge_alarm forwarding_revoked for networkId.
func (self *Sidecar) raiseForwardingRevoked(networkId sidecar.Id) {
	self.events.offer(sidecar.SidecarEvent{
		Kind:        sidecar.SidecarEventBridgeAlarm,
		BridgeAlarm: sidecar.SidecarBridgeAlarm(SidecarBridgeAlarmForwardingRevoked),
	})
}

// Sends the witness hint while the role knows the device: every WitnessInterval, and a due setup found then runs.
func (self *Sidecar) runForwardingWitness(ctx context.Context) {
	forwarding := self.forwardingState()
	for {
		self.sendForwardingWitness(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(forwarding.settings.WitnessInterval):
		}
	}
}

// One witness: the core's or the daemon's pin and fork to the component when either moved, and a setup when one is
// due. Answers whether a hint went.
func (self *Sidecar) sendForwardingWitness(ctx context.Context) bool {
	forwarding := self.forwardingState()
	forwarding.stateLock.Lock()
	device := forwarding.device
	forwarding.stateLock.Unlock()
	if device == nil {
		return false
	}
	forwarding.deliverPending(device)
	networkId, core, err := self.forwardingNetwork()
	if err != nil {
		return false
	}
	var pin *bridge.AuthPin
	var fork bool
	var due string
	if core != nil {
		witness, err := core.ForwardingWitness(networkId)
		if err != nil {
			return false
		}
		pin, fork, due = witness.Pin, witness.Fork, string(witness.SetupDue)
	} else {
		witness, err := self.relayForwardingWitness(ctx)
		if err != nil {
			return false
		}
		pin, fork, due = witness.Pin, witness.Fork, witness.SetupDue
	}
	if due != "" {
		go self.runForwardingSetup(false)
	}
	moved := func() bool {
		forwarding.stateLock.Lock()
		defer forwarding.stateLock.Unlock()
		if forwarding.sentOnce && forwarding.sentPinVersion == pin.GetVersion() && forwarding.sentFork == fork {
			return false
		}
		return true
	}()
	if !moved || pin == nil {
		return false
	}
	if err := device.forwardingWitness(connect.Id(networkId), pin, fork); err != nil {
		return false
	}
	forwarding.stateLock.Lock()
	forwarding.sentOnce, forwarding.sentPinVersion, forwarding.sentFork = true, pin.GetVersion(), fork
	forwarding.stateLock.Unlock()
	return true
}

// The person's confirmation in the app, after bridge_alarm forwarding_revoked, that forwarding may be set up again in
// networkId after another signer revoked it (DESIGN-DETAILS 4.2.6): the core or the daemon records it, and the setup
// runs. A person's act in the app's UI, never MCP's. The result is Ok once the component took the hand-off.
func (self *Sidecar) ConfirmForwardingSetup(networkId *Id, callback SidecarResultCallback) {
	trace(func() {
		go func() {
			result, err := traceWithReturnError(func() (*SidecarResult, error) {
				return sidecarResultOf("", self.confirmForwardingSetup(networkId)), nil
			})
			if callback != nil {
				callback.Result(result, err)
			}
		}()
	})
}

// ConfirmForwardingSetup's work.
func (self *Sidecar) confirmForwardingSetup(networkId *Id) error {
	if networkId == nil {
		return errors.New("a confirmation names its network")
	}
	followed, core, err := self.forwardingNetwork()
	if err != nil {
		return err
	}
	if followed != sidecar.Id(networkId.id) {
		return errors.New("this sidecar's network is another")
	}
	if core != nil {
		if err := core.ConfirmForwardingSetup(followed); err != nil {
			return err
		}
	}
	return self.runForwardingSetup(true)
}

// Ends the witness loop: the Sidecar closed.
func (self *Sidecar) stopForwarding() {
	self.stateLock.Lock()
	forwarding := self.forwarding
	self.stateLock.Unlock()
	if forwarding == nil {
		return
	}
	forwarding.stateLock.Lock()
	defer forwarding.stateLock.Unlock()
	if forwarding.cancel != nil {
		forwarding.cancel()
	}
}
