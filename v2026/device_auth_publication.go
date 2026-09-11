package sdk

import "sync"

// deviceAuthPublicationGate prevents an API callback captured before device
// teardown from publishing auth after that teardown. Close is callback-safe:
// it only rejects new admissions. Lifecycle join waits for Done after the
// callback returns, so callbacks never join themselves.
type deviceAuthPublicationGate struct {
	stateLock     sync.Mutex
	active        bool
	inFlightCount int
	done          chan struct{}
	// Tests install this before invoking auth callbacks to hold an admitted
	// lease outside the gate lock. Production leaves it nil.
	testingAfterAdmission func()
	// Holds the actual callback after its API check but before persistence.
	// Installed before the callback starts; invoked without any state lock.
	testingBeforePersistence func()
}

// A new device begins with auth publication enabled.
func newDeviceAuthPublicationGate() *deviceAuthPublicationGate {
	return &deviceAuthPublicationGate{
		active: true,
		done:   make(chan struct{}),
	}
}

// Admission is an explicit lease. The returned release must be called once;
// nil means teardown already retired this publisher.
func (self *deviceAuthPublicationGate) Begin() func() {
	self.stateLock.Lock()
	if !self.active {
		self.stateLock.Unlock()
		return nil
	}
	self.inFlightCount += 1
	self.stateLock.Unlock()
	if self.testingAfterAdmission != nil {
		self.testingAfterAdmission()
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.inFlightCount -= 1
			self.finishWithLock()
		})
	}
}

// New callbacks are rejected immediately. Existing callback owners finish on
// their own goroutine and release the join boundary.
func (self *deviceAuthPublicationGate) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active = false
	self.finishWithLock()
}

// Closed after retirement and the last admitted callback both complete.
func (self *deviceAuthPublicationGate) Done() <-chan struct{} {
	return self.done
}

// Caller holds stateLock.
func (self *deviceAuthPublicationGate) finishWithLock() {
	if !self.active && self.inFlightCount == 0 {
		select {
		case <-self.done:
		default:
			close(self.done)
		}
	}
}
