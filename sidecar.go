//go:build !ios_extension && !js

// sdk.Sidecar (§18.5, D-SDK-2): one network's sidecar for an app. The local backend runs the core in this process (the
// Android and iOS apps); the remote one forwards to `ur daemon`'s control mode (§5.8; SDK-M8-02's NewSidecarRemote).
// Only bound types cross: no sidecar-package value but the Go-only Core and CloseAndWait, which gobind skips. Every
// exported call runs under trace, since a panic that crosses gobind exits the app (R-SDK-5).
//
// The shell is dark (plan-SDK-small-readings #10): §18.17 lands SignEdgeLogin and the directory's seams on it at M6,
// and M8 adds the hosts' calls (SDK-M8-01's EnrollWithDevice, Unlock and state; the wrappers' GetEvs and its
// siblings). NewSidecarLocal opens the instance under StateDir with the app's key store and runs the core on it,
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

// The control-mode client a remote-mode Sidecar forwards each call to (§5.8). SDK-M8-02 builds it and grows it; nil
// on the local backend.
type sidecarRemote interface {
	// The daemon's sidecar_id, nil before it has one.
	sidecarId() *Id
	// Ends the control session.
	close()
}

// What a test adjusts in a local Sidecar. Production passes the zero value.
type sidecarLocalOptions struct {
	// The core's settings, last, before the core runs.
	adjustCoreSettings func(settings *sidecar.SidecarSettings)
	// Nil: the OS backend over StateDir.
	backend sidecar.SidecarStoreBackend
	// Nil: defaultSidecarEventSettings.
	eventSettings *sidecarEventSettings
}

// One network's sidecar. Safe for concurrent use.
type Sidecar struct {
	ctx            context.Context
	cancel         context.CancelFunc
	settings       *SidecarSettings
	presenceSigner SidecarPresenceSigner
	events         *sidecarEventDispatch
	// closed once Close's teardown has finished
	closeDone chan struct{}

	stateLock sync.Mutex
	local     *sidecar.Sidecar
	// the local backend's instance, closed once the core's run loop has ended
	instance *sidecar.SidecarInstance
	remote   sidecarRemote
	// ends the core's event callback
	removeEventCallback func()
	closed              bool
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
	settings, err := checkedSidecarSettings(settings, options.backend == nil)
	if err != nil {
		return nil, err
	}
	components := settings.Components.getAll()

	coreSettings := sidecar.DefaultSidecarSettings()
	coreSettings.Instance = settings.Instance
	if 0 < settings.MemoryBudgetByteCount {
		coreSettings.MemoryBudgetByteCount = settings.MemoryBudgetByteCount
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
	local, err := sidecar.NewSidecar(cancelCtx, &sdkSidecarHost{store: instance.Store(networkId), keyStore: keyStoreAdapter}, nil, coreSettings)
	if err != nil {
		instance.Close()
		cancel()
		return nil, err
	}

	eventSettings := options.eventSettings
	if eventSettings == nil {
		eventSettings = defaultSidecarEventSettings()
	}
	self := &Sidecar{
		ctx:            cancelCtx,
		cancel:         cancel,
		settings:       settings,
		presenceSigner: presenceSigner,
		events:         newSidecarEventDispatch(cancelCtx, eventSettings),
		closeDone:      make(chan struct{}),
		local:          local,
		instance:       instance,
	}
	self.removeEventCallback = local.AddEventCallback(self.events.offer)
	for _, name := range components {
		factory, _ := sidecarComponentFactoryOf(name)
		component, err := traceWithReturnError(func() (sidecar.SidecarComponent, error) {
			return factory(local, settings)
		})
		if err == nil && component == nil {
			err = errors.New("its factory built nothing")
		}
		if err == nil {
			if _, addErr := local.AddComponent(component); addErr != nil {
				component.Close()
				err = addErr
			}
		}
		if err != nil {
			self.Close()
			return nil, fmt.Errorf("component %s: %w", name, err)
		}
	}
	// the key store's wrap of U, as ur daemon opens it at start. A store that is locked, failed or lost creates
	// nothing and leaves the core Locked or Unenrolled(keystore_lost); the app calls Unlock("") once it can answer
	local.Unlock(nil)
	return self, nil
}

// A copy of settings that NewSidecarLocal can run: the default instance for an empty one, an instance name the core
// takes, an absolute StateDir where one is needed, no negative budget, and only components this sdk binds, each
// once.
func checkedSidecarSettings(settings *SidecarSettings, needsStateDir bool) (*SidecarSettings, error) {
	if settings == nil {
		return nil, errors.New("a local sidecar needs settings")
	}
	checked := &SidecarSettings{
		Instance:              settings.Instance,
		StateDir:              settings.StateDir,
		Components:            NewStringList(),
		MemoryBudgetByteCount: settings.MemoryBudgetByteCount,
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
			if _, ok := sidecarComponentFactoryOf(name); !ok {
				return nil, fmt.Errorf("the sidecar component %q is not bound in this sdk yet", name)
			}
			checked.Components.Add(name)
		}
	}
	return checked, nil
}

// The backends, under the lock.
func (self *Sidecar) backends() (*sidecar.Sidecar, sidecarRemote) {
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
		var remote sidecarRemote
		var removeEventCallback func()
		alreadyClosed := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return true
			}
			self.closed = true
			local, instance, remote, removeEventCallback = self.local, self.instance, self.remote, self.removeEventCallback
			return false
		}()
		if alreadyClosed {
			return
		}
		if removeEventCallback != nil {
			removeEventCallback()
		}
		self.events.close()
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
