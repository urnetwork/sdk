//go:build !ios_extension && !js

// sdk.Sidecar (§18.5, D-SDK-2): one network's sidecar for an app. The local backend runs the core in this process (the
// Android and iOS apps); the remote one forwards to `ur daemon`'s control mode (§5.8; NewSidecarRemote in
// sidecar_remote.go, over sidecar_backend.go's sidecarBackend).
// Only bound types cross: no sidecar-package value but the Go-only Core and CloseAndWait, which gobind skips. Every
// exported call runs under trace, since a panic that crosses gobind exits the app (R-SDK-5).
//
// The shell is dark (plan-SDK-small-readings #10): §18.17 lands SignEdgeLogin and the directory's seams on it at M6,
// and M8 adds the hosts' calls (SDK-M8-01's EnrollWithDevice, Unlock, GetState and SetPushToken in sidecar_enroll.go,
// sidecar_state.go and sidecar_push.go, over the host services of sidecar_host_services.go and the host class of
// sidecar_host_class.go; the wrappers' GetEvs and its siblings). NewSidecarLocal opens the instance under StateDir with the app's key store and runs the core on it,
// opening U by the key store's wrap as `ur daemon` does at start: a store that answers Locked or Failed creates
// nothing and leaves the core Locked until it can answer (D-SDK-15, §5.9's table). Whatever components the app names,
// the core runs its dns component, as each of ur daemon's networks does, so its DnsClient feeds the network's label
// table and follows the zone's hints from the core's first client generation on (§12.5's label row). On a host class
// that stays up, desktop's, the core runs D-TRUST-9's zone watch beside it, as ur daemon does and as that class runs
// D-TLS-8's CT watch; the foreground-only classes, iOS and Android, run none and leave it to the network's daemons, as
// they leave the CT watch (QD-1384).
//
// Outward calls pass the core's gate, Sidecar.Authorize, on both backends, and come back approval_pending:<id>
// (sidecarResultOf), so no wrapper adds a second check. Nothing here imports mcp/ or cli/ (R-SDK-4).

package sdk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

// What a test adjusts in a local Sidecar. Production passes the zero value.
type sidecarLocalOptions struct {
	// The core's settings, last, before the core runs.
	adjustCoreSettings func(settings *sidecar.SidecarSettings)
	// Nil: the OS backend over StateDir.
	backend sidecar.SidecarStoreBackend
	// Nil: defaultSidecarEventSettings.
	eventSettings *sidecarEventSettings
	// Nil: the core scans this host's stores for a readable network credential (D-TRUST-3), as an app's does. A test
	// sets what the scan finds, so the build machine's own app state never decides it.
	hostCredentialStores func() []sidecar.SidecarHostCredentialStore
	// Factories by component name that a test puts in place of the registered ones. Nil takes the registry's.
	componentFactories map[string]sidecarComponentFactory
	// Nil: defaultSidecarEvsSettings.
	evsSettings *sidecarEvsSettings
	// Nil: defaultSidecarSocketsSettings.
	socketsSettings *sidecarSocketsSettings
	// The zone watch's, on a host class that stays up. Nil: the core's DefaultDnsWatchComponentSettings, whose public
	// resolvers a test binary refuses, so a test that composes the watch names its own DNS here.
	dnsWatchSettings *sidecar.DnsWatchComponentSettings
	// Called once the components that waited for the core's identity have been built for it
	// (addDeferredComponentsOnceIdentified). Nil calls nothing.
	deferredComponentsBuilt func()
	// The free bytes on StateDir's volume a phone's search cap reads (sidecar_search_host.go). Nil: statfs.
	searchFreeByteCount func(dir string) (uint64, error)
	// The clock a background window's deadline is read on. Nil: time.Now.
	searchNow func() time.Time
}

// The factory of name: a test's in place of the registry's.
func (self *sidecarLocalOptions) componentFactoryOf(name string) (sidecarComponentFactory, bool) {
	if factory, ok := self.componentFactories[name]; ok {
		return factory, true
	}
	return sidecarComponentFactoryOf(name)
}

// A host whose credential scan a test set (sidecarLocalOptions.hostCredentialStores).
type sdkSidecarScannedHost struct {
	*sdkSidecarHost
	stores func() []sidecar.SidecarHostCredentialStore
}

// What the test set. See sidecar.SidecarHostCredentialScanner.
func (self *sdkSidecarScannedHost) HostCredentialStores() []sidecar.SidecarHostCredentialStore {
	return self.stores()
}

// One network's sidecar. Safe for concurrent use.
type Sidecar struct {
	ctx            context.Context
	cancel         context.CancelFunc
	settings       *SidecarSettings
	presenceSigner SidecarPresenceSigner
	events         *sidecarEventDispatch
	// the local backend's host, whose token source EnrollWithDevice fills; nil on the remote one
	host *sdkSidecarHost
	// §18.4's host class
	hostClass *sidecarHostClass
	// closed once Close's teardown has finished
	closeDone chan struct{}
	// held across one build of the waiting components, so a second waits for the first to finish: an
	// EnrollWithDevice that returns finds them built, whichever started them. Taken before stateLock, never inside it.
	deferredBuildLock sync.Mutex

	stateLock sync.Mutex
	local     *sidecar.Sidecar
	// the local backend's instance, closed once the core's run loop has ended
	instance *sidecar.SidecarInstance
	// the remote backend (sidecar_backend.go, sidecar_remote.go); nil on the local one
	remote sidecarBackend
	// ends the core's event callback
	removeEventCallback func()
	closed              bool
	// the components the factories built, by name, each attached to the core
	components map[string]sidecar.SidecarComponent
	// the names whose factories wait for the core's enrollment, in the settings' order
	deferredComponents []string
	// what builds a component by name: the registry's factories, or a test's
	componentFactoryOf func(name string) (sidecarComponentFactory, bool)
	// the wrappers' tunables
	evsSettings     *sidecarEvsSettings
	socketsSettings *sidecarSocketsSettings
	// the wrappers, made at their first Get
	evs     *SidecarEvs
	sockets *SidecarSockets
	// the team directory's wrapper (sidecar_directory.go), made at the first GetDirectory in a team network; its
	// tunables, nil for the defaults; and a test's backend in place of this Sidecar's own, nil otherwise
	directory         *SidecarDirectory
	directorySettings *sidecarDirectorySettings
	directoryBackend  sidecarDirectoryBackend
	// the bounds SignEdgeLogin holds its arguments to (sidecar_edge_login.go), nil for the defaults
	edgeLoginSettings *sidecarEdgeLoginSettings
	// the mail wrapper (sidecar_mail.go), made at the first GetMail; its tunables, nil for the defaults; and a test's
	// backend in place of this Sidecar's own, nil otherwise
	mail         *SidecarMail
	mailSettings *sidecarMailSettings
	mailBackend  sidecarMailBackend
	// forwarding's setup for the app's VPN component (sidecar_forward_setup.go), made at the first use
	forwarding *sidecarForwarding
	// the approvals and device revocation wrapper (sidecar_approvals.go), made at the first GetApprovals; its
	// tunables, nil for the defaults; and a test's backend in place of this Sidecar's own, nil otherwise
	approvals         *SidecarApprovals
	approvalsSettings *sidecarApprovalsSettings
	approvalsBackend  sidecarApprovalsBackend
	// the search wrapper (sidecar_search.go), made at the first GetSearch; its tunables, nil for the defaults; a test's
	// backend in place of this Sidecar's own, nil otherwise; and the host's search state (sidecar_search_host.go), which
	// SetForeground and RunBackgroundSync report to, nil on the remote backend
	search         *SidecarSearch
	searchSettings *sidecarSearchSettings
	searchBackend  sidecarSearchBackend
	searchHost     *sidecarSearchHost
	// DESIGN-COMPUTE §15.3's wrappers, each made once at its accessor's first call (CSURF-M11-01) by the constructor in
	// its area's file, and closed with the Sidecar where the area gives it a close
	compute       *SidecarCompute
	models        *SidecarModels
	enclaves      *SidecarEnclaves
	agents        *SidecarAgents
	calendar      *SidecarCalendar
	connections   *SidecarConnections
	listeners     *SidecarListeners
	autoApprovals *SidecarAutoApprovals
	// the app process's support log ring on the local backend (sidecar_support.go), nil on the remote one or where
	// none opens
	supportLog *sidecarSupportLog
	// the app's compute hosts, as its setters last set them (sidecar_compute_host.go); nil until set
	connectHost          SidecarConnectHost
	localInferenceHost   SidecarLocalInferenceHost
	safetyClassifierHost SidecarSafetyClassifierHost
}

// A Sidecar on the local backend: the instance under settings' StateDir, the app's keyStore wrapping U, networkSpace's
// service urls, and settings' components by name. presenceSigner may be nil, in which case another device approves.
func NewSidecarLocal(networkSpace *NetworkSpace, keyStore SidecarKeyStore, presenceSigner SidecarPresenceSigner, settings *SidecarSettings) (*Sidecar, error) {
	return traceWithReturnError(func() (*Sidecar, error) {
		return newSidecarLocal(context.Background(), networkSpace, keyStore, presenceSigner, settings, &sidecarLocalOptions{})
	})
}

// NewSidecarLocal with a test's options. Everything it opened is closed when it refuses.
func newSidecarLocal(
	ctx context.Context,
	networkSpace *NetworkSpace,
	keyStore SidecarKeyStore,
	presenceSigner SidecarPresenceSigner,
	settings *SidecarSettings,
	options *sidecarLocalOptions,
) (*Sidecar, error) {
	if networkSpace == nil {
		return nil, errors.New("a local sidecar needs a network space")
	}
	if keyStore == nil {
		return nil, errors.New("a local sidecar needs the app's key store")
	}
	settings, err := checkedSidecarSettingsWith(settings, options.backend == nil, options.componentFactoryOf)
	if err != nil {
		return nil, err
	}
	components := settings.Components.getAll()

	coreSettings := sidecar.DefaultSidecarSettings()
	coreSettings.Instance = settings.Instance
	hostClass, err := applySidecarHostClass(settings, coreSettings)
	if err != nil {
		return nil, err
	}
	// the class's search, as an app host's: its policy, working set and caps (SDK-M10-02)
	searchHost := newSidecarSearchHost(hostClass, settings.StateDir, options.searchFreeByteCount, options.searchNow)
	searchHost.apply(coreSettings)
	if err := applySidecarServiceUrls(networkSpace, coreSettings); err != nil {
		return nil, err
	}
	if options.adjustCoreSettings != nil {
		options.adjustCoreSettings(coreSettings)
	}

	backend := options.backend
	if backend == nil {
		backend, err = sidecar.NewSidecarOsBackend(settings.StateDir)
		if err != nil {
			return nil, err
		}
	}
	keyStoreAdapter := newSidecarKeyStoreAdapter(keyStore)
	// §28.7's support log, which the core writes through (sidecar_support.go)
	supportLog := openSidecarSupportLog(settings.StateDir, coreSettings)
	cancelCtx, cancel := context.WithCancel(ctx)
	instance, err := sidecar.NewSidecarInstance(cancelCtx, &sidecar.SidecarInstanceArgs{
		Backend:   backend,
		KeyStore:  keyStoreAdapter,
		MachineId: sidecar.SidecarMachineId(),
		Settings:  coreSettings,
	})
	if err != nil {
		supportLog.close()
		cancel()
		return nil, err
	}
	var networkId *sidecar.Id
	if records := instance.Networks(); 0 < len(records) {
		// one network a Sidecar until several networks land (CORE-M6-01)
		networkId = &records[0].NetworkId
	}
	host := &sdkSidecarHost{store: instance.Store(networkId), keyStore: keyStoreAdapter}
	var coreHost sidecar.SidecarHost = host
	if options.hostCredentialStores != nil {
		coreHost = &sdkSidecarScannedHost{sdkSidecarHost: host, stores: options.hostCredentialStores}
	}
	local, err := sidecar.NewSidecar(cancelCtx, coreHost, nil, coreSettings)
	if err != nil {
		instance.Close()
		supportLog.close()
		cancel()
		return nil, err
	}

	eventSettings := options.eventSettings
	if eventSettings == nil {
		eventSettings = defaultSidecarEventSettings()
	}
	evsSettings := options.evsSettings
	if evsSettings == nil {
		evsSettings = defaultSidecarEvsSettings()
	}
	socketsSettings := options.socketsSettings
	if socketsSettings == nil {
		socketsSettings = defaultSidecarSocketsSettings()
	}
	self := &Sidecar{
		ctx:                cancelCtx,
		cancel:             cancel,
		settings:           settings,
		presenceSigner:     presenceSigner,
		events:             newSidecarEventDispatch(cancelCtx, eventSettings),
		host:               host,
		hostClass:          hostClass,
		searchHost:         searchHost,
		closeDone:          make(chan struct{}),
		local:              local,
		instance:           instance,
		components:         map[string]sidecar.SidecarComponent{},
		componentFactoryOf: options.componentFactoryOf,
		evsSettings:        evsSettings,
		socketsSettings:    socketsSettings,
		supportLog:         supportLog,
	}
	self.removeEventCallback = local.AddEventCallback(self.events.offer)
	// whatever components the app names, the network's label table, which every zone the core's DnsClient fetches
	// writes, and the zone's hints, from the core's first client generation on (the core's dns component, §12.5's
	// label row), as ur daemon adds it to each network's Sidecar; the core closes it with itself
	if _, err := local.AddComponent(local.NewDnsComponent()); err != nil {
		self.Close()
		return nil, fmt.Errorf("the dns component: %w", err)
	}
	// D-TRUST-9's zone watch on a host class that stays up, as ur daemon adds it beside the dns component and as that
	// class runs the CT watch (sidecar_dns_watch.go; QD-1384); a foreground-only class leaves it to the network's daemons
	if watch := newSidecarDnsWatchComponent(local, hostClass, options.dnsWatchSettings); watch != nil {
		if _, err := local.AddComponent(watch); err != nil {
			watch.Close()
			self.Close()
			return nil, fmt.Errorf("the zone watch: %w", err)
		}
	}
	for _, name := range components {
		if err := self.addComponent(local, name); err != nil {
			if errors.Is(err, errSidecarComponentNotEnrolled) {
				// built once EnrollWithDevice has enrolled the core, or once a restart's unlock gives it its identity
				// (addDeferredComponents)
				self.deferredComponents = append(self.deferredComponents, name)
				continue
			}
			self.Close()
			return nil, fmt.Errorf("component %s: %w", name, err)
		}
	}
	// the key store's wrap of U, as ur daemon opens it at start. A store that is locked, failed or lost creates
	// nothing and leaves the core Locked or Unenrolled(keystore_lost); the app calls Unlock("") once it can answer
	local.Unlock(nil)
	go self.addDeferredComponentsOnceIdentified(local, options.deferredComponentsBuilt)
	return self, nil
}

// Builds the components that waited, once the core holds an identity, unless the Sidecar ends first, then calls built
// when it is set: at a restart the keychain's opening, by the Unlock above or the app's own, gives the core its
// identity with no EnrollWithDevice to build them, and a component that needs the identity, tls among them, waits for
// it (TLS-M8-01). A first enrollment builds them in EnrollWithDevice as well; whichever comes second finds none left.
func (self *Sidecar) addDeferredComponentsOnceIdentified(local *sidecar.Sidecar, built func()) {
	if _, err := awaitSidecarIdentity(self.ctx, local); err != nil {
		return
	}
	self.addDeferredComponents(local)
	if built != nil {
		built()
	}
}

// Builds name's component by its factory and attaches it to local, keeping it by name. A factory's refusal, its
// errSidecarComponentNotEnrolled among them, attaches nothing.
func (self *Sidecar) addComponent(local *sidecar.Sidecar, name string) error {
	factory, ok := self.componentFactoryOf(name)
	if !ok {
		return fmt.Errorf("the sidecar component %q is not bound in this sdk yet", name)
	}
	component, err := traceWithReturnError(func() (sidecar.SidecarComponent, error) {
		return factory(self.ctx, local, self.settings)
	})
	if err != nil {
		return err
	}
	if component == nil {
		return errors.New("its factory built nothing")
	}
	if _, err := local.AddComponent(component); err != nil {
		component.Close()
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.components[name] = component
	return nil
}

// Builds and attaches each component that waited for the enrollment, once EnrollWithDevice has enrolled the core. One
// that still refuses waits on for the next enrollment, its wrapper answering unavailable meanwhile, and the enrollment
// stands: it answers the names that wait. A build already running finishes before this one looks.
func (self *Sidecar) addDeferredComponents(local *sidecar.Sidecar) []string {
	self.deferredBuildLock.Lock()
	defer self.deferredBuildLock.Unlock()
	// Every local member host follows its verified configuration, including one whose
	// exported components use only sockets, mail or forwarding.
	if replica, err := local.OpenMemberReplica(self.ctx); err == nil {
		replica.SetPresenceCheck(sidecarPresenceCheckOf(self.presenceSigner))
	}
	names := func() []string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		names := self.deferredComponents
		self.deferredComponents = nil
		return names
	}()
	waiting := []string{}
	for _, name := range names {
		if err := self.addComponent(local, name); err != nil {
			waiting = append(waiting, name)
		}
	}
	if 0 < len(waiting) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.deferredComponents = append(waiting, self.deferredComponents...)
	}
	return waiting
}

// The component the factories built under name, nil while none is attached.
func (self *Sidecar) component(name string) sidecar.SidecarComponent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.components[name]
}

// A copy of settings that NewSidecarLocal can run: the default instance for an empty one, an instance name the core
// takes, an absolute StateDir where one is needed, no negative budget, and only components this sdk binds, each
// once.
func checkedSidecarSettings(settings *SidecarSettings, needsStateDir bool) (*SidecarSettings, error) {
	return checkedSidecarSettingsWith(settings, needsStateDir, sidecarComponentFactoryOf)
}

// checkedSidecarSettings with the factories of factoryOf.
func checkedSidecarSettingsWith(settings *SidecarSettings, needsStateDir bool, factoryOf func(name string) (sidecarComponentFactory, bool)) (*SidecarSettings, error) {
	if settings == nil {
		return nil, errors.New("a local sidecar needs settings")
	}
	checked := &SidecarSettings{
		Instance:              settings.Instance,
		StateDir:              settings.StateDir,
		Components:            NewStringList(),
		MemoryBudgetByteCount: settings.MemoryBudgetByteCount,
		HostClass:             settings.HostClass,
		DaemonSocketDir:       settings.DaemonSocketDir,
	}
	if _, err := sidecarHostClassOf(settings.HostClass); err != nil {
		return nil, err
	}
	instance, err := sidecar.SidecarInstanceName(settings.Instance)
	if err != nil {
		return nil, err
	}
	checked.Instance = instance
	if needsStateDir && (settings.StateDir == "" || !filepath.IsAbs(settings.StateDir)) {
		return nil, errors.New("a local sidecar's StateDir must be an absolute directory")
	}
	if settings.MemoryBudgetByteCount < 0 {
		return nil, errors.New("a sidecar's memory budget must not be negative")
	}
	if settings.Components != nil {
		for _, name := range settings.Components.getAll() {
			if !slices.Contains(sidecarComponentNames, name) {
				return nil, fmt.Errorf("no sidecar component %q", name)
			}
			if checked.Components.Contains(name) {
				return nil, fmt.Errorf("the sidecar component %q is named twice", name)
			}
			if _, ok := factoryOf(name); !ok {
				return nil, fmt.Errorf("the sidecar component %q is not bound in this sdk yet", name)
			}
			checked.Components.Add(name)
		}
	}
	return checked, nil
}

// The backends, under the lock.
func (self *Sidecar) backends() (*sidecar.Sidecar, sidecarBackend) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.local, self.remote
}

// This sidecar's sidecar_id, nil before it has one: while its keychain has never opened, and before enrollment.
func (self *Sidecar) GetSidecarId() *Id {
	sidecarId, err := traceWithReturn(func() *Id {
		local, remote := self.backends()
		if local != nil {
			identity := local.Identity()
			if identity == nil {
				return nil
			}
			return newId([16]byte(identity.SidecarId))
		}
		if remote != nil {
			return remote.sidecarId()
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return sidecarId
}

// Calls listener with each event from here on, in order, on a Go thread, until the answer closes (§18.5).
func (self *Sidecar) AddEventListener(listener SidecarEventListener) Sub {
	sub, err := traceWithReturn(func() Sub {
		if listener == nil {
			return newSub(func() {})
		}
		remove := self.events.addListener(listener)
		var once sync.Once
		return newSub(func() {
			once.Do(remove)
		})
	})
	if err != nil {
		return newSub(func() {})
	}
	return sub
}

// Ends the Sidecar without waiting: no event reaches a listener after it returns, the core's run loop ends its
// generation and closes each component, and then the instance closes. Safe to call again and from a listener.
func (self *Sidecar) Close() {
	trace(func() {
		var local *sidecar.Sidecar
		var instance *sidecar.SidecarInstance
		var remote sidecarBackend
		var removeEventCallback func()
		var evs *SidecarEvs
		var directory *SidecarDirectory
		var mail *SidecarMail
		var approvals *SidecarApprovals
		var search *SidecarSearch
		var computeWrappers []any
		alreadyClosed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return true
			}
			self.closed = true
			local, instance, remote, removeEventCallback = self.local, self.instance, self.remote, self.removeEventCallback
			evs = self.evs
			directory = self.directory
			mail = self.mail
			approvals = self.approvals
			search = self.search
			computeWrappers = self.computeWrappersWithLock()
			return false
		}()
		if alreadyClosed {
			return
		}
		if removeEventCallback != nil {
			removeEventCallback()
		}
		self.events.close()
		// no repo change reaches a listener after Close returns, as no event does
		if evs != nil {
			evs.close()
		}
		// nor a page of the directory, whose open view closes with it
		if directory != nil {
			directory.close()
		}
		// and a mail call in flight answers unavailable
		if mail != nil {
			mail.close()
		}
		// as does an approvals call, and no pending list reaches a listener after it
		if approvals != nil {
			approvals.close()
		}
		// and every open search ends, its pages with it
		if search != nil {
			search.close()
		}
		// and each compute wrapper its area gives a close ends what it holds (§15.3)
		for _, wrapper := range computeWrappers {
			if closer, ok := wrapper.(sidecarComputeWrapperCloser); ok {
				closer.close()
			}
		}
		// and no witness goes to the VPN component after it
		self.stopForwarding()
		if remote != nil {
			remote.close()
		}
		if local == nil {
			self.cancel()
			close(self.closeDone)
			return
		}
		local.Close()
		go func() {
			defer close(self.closeDone)
			// the instance outlives the run loop, which seals its last writes through it
			local.CloseAndWait(context.Background())
			instance.Close()
			// the app's stop, recorded once the core can write nothing more (sidecar_support.go)
			self.supportLog.close()
			// the child token's session goes once no generation can read it
			if self.host != nil {
				if session := self.host.swapSession(nil); session != nil {
					session.close()
				}
			}
			self.cancel()
		}()
	})
}

// Close, then waits until the core's run loop has ended and the instance has closed, or ctx ends. Go only: never
// from a listener, which it may wait for (connect/CODESTYLE.md:171).
func (self *Sidecar) CloseAndWait(ctx context.Context) error {
	self.Close()
	select {
	case <-self.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The core of a local Sidecar, nil on the remote backend. Go only: gobind skips it, and an app never holds a
// sidecar-package value.
func (self *Sidecar) Core() *sidecar.Sidecar {
	local, _ := self.backends()
	return local
}

// What a compute call answers where its area has not filled its sdk file yet (§15.3; CSURF-M11-01): never a nil, an
// empty success or a default in its place. Each area's task fills its file, and its calls stop answering this.
var ErrSidecarComputeNotBuilt = errors.New("not built in this sdk yet")

// What a compute wrapper's constructor and an area's sdk function are given: the Sidecar's ctx, which ends at its
// Close, and its backends, the core on the local backend or the daemon on the remote one, whose methods are each
// area's (sidecarBackend.call). Read before stateLock is taken, since a wrapper's constructor runs under it and must
// neither block nor take it.
type sidecarComputeArgs struct {
	ctx    context.Context
	local  *sidecar.Sidecar
	remote sidecarBackend
}

// This Sidecar's args, read under stateLock.
func (self *Sidecar) computeArgs() *sidecarComputeArgs {
	local, remote := self.backends()
	return &sidecarComputeArgs{ctx: self.ctx, local: local, remote: remote}
}

// What a compute wrapper implements, in its area's file, to end what it holds at the Sidecar's Close.
type sidecarComputeWrapperCloser interface {
	close()
}

// The compute wrappers made so far, for Close. Called with stateLock held.
func (self *Sidecar) computeWrappersWithLock() []any {
	wrappers := []any{}
	add := func(wrapper any, made bool) {
		if made {
			wrappers = append(wrappers, wrapper)
		}
	}
	add(self.compute, self.compute != nil)
	add(self.models, self.models != nil)
	add(self.enclaves, self.enclaves != nil)
	add(self.agents, self.agents != nil)
	add(self.calendar, self.calendar != nil)
	add(self.connections, self.connections != nil)
	add(self.listeners, self.listeners != nil)
	add(self.autoApprovals, self.autoApprovals != nil)
	return wrappers
}

// The wrapper in slot, made by newWrapper at the first call, under stateLock, and the same one at every call after,
// the Sidecar open or closed: a compute accessor never answers nil. newWrapper runs under stateLock, so it only builds.
func sidecarComputeWrapperOf[T any](self *Sidecar, slot **T, newWrapper func(args *sidecarComputeArgs) *T) *T {
	args := self.computeArgs()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if *slot == nil {
		*slot = newWrapper(args)
	}
	return *slot
}

// A Harness on this Sidecar (§15.3, §7): its settings as JSON, the agents it launches and the events it emits
// (sidecar_harness.go).
func (self *Sidecar) NewHarness(settingsJson string) (*SidecarHarness, error) {
	return traceWithReturnError(func() (*SidecarHarness, error) {
		return newSidecarHarness(self.computeArgs(), settingsJson)
	})
}

// The compute balance, purchases, auto-renew, budgets and the spend view (§15.3, §9; sidecar_compute.go).
func (self *Sidecar) Compute() *SidecarCompute {
	compute, _ := traceWithReturn(func() *SidecarCompute {
		return sidecarComputeWrapperOf(self, &self.compute, newSidecarCompute)
	})
	return compute
}

// The model catalog, local model pulls and the person's models key (§15.3, §6, §16.4; sidecar_models.go).
func (self *Sidecar) Models() *SidecarModels {
	models, _ := traceWithReturn(func() *SidecarModels {
		return sidecarComputeWrapperOf(self, &self.models, newSidecarModels)
	})
	return models
}

// The network's enclaves (§15.3, §11; sidecar_enclaves.go).
func (self *Sidecar) Enclaves() *SidecarEnclaves {
	enclaves, _ := traceWithReturn(func() *SidecarEnclaves {
		return sidecarComputeWrapperOf(self, &self.enclaves, newSidecarEnclaves)
	})
	return enclaves
}

// A terminal session over the bundled SSH to host as user, cols by rows, its output to listener (§15.3, §12;
// sidecar_ssh.go).
func (self *Sidecar) OpenSsh(host string, user string, cols int, rows int, listener SidecarSshListener) (*SidecarSshSession, error) {
	return traceWithReturnError(func() (*SidecarSshSession, error) {
		return openSidecarSsh(self.computeArgs(), host, user, cols, rows, listener)
	})
}

// The recURd skill's agents, their inboxes and the agent graph (§15.3, §20; sidecar_agents.go).
func (self *Sidecar) Agents() *SidecarAgents {
	agents, _ := traceWithReturn(func() *SidecarAgents {
		return sidecarComputeWrapperOf(self, &self.agents, newSidecarAgents)
	})
	return agents
}

// The person's calendar links and events (§15.3, §23; sidecar_calendar.go).
func (self *Sidecar) Calendar() *SidecarCalendar {
	calendar, _ := traceWithReturn(func() *SidecarCalendar {
		return sidecarComputeWrapperOf(self, &self.calendar, newSidecarCalendar)
	})
	return calendar
}

// The member's connected services (§15.3, §25; sidecar_connections.go).
func (self *Sidecar) Connections() *SidecarConnections {
	connections, _ := traceWithReturn(func() *SidecarConnections {
		return sidecarComputeWrapperOf(self, &self.connections, newSidecarConnections)
	})
	return connections
}

// The network's listeners (§15.3, §26; sidecar_listeners.go).
func (self *Sidecar) Listeners() *SidecarListeners {
	listeners, _ := traceWithReturn(func() *SidecarListeners {
		return sidecarComputeWrapperOf(self, &self.listeners, newSidecarListeners)
	})
	return listeners
}

// Every disclosure of the newest verified policy, as JSON (§15.3, §2.8; sidecar_disclosures.go).
func (self *Sidecar) Disclosures() (string, error) {
	return traceWithReturnError(func() (string, error) {
		return sidecarDisclosures(self.computeArgs())
	})
}

// The person's acknowledgment of disclosure id, their act in the app's UI after it showed the whole text (§15.3,
// §2.8, D-CSURF-1; sidecar_disclosures.go). No tool, MCP call or RPC reaches it.
func (self *Sidecar) AckDisclosure(id string) error {
	return traceWithError(func() error {
		return ackSidecarDisclosure(self.computeArgs(), id)
	})
}

// An Owner's or Admin's acknowledgment of disclosure id for the network, from the app's network settings (§15.3,
// §2.8, CO-41; sidecar_disclosures.go).
func (self *Sidecar) AckNetworkDisclosure(id string) error {
	return traceWithError(func() error {
		return ackSidecarNetworkDisclosure(self.computeArgs(), id)
	})
}

// The person's automatic approvals: their mode, the network's limit and the day's digest (§15.3, §27;
// sidecar_auto_approvals.go).
func (self *Sidecar) AutoApprovals() *SidecarAutoApprovals {
	autoApprovals, _ := traceWithReturn(func() *SidecarAutoApprovals {
		return sidecarComputeWrapperOf(self, &self.autoApprovals, newSidecarAutoApprovals)
	})
	return autoApprovals
}

// The app's browser seam for connected services (§15.3, §25.5; sidecar_compute_host.go).
func (self *Sidecar) SetConnectHost(host SidecarConnectHost) {
	trace(func() {
		setSidecarComputeHost(self, &self.connectHost, host, sidecarConnectHostAdapter)
	})
}

// The app's local inference engine (§15.3, §6.5; sidecar_compute_host.go).
func (self *Sidecar) SetLocalInferenceHost(host SidecarLocalInferenceHost) {
	trace(func() {
		setSidecarComputeHost(self, &self.localInferenceHost, host, sidecarLocalInferenceHostAdapter)
	})
}

// The app's safety classifier (§15.3, §27.5; sidecar_compute_host.go).
func (self *Sidecar) SetSafetyClassifierHost(host SidecarSafetyClassifierHost) {
	trace(func() {
		setSidecarComputeHost(self, &self.safetyClassifierHost, host, sidecarSafetyClassifierHostAdapter)
	})
}
