//go:build !js

// The VPN component's forwarding host in DeviceLocal (DESIGN-DETAILS 4.2.6, the component's side; §18.6's Setup,
// Following and posture; D-SDK-13, D-SDK-20, D-SDK-32, DP1, owner decision 197; SDK-M8-16): one forwarding role per
// network a hand-off names, each with its own key sets (device_sidecar_forward_keys.go), sealed store
// (device_sidecar_forward_store.go), posture, Forwarder and forwarding client F, so forwarding never waits on the app.
//
//	keys       C0: keys(networkId) generates a key set into next, the root key with it where none is, and answers the
//	           public halves alone, with this device as host client in its device client's network; a repeat while
//	           next is unpromoted answers the same set. Keys are made only here, at a setup
//	hand-off   C7: handOff checks that the Signed Delegation names exactly next's public keys, the Pending's sidecar
//	           and, in the device client's network, this device, signed by that sidecar; keeps F's token in the key
//	           store, the hand-off without it in the sealed store and the Delegation's id beside next; anchors the
//	           posture while it is legacy; and runs the network's Forwarder on F
//	promotion  C8: once the component's own posture holds a state at or past delegation_at that shows that Delegation
//	           live, next becomes current and the old set goes; until then current signs, and a kill between the
//	           hand-off and the promotion keeps it signing
//	following  the Forwarder follows the log on F, applies what the posture can check at once and compiles the rest,
//	           handing states to the network's posture (the sink), all with no sidecar role running; team hints come by
//	           §6.8's long-poll on F, which reaches every network (D's pushes on 21842 are not wired here yet)
//	refresh    F refreshes itself by its own proofs at 40 to 50% of its lifetime, keeping the new token before it is
//	           presented; a token past its exp at start holds the posture pending (forward_client_lapsed) until a new
//	           hand-off, by the posture's fork hold, which a witness's fork shares
//	witness    witness(networkId, pin, fork) hands the app's witnessed pin to the Forwarder, which follows to it and
//	           raises head_alarm on a pin off its chain; a fork holds the posture pending until a later witness clears it
//	wipe       the sidecar's own sign-out, which leaves the posture legacy, erases the network's keys, store and floor
//
// The router (device_sidecar.go) takes each network's block, the first-joined's where two collide, and hosting runs
// only in the device client's network, the one role whose hook the exit NAT takes. Forwarding runs whenever the VPN
// component runs: no app setting stops it, and EnableBridge is a release gate only (§17.16). The settings follow the
// host class: an Apple extension's compile within 10 MiB under a 30 MiB heap ceiling, Android's within 16 MiB, a
// desktop service's within 64 MiB (DefaultForwarderSettings).
//
// On Android the VPN service shares the app's process until app work gives it one of its own (§18.15, §18.16): this
// file's key store, names and directory are apart from the sidecar role's meanwhile, and the process boundary is the
// app's to build. Untagged: the tunnel links it, naming the root only through the Forwarder and its client. Logs name
// network ids and positions, never a key, token or address (I-7, I-13). Safe for concurrent use.

package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// What a VPN component's forwarding roles are tuned by, beside each Forwarder's own settings.
type deviceForwardingSettings struct {
	// Where in F's lifetime, iat to exp, it refreshes: a point drawn between these fractions by a hash of the token,
	// so every process takes the same point for one token (§18.6's after row): 40% and 50%.
	RefreshWindowStart float64
	RefreshWindowEnd   float64
	// The wait after a failed refresh, doubling to the max: 10 s and 10 min.
	RefreshRetryMinTimeout time.Duration
	RefreshRetryMaxTimeout time.Duration
	// The bound of one long-poll on F, which api holds up to 25 s: 40 s.
	PollTimeout time.Duration
	// The wait after a failed poll, doubling to the max: 1 s and 60 s.
	PollRetryMinTimeout time.Duration
	PollRetryMaxTimeout time.Duration
	// How often a role looks whether its handed-off key set may be promoted (C8): 1 min.
	PromotionCheckInterval time.Duration
	// Each Forwarder's settings; nil takes DefaultForwarderSettings of the device's host class.
	Forwarder *sidecar.ForwarderSettings
}

// The defaults, a fresh value each call.
func defaultDeviceForwardingSettings() *deviceForwardingSettings {
	return &deviceForwardingSettings{
		RefreshWindowStart:     0.4,
		RefreshWindowEnd:       0.5,
		RefreshRetryMinTimeout: 10 * time.Second,
		RefreshRetryMaxTimeout: 10 * time.Minute,
		PollTimeout:            40 * time.Second,
		PollRetryMinTimeout:    time.Second,
		PollRetryMaxTimeout:    60 * time.Second,
		PromotionCheckInterval: time.Minute,
	}
}

// The forwarding host class of this build: an Apple network extension on iOS, or on macOS under the ios_extension leg;
// Android's VPN service; a desktop service everywhere else.
func deviceForwarderHostClass() sidecar.ForwarderHostClass {
	switch {
	case runtime.GOOS == "ios", runtime.GOOS == "darwin" && deviceForwardAppleExtension:
		return sidecar.ForwarderHostApple
	case runtime.GOOS == "android":
		return sidecar.ForwarderHostAndroid
	}
	return sidecar.ForwarderHostDesktop
}

// The key set states a status names (4.2.6). Format values: the status mirror carries them.
const (
	deviceForwardKeySetNone      = "none"
	deviceForwardKeySetGenerated = "generated"
	deviceForwardKeySetDelegated = "delegated"
	deviceForwardKeySetActive    = "active"
)

// What a role's Forwarder and F are built over, which the device supplies once it is built: the network space's urls,
// the client strategy's transport, and the device client's id and ClientKey public key.
type deviceForwardingDeps struct {
	ctx         context.Context
	apiUrl      string
	platformUrl string
	transport   http.RoundTripper
	hostClient  func() (clientId []byte, publicKey []byte)
}

// A device's forwarding roles (the file's comment). Nil on a device given no forwarding key store or directory, which
// refuses every setup.
type deviceLocalForwardingHost struct {
	owner           *deviceLocalBridgeOwner
	secrets         *deviceForwardSecrets
	directoryPath   string
	networks        *deviceForwardNetworks
	deviceNetworkId connect.Id
	bridgeSettings  *sidecardevice.BridgeSettings
	settings        *deviceForwardingSettings
	log             connect.Logger

	stateLock sync.Mutex
	roles     map[connect.Id]*deviceLocalForwarding
	deps      *deviceForwardingDeps
	closed    bool
}

// The host of a device built on settings for networkId, or nil where the app gave no forwarding key store or no
// absolute ForwardStateDir: such a device keeps the M4 seam's store and sets nothing up.
func newDeviceLocalForwardingHost(settings *DeviceLocalSettings, networkId connect.Id, bridgeSettings *sidecardevice.BridgeSettings, log connect.Logger) *deviceLocalForwardingHost {
	if settings.forwardKeyStore == nil || settings.ForwardStateDir == "" || !filepath.IsAbs(settings.ForwardStateDir) {
		return nil
	}
	forwardingSettings := settings.forwardingSettings
	if forwardingSettings == nil {
		forwardingSettings = defaultDeviceForwardingSettings()
	}
	directoryPath := filepath.Join(settings.ForwardStateDir, deviceForwardDirectoryName)
	return &deviceLocalForwardingHost{
		secrets:         &deviceForwardSecrets{store: settings.forwardKeyStore},
		directoryPath:   directoryPath,
		networks:        &deviceForwardNetworks{directoryPath: directoryPath},
		deviceNetworkId: networkId,
		bridgeSettings:  bridgeSettings,
		settings:        forwardingSettings,
		log:             log,
		roles:           map[connect.Id]*deviceLocalForwarding{},
	}
}

// The role of networkId, made with its keys and files where create, else nil for one with none.
func (self *deviceLocalForwardingHost) role(networkId connect.Id, create bool) (*deviceLocalForwarding, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errDeviceBridgeClosed
	}
	if role := self.roles[networkId]; role != nil || !create {
		return role, nil
	}
	files, err := newSdkForwardFileStore(filepath.Join(self.directoryPath, strings.ReplaceAll(networkId.String(), "-", "")))
	if err != nil {
		return nil, err
	}
	role := &deviceLocalForwarding{
		host:      self,
		networkId: networkId,
		hosting:   networkId == self.deviceNetworkId,
		keys:      newDeviceForwardKeys(self.secrets, networkId),
		files:     files,
		settings:  self.settings,
		log:       self.log,
	}
	role.sink = &deviceForwardSink{role: role}
	self.roles[networkId] = role
	return role, nil
}

// The posture store and bridge settings of networkId's role, its floor among them, restored at the device's start
// before the first packet: the sealed posture/ view where the root key reads Present, else a store failing every call,
// which a posture restores as pending, never legacy, so a key store that cannot answer never reopens a LAN (§5.9).
func (self *deviceLocalForwardingHost) postureStore(role *deviceLocalForwarding) (sidecardevice.PostureStore, *sidecardevice.BridgeSettings) {
	bridgeSettings := *self.bridgeSettings
	postureSettings := sidecardevice.DefaultPostureSettings()
	if self.bridgeSettings.PostureSettings != nil {
		copied := *self.bridgeSettings.PostureSettings
		postureSettings = &copied
	}
	postureSettings.Floor = role.keys.floor()
	bridgeSettings.PostureSettings = postureSettings
	if err := role.openStore(false); err != nil {
		self.log.Infof("[forward]%s the forwarding store stays closed: %v\n", role.networkId, err)
		return &failedPostureStore{err: err}, &bridgeSettings
	}
	return &deviceForwardPrefixStore{store: role.sealed, prefix: deviceForwardPosturePrefix}, &bridgeSettings
}

// Restores every listed network's role at the device's start, before the first packet: the device client's network's
// posture into the owner's primary while it holds none, every other network's as a role of its own. A networks file
// that does not read restores none, and the device client's posture stays as the owner restored it.
func (self *deviceLocalForwardingHost) restore(owner *deviceLocalBridgeOwner) error {
	self.owner = owner
	owner.joinOrder = func() []connect.Id {
		networkIds, _ := self.networks.list()
		return networkIds
	}
	networkIds, err := self.networks.list()
	if err != nil {
		self.log.Infof("[forward]the forwarding networks do not list: %v\n", err)
		return nil
	}
	for _, networkId := range networkIds {
		role, err := self.role(networkId, true)
		if err != nil {
			return err
		}
		store, bridgeSettings := self.postureStore(role)
		if role.hosting {
			// the primary takes the sealed store only while it holds no bridge, which it then restores from it
			if owner.usePrimaryStore(store, bridgeSettings) {
				if err := owner.restore(); err != nil {
					return err
				}
			}
			continue
		}
		if err := owner.addRole(networkId, store, bridgeSettings); err != nil {
			return err
		}
	}
	return nil
}

// Starts every restored role's Forwarder once the device is built, on deps.
func (self *deviceLocalForwardingHost) start(deps *deviceForwardingDeps) {
	roles := func() []*deviceLocalForwarding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deps = deps
		roles := []*deviceLocalForwarding{}
		for _, role := range self.roles {
			roles = append(roles, role)
		}
		return roles
	}()
	for _, role := range roles {
		if err := role.restart(deps); err != nil {
			self.log.Infof("[forward]%s no forwarder runs yet: %v\n", role.networkId, err)
		}
	}
}

// The device client's id and ClientKey public key for networkId's keys and hand-off: set only in its own network.
func (self *deviceLocalForwardingHost) hostClientOf(networkId connect.Id) ([]byte, []byte) {
	if networkId != self.deviceNetworkId {
		return nil, nil
	}
	self.stateLock.Lock()
	deps := self.deps
	self.stateLock.Unlock()
	if deps == nil || deps.hostClient == nil {
		return nil, nil
	}
	return deps.hostClient()
}

// C0 (the file's comment).
func (self *deviceLocalForwardingHost) keys(networkId connect.Id) (*bridge.ForwardingKeys, error) {
	role, err := self.role(networkId, true)
	if err != nil {
		return nil, err
	}
	set, err := role.keys.generate()
	if err != nil {
		return nil, err
	}
	hostClientId, hostClientPublicKey := self.hostClientOf(networkId)
	return set.publicKeys(hostClientId, hostClientPublicKey)
}

// C7 (the file's comment).
func (self *deviceLocalForwardingHost) handOff(handOff *bridge.ForwardingHandOff) error {
	if handOff == nil || handOff.GetPending() == nil {
		return fmt.Errorf("%w: no hand-off", errDeviceForwardHandOffRefused)
	}
	networkId, err := connect.IdFromBytes(handOff.GetPending().GetNetworkId())
	if err != nil {
		return fmt.Errorf("%w: no network", errDeviceForwardHandOffRefused)
	}
	role, err := self.role(networkId, false)
	if err != nil {
		return err
	}
	if role == nil {
		return fmt.Errorf("%w: no key set was generated for the network", errDeviceForwardHandOffRefused)
	}
	next, status := role.keys.loadSet(deviceForwardSlotNext)
	if status != deviceForwardSecretPresent {
		if status == deviceForwardSecretAbsent {
			return fmt.Errorf("%w: no key set waits for a hand-off", errDeviceForwardHandOffRefused)
		}
		return deviceForwardStatusErr(status)
	}
	hostClientId, hostClientPublicKey := self.hostClientOf(networkId)
	delegationId, err := checkDeviceForwardHandOff(handOff, next, hostClientId, hostClientPublicKey)
	if err != nil {
		return err
	}
	if err := role.openStore(true); err != nil {
		return err
	}
	store, bridgeSettings := self.postureStore(role)
	if role.hosting {
		self.owner.usePrimaryStore(store, bridgeSettings)
	} else if err := self.owner.addRole(networkId, store, bridgeSettings); err != nil {
		return err
	}
	if err := role.keys.setToken(handOff.GetForwardClientJwt()); err != nil {
		return err
	}
	if err := role.writeSetup(handOff); err != nil {
		return err
	}
	if err := role.keys.recordDelegation(handOff.GetKeySetId(), delegationId); err != nil {
		return err
	}
	if err := self.networks.add(networkId); err != nil {
		return err
	}
	self.stateLock.Lock()
	deps := self.deps
	self.stateLock.Unlock()
	if deps == nil {
		// the device is not built yet: its start runs the Forwarder
		return nil
	}
	return role.restart(deps)
}

// The app's witnessed pin and fork for networkId (§18.6's witness row).
func (self *deviceLocalForwardingHost) witness(networkId connect.Id, pin *bridge.AuthPin, fork bool) error {
	role, err := self.role(networkId, false)
	if err != nil {
		return err
	}
	if role == nil {
		return errDeviceBridgeLegacy
	}
	return role.witness(pin, fork)
}

// networkId's forwarding signer for its links (SDK-M8-08), refusing while no key set is current.
func (self *deviceLocalForwardingHost) signer(networkId connect.Id) (*sidecar.ForwardingSigner, error) {
	role, err := self.role(networkId, false)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, errDeviceBridgeLegacy
	}
	forwarder := role.currentForwarder()
	if forwarder == nil {
		return nil, errors.New("no forwarder runs for the network")
	}
	return forwarder.NewSigner(nil)
}

// Every role's status, in the order the networks joined.
func (self *deviceLocalForwardingHost) status() []deviceForwardingStatus {
	networkIds, _ := self.networks.list()
	self.stateLock.Lock()
	roles := []*deviceLocalForwarding{}
	for _, role := range self.roles {
		roles = append(roles, role)
	}
	self.stateLock.Unlock()
	slices.SortStableFunc(roles, func(a *deviceLocalForwarding, b *deviceLocalForwarding) int {
		return deviceForwardJoinIndex(networkIds, a.networkId) - deviceForwardJoinIndex(networkIds, b.networkId)
	})
	statuses := []deviceForwardingStatus{}
	for _, role := range roles {
		statuses = append(statuses, role.status())
	}
	return statuses
}

// Where networkId joined in networkIds, past every listed one when unlisted.
func deviceForwardJoinIndex(networkIds []connect.Id, networkId connect.Id) int {
	if index := slices.Index(networkIds, networkId); 0 <= index {
		return index
	}
	return len(networkIds)
}

// Stops every Forwarder; the postures keep what they hold. Waits for each Forwarder's goroutine.
func (self *deviceLocalForwardingHost) close() {
	roles := func() []*deviceLocalForwarding {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		roles := []*deviceLocalForwarding{}
		for _, role := range self.roles {
			roles = append(roles, role)
		}
		return roles
	}()
	for _, role := range roles {
		role.stop()
	}
}

// The sidecar's own sign-out left networkId's posture legacy (4.2.6's wiped): its Forwarder stops and its keys, store,
// floor and listing go. On a goroutine of its own, since the Forwarder that applied the sign-out calls in from its own.
func (self *deviceLocalForwardingHost) wipe(role *deviceLocalForwarding) {
	go func() {
		role.stop()
		if err := role.keys.wipe(); err != nil {
			self.log.Infof("[forward]%s the wipe left keys: %v\n", role.networkId, err)
		}
		if err := role.files.removeAll(); err != nil {
			self.log.Infof("[forward]%s the wipe left files: %v\n", role.networkId, err)
		}
		if err := self.networks.remove(role.networkId); err != nil {
			self.log.Infof("[forward]%s the wipe left the listing: %v\n", role.networkId, err)
		}
		if !role.hosting && self.owner != nil {
			self.owner.removeRole(role.networkId)
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.roles[role.networkId] == role {
			delete(self.roles, role.networkId)
		}
	}()
}

// One network's forwarding role (the file's comment).
type deviceLocalForwarding struct {
	host      *deviceLocalForwardingHost
	networkId connect.Id
	hosting   bool
	keys      *deviceForwardKeys
	files     *sdkForwardFileStore
	sink      *deviceForwardSink
	settings  *deviceForwardingSettings
	log       connect.Logger

	stateLock sync.Mutex
	// the sealed store and the Forwarder's view of it, nil until the root key reads Present
	sealed         sidecardevice.PostureStore
	forwarderStore *deviceForwarderStore
	forwarder      *sidecar.Forwarder
	client         *sidecar.ForwardingClient
	// the hand-off the Forwarder runs on, without its token
	setup *bridge.ForwardingHandOff
	// ends the refresh, poll and promotion loops of the Forwarder running
	cancel context.CancelFunc
	loops  sync.WaitGroup
	// the posture's fork hold: F lapsed, or the app's sidecar role reported a fork
	lapsed      bool
	witnessFork bool
	// F's token's exp, zero for none
	expiresAt time.Time
	// wakes the promotion loop at once after a state was handed
	promotionWanted chan struct{}
}

// Opens the sealed store over the root key: made where create and none is, else Present's only.
func (self *deviceLocalForwarding) openStore(create bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sealed != nil {
		return nil
	}
	rootKey, err := self.keys.rootKey(create)
	if err != nil {
		return err
	}
	sealed, err := sidecardevice.NewSealedPostureStore(self.files, rootKey)
	if err != nil {
		return err
	}
	self.sealed = sealed
	self.forwarderStore = &deviceForwarderStore{sealed: sealed, files: self.files}
	return nil
}

// Keeps the hand-off without its token in the sealed store (4.2.2's setup record).
func (self *deviceLocalForwarding) writeSetup(handOff *bridge.ForwardingHandOff) error {
	stripped := proto.Clone(handOff).(*bridge.ForwardingHandOff)
	stripped.ForwardClientJwt = ""
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(stripped)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	sealed := self.sealed
	self.stateLock.Unlock()
	if sealed == nil {
		return errors.New("the forwarding store is closed")
	}
	return sealed.Write(deviceForwardSetupName, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}

// The hand-off the sealed store keeps, nil for none.
func (self *deviceLocalForwarding) readSetup() (*bridge.ForwardingHandOff, error) {
	self.stateLock.Lock()
	sealed := self.sealed
	self.stateLock.Unlock()
	if sealed == nil {
		return nil, errors.New("the forwarding store is closed")
	}
	reader, err := sealed.Open(deviceForwardSetupName)
	if errors.Is(err, sidecardevice.ErrPostureStoreAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data := bytes.Buffer{}
	if _, err := data.ReadFrom(reader); err != nil {
		return nil, err
	}
	setup := &bridge.ForwardingHandOff{}
	if err := proto.Unmarshal(data.Bytes(), setup); err != nil {
		return nil, err
	}
	return setup, nil
}

// Stops the running Forwarder and its loops, then runs a new one on the kept hand-off and token over deps: what a
// hand-off and the device's start call. Nothing runs without a hand-off, a token and a client key set.
func (self *deviceLocalForwarding) restart(deps *deviceForwardingDeps) error {
	self.stop()
	if err := self.openStore(false); err != nil {
		return err
	}
	setup, err := self.readSetup()
	if err != nil {
		return err
	}
	if setup == nil {
		return errors.New("no hand-off is kept")
	}
	byJwt, err := self.keys.token()
	if err != nil {
		return err
	}
	if byJwt == "" {
		return errors.New("no forwarding client token is kept")
	}
	clientSet, err := self.keys.clientSet()
	if err != nil {
		return err
	}
	if clientSet == nil {
		return errors.New("no key set holds the forwarding client's key")
	}
	apiSettings, err := deviceForwardApiSettings(deps.apiUrl, deps.platformUrl)
	if err != nil {
		return err
	}
	client, err := sidecar.NewForwardingClient(&sidecar.ForwardingClientArgs{
		Settings:      apiSettings,
		Transport:     deps.transport,
		ByJwt:         byJwt,
		ClientKeySeed: clientSet.clientSeed,
	})
	if err != nil {
		return err
	}
	sidecarId, err := sidecar.IdFromBytes(setup.GetPending().GetSidecarId())
	if err != nil {
		return err
	}
	configRepoId, err := sidecar.IdFromBytes(setup.GetConfigRepoId())
	if err != nil {
		return err
	}
	configGroupId, err := sidecar.IdFromBytes(setup.GetConfigGroupId())
	if err != nil {
		return err
	}
	expiresAt := deviceForwardTokenExpiry(byJwt)
	lapsed := !expiresAt.IsZero() && !time.Now().Before(expiresAt)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.setup = setup
		self.client = client
		self.expiresAt = expiresAt
		self.lapsed = lapsed
		self.promotionWanted = make(chan struct{}, 1)
	}()
	if lapsed {
		// a token past its exp at start: pending until a new hand-off (forward_client_lapsed)
		self.log.Infof("[forward]%s the forwarding client lapsed\n", self.networkId)
		self.sink.applyFork()
	}
	forwarderSettings := self.settings.Forwarder
	if forwarderSettings == nil {
		forwarderSettings = sidecar.DefaultForwarderSettings(deviceForwarderHostClass())
	}
	forwarder, err := sidecar.NewForwarder(deps.ctx, &sidecar.ForwarderArgs{
		NetworkId:     sidecar.Id(self.networkId),
		SidecarId:     sidecarId,
		Pending:       setup.GetPending(),
		ConfigRepoId:  configRepoId,
		ConfigGroupId: configGroupId,
		Projection:    client,
		Gkm:           client.Gkm(),
		Binding:       client.Binding,
		ClientLapsed:  self.lapsedNow,
		Store:         self.forwarderStore,
		Keys:          self.keys,
		Sink:          self.sink,
		Settings:      forwarderSettings,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(deps.ctx)
	self.stateLock.Lock()
	self.forwarder = forwarder
	self.cancel = cancel
	self.stateLock.Unlock()
	self.loops.Add(3)
	go self.runRefresh(ctx)
	go self.runPoll(ctx)
	go self.runPromotion(ctx)
	return nil
}

// Stops the Forwarder and its loops, waiting for each. The posture keeps what it holds.
func (self *deviceLocalForwarding) stop() {
	forwarder, cancel := func() (*sidecar.Forwarder, context.CancelFunc) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		forwarder, cancel := self.forwarder, self.cancel
		self.forwarder, self.cancel = nil, nil
		return forwarder, cancel
	}()
	if cancel != nil {
		cancel()
	}
	self.loops.Wait()
	if forwarder != nil {
		forwarder.Close()
	}
}

// The Forwarder running, nil for none.
func (self *deviceLocalForwarding) currentForwarder() *sidecar.Forwarder {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.forwarder
}

// Whether F lapsed: its token past its exp. The Forwarder reads it when a follow fails.
func (self *deviceLocalForwarding) lapsedNow() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lapsed || (!self.expiresAt.IsZero() && !time.Now().Before(self.expiresAt))
}

// The app's witnessed pin and fork, to the Forwarder.
func (self *deviceLocalForwarding) witness(pin *bridge.AuthPin, fork bool) error {
	forwarder := self.currentForwarder()
	if forwarder == nil {
		return errors.New("no forwarder runs for the network")
	}
	return forwarder.Witness(pin, fork)
}

// F's refresh by its own proofs at its window's point, the new token kept before it is presented (the file's comment).
func (self *deviceLocalForwarding) runRefresh(ctx context.Context) {
	defer self.loops.Done()
	retryCount := 0
	for {
		self.stateLock.Lock()
		client := self.client
		self.stateLock.Unlock()
		byJwt, err := self.keys.token()
		wait := self.settings.RefreshRetryMaxTimeout
		if err == nil && byJwt != "" {
			if at, ok := deviceForwardRefreshAt(byJwt, self.settings); ok {
				wait = time.Until(at)
			}
		}
		if 0 < retryCount {
			wait = deviceForwardBackoff(self.settings.RefreshRetryMinTimeout, self.settings.RefreshRetryMaxTimeout, retryCount)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(wait, 0)):
		}
		if client == nil {
			retryCount += 1
			continue
		}
		newJwt, err := client.Refresh(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			retryCount += 1
			self.log.Infof("[forward]%s the forwarding client's refresh failed: %v\n", self.networkId, err)
			if self.lapsedNow() {
				self.markLapsed()
			}
			continue
		}
		if err := self.keys.setToken(newJwt); err != nil {
			// not kept, so not presented (D-CORE-8's order)
			retryCount += 1
			continue
		}
		client.SetByJwt(newJwt)
		retryCount = 0
		self.stateLock.Lock()
		self.expiresAt = deviceForwardTokenExpiry(newJwt)
		self.stateLock.Unlock()
	}
}

// Holds the posture pending for a lapsed F.
func (self *deviceLocalForwarding) markLapsed() {
	self.stateLock.Lock()
	changed := !self.lapsed
	self.lapsed = true
	self.stateLock.Unlock()
	if changed {
		self.sink.applyFork()
	}
}

// §6.8's long-poll on F: each team hint's position, and a resync or a gap as a hint naming none, to the Forwarder.
func (self *deviceLocalForwarding) runPoll(ctx context.Context) {
	defer self.loops.Done()
	var cursor []byte
	var lastSeq uint64
	retryCount := 0
	for ctx.Err() == nil {
		self.stateLock.Lock()
		client, forwarder := self.client, self.forwarder
		self.stateLock.Unlock()
		if client == nil || forwarder == nil {
			return
		}
		pollCtx, cancel := context.WithTimeout(ctx, self.settings.PollTimeout)
		response, err := client.SyncPoll(pollCtx, cursor)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			retryCount += 1
			select {
			case <-ctx.Done():
				return
			case <-time.After(deviceForwardBackoff(self.settings.PollRetryMinTimeout, self.settings.PollRetryMaxTimeout, retryCount)):
			}
			continue
		}
		retryCount = 0
		if 0 < len(response.GetCursor()) {
			cursor = response.GetCursor()
		}
		if reset := response.GetReset_(); reset != nil {
			forwarder.ReceiveHint(0)
			for _, version := range reset.GetVersions() {
				if version.GetTopic() == sidecar.PushTopicTeam {
					forwarder.ReceiveHint(version.GetVersion())
				}
			}
		}
		for _, hint := range response.GetHints() {
			if lastSeq != 0 && hint.GetSeq() != lastSeq+1 {
				// hints were lost: follow whatever they named
				forwarder.ReceiveHint(0)
			}
			lastSeq = hint.GetSeq()
			if hint.GetTopic() == sidecar.PushTopicTeam {
				forwarder.ReceiveHint(hint.GetVersion())
			}
		}
	}
}

// Looks for C8 each PromotionCheckInterval and after each state handed.
func (self *deviceLocalForwarding) runPromotion(ctx context.Context) {
	defer self.loops.Done()
	self.stateLock.Lock()
	wanted := self.promotionWanted
	self.stateLock.Unlock()
	for {
		self.checkPromotion()
		select {
		case <-ctx.Done():
			return
		case <-wanted:
		case <-time.After(self.settings.PromotionCheckInterval):
		}
	}
}

// C8: next becomes current once the posture's state in force is the Forwarder's compile at or past delegation_at and
// shows the hand-off's Delegation live for this sidecar. Answers whether it promoted.
func (self *deviceLocalForwarding) checkPromotion() bool {
	self.stateLock.Lock()
	forwarder, setup := self.forwarder, self.setup
	self.stateLock.Unlock()
	if forwarder == nil || setup == nil {
		return false
	}
	delegationId, err := sidecar.DelegationId(setup.GetSignedDelegation())
	if err != nil {
		return false
	}
	status := forwarder.Status()
	decision := status.Decision
	switch {
	case !bytes.Equal(status.ForwardDelegationId, delegationId[:]),
		status.CompiledPin.GetVersion() < setup.GetDelegationAt().GetVersion(),
		decision.State != sidecar.BridgeSinkEnforcing,
		decision.StatePin.GetVersion() != status.CompiledPin.GetVersion():
		return false
	}
	promoted, err := self.keys.promote(delegationId[:])
	if err != nil {
		self.log.Infof("[forward]%s the key set's promotion failed: %v\n", self.networkId, err)
		return false
	}
	if promoted {
		self.log.Infof("[forward]%s the handed-off key set is current\n", self.networkId)
	}
	return promoted
}

// What the role shows.
func (self *deviceLocalForwarding) status() deviceForwardingStatus {
	self.stateLock.Lock()
	forwarder, lapsed, expiresAt := self.forwarder, self.lapsed, self.expiresAt
	self.stateLock.Unlock()
	status := deviceForwardingStatus{
		NetworkId:              self.networkId,
		Hosting:                self.hosting,
		Lapsed:                 lapsed,
		ForwardClientExpiresAt: expiresAt,
		KeySet:                 deviceForwardKeySetNone,
	}
	if self.host != nil && self.host.owner != nil {
		status.Posture = self.host.owner.roleDecision(self.networkId).State
	}
	if next, nextStatus := self.keys.loadSet(deviceForwardSlotNext); nextStatus == deviceForwardSecretPresent {
		status.KeySet = deviceForwardKeySetGenerated
		if next.delegationId != nil {
			status.KeySet = deviceForwardKeySetDelegated
		}
	}
	if current, currentStatus := self.keys.loadSet(deviceForwardSlotCurrent); currentStatus == deviceForwardSecretPresent && current.delegationId != nil && status.KeySet != deviceForwardKeySetDelegated {
		status.KeySet = deviceForwardKeySetActive
	}
	if forwarder != nil {
		forwarderStatus := forwarder.Status()
		status.TipN = forwarderStatus.Tip.GetVersion()
		status.CompiledN = forwarderStatus.CompiledPin.GetVersion()
		status.CompileCount = forwarderStatus.CompileCount
		status.CompileErrorCount = forwarderStatus.CompileErrorCount
		status.CompileRefusedCount = forwarderStatus.CompileRefusedCount
		for _, alarm := range forwarderStatus.Alarms {
			status.Alarms = append(status.Alarms, string(alarm.Reason))
		}
	}
	return status
}

// The network's posture as the Forwarder's sink (sidecar.ForwarderSink), through the owner's role for the network:
// the device client's network's is the primary, every other network's its own.
type deviceForwardSink struct {
	role *deviceLocalForwarding
}

var _ sidecar.ForwarderSink = (*deviceForwardSink)(nil)

// The owner's role call for the network.
func (self *deviceForwardSink) onRole(build bool, call func(bridge deviceBridgeOwned) error) error {
	owner := self.role.host.owner
	if owner == nil {
		return errDeviceBridgeClosed
	}
	return owner.onRole(self.role.networkId, build, call)
}

// legacy -> pending, building the role's bridge, which takes the fork hold at once where one stands (a lapsed F).
func (self *deviceForwardSink) SetPending(pending *bridge.Pending) error {
	if err := self.onRole(true, func(bridge deviceBridgeOwned) error {
		return bridge.SetPending(pending)
	}); err != nil {
		return err
	}
	self.role.stateLock.Lock()
	hold := self.role.witnessFork || self.role.lapsed
	self.role.stateLock.Unlock()
	if hold {
		return self.applyFork()
	}
	return nil
}

// A compiled state with its log run; a promotion is looked for after it.
func (self *deviceForwardSink) SetState(state *bridge.BridgeState, log *sidecar.BridgeStateLog) error {
	err := self.onRole(false, func(bridge deviceBridgeOwned) error {
		return bridge.SetState(state, devicePostureLogOf(log))
	})
	if err == nil {
		self.role.stateLock.Lock()
		wanted := self.role.promotionWanted
		self.role.stateLock.Unlock()
		select {
		case wanted <- struct{}{}:
		default:
		}
	}
	return err
}

// The entries after the posture's pin; the sidecar's own sign-out among them, which moves a posture past legacy back to
// legacy, wipes the role. A posture legacy before the call wipes nothing, so a role whose anchor did not take keeps its
// keys.
func (self *deviceForwardSink) ApplyDeltas(log *sidecar.BridgeStateLog) error {
	owner := self.role.host.owner
	before := owner.roleDecision(self.role.networkId).State
	err := self.onRole(false, func(bridge deviceBridgeOwned) error {
		return bridge.ApplyDeltas(devicePostureLogOf(log))
	})
	if before != sidecardevice.PostureLegacy && owner.roleDecision(self.role.networkId).State == sidecardevice.PostureLegacy {
		self.role.host.wipe(self.role)
	}
	return err
}

// The app's sidecar role found a fork, or cleared it: held with a lapse.
func (self *deviceForwardSink) SetFork(fork bool) error {
	self.role.stateLock.Lock()
	self.role.witnessFork = fork
	self.role.stateLock.Unlock()
	return self.applyFork()
}

// Puts the fork hold on the posture: a witness's fork or a lapsed F.
func (self *deviceForwardSink) applyFork() error {
	self.role.stateLock.Lock()
	hold := self.role.witnessFork || self.role.lapsed
	self.role.stateLock.Unlock()
	return self.onRole(false, func(bridge deviceBridgeOwned) error {
		return bridge.SetFork(hold)
	})
}

// The posture's decision, as the Forwarder reads it.
func (self *deviceForwardSink) Decision() sidecar.BridgeSinkDecision {
	owner := self.role.host.owner
	if owner == nil {
		return sidecar.BridgeSinkDecision{State: sidecar.BridgeSinkLegacy}
	}
	timeout := sidecardevice.DefaultPostureSettings().NarrowingOverdueTimeout
	if settings := self.role.host.bridgeSettings; settings != nil && settings.PostureSettings != nil {
		timeout = settings.PostureSettings.NarrowingOverdueTimeout
	}
	return deviceForwardSinkDecisionOf(owner.roleDecision(self.role.networkId), timeout)
}

// The root's log run as device.PostureLog, which never links the root's type.
func devicePostureLogOf(log *sidecar.BridgeStateLog) *sidecardevice.PostureLog {
	if log == nil {
		return nil
	}
	return &sidecardevice.PostureLog{Entries: log.Entries, OpenRows: log.OpenRows}
}

// A posture's decision as the Forwarder reads it: legacy, pending (a fork, a full overlay or no state among it) or
// enforcing; the pins; the waiting count and the next time an alarm is due.
func deviceForwardSinkDecisionOf(decision sidecardevice.PostureDecision, overdueTimeout time.Duration) sidecar.BridgeSinkDecision {
	sink := sidecar.BridgeSinkDecision{
		Pin:           decision.Pin,
		Fork:          decision.Fork,
		CompileWanted: decision.CompileWanted,
		WaitingCount:  len(decision.Waiting),
	}
	switch {
	case decision.State == sidecardevice.PostureLegacy:
		sink.State = sidecar.BridgeSinkLegacy
	case decision.State.Enforcing() && !decision.Fork && decision.OverlayFullThroughN == 0:
		sink.State = sidecar.BridgeSinkEnforcing
	default:
		sink.State = sidecar.BridgeSinkPending
	}
	if decision.BridgeState != nil {
		sink.StatePin = decision.BridgeState.GetAuth()
	}
	if overdue := decision.Overdue; overdue != nil {
		raised := slices.ContainsFunc(decision.Alarms, func(alarm sidecardevice.PostureAlarm) bool {
			return alarm.Kind == sidecardevice.PostureAlarmCompileOverdue
		})
		sink.Overdue = &sidecar.BridgeSinkOverdue{N: overdue.N, Age: overdue.Age, Raised: raised}
		sink.NextAlarmAt = overdue.NextAlarmAt
	} else if 0 < len(decision.Waiting) && 0 < overdueTimeout {
		oldest := decision.Waiting[0].FirstSeen
		for _, waiting := range decision.Waiting {
			if waiting.FirstSeen.Before(oldest) {
				oldest = waiting.FirstSeen
			}
		}
		sink.NextAlarmAt = oldest.Add(overdueTimeout)
	}
	return sink
}

// A jwt's exp, read without verifying it, zero for one that does not read.
func deviceForwardTokenExpiry(byJwt string) time.Time {
	_, expiresAt, ok := deviceForwardTokenTimes(byJwt)
	if !ok {
		return time.Time{}
	}
	return expiresAt
}

// A jwt's iat and exp, read without verifying them: /auth/refresh is what checks a token.
func deviceForwardTokenTimes(byJwt string) (time.Time, time.Time, bool) {
	parts := strings.Split(byJwt, ".")
	if len(parts) != 3 {
		return time.Time{}, time.Time{}, false
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	claims := struct {
		Iat float64 `json:"iat"`
		Exp float64 `json:"exp"`
	}{}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil || claims.Exp <= 0 || claims.Exp <= claims.Iat || math.MaxInt32 < claims.Exp {
		return time.Time{}, time.Time{}, false
	}
	return time.Unix(int64(claims.Iat), 0), time.Unix(int64(claims.Exp), 0), true
}

// When F refreshes: iat plus its lifetime times a point in [RefreshWindowStart, RefreshWindowEnd) drawn by the token's
// hash (the file's comment).
func deviceForwardRefreshAt(byJwt string, settings *deviceForwardingSettings) (time.Time, bool) {
	issuedAt, expiresAt, ok := deviceForwardTokenTimes(byJwt)
	if !ok {
		return time.Time{}, false
	}
	digest := sha256.Sum256([]byte(byJwt))
	draw := float64(binary.BigEndian.Uint64(digest[:8])) / float64(math.MaxUint64)
	fraction := settings.RefreshWindowStart + (settings.RefreshWindowEnd-settings.RefreshWindowStart)*draw
	lifetime := expiresAt.Sub(issuedAt)
	return issuedAt.Add(time.Duration(float64(lifetime) * fraction)), true
}

// min doubled count-1 times, at most max.
func deviceForwardBackoff(minTimeout time.Duration, maxTimeout time.Duration, count int) time.Duration {
	timeout := minTimeout
	for i := 1; i < count && timeout < maxTimeout; i += 1 {
		timeout *= 2
	}
	return min(timeout, maxTimeout)
}

// The labels the sidecar's services answer under beside api's (sidecar's host list, SidecarHostListV1, which the
// tunnel's leg does not link): a format value TestForwardUrlsMatchTheHostList holds equal on the host legs.
var deviceForwardServiceLabels = struct {
	api, auth, evs, gkm, sockets, dns string
}{api: "api", auth: "auth", evs: "evs", gkm: "gkm", sockets: "sockets", dns: "dns"}

// A forwarding client's settings: the network space's api and platform urls, and the services' urls beside api's.
func deviceForwardApiSettings(apiUrl string, platformUrl string) (*sidecar.SidecarSettings, error) {
	settings := sidecar.DefaultSidecarSettings()
	settings.ApiUrl = apiUrl
	settings.PlatformUrl = platformUrl
	labels := deviceForwardServiceLabels
	for _, service := range []struct {
		label string
		into  *string
	}{
		{labels.auth, &settings.AuthUrl},
		{labels.evs, &settings.EvsUrl},
		{labels.gkm, &settings.GkmUrl},
		{labels.sockets, &settings.SocketsUrl},
		{labels.dns, &settings.DnsUrl},
	} {
		serviceUrl, err := sidecarServiceUrlOf(apiUrl, labels.api, service.label)
		if err != nil {
			return nil, err
		}
		*service.into = serviceUrl
	}
	return settings, nil
}

// apiUrl with its first label's apiLabel swapped for serviceLabel, an env's "<env>-" prefix, the port and the path
// kept. An api url whose first label is not apiLabel, alone or after a prefix, refuses.
func sidecarServiceUrlOf(apiUrl string, apiLabel string, serviceLabel string) (string, error) {
	parsed, err := url.Parse(apiUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", errors.New("the network space's api url is not an https url")
	}
	first, rest, ok := strings.Cut(parsed.Hostname(), ".")
	if !ok || rest == "" {
		return "", errors.New("the network space's api url has no host to put the sidecar's services under")
	}
	var label string
	switch {
	case first == apiLabel:
		label = serviceLabel
	case strings.HasSuffix(first, "-"+apiLabel):
		label = strings.TrimSuffix(first, apiLabel) + serviceLabel
	default:
		return "", fmt.Errorf("the network space's api host does not start with %s", apiLabel)
	}
	host := label + "." + rest
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	serviceUrl := &url.URL{Scheme: parsed.Scheme, Host: host, Path: parsed.Path, RawPath: parsed.RawPath}
	return serviceUrl.String(), nil
}

// Starts the VPN component's Forwarders once the device is built: F's requests dial by the device's client strategy,
// the urls are the network space's, and the device client's id and ClientKey public key name this device as host in
// its own network. Nothing without forwarding.
func (self *DeviceLocal) startForwarding() {
	if self.bridgeOwner == nil || self.bridgeOwner.forwarding == nil {
		return
	}
	deps := &deviceForwardingDeps{
		ctx:         self.ctx,
		apiUrl:      self.networkSpace.GetApiUrl(),
		platformUrl: self.networkSpace.GetPlatformUrl(),
		transport:   sidecar.NewForwardingTransport(self.ctx, self.clientStrategy),
		hostClient: func() ([]byte, []byte) {
			client := self.providerClientSnapshot()
			if client == nil || client.ClientKeyManager() == nil {
				return nil, nil
			}
			return bytes.Clone(self.clientId[:]), bytes.Clone(client.ClientKeyManager().PublicKey())
		},
	}
	if self.settings.testingForwardingDeps != nil {
		self.settings.testingForwardingDeps(deps)
	}
	self.bridgeOwner.forwarding.start(deps)
}
