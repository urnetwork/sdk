package sdk

// device_local_transport_delay.go -- the transport quality of a local device
// (transport_delay.go): the getter, the listener, the samples of each multi
// client generation, and the watch that turns the window into callbacks.
//
// applyDestination resets the window for each multi client it builds and
// subscribes to the multi client's delay stats, which the transports of every
// window client publish (DELAY.md §9, D9); the provider client is not
// subscribed (D3). A sample lands on the transport goroutine that published
// it, which must not block, so it takes only the window's own lock, never the
// device's, and wakes watchTransportDelay, which evaluates the window and
// calls the listeners. A device built with EnableTransportDelay off, which is
// every hosted device, subscribes to nothing, runs no watch and reads normal
// operation.

import (
	"time"

	"github.com/urnetwork/connect"
)

// GetTransportDelay is the transport quality of the device's window transports
// (Device.GetTransportDelay), as the watch last evaluated it. Never nil.
func (self *DeviceLocal) GetTransportDelay() *TransportDelay {
	return self.transportDelay.get()
}

// AddTransportDelayChangeListener subscribes to each change of either
// direction's quality level and of Fallback.
func (self *DeviceLocal) AddTransportDelayChangeListener(listener TransportDelayChangeListener) Sub {
	callbackId := self.transportDelayChangeListeners.Add(listener)
	return newSub(func() {
		self.transportDelayChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) transportDelayChanged(delay *TransportDelay) {
	for _, listener := range self.transportDelayChangeListeners.Get() {
		connect.HandleError(func() {
			listener.TransportDelayChanged(cloneTransportDelay(delay))
		})
	}
}

// Takes one delay sample of a window transport of the multi client generation
// connectionGeneration, on the transport goroutine that published it.
func (self *DeviceLocal) updateTransportDelayStats(
	connectionGeneration int64,
	multi *connect.RemoteUserNatMultiClient,
	stats *connect.TransportDelayStats,
) {
	if self.transportDelay.add(connectionGeneration, multi, stats, time.Now()) {
		self.notifyTransportDelay()
	}
}

// Wakes watchTransportDelay. A wake already pending covers this one.
func (self *DeviceLocal) notifyTransportDelay() {
	select {
	case self.transportDelayNotify <- struct{}{}:
	default:
	}
}

// watchTransportDelay evaluates the window on each new sample, on each reset,
// and once a second while any sample is live, so a direction returns to 0
// without new traffic (D4). It calls the listeners when either direction's
// level or Fallback changed since the last call (D7), outside every lock and
// never on a transport goroutine: the DeviceLocalRpc listener can wait on the
// reverse rpc's back pressure.
func (self *DeviceLocal) watchTransportDelay() {
	// the listeners start from normal operation, which is what the getter
	// reads before the first sample
	notified := noneTransportDelay()
	evaluateTimer := time.NewTimer(transportDelayEvaluateInterval)
	evaluateTimer.Stop()
	defer evaluateTimer.Stop()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.transportDelayNotify:
		case <-evaluateTimer.C:
		}
		delay, live := self.transportDelay.evaluate(time.Now())
		if live {
			evaluateTimer.Reset(transportDelayEvaluateInterval)
		} else {
			evaluateTimer.Stop()
		}
		if transportDelayLevelChanged(notified, delay) {
			notified = delay
			// contain a panic to the emit: a failed emit must never end the watch
			connect.HandleError(func() {
				self.transportDelayChanged(delay)
			})
		}
	}
}
