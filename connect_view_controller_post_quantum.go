//go:build !ios_extension

package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
)

// The connect screen's notice that the selected location has no
// post-quantum exits right now (owner, 2026-11-24: "Tell them, let them
// choose"). With post-quantum encryption on, discovery draws only providers
// that negotiate it, so a location whose providers none do never connects;
// instead of a connection that stays connecting, the apps say "No
// post-quantum exits in <location> right now" and offer two actions: pick
// another location, or turn post-quantum encryption off, saved as the
// option's switch saves it. Nothing changes without the user: the device
// keeps the selected location and the setting, and keeps trying. Immutable.
type PostQuantumUnavailable struct {
	// the selected location with no post-quantum exits; name it with its
	// Name, as the location list shows it
	Location *ConnectLocation `json:"location"`
}

// fires on every change of `ConnectViewController.GetPostQuantumUnavailable`.
// Signal only: re-read GetPostQuantumUnavailable, which is always the latest
// value.
type PostQuantumUnavailableListener interface {
	PostQuantumUnavailableChanged()
}

// The tunables of the post-quantum supply check behind the notice.
type postQuantumSupplySettings struct {
	// The longest one check's two find-providers2 requests may take together.
	// A check that does not answer leaves the notice as it was and is retried
	// after RecheckInterval. Non-positive bounds it by the controller's
	// lifetime only.
	CheckTimeout time.Duration
	// How long after a check that found no post-quantum exits, or that failed,
	// the location is checked again, so the notice clears when supply returns
	// without the user acting. Non-positive checks once.
	RecheckInterval time.Duration
	// The providers each request asks for. One answers the question; the
	// server reads its usual minimum block either way.
	ProviderCount int
}

func defaultPostQuantumSupplySettings() *postQuantumSupplySettings {
	return &postQuantumSupplySettings{
		CheckTimeout: 15 * time.Second,
		// about the time a window gives up on a location with no provider
		// (connect's window outcome watchdog, twice 45s)
		RecheckInterval: 90 * time.Second,
		ProviderCount:   1,
	}
}

// A device that answers its profile without a remote call
// (DeviceLocal, DeviceRemote).
type performanceProfileHolder interface {
	knownPerformanceProfile() (*PerformanceProfile, bool)
}

// Keeps the controller's notice with the device's post-quantum setting.
type connectViewControllerPerformanceProfileListener struct {
	vc *ConnectViewController
}

func (self *connectViewControllerPerformanceProfileListener) PerformanceProfileChanged(*PerformanceProfile) {
	self.vc.checkPostQuantumSupply()
}

// The notice, nil while the selected location has post-quantum exits, while
// post-quantum encryption is off, while the selection is not a location or a
// group (best available always has them; a device of the caller's own network
// is answered without the requirement), while disconnected or connected, and
// until a check has answered.
func (self *ConnectViewController) GetPostQuantumUnavailable() *PostQuantumUnavailable {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postQuantumUnavailable
}

func (self *ConnectViewController) AddPostQuantumUnavailableListener(listener PostQuantumUnavailableListener) Sub {
	callbackId := self.postQuantumUnavailableListeners.Add(listener)
	return newSub(func() {
		self.postQuantumUnavailableListeners.Remove(callbackId)
	})
}

func (self *ConnectViewController) postQuantumUnavailableChanged() {
	for _, listener := range self.postQuantumUnavailableListeners.Get() {
		connect.HandleError(func() {
			listener.PostQuantumUnavailableChanged()
		})
	}
}

// Whether the selection is a target whose supply a find-providers2 request
// can check: a location or a group.
func postQuantumSupplyCheckable(location *ConnectLocation) bool {
	if location == nil || location.ConnectLocationId == nil {
		return false
	}
	connectLocationId := location.ConnectLocationId
	if connectLocationId.BestAvailable || connectLocationId.ClientId != nil {
		return false
	}
	return connectLocationId.LocationId != nil || connectLocationId.LocationGroupId != nil
}

// Starts a check of the selected location's post-quantum supply, replacing a
// check in flight. Called on every change of the selection, the connection
// and the device's profile, often from a device listener: no device call is
// made here, the check reads the device on its own goroutine.
func (self *ConnectViewController) checkPostQuantumSupply() {
	var checkCtx context.Context
	var generation uint64
	var cancelPrevious context.CancelFunc
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.postQuantumSupplyGeneration += 1
		generation = self.postQuantumSupplyGeneration
		cancelPrevious = self.postQuantumSupplyCancel
		self.postQuantumSupplyCancel = nil
		if !self.closed {
			var cancel context.CancelFunc
			checkCtx, cancel = context.WithCancel(self.ctx)
			self.postQuantumSupplyCancel = cancel
		}
	}()
	if cancelPrevious != nil {
		cancelPrevious()
	}
	if checkCtx != nil {
		go connect.HandleError(func() {
			self.runPostQuantumSupplyCheck(checkCtx, generation)
		})
	}
}

// Ends the check and clears the notice: the connection has providers, so
// the location has exits for it.
func (self *ConnectViewController) endPostQuantumSupplyCheck() {
	var cancel context.CancelFunc
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.postQuantumSupplyGeneration += 1
		cancel = self.postQuantumSupplyCancel
		self.postQuantumSupplyCancel = nil
		if self.postQuantumUnavailable != nil {
			self.postQuantumUnavailable = nil
			changed = true
		}
	}()
	if cancel != nil {
		cancel()
	}
	if changed {
		self.postQuantumUnavailableChanged()
	}
}

// Decides whether a check applies, then checks until the location has
// post-quantum exits, the check is replaced, or the controller closes, once
// per RecheckInterval. A notice for the same location stands until the check
// answers; one that no longer applies (another location, post-quantum off,
// disconnected, connected) clears at once.
func (self *ConnectViewController) runPostQuantumSupplyCheck(ctx context.Context, generation uint64) {
	// a remote device answers from what it holds, with no call to the device:
	// its syncs and profile reports keep it current, and each report starts a
	// new check through the profile listener. Not known yet is no notice.
	var performanceProfile *PerformanceProfile
	known := true
	if holder, ok := self.device.(performanceProfileHolder); ok {
		performanceProfile, known = holder.knownPerformanceProfile()
	} else {
		performanceProfile = self.device.GetPerformanceProfile()
	}
	postQuantum := known && performanceProfile != nil && performanceProfile.PostQuantumEncryption

	var location *ConnectLocation
	current := false
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.postQuantumSupplyGeneration != generation {
			return
		}
		current = true
		if self.connected && self.connectionStatus != Connected &&
			postQuantum && postQuantumSupplyCheckable(self.selectedLocation) {
			location = self.selectedLocation
		}
		if self.postQuantumUnavailable != nil &&
			(location == nil || !connectLocationTransportEqual(self.postQuantumUnavailable.Location, location)) {
			self.postQuantumUnavailable = nil
			changed = true
		}
	}()
	if changed {
		self.postQuantumUnavailableChanged()
	}
	if !current || location == nil {
		return
	}

	settings := self.postQuantumSupplySettings
	for {
		unavailable, err := self.findPostQuantumSupply(ctx, location)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			var postQuantumUnavailable *PostQuantumUnavailable
			if unavailable {
				postQuantumUnavailable = &PostQuantumUnavailable{Location: location}
			}
			self.setPostQuantumUnavailable(postQuantumUnavailable, generation)
		} else {
			deviceLog(self.device).Infof("[cvc]post-quantum supply check failed: %v", err)
		}
		if checked := self.testingPostQuantumSupplyChecked; checked != nil {
			checked()
		}
		if err == nil && !unavailable || settings.RecheckInterval <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(settings.RecheckInterval):
		}
	}
}

// Whether the location has no post-quantum exits but has others, so turning
// post-quantum encryption off would connect: a find-providers2 request with
// the post-quantum requirement answers none, and one without it answers some.
// A location with no exits at all is not a post-quantum notice. Both requests
// ask as a window does (not strict), so a city a window answers from its
// region or country is answered the same way here.
func (self *ConnectViewController) findPostQuantumSupply(ctx context.Context, location *ConnectLocation) (bool, error) {
	settings := self.postQuantumSupplySettings
	if 0 < settings.CheckTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settings.CheckTimeout)
		defer cancel()
	}
	api := self.device.GetApi()
	find := func(requireModernProvider bool) (int, error) {
		result, err := api.FindProviders2SyncWithContext(ctx, &FindProviders2Args{
			Specs:                 locationProviderSpecs(location),
			Count:                 settings.ProviderCount,
			RequireModernProvider: requireModernProvider,
		})
		if err != nil {
			return 0, err
		}
		if result == nil || result.ProviderStats == nil {
			return 0, nil
		}
		return result.ProviderStats.Len(), nil
	}
	modernCount, err := find(true)
	if err != nil || 0 < modernCount {
		return false, err
	}
	count, err := find(false)
	if err != nil {
		return false, err
	}
	return 0 < count, nil
}

// Writes a check's answer while its check is still current.
func (self *ConnectViewController) setPostQuantumUnavailable(postQuantumUnavailable *PostQuantumUnavailable, generation uint64) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.postQuantumSupplyGeneration != generation || self.closed {
			return
		}
		if (self.postQuantumUnavailable == nil) != (postQuantumUnavailable == nil) {
			self.postQuantumUnavailable = postQuantumUnavailable
			changed = true
		}
	}()
	if changed {
		self.postQuantumUnavailableChanged()
	}
}
