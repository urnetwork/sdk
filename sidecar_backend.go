//go:build !ios_extension

// The backend seam behind sdk.Sidecar (D-SDK-2; SDK-M8-02) and the remote backend, which forwards to `ur daemon`'s
// control mode (§5.8) and holds no key, token or state of its own.
//
//	channel   one control session: JSON-RPC 2.0 calls, one at a time, as the daemon answers them. On a desktop it is
//	          the root's DaemonClient on the instance's socket or a sandboxed app's app-group socket
//	          (sidecar_remote_dial.go); on js a host-supplied `ur native-host` port (sidecar_remote_js.go). This file
//	          links no sidecar package, so sdk.wasm shares it and stays under its gate (plan-SDK-small-readings #7)
//	state     the daemon's, read by its status method and never written here: locked, unenrolled, offline, online or
//	          revoked with its reason, as the daemon's core decided them (D-SDK-14). With no daemon answering the
//	          backend reads offline and starts nothing (R-SDK-9, plan-SDK-small-readings #9)
//	network   the one the pairing bound (pair's reply), else the daemon's only network, else its first
//	generation every status names its generation, and one older than the newest this backend took for the same
//	          sidecar_id is dropped, so an answer that crossed a re-pair cannot end the new pairing (D-SDK-14)
//	replies   only the fields this file names are read; nothing else of a reply reaches an app. A refusal's text that
//	          repeats the child's token is replaced before it is surfaced (I-7)
//
// The method names are their owners' (ur daemon's status, CORE-M8-02's pair, BRIDGE-M8-06's forwarding methods),
// spelled here because the js leg cannot import them; sidecar_remote_test.go holds them equal.

package sdk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// What a Sidecar's calls reach when its core runs in another process: the remote backend. The local backend's calls
// still read the core directly (sidecar.go); folding both behind this one interface is the review phase's.
type sidecarBackend interface {
	// The daemon's sidecar_id for this Sidecar's network, nil before it has one.
	sidecarId() *Id
	// The daemon's state and its reason (§18.5's State), as GetState names them; offline while no daemon answers.
	state() (state string, reason string)
	// The client generation the state belongs to.
	generation() int64
	// Reads the daemon's status now.
	refresh(ctx context.Context) error
	// §5.8's pairing: gives the daemon the child's token, and answers the network and generation it bound.
	pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error)
	// One control call for a wrapper, the paired network named where the method takes one. confirmed says a person
	// confirmed it in the app's UI (D-CORE-13).
	call(ctx context.Context, method string, params any, confirmed bool, result any) error
	// Ends the control session.
	close()
}

// The control methods the remote backend calls, as their owners named them.
const (
	sidecarControlMethodStatus            = "status"
	sidecarControlMethodPair              = "pair"
	sidecarControlMethodForwardingSetup   = "bridge.forwarding.setup"
	sidecarControlMethodForwardingWitness = "bridge.forwarding.witness"
)

// §18.5's states, as the daemon names them.
const (
	sidecarRemoteStateLocked     = "locked"
	sidecarRemoteStateUnenrolled = "unenrolled"
	sidecarRemoteStateOffline    = "offline"
	sidecarRemoteStateOnline     = "online"
	sidecarRemoteStateRevoked    = "revoked"
)

// The one reason a revoked state carries: a SidecarRevoke the daemon's core verified (D-SDK-14).
const sidecarRemoteReasonSigned = "signed"

// What the backend falls to when the daemon answers revoked without its verified reason.
const sidecarRemoteReasonRefused = "refused"

// status's reply, as far as the backend reads it.
type sidecarControlStatus struct {
	State      string                        `json:"state"`
	Reason     string                        `json:"reason"`
	Generation int64                         `json:"generation"`
	Networks   []sidecarControlNetworkStatus `json:"networks"`
}

// One hosted network in status.
type sidecarControlNetworkStatus struct {
	NetworkId  *Id    `json:"network_id"`
	SidecarId  *Id    `json:"sidecar_id"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	Generation int64  `json:"generation"`
}

// pair's params: the child's token, which the app minted and gives up.
type sidecarControlPairParams struct {
	ChildJwt string `json:"child_jwt"`
}

// pair's reply: no token.
type sidecarControlPairReply struct {
	NetworkId  *Id   `json:"network_id"`
	Generation int64 `json:"generation"`
}

// bridge.forwarding.setup's params: the component's bridge.ForwardingKeys, encoded, public halves only.
type sidecarControlForwardingSetupParams struct {
	Network                string `json:"network,omitempty"`
	Keys                   []byte `json:"keys"`
	ConfirmAfterRevocation bool   `json:"confirm_after_revocation,omitempty"`
}

// bridge.forwarding.setup's reply: the bridge.ForwardingHandOff, encoded, for the app's VPN component alone.
type sidecarControlForwardingSetupReply struct {
	HandOff []byte `json:"hand_off"`
}

// A network-scoped call's params with nothing else.
type sidecarControlNetworkParams struct {
	Network string `json:"network,omitempty"`
}

// bridge.forwarding.witness's reply.
type sidecarControlForwardingWitnessReply struct {
	// The bridge.AuthPin, encoded.
	Pin       []byte `json:"pin"`
	Witnessed bool   `json:"witnessed"`
	Fork      bool   `json:"fork"`
	// Why a setup is due, empty when none is.
	SetupDue string `json:"setup_due,omitempty"`
}

// The daemon's refusal of one call: its JSON-RPC error.
type sidecarControlError struct {
	Code    int
	Message string
}

// The daemon's message, which it writes knowing a same-user agent may read it.
func (self *sidecarControlError) Error() string {
	return self.Message
}

var (
	// No daemon answers: the backend is offline and starts none (R-SDK-9).
	errSidecarRemoteOffline = errors.New("no ur daemon answers this instance's control socket")
	// The Sidecar closed.
	errSidecarRemoteClosed = errors.New("the remote-mode sidecar is closed")
	// A pairing the daemon answered without a network or a generation.
	errSidecarRemotePairReply = errors.New("the daemon answered the pairing with no binding")
	// A pairing's refusal whose text repeated the child's token.
	errSidecarRemotePairRefused = errors.New("the daemon refused the pairing")
)

// One control session.
type sidecarControlChannel interface {
	// Calls method and decodes its result into result when that is not nil. A daemon's refusal is a
	// *sidecarControlError; any other error means the session is gone.
	call(ctx context.Context, method string, params any, confirmed bool, result any) error
	close()
}

// Opens a control session, or fails when no daemon answers. It never starts one.
type sidecarControlDial func(ctx context.Context) (sidecarControlChannel, error)

// The remote backend's tunables.
type sidecarRemoteSettings struct {
	// How often the backend reads the daemon's status while the Sidecar is open.
	StatusPollInterval time.Duration
	// The bound of one control call, its dial included.
	CallTimeout time.Duration
	// The bound of a pairing, which binds on the server.
	PairTimeout time.Duration
}

// The defaults: a status read every 2 s, 30 s a call, 60 s a pairing.
func defaultSidecarRemoteSettings() *sidecarRemoteSettings {
	return &sidecarRemoteSettings{
		StatusPollInterval: 2 * time.Second,
		CallTimeout:        30 * time.Second,
		PairTimeout:        60 * time.Second,
	}
}

// Where the daemon's sidecar stands, as the backend last read it.
type sidecarRemoteStatus struct {
	state      string
	reason     string
	generation int64
	sidecarId  *Id
}

// The remote backend over a control channel. Safe for concurrent use.
type sidecarRemoteBackend struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *sidecarRemoteSettings
	dial     sidecarControlDial
	// called with each change of the state, its reason or its generation; it must not block
	stateChanged func(generation int64, state string, reason string)
	pollDone     chan struct{}

	// one call at a time, as the daemon answers them; guards channel
	callLock sync.Mutex
	channel  sidecarControlChannel

	stateLock sync.Mutex
	status    sidecarRemoteStatus
	// the newest generation taken for status's sidecar_id, from a status or a pairing: nothing older is taken
	generationFloor int64
	// the network the pairing bound, nil before one
	networkId *Id
	closed    bool
}

var _ sidecarBackend = (*sidecarRemoteBackend)(nil)

// A backend that dials with dial, offline until its first status. stateChanged may be nil. The caller starts run.
func newSidecarRemoteBackend(ctx context.Context, dial sidecarControlDial, settings *sidecarRemoteSettings, stateChanged func(generation int64, state string, reason string)) *sidecarRemoteBackend {
	cancelCtx, cancel := context.WithCancel(ctx)
	if stateChanged == nil {
		stateChanged = func(generation int64, state string, reason string) {}
	}
	return &sidecarRemoteBackend{
		ctx:          cancelCtx,
		cancel:       cancel,
		settings:     settings,
		dial:         dial,
		stateChanged: stateChanged,
		pollDone:     make(chan struct{}),
		status:       sidecarRemoteStatus{state: sidecarRemoteStateOffline},
	}
}

// Reads the daemon's status at once and then every StatusPollInterval until the backend closes. A read that fails leaves the backend
// offline, and the next one dials again.
func (self *sidecarRemoteBackend) run() {
	defer close(self.pollDone)
	self.refresh(self.ctx)
	ticker := time.NewTicker(self.settings.StatusPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-ticker.C:
			self.refresh(self.ctx)
		}
	}
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) sidecarId() *Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.sidecarId
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) state() (string, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.state, self.status.reason
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) generation() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.status.generation
}

// The network the pairing bound, as a control call's network argument: empty before a pairing, so the daemon picks by
// §5.8's rule.
func (self *sidecarRemoteBackend) network() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.networkId == nil {
		return ""
	}
	return self.networkId.String()
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) refresh(ctx context.Context) error {
	reply := &sidecarControlStatus{}
	if err := self.callBounded(ctx, self.settings.CallTimeout, sidecarControlMethodStatus, nil, false, reply); err != nil {
		return err
	}
	self.applyStatus(reply)
	return nil
}

// See sidecarBackend.
func (self *sidecarRemoteBackend) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	return self.callBounded(ctx, self.settings.CallTimeout, method, params, confirmed, result)
}

// One call within timeout, dialing a session when none is open. A session that fails is dropped and the backend reads
// offline, so the next call dials again; a daemon's refusal keeps the session.
func (self *sidecarRemoteBackend) callBounded(ctx context.Context, timeout time.Duration, method string, params any, confirmed bool, result any) error {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// a close ends a call in flight
	stop := context.AfterFunc(self.ctx, cancel)
	defer stop()

	self.callLock.Lock()
	defer self.callLock.Unlock()
	if self.ctx.Err() != nil {
		return errSidecarRemoteClosed
	}
	if self.channel == nil {
		channel, err := self.dial(callCtx)
		if err != nil {
			self.applyOffline()
			return errors.Join(errSidecarRemoteOffline, err)
		}
		self.channel = channel
	}
	err := self.channel.call(callCtx, method, params, confirmed, result)
	if err == nil {
		return nil
	}
	var refusal *sidecarControlError
	if errors.As(err, &refusal) {
		return refusal
	}
	self.channel.close()
	self.channel = nil
	if self.ctx.Err() != nil {
		return errSidecarRemoteClosed
	}
	self.applyOffline()
	return errors.Join(errSidecarRemoteOffline, err)
}

// See sidecarBackend. The token is in the request alone: the reply names the binding, and a refusal that repeats the
// token is replaced.
func (self *sidecarRemoteBackend) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	if childJwt == "" {
		return nil, errors.New("a pairing needs the child's token")
	}
	reply := &sidecarControlPairReply{}
	err := self.callBounded(ctx, self.settings.PairTimeout, sidecarControlMethodPair, &sidecarControlPairParams{ChildJwt: childJwt}, false, reply)
	if err != nil {
		if strings.Contains(err.Error(), childJwt) {
			return nil, errSidecarRemotePairRefused
		}
		return nil, err
	}
	if reply.NetworkId == nil || reply.Generation <= 0 {
		return nil, errSidecarRemotePairReply
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.networkId == nil || self.networkId.Cmp(reply.NetworkId) != 0 {
			// the daemon's sidecar of this network, which the next status names: its own id and generations
			self.networkId = reply.NetworkId
			self.status.sidecarId = nil
			self.generationFloor = reply.Generation
			return
		}
		// the same sidecar's next generation: nothing older ends it from here on
		if self.generationFloor < reply.Generation {
			self.generationFloor = reply.Generation
		}
	}()
	return reply, nil
}

// Relays forwarding's setup for the app's VPN component (§18.6; BRIDGE-M8-06): the component's public keys out and
// the hand-off back, both as their encoded messages. confirmAfterRevocation carries the person's confirmation in the
// app's UI of setting forwarding up again after another signer's revocation.
func (self *sidecarRemoteBackend) forwardingSetup(ctx context.Context, keys []byte, confirmAfterRevocation bool) ([]byte, error) {
	reply := &sidecarControlForwardingSetupReply{}
	params := &sidecarControlForwardingSetupParams{Network: self.network(), Keys: keys, ConfirmAfterRevocation: confirmAfterRevocation}
	if err := self.call(ctx, sidecarControlMethodForwardingSetup, params, confirmAfterRevocation, reply); err != nil {
		return nil, err
	}
	if len(reply.HandOff) == 0 {
		return nil, errors.New("the daemon answered forwarding's setup with no hand-off")
	}
	return reply.HandOff, nil
}

// Relays forwarding's witness hint (§18.6): the daemon's pin, whether it is the witnessed one, a fork, and why a setup
// is due.
func (self *sidecarRemoteBackend) forwardingWitness(ctx context.Context) (*sidecarControlForwardingWitnessReply, error) {
	reply := &sidecarControlForwardingWitnessReply{}
	if err := self.call(ctx, sidecarControlMethodForwardingWitness, &sidecarControlNetworkParams{Network: self.network()}, false, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// See sidecarBackend. A call in flight ends, the session closes and the status reads stop.
func (self *sidecarRemoteBackend) close() {
	alreadyClosed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return true
		}
		self.closed = true
		return false
	}()
	if alreadyClosed {
		return
	}
	self.cancel()
	self.callLock.Lock()
	defer self.callLock.Unlock()
	if self.channel != nil {
		self.channel.close()
		self.channel = nil
	}
}

// The row of reply this backend follows: the paired network's, unenrolled when the daemon hosts it no longer; before
// a pairing the daemon's only network, else its first, which the reply's own fields name.
func (self *sidecarRemoteBackend) statusOf(reply *sidecarControlStatus, networkId *Id) sidecarRemoteStatus {
	rowStatus := func(row *sidecarControlNetworkStatus) sidecarRemoteStatus {
		return sidecarRemoteStatus{state: row.State, reason: row.Reason, generation: row.Generation, sidecarId: row.SidecarId}
	}
	if networkId != nil {
		for i := range reply.Networks {
			if row := &reply.Networks[i]; row.NetworkId != nil && row.NetworkId.Cmp(networkId) == 0 {
				return rowStatus(row)
			}
		}
		return sidecarRemoteStatus{state: sidecarRemoteStateUnenrolled}
	}
	if len(reply.Networks) == 1 {
		return rowStatus(&reply.Networks[0])
	}
	return sidecarRemoteStatus{state: reply.State, reason: reply.Reason, generation: reply.Generation}
}

// Takes a status the daemon answered. One of an older generation of the same sidecar_id is dropped (D-SDK-14). A state
// this sdk does not know reads offline, a daemon that runs no sidecar among them, and revoked without the reason only
// a verified SidecarRevoke carries reads unenrolled(refused): nothing but signed means revoked.
func (self *sidecarRemoteBackend) applyStatus(reply *sidecarControlStatus) {
	changed, next := func() (bool, sidecarRemoteStatus) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false, self.status
		}
		next := self.statusOf(reply, self.networkId)
		switch next.state {
		case sidecarRemoteStateLocked, sidecarRemoteStateUnenrolled, sidecarRemoteStateOffline, sidecarRemoteStateOnline:
		case sidecarRemoteStateRevoked:
			if next.reason != sidecarRemoteReasonSigned {
				next.state, next.reason = sidecarRemoteStateUnenrolled, sidecarRemoteReasonRefused
			}
		default:
			next.state, next.reason = sidecarRemoteStateOffline, ""
		}
		current := self.status
		sameSidecar := current.sidecarId == nil || next.sidecarId == nil || current.sidecarId.Cmp(next.sidecarId) == 0
		if sameSidecar && next.generation < self.generationFloor {
			// an answer from before a re-pair
			return false, current
		}
		if sameSidecar && next.sidecarId == nil {
			next.sidecarId = current.sidecarId
		}
		self.generationFloor = next.generation
		self.status = next
		return next.state != current.state || next.reason != current.reason || next.generation != current.generation, next
	}()
	if changed {
		self.stateChanged(next.generation, next.state, next.reason)
	}
}

// No daemon answers: offline, the sidecar_id and generation kept for when one does.
func (self *sidecarRemoteBackend) applyOffline() {
	changed, next := func() (bool, sidecarRemoteStatus) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.status.state == sidecarRemoteStateOffline {
			return false, self.status
		}
		self.status.state, self.status.reason = sidecarRemoteStateOffline, ""
		return true, self.status
	}()
	if changed {
		self.stateChanged(next.generation, next.state, next.reason)
	}
}
