//go:build !ios_extension

package sdk

// The connect screen's notice that the selected location has no post-quantum
// exits right now (post_quantum_unavailable.go). The device decides it
// (device_local_post_quantum.go) and the controller mirrors the device's
// notice, so the connect screen and the surfaces that read the device show
// one notice from one check. On ios the device is the packet tunnel
// extension, which keeps deciding while the app is in the background.
//
// The controller shows the device's notice only while it still describes
// what the controller shows: post-quantum encryption is on, the notice is for
// the selected location, a location is connected (set), and the connection is
// not connected. The device clears its notice on each of those too; the
// controller hides it at once, without waiting for the device's next change
// to arrive, which on ios crosses the device rpc.

import (
	"github.com/urnetwork/connect/v2026"
)

// fires on every change of `ConnectViewController.GetPostQuantumUnavailable`.
// Signal only: re-read GetPostQuantumUnavailable, which is always the latest
// value.
type PostQuantumUnavailableListener interface {
	PostQuantumUnavailableChanged()
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

func (self *connectViewControllerPerformanceProfileListener) PerformanceProfileChanged(performanceProfile *PerformanceProfile) {
	self.vc.setPostQuantumEncryptionOff(performanceProfile == nil || !performanceProfile.PostQuantumEncryption)
}

// Mirrors the device's notice into the controller.
type connectViewControllerPostQuantumUnavailableListener struct {
	vc *ConnectViewController
}

func (self *connectViewControllerPostQuantumUnavailableListener) PostQuantumUnavailableChanged(postQuantumUnavailable *PostQuantumUnavailable) {
	self.vc.setDevicePostQuantumUnavailable(postQuantumUnavailable, true)
}

// Subscribes to the device's notice and post-quantum setting, then reads
// both. A change that lands during the reads arrives through the listeners,
// and a listener's value wins over a read made before it.
func (self *ConnectViewController) listenToPostQuantumUnavailable() {
	self.performanceProfileChangedSub = self.device.AddPerformanceProfileChangeListener(
		&connectViewControllerPerformanceProfileListener{vc: self},
	)
	self.postQuantumUnavailableChangedSub = self.device.AddPostQuantumUnavailableChangeListener(
		&connectViewControllerPostQuantumUnavailableListener{vc: self},
	)
	// a remote device answers its profile from what it holds, with no call to
	// the device process; not known yet hides nothing, the device decides
	if holder, ok := self.device.(performanceProfileHolder); ok {
		if performanceProfile, known := holder.knownPerformanceProfile(); known {
			self.setPostQuantumEncryptionOff(performanceProfile == nil || !performanceProfile.PostQuantumEncryption)
		}
	}
	self.setDevicePostQuantumUnavailable(self.device.GetPostQuantumUnavailable(), false)
}

// The notice, nil while the device has none (see
// Device.GetPostQuantumUnavailable), and while the controller shows
// something it does not describe: post-quantum encryption off, another
// selection, disconnected or connected. Takes only the controller's own lock,
// never a device call, so an app reads it on its main thread.
func (self *ConnectViewController) GetPostQuantumUnavailable() *PostQuantumUnavailable {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postQuantumUnavailableWithLock()
}

func (self *ConnectViewController) postQuantumUnavailableWithLock() *PostQuantumUnavailable {
	postQuantumUnavailable := self.devicePostQuantumUnavailable
	if postQuantumUnavailable == nil || self.postQuantumEncryptionOff ||
		!self.connected || self.connectionStatus == Connected ||
		!connectLocationTransportEqual(postQuantumUnavailable.Location, self.selectedLocation) {
		return nil
	}
	return postQuantumUnavailable
}

func (self *ConnectViewController) AddPostQuantumUnavailableListener(listener PostQuantumUnavailableListener) Sub {
	callbackId := self.postQuantumUnavailableListeners.Add(listener)
	return newSub(func() {
		self.postQuantumUnavailableListeners.Remove(callbackId)
	})
}

// Notifies the listeners when GetPostQuantumUnavailable answers otherwise than
// at the last notification. Called after every change it reads.
func (self *ConnectViewController) postQuantumUnavailableUpdated() {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		postQuantumUnavailable := self.postQuantumUnavailableWithLock()
		if postQuantumUnavailableEqual(self.notifiedPostQuantumUnavailable, postQuantumUnavailable) {
			return
		}
		self.notifiedPostQuantumUnavailable = postQuantumUnavailable
		changed = true
	}()
	if changed {
		for _, listener := range self.postQuantumUnavailableListeners.Get() {
			connect.HandleError(func() {
				listener.PostQuantumUnavailableChanged()
			})
		}
	}
}

// Takes the device's notice. A read the controller made before a listener
// call landed never replaces the listener's newer value.
func (self *ConnectViewController) setDevicePostQuantumUnavailable(postQuantumUnavailable *PostQuantumUnavailable, fromListener bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !fromListener && self.devicePostQuantumUnavailableHeard {
			return
		}
		if fromListener {
			self.devicePostQuantumUnavailableHeard = true
		}
		self.devicePostQuantumUnavailable = postQuantumUnavailable
	}()
	self.postQuantumUnavailableUpdated()
}

func (self *ConnectViewController) setPostQuantumEncryptionOff(postQuantumEncryptionOff bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.postQuantumEncryptionOff = postQuantumEncryptionOff
	}()
	self.postQuantumUnavailableUpdated()
}
