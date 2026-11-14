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
// nothing and leaves the core Locked until it can answer (D-SDK-15, §5.9's table).
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
	// Called once the components that waited for the core's identity have been built for it
	// (addDeferredComponentsOnceIdentified). Nil calls nothing.
	deferredComponentsBuilt func()
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
	cancelCtx, cancel := context.WithCancel(ctx)
	instance, err := sidecar.NewSidecarInstance(cancelCtx, &sidecar.SidecarInstanceArgs{
		Backend:   backend,
		KeyStore:  keyStoreAdapter,
		MachineId: sidecar.SidecarMachineId(),
		Settings:  coreSettings,
	})
	if err != nil {
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
		closeDone:          make(chan struct{}),
		local:              local,
		instance:           instance,
		components:         map[string]sidecar.SidecarComponent{},
		componentFactoryOf: options.componentFactoryOf,
		evsSettings:        evsSettings,
		socketsSettings:    socketsSettings,
	}
	self.removeEventCallback = local.AddEventCallback(self.events.offer)
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
	if 0 < len(self.deferredComponents) {
		go self.addDeferredComponentsOnceIdentified(local, options.deferredComponentsBuilt)
	}
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
		alreadyClosed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return true
			}
			self.closed = true
			local, instance, remote, removeEventCallback = self.local, self.instance, self.remote, self.removeEventCallback
			evs = self.evs
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
