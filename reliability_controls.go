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
	// QuicRebindOnExitLoss re-pins an established quic (udp/443) flow to a
	// live replacement exit inside the removal of its dying exit, so the
	// app's next packet egresses through a warm exit and the server
	// path-validates the same quic connection id from a new address --
	// recovery in one packet interval instead of waiting out a re-race.
	// false restores teardown-on-removal for every flow, the A/B comparison
	// point
	QuicRebindOnExitLoss bool
	// DialFailureRerace moves a flow to another exit when a provider reports it
	// could not open the upstream connection, instead of letting the flow hang
	DialFailureRerace bool
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
	// BlackholeReceiveTimeoutMillis bounds the weaker blackhole signal: the
	// provider is acknowledging our sends, so it is alive, but nothing has
	// come back from the destination. That is ambiguous -- a flow waiting on a
	// slow origin looks the same -- and removing an exit kills every flow on
	// it, so it gets a longer bar than the unambiguous "acknowledges nothing"
	// case. At 5s this removed a provider roughly every 18s under real load.
	// 0 disables the check, which is the comparison point for measuring how
	// much churn it causes.
	BlackholeReceiveTimeoutMillis int64
	// MaxFlowsPerExit bounds how many live flows may be pinned to one exit.
	// Providers are split-tcp, so removing an exit destroys every flow on it.
	// Measured over 40 minutes of real use: 25 removals destroyed 821 flows,
	// but four of them accounted for 756 -- the worst single event was 484
	// connections at once, a visible stall. 0 is unbounded, the previous
	// behavior. The cost is that a site's flows can be split across exits, so
	// it sees more than one egress ip.
	MaxFlowsPerExit int32
	// UplinkStalenessGateMillis is how long the whole tunnel may go without a
	// single provider-originated ingress packet before the receive-branch
	// blackhole verdicts are held as inadmissible. Tunnel-wide silence
	// convicts the phone's own uplink, not the providers: one wifi network
	// migration executed 7 exits in 79s, every verdict no-receive-ack with
	// nothing received anywhere. 0 disables the gate, which is the comparison
	// point for measuring how many verdicts it holds.
	UplinkStalenessGateMillis int64
	// SoftVerdictDemote demotes the soft removal verdicts (no-receive-ack,
	// no-receive-syn, stats-unhealthy) against an exit carrying live flows:
	// the exit is warned out of selection and kept, its established flows
	// running, until it is flowless or the same evidence has held
	// continuously past the sustained bound. false restores the pre-change
	// execute-immediately behavior, which is the A/B comparison point.
	SoftVerdictDemote bool
	// RemovalBudgetCount and RemovalBudgetWindowMillis are the verdict-removal
	// storm breaker: at most this many verdict-driven removals per window per
	// budget window, the rest deferred (warned and kept) until budget ages
	// back in. A removal storm is more likely one local cause than that many
	// independent provider failures. User action, dead-transport cleanup,
	// lifetime drains, and capacity collapse are exempt. 0 count turns the
	// breaker off.
	RemovalBudgetCount        int32
	RemovalBudgetWindowMillis int64
	// StandingReserve sizes each window one spare exit beyond its computed
	// target (bounded by the window hard max), so a failed or draining exit's
	// replacement is already connected when it is needed -- measured without
	// it, failover backfill took ~45s because replacement only started after
	// a loss. false restores exact-target sizing, the A/B comparison point.
	StandingReserve bool
	// EffectiveTierSelection ranks exits for new flows by the platform tier
	// plus live demerits (dial starvation +2, active or recently survived
	// quarantine +2, unhealthy stats window +1), so a provider failing dials
	// falls in the ranking within about a second while promotion back is
	// slow and requires positive evidence. false selects on the static
	// platform tier alone, the A/B comparison point.
	EffectiveTierSelection bool
	// MinBlackholeDestinations is how many distinct send destinations the
	// stats window must contain before the no-receive-ack blackhole verdict
	// can fire, so one dead website's silence cannot convict an exit that is
	// demonstrably alive. 0 or 1 restores the single-destination behavior,
	// the A/B comparison point.
	MinBlackholeDestinations int32
}

func reliabilitySettingsFromConnect(reliabilitySettings *connect.ReliabilitySettings) *ReliabilitySettings {
	if reliabilitySettings == nil {
		return &ReliabilitySettings{}
	}
	return &ReliabilitySettings{
		UdpTeardownSignal:             reliabilitySettings.UdpTeardownSignal,
		QuicRebindOnExitLoss:          reliabilitySettings.QuicRebindOnExitLoss,
		DialFailureRerace:             reliabilitySettings.DialFailureRerace,
		TcpCollapseMaxHoldMillis:      reliabilitySettings.TcpCollapseMaxHold.Milliseconds(),
		SendStallTimeoutMillis:        reliabilitySettings.SendStallTimeout.Milliseconds(),
		ClusterAffinityFallback:       reliabilitySettings.ClusterAffinityFallback,
		ServerNameAffinityBridge:      reliabilitySettings.ServerNameAffinityBridge,
		SequenceIdleTimeoutMillis:     reliabilitySettings.SequenceIdleTimeout.Milliseconds(),
		TcpSequenceIdleTimeoutMillis:  reliabilitySettings.TcpSequenceIdleTimeout.Milliseconds(),
		BlackholeReceiveTimeoutMillis: reliabilitySettings.BlackholeReceiveTimeout.Milliseconds(),
		MaxFlowsPerExit:               int32(reliabilitySettings.MaxFlowsPerExit),
		UplinkStalenessGateMillis:     reliabilitySettings.UplinkStalenessGate.Milliseconds(),
		SoftVerdictDemote:             reliabilitySettings.SoftVerdictDemote,
		RemovalBudgetCount:            int32(reliabilitySettings.RemovalBudgetCount),
		RemovalBudgetWindowMillis:     reliabilitySettings.RemovalBudgetWindow.Milliseconds(),
		StandingReserve:               reliabilitySettings.StandingReserve,
		EffectiveTierSelection:        reliabilitySettings.EffectiveTierSelection,
		MinBlackholeDestinations:      int32(reliabilitySettings.MinBlackholeDestinations),
	}
}

func (self *ReliabilitySettings) toConnect() *connect.ReliabilitySettings {
	return &connect.ReliabilitySettings{
		UdpTeardownSignal:        self.UdpTeardownSignal,
		QuicRebindOnExitLoss:     self.QuicRebindOnExitLoss,
		DialFailureRerace:        self.DialFailureRerace,
		TcpCollapseMaxHold:       millis(self.TcpCollapseMaxHoldMillis),
		SendStallTimeout:         millis(self.SendStallTimeoutMillis),
		ClusterAffinityFallback:  self.ClusterAffinityFallback,
		ServerNameAffinityBridge: self.ServerNameAffinityBridge,
		SequenceIdleTimeout:      millis(self.SequenceIdleTimeoutMillis),
		TcpSequenceIdleTimeout:   millis(self.TcpSequenceIdleTimeoutMillis),
		BlackholeReceiveTimeout:  millis(self.BlackholeReceiveTimeoutMillis),
		MaxFlowsPerExit:          int(self.MaxFlowsPerExit),
		UplinkStalenessGate:      millis(self.UplinkStalenessGateMillis),
		SoftVerdictDemote:        self.SoftVerdictDemote,
		RemovalBudgetCount:       int(self.RemovalBudgetCount),
		RemovalBudgetWindow:      millis(self.RemovalBudgetWindowMillis),
		StandingReserve:          self.StandingReserve,
		EffectiveTierSelection:   self.EffectiveTierSelection,
		MinBlackholeDestinations: int(self.MinBlackholeDestinations),
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
	// DialFailureCount is how many upstream dials this exit has reported
	// failing in the recent window, the signal that it is out of capacity
	DialFailureCount int32
	// Tier is the platform's rank for this provider (0 is best). Only the best
	// rank present is raced until it fills, so an exit with 0 flows on a higher
	// tier is a spare, not a failure
	Tier int32
	// EffectiveTier is the rank selection actually uses: Tier plus live
	// demerits for dial starvation, an active or recently survived
	// quarantine, and an unhealthy stats window. Greater than Tier means the
	// exit is currently demoted; equal means clean (or effective-tier
	// selection is off)
	EffectiveTier int32
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
				ClientId:         newId(exit.ClientId),
				WindowType:       exit.WindowType.RankMode(),
				Warning:          exit.Warning,
				Done:             exit.Done,
				P2pOnly:          exit.P2pOnly,
				FlowCount:        int32(exit.FlowCount),
				DialFailureCount: int32(exit.DialFailureCount),
				Tier:             int32(exit.Tier),
				EffectiveTier:    int32(exit.EffectiveTier),
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

// ReliabilityMetrics is what the toggles above are judged against.
//
// The controls are only worth having if a change can be shown to help, and
// "the freeze felt shorter" is not a measurement. These counters give each
// candidate a number: how many connections a provider failure destroys, and
// how long the sites behind them stay dark.
//
// Counts are int64 rather than uint64 because gomobile does not bind unsigned
// types, and durations are milliseconds for the same reason time.Duration is
// not bound.
type ReliabilityMetrics struct {
	// FlowsOpened is how many flows have been started since the last reset.
	// It is the denominator that makes the loss counts comparable between two
	// runs of different lengths.
	FlowsOpened int64

	// ExitLossEvents is how many exits have died; FlowsLostToExit is how many
	// connections died with them.
	ExitLossEvents  int64
	FlowsLostToExit int64
	// MaxFlowsLostInOneEvent is the worst single failure observed, which is
	// what the user actually experiences -- an average of 4 is no comfort if
	// one event took out 55.
	MaxFlowsLostInOneEvent int64
	// MeanFlowsLostPerExitLoss is the blast radius: connections lost per
	// provider failure. Lower is the goal, and it is the headline number for
	// comparing a candidate against the shipped behavior.
	MeanFlowsLostPerExitLoss float64

	// RecoveryCount is how many destinations came back after losing their
	// exit; RecoveryMissed is how many never did inside the tracking window.
	// A fix that abandons flows rather than recovering them shows up as a
	// rising RecoveryMissed, so the two have to be read together.
	RecoveryCount  int64
	RecoveryMissed int64
	// RecoveryMeanMillis and RecoveryMaxMillis span from an exit dying to the
	// first packet back from that destination over a replacement exit. This
	// is the interval the user sits through.
	RecoveryMeanMillis int64
	RecoveryMaxMillis  int64
	// RecoveryPending is how many destinations are still dark right now.
	RecoveryPending int32

	// DialFailuresIntercepted counts provider could-not-connect signals seen;
	// FlowsReraced counts how many of those flows were quietly moved to another
	// exit instead of being left to hang. The gap between them is the failures
	// that were already established or otherwise not eligible to move.
	DialFailuresIntercepted int64
	FlowsReraced            int64

	// FlowsRebound counts established quic flows proactively re-pinned to a
	// replacement exit inside a removal instead of torn down (the proactive
	// rebind). RebindsAccepted are rebinds whose destination answered on the
	// same local source port -- the server accepted the quic path migration;
	// RebindsRedialed answered on a new port -- the app re-dialed around the
	// moved connection. Their sum can lag FlowsRebound when a destination
	// never answers inside the tracking window. The accepted/redialed split
	// is the field answer to how well servers actually accept path changes.
	FlowsRebound    int64
	RebindsAccepted int64
	RebindsRedialed int64

	// VerdictsHeldUplinkStale and VerdictsHeldTransportDown count blackhole
	// verdicts suppressed because the evidence was inadmissible -- the local
	// uplink was stale (tunnel-wide silence convicts the phone, not the
	// provider), or the channel's own transport was known down.
	// RemovalsDeferred counts provider removals postponed while such a hold
	// was in effect. Against the 7-exits-in-79s network-migration incident,
	// these are the executions that did not happen.
	VerdictsHeldUplinkStale   int64
	VerdictsHeldTransportDown int64
	RemovalsDeferred          int64
}

// GetReliabilityMetrics reports what provider failures have cost since the
// last reset. Safe to call while disconnected, which reads back as zeros.
func (self *DeviceLocal) GetReliabilityMetrics() *ReliabilityMetrics {
	multi, ok := self.multiClient()
	if !ok {
		return &ReliabilityMetrics{}
	}

	s := multi.ReliabilityMetrics()
	return &ReliabilityMetrics{
		FlowsOpened:               int64(s.FlowsOpened),
		ExitLossEvents:            int64(s.ExitLossEvents),
		FlowsLostToExit:           int64(s.FlowsLostToExit),
		MaxFlowsLostInOneEvent:    int64(s.MaxFlowsLostInOneEvent),
		MeanFlowsLostPerExitLoss:  s.MeanFlowsLostPerExitLoss,
		RecoveryCount:             int64(s.RecoveryCount),
		RecoveryMissed:            int64(s.RecoveryMissed),
		RecoveryMeanMillis:        s.RecoveryMeanNanos / int64(time.Millisecond),
		RecoveryMaxMillis:         s.RecoveryMaxNanos / int64(time.Millisecond),
		RecoveryPending:           int32(s.RecoveryPending),
		DialFailuresIntercepted:   int64(s.DialFailuresIntercepted),
		FlowsReraced:              int64(s.FlowsReraced),
		FlowsRebound:              int64(s.FlowsRebound),
		RebindsAccepted:           int64(s.RebindsAccepted),
		RebindsRedialed:           int64(s.RebindsRedialed),
		VerdictsHeldUplinkStale:   int64(s.VerdictsHeldUplinkStale),
		VerdictsHeldTransportDown: int64(s.VerdictsHeldTransportDown),
		RemovalsDeferred:          int64(s.RemovalsDeferred),
	}
}

// ResetReliabilityMetrics zeroes the counters. An A/B run is: reset, set the
// config, drive the same workload, read the metrics back.
func (self *DeviceLocal) ResetReliabilityMetrics() {
	if multi, ok := self.multiClient(); ok {
		multi.ResetReliabilityMetrics()
	}
}
