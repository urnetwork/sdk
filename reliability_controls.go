package sdk

import (
	"time"

	"github.com/urnetwork/connect"
)

// gomobile does not bind time.Duration, so durations cross the boundary as
// milliseconds
func millis(milliseconds int64) time.Duration {
	return time.Duration(milliseconds) * time.Millisecond
}

// multiClient returns the remote multi client when one is connected. Every
// control here is a no-op while disconnected, since there are no exits to act
// on -- the menu reads that back as an empty exit list rather than an error.
func (self *DeviceLocal) multiClient() (*connect.RemoteUserNatMultiClient, bool) {
	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()
	multi, ok := remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
	return multi, ok
}

// Developer-menu controls for the multi-exit reliability behavior.
//
// Six reliability fixes ship default-on, each addressing a different way a
// flow can freeze when its exit misbehaves. Which one matters for a given
// user is not something the code can decide -- it needs measuring against a
// live connection. These bind the connect-side primitives so the app can turn
// each fix off and back on while a freeze is happening, watch the exits, and
// reproduce the failures deliberately instead of waiting for them.
//
// Everything here is gomobile-safe: no maps, no slices across the boundary,
// lists via the exportedList wrapper, durations as milliseconds since gomobile
// does not bind time.Duration.

// ReliabilitySettings mirrors connect.ReliabilitySettings with gomobile-safe
// field types.
type ReliabilitySettings struct {
	// UdpTeardownSignal sends an icmp unreachable when a udp flow's exit is
	// removed, so dns and quic learn the path is gone instead of stalling
	UdpTeardownSignal bool
	// TcpCollapseMaxHoldMillis bounds how long a stalled exit may keep
	// swallowing a sender's retransmits. 0 disables the bound
	TcpCollapseMaxHoldMillis int64
	// SendStallTimeoutMillis is how long an exit may hold unacknowledged sends
	// before it is treated as failed and removed. 0 waits for the 30s ack
	// timeout instead, which is what freezes a flow until the app retries
	SendStallTimeoutMillis int64
	// ClusterAffinityFallback groups a site's ips when no hostname is known
	ClusterAffinityFallback bool
	// ServerNameAffinityBridge converges late-named flows onto the exit an
	// earlier flow to the same destination already uses
	ServerNameAffinityBridge bool
	// SequenceIdleTimeoutMillis is the idle bound for non-tcp flows
	SequenceIdleTimeoutMillis int64
	// TcpSequenceIdleTimeoutMillis is the idle bound for tcp flows. 0 falls
	// back to SequenceIdleTimeoutMillis
	TcpSequenceIdleTimeoutMillis int64
}

func reliabilitySettingsFromConnect(reliabilitySettings *connect.ReliabilitySettings) *ReliabilitySettings {
	if reliabilitySettings == nil {
		return &ReliabilitySettings{}
	}
	return &ReliabilitySettings{
		UdpTeardownSignal:            reliabilitySettings.UdpTeardownSignal,
		TcpCollapseMaxHoldMillis:     reliabilitySettings.TcpCollapseMaxHold.Milliseconds(),
		SendStallTimeoutMillis:       reliabilitySettings.SendStallTimeout.Milliseconds(),
		ClusterAffinityFallback:      reliabilitySettings.ClusterAffinityFallback,
		ServerNameAffinityBridge:     reliabilitySettings.ServerNameAffinityBridge,
		SequenceIdleTimeoutMillis:    reliabilitySettings.SequenceIdleTimeout.Milliseconds(),
		TcpSequenceIdleTimeoutMillis: reliabilitySettings.TcpSequenceIdleTimeout.Milliseconds(),
	}
}

func (self *ReliabilitySettings) toConnect() *connect.ReliabilitySettings {
	return &connect.ReliabilitySettings{
		UdpTeardownSignal:        self.UdpTeardownSignal,
		TcpCollapseMaxHold:       millis(self.TcpCollapseMaxHoldMillis),
		SendStallTimeout:         millis(self.SendStallTimeoutMillis),
		ClusterAffinityFallback:  self.ClusterAffinityFallback,
		ServerNameAffinityBridge: self.ServerNameAffinityBridge,
		SequenceIdleTimeout:      millis(self.SequenceIdleTimeoutMillis),
		TcpSequenceIdleTimeout:   millis(self.TcpSequenceIdleTimeoutMillis),
	}
}

// Exit is one provider channel, as shown in the developer menu.
type Exit struct {
	ClientId *Id
	// WindowType is "quality", "speed", or "" for auto
	WindowType string
	// Warning marks an exit new flows already avoid -- unhealthy, or past its
	// lifetime and draining
	Warning bool
	Done    bool
	P2pOnly bool
	// FlowCount is how many live flows are pinned to this exit. A site split
	// across exits shows up as flows spread over several entries
	FlowCount int32
}

type ExitList struct {
	exportedList[*Exit]
}

func NewExitList() *ExitList {
	return &ExitList{
		exportedList: *newExportedList[*Exit](),
	}
}

// GetReliabilitySettings reports the reliability behavior currently in effect,
// which is the runtime override when one is set and the shipped defaults
// otherwise.
func (self *DeviceLocal) GetReliabilitySettings() *ReliabilitySettings {
	if multi, ok := self.multiClient(); ok {
		return reliabilitySettingsFromConnect(multi.ReliabilitySettings())
	}
	return &ReliabilitySettings{}
}

// SetReliabilitySettings overrides the reliability behavior at runtime. Takes
// effect on the next packet -- no reconnect -- so a fix can be switched off
// and back on while a freeze is happening.
func (self *DeviceLocal) SetReliabilitySettings(reliabilitySettings *ReliabilitySettings) {
	if multi, ok := self.multiClient(); ok {
		multi.SetReliabilitySettings(reliabilitySettings.toConnect())
	}
}

// ResetReliabilitySettings clears any override, restoring the shipped
// behavior. This is the "put it back" the menu needs so an experiment can
// always be undone.
func (self *DeviceLocal) ResetReliabilitySettings() {
	if multi, ok := self.multiClient(); ok {
		multi.SetReliabilitySettings(nil)
	}
}

// GetExits lists the current provider channels with the flow count pinned to
// each.
func (self *DeviceLocal) GetExits() *ExitList {
	exits := NewExitList()
	if multi, ok := self.multiClient(); ok {
		for _, exit := range multi.Exits() {
			exits.Add(&Exit{
				ClientId:   newId(exit.ClientId),
				WindowType: exit.WindowType.RankMode(),
				Warning:    exit.Warning,
				Done:       exit.Done,
				P2pOnly:    exit.P2pOnly,
				FlowCount:  int32(exit.FlowCount),
			})
		}
	}
	return exits
}

// DropExit kills a single exit, as if that provider had died, leaving the
// others working. This is the failure the teardown fixes address -- unlike
// Shuffle, which replaces every exit at once and looks nothing like a real
// outage. Returns false if the exit is no longer in the window.
func (self *DeviceLocal) DropExit(clientId *Id) bool {
	if multi, ok := self.multiClient(); ok {
		return multi.DropExit(clientId.toConnectId())
	}
	return false
}

// StallExit makes an exit swallow packets without acknowledging them and
// without erroring, so it is neither healthy nor detectably dead. That is the
// state the tcp collapse bound exists for, and it is otherwise reachable only
// by waiting for a provider to misbehave at the right moment.
func (self *DeviceLocal) StallExit(clientId *Id, stalled bool) bool {
	if multi, ok := self.multiClient(); ok {
		return multi.StallExit(clientId.toConnectId(), stalled)
	}
	return false
}

// ShuffleExits replaces every exit at once. Useful for forcing a full
// re-selection; see DropExit for the single-exit case.
func (self *DeviceLocal) ShuffleExits() {
	if multi, ok := self.multiClient(); ok {
		multi.Shuffle()
	}
}
