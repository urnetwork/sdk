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
	// AffinityStickyPastCap exempts a site's OWN growth from the flow cap: a
	// new flow whose site already lives on an exit stays there even past
	// MaxFlowsPerExit, while races and rebinds (placements that would put a
	// new site on the exit) stay capped. This is what keeps a busy site on
	// one egress ip -- video cdns bind signed media urls to the client ip,
	// and the cap veto was splitting exactly the sites that were busiest.
	// false restores the veto, the A/B comparison point.
	AffinityStickyPastCap bool
	// QuarantineGroupFollow lets a quarantined exit keep inheriting new
	// flows from sites already living on it, so a bench does not split the
	// site's egress ip. New sites, races, and rebinds still avoid the exit.
	// false restores the scatter, the A/B comparison point.
	QuarantineGroupFollow bool
	// GroupFollowWindowMillis is the follow's safety gate: a site follows its
	// benched exit only through the FIRST this-long of a quarantine episode,
	// when the verdict is least proven. A bench that sustains toward the
	// drain-to-conviction execution stops collecting flows first. 0 disables
	// the follow entirely.
	GroupFollowWindowMillis int64
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
	// BlackholeLoadCorroboration widens the corroboration with load: the
	// effective distinct-destination requirement for the soft no-receive
	// verdict is max(MinBlackholeDestinations, flowCount/this). The busier
	// the exit, the broader the silence must be before suspicion is
	// admissible -- at the default 8, a 24-flow exit needs 3 silent
	// destinations instead of 2. 0 disables the scaling, the flat A/B
	// comparison point.
	BlackholeLoadCorroboration int32
	// ProviderProbe enables client-side provider qualification: crafted tcp
	// and dns probes through each exit to real destinations, where an answer
	// PROVES the provider dials the internet and a non-answer proves nothing
	// at all -- probes qualify, they never convict. Off removes the whole
	// mechanism (no probes, no demerit, no admit preference), the A/B
	// comparison point.
	ProviderProbe bool
	// ProbeTimeoutMillis bounds one probe pass. 0 falls back to the built-in
	// 4s. It bounds how long positive evidence is waited for, never a timer
	// that produces a verdict.
	ProbeTimeoutMillis int64
	// ProbeSampleHostCount is how many health hosts one qualification pass
	// asks about. 0 (the default) means the ENTIRE embedded table every pass;
	// a positive value narrows the pass to a rotating block of that many
	// hosts. Width costs a few kilobytes per pass, never wall time -- every
	// probe of a pass is in flight together against one timeout.
	ProbeSampleHostCount int32
	// ProbeSilenceWarnStreak: after this many consecutive probe passes
	// answered with total silence (no answer, no dns resolution), the exit is
	// warned out of new-flow placement -- the compensation for provider
	// devices that leave the network mid-session. Placement only: removal
	// stays traffic-based, and any evidence of life clears the streak. 0 is
	// off.
	ProbeSilenceWarnStreak int32
	// EvaluationPoolMultiple makes window expansion request and ping-evaluate
	// this multiple of the candidates it needs, admit the needed count
	// preferring qualified providers, and politely cancel the flowless
	// surplus. Applies to the candidate-request count only -- window size
	// bounds are untouched. 1 restores exact-count evaluation, the A/B
	// comparison point; 2 is the mainnet-aggressive default.
	EvaluationPoolMultiple int32
	// FormationPollTimeoutMillis is how often a flow with no candidate exits at
	// all re-checks its forming window. While the window is empty there is
	// nothing to race, only the wait for the first client to land, and polling
	// that wait at the 2s send-retry pace left the first dns+syn of a fresh
	// connect sitting up to 2s after an exit was already usable. 0 falls back to
	// the send-retry pace, the pre-change behavior -- unlike the other duration
	// knobs here, 0 is not "off".
	FormationPollTimeoutMillis int64
	// BusyProbe interposes an active liveness probe on the send-stall bar:
	// instead of convicting a stalled exit immediately, one control ping is
	// fired through it with a snappy budget -- an ack acquits (the exit is
	// congested but alive, the stall clock is cleared), a timeout convicts with
	// the same "send stalled" reason. A congested-but-alive exit answers and
	// keeps its flows; a dead one is still removed. false convicts immediately,
	// the A/B comparison point.
	BusyProbe bool
	// BusyProbeBudgetMillis is how long a busy probe waits for its ack before
	// the exit is convicted. 0 derives max(1s, SendStallTimeout/2). Only has an
	// effect while BusyProbe is on.
	BusyProbeBudgetMillis int64
	// SchedulerPauseToleranceMillis is how much later than armed a timer may
	// fire before the gap is read as a host suspend (doze, freezer, thermal)
	// rather than a real stall: verdicts collected across the pause are held and
	// the receive clocks rebased, so a just-resumed phone does not convict every
	// exit at once. 0 disables the suspend detector, the A/B comparison point.
	SchedulerPauseToleranceMillis int64
	// SchedulerPauseRecoveryTimeoutMillis is how long after a detected suspend
	// the hold stays in effect, giving the transports time to re-register and
	// the first return packets to land before convictions resume. 0 falls back
	// to the built-in 5s (only meaningful while the detector is on).
	SchedulerPauseRecoveryTimeoutMillis int64
	// BlackholeConnectComparativeTimeoutMillis is the shorter bar the
	// no-receive-syn blackhole branch fires at while the rest of the pool is
	// demonstrably working -- two sibling exits receiving return traffic right
	// now removes the ambiguity that makes the full 30s bar patient, so an exit
	// that has established nothing is cut ~20s sooner. 0 disables the cut,
	// restoring the single full bar, the A/B comparison point.
	BlackholeConnectComparativeTimeoutMillis int64
	// HeartbeatIntervalMillis is how often the multi client logs one line
	// summarizing live state (exits, proven, quarantined, flows, held, rebinds,
	// probes). Mirrored so a field capture can silence the beat to keep an hour
	// of buffer, or speed it up to spot a transition, without a reconnect. 0
	// disables the heartbeat.
	HeartbeatIntervalMillis int64
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
		MaxFlowsPerExit:                   int32(reliabilitySettings.MaxFlowsPerExit),
		AffinityStickyPastCap:             reliabilitySettings.AffinityStickyPastCap,
		QuarantineGroupFollow:             reliabilitySettings.QuarantineGroupFollow,
		GroupFollowWindowMillis:           reliabilitySettings.GroupFollowWindow.Milliseconds(),
		UplinkStalenessGateMillis:         reliabilitySettings.UplinkStalenessGate.Milliseconds(),
		SoftVerdictDemote:             reliabilitySettings.SoftVerdictDemote,
		RemovalBudgetCount:            int32(reliabilitySettings.RemovalBudgetCount),
		RemovalBudgetWindowMillis:     reliabilitySettings.RemovalBudgetWindow.Milliseconds(),
		StandingReserve:               reliabilitySettings.StandingReserve,
		EffectiveTierSelection:        reliabilitySettings.EffectiveTierSelection,
		MinBlackholeDestinations:      int32(reliabilitySettings.MinBlackholeDestinations),
		BlackholeLoadCorroboration:    int32(reliabilitySettings.BlackholeLoadCorroboration),
		ProviderProbe:                 reliabilitySettings.ProviderProbe,
		ProbeTimeoutMillis:            reliabilitySettings.ProbeTimeout.Milliseconds(),
		ProbeSampleHostCount:          int32(reliabilitySettings.ProbeSampleHostCount),
		ProbeSilenceWarnStreak:        int32(reliabilitySettings.ProbeSilenceWarnStreak),
		EvaluationPoolMultiple:        int32(reliabilitySettings.EvaluationPoolMultiple),

		FormationPollTimeoutMillis:               reliabilitySettings.FormationPollTimeout.Milliseconds(),
		BusyProbe:                                reliabilitySettings.BusyProbe,
		BusyProbeBudgetMillis:                    reliabilitySettings.BusyProbeBudget.Milliseconds(),
		SchedulerPauseToleranceMillis:            reliabilitySettings.SchedulerPauseTolerance.Milliseconds(),
		SchedulerPauseRecoveryTimeoutMillis:      reliabilitySettings.SchedulerPauseRecoveryTimeout.Milliseconds(),
		BlackholeConnectComparativeTimeoutMillis: reliabilitySettings.BlackholeConnectComparativeTimeout.Milliseconds(),
		HeartbeatIntervalMillis:                  reliabilitySettings.HeartbeatInterval.Milliseconds(),
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
		MaxFlowsPerExit:             int(self.MaxFlowsPerExit),
		AffinityStickyPastCap:       self.AffinityStickyPastCap,
		QuarantineGroupFollow:       self.QuarantineGroupFollow,
		GroupFollowWindow:           millis(self.GroupFollowWindowMillis),
		UplinkStalenessGate:         millis(self.UplinkStalenessGateMillis),
		SoftVerdictDemote:        self.SoftVerdictDemote,
		RemovalBudgetCount:       int(self.RemovalBudgetCount),
		RemovalBudgetWindow:      millis(self.RemovalBudgetWindowMillis),
		StandingReserve:          self.StandingReserve,
		EffectiveTierSelection:   self.EffectiveTierSelection,
		MinBlackholeDestinations:   int(self.MinBlackholeDestinations),
		BlackholeLoadCorroboration: int(self.BlackholeLoadCorroboration),
		ProviderProbe:              self.ProviderProbe,
		ProbeTimeout:             millis(self.ProbeTimeoutMillis),
		ProbeSampleHostCount:     int(self.ProbeSampleHostCount),
		ProbeSilenceWarnStreak:   int(self.ProbeSilenceWarnStreak),
		EvaluationPoolMultiple:   int(self.EvaluationPoolMultiple),

		FormationPollTimeout:               millis(self.FormationPollTimeoutMillis),
		BusyProbe:                          self.BusyProbe,
		BusyProbeBudget:                    millis(self.BusyProbeBudgetMillis),
		SchedulerPauseTolerance:            millis(self.SchedulerPauseToleranceMillis),
		SchedulerPauseRecoveryTimeout:      millis(self.SchedulerPauseRecoveryTimeoutMillis),
		BlackholeConnectComparativeTimeout: millis(self.BlackholeConnectComparativeTimeoutMillis),
		HeartbeatInterval:                  millis(self.HeartbeatIntervalMillis),
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
	// Quarantined is the narrower state behind a warning: a blackhole verdict
	// matured against this exit and was demoted rather than executed because the
	// exit is carrying flows. Reported apart from Warning because "out of
	// selection" and "out of selection because a verdict was held" are different
	// facts to a reconstruction
	Quarantined bool
	// WarningCause names WHY the exit is warned: "draining" (past lifetime,
	// healthy, retiring), "starved" (its upstream failing dials),
	// "unhealthy" (a verdict demoted or deferred against it), or "" when
	// only quarantined or not warned at all.
	WarningCause string
	Done         bool
	P2pOnly      bool
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
	// quarantine, an unhealthy stats window, and an unproven qualification.
	// Greater than Tier means the exit is currently demoted; equal means
	// clean (or effective-tier selection is off)
	EffectiveTier int32
	// Proven reports a current qualification: a probe pass or live receive
	// traffic proved this provider dials real destinations within the last
	// ~30 minutes. False is "not yet proven", never "bad" -- the probe design
	// records no negative state to report
	Proven bool
	// ProbeAgeSeconds is how long ago the provider was last proven; -1 means
	// never. Can exceed the qualification window (then Proven is false)
	ProbeAgeSeconds int64
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
//
// nil while there is no multi client: nothing is in force, so there is no
// effective config to report. It deliberately does NOT report a zero struct,
// because this is the read half of a read-modify-WRITE: every caller reads
// the effective settings, changes one field, and writes the whole struct
// back (SetReliabilitySettings takes no partials). A zero struct handed to
// that loop is indistinguishable from "every fix off", and writing it back
// turns a single toggle into an all-off override -- silently, for the rest
// of the session, since the override outlives the disconnected moment that
// produced it. nil makes the disconnected case unusable by construction
// instead of quietly wrong.
//
// Callers treat nil as "the controls have nothing to act on": the android
// developer menu holds `ReliabilitySettings?` and gates its whole section on
// it, and the ios developer screen does the same over the rpc bridge.
func (self *DeviceLocal) GetReliabilitySettings() *ReliabilitySettings {
	if multi, ok := self.multiClient(); ok {
		return reliabilitySettingsFromConnect(multi.ReliabilitySettings())
	}
	return nil
}

// SetReliabilitySettings overrides the reliability behavior at runtime. Takes
// effect on the next packet -- no reconnect -- so a fix can be switched off
// and back on while a freeze is happening.
//
// The override is ALSO stored on the device and re-applied to every multi
// client it builds, because the multi client is rebuilt on every connect and
// the override otherwise lives only on the current one. Without that copy an
// override set while disconnected never takes effect at all, and one set
// while connected dies at the next reconnect -- silently, which is the whole
// failure class these controls exist to make observable. Safe while
// disconnected: it is stored now and applied when a window is built.
//
// nil is ignored; use ResetReliabilitySettings to clear the override.
func (self *DeviceLocal) SetReliabilitySettings(reliabilitySettings *ReliabilitySettings) {
	if reliabilitySettings == nil {
		return
	}
	// convert once, under no lock, and store an owned copy so a caller
	// mutating its struct afterwards cannot reach the device's state
	connectReliabilitySettings := reliabilitySettings.toConnect()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reliabilitySettings = connectReliabilitySettings
	}()
	if multi, ok := self.multiClient(); ok {
		multi.SetReliabilitySettings(connectReliabilitySettings)
	}
}

// ResetReliabilitySettings clears any override, restoring the shipped
// behavior. This is the "put it back" the menu needs so an experiment can
// always be undone. Clears the device-held copy too, so the override is not
// re-applied to the next multi client.
func (self *DeviceLocal) ResetReliabilitySettings() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reliabilitySettings = nil
	}()
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
			// -1 crosses the boundary as the "never proven" sentinel; any
			// non-negative age is truncated to whole seconds
			probeAgeSeconds := int64(-1)
			if 0 <= exit.ProbeAge {
				probeAgeSeconds = int64(exit.ProbeAge / time.Second)
			}
			exits.Add(&Exit{
				ClientId:         newId(exit.ClientId),
				WindowType:       exit.WindowType.RankMode(),
				Warning:          exit.Warning,
				Quarantined:      exit.Quarantined,
				WarningCause:     exit.WarningCause,
				Done:             exit.Done,
				P2pOnly:          exit.P2pOnly,
				FlowCount:        int32(exit.FlowCount),
				DialFailureCount: int32(exit.DialFailureCount),
				Tier:             int32(exit.Tier),
				EffectiveTier:    int32(exit.EffectiveTier),
				Proven:           exit.Proven,
				ProbeAgeSeconds:  probeAgeSeconds,
			})
		}
	}
	return exits
}

// DestinationExit is one (destination ip, exit) pairing in the live flow
// table: FlowCount flows to DestinationIp currently ride the exit ClientId.
// This is the join the Local statistics screen renders -- the block-action
// rows name the sites (hosts + cluster ips), and this says which egress each
// ip is riding right now.
type DestinationExit struct {
	// DestinationIp is the flow destination, as a string ip literal
	DestinationIp string
	ClientId      *Id
	FlowCount     int32
}

type DestinationExitList struct {
	exportedList[*DestinationExit]
}

func NewDestinationExitList() *DestinationExitList {
	return &DestinationExitList{
		exportedList: *newExportedList[*DestinationExit](),
	}
}

// GetDestinationExits reports which exit currently carries each destination
// ip, aggregated over live flows. Pull-model: the answer reflects the CURRENT
// exit after any re-race or rebind, which is what lets the statistics screen
// stay live. Empty while disconnected.
func (self *DeviceLocal) GetDestinationExits() *DestinationExitList {
	destinationExits := NewDestinationExitList()
	if multi, ok := self.multiClient(); ok {
		for _, destinationExit := range multi.DestinationExits() {
			destinationExits.Add(&DestinationExit{
				DestinationIp: destinationExit.DestinationIp.String(),
				ClientId:      newId(destinationExit.ClientId),
				FlowCount:     int32(destinationExit.FlowCount),
			})
		}
	}
	return destinationExits
}

// FlowOwnerLookup is the platform's resolver for "which PINNED app owns this
// flow" -- "" for none, which covers both "no app pin rules" and "the owner
// is not pinned". On android the implementation is one
// ConnectivityManager.getConnectionOwnerUid binder call checked against the
// pinned apps' uids (api 29+). Called once per NEW flow (the go side caches
// per flow key), never per packet. gomobile-safe: basic types only.
type FlowOwnerLookup interface {
	PinnedFlowAppId(version int32, protocol int32, sourceIp string, sourcePort int32, destinationIp string, destinationPort int32) string
}

// SetFlowOwnerLookup installs (or, with nil, removes) the platform flow-owner
// resolver that powers per-app pinning. Safe at runtime and safe while
// disconnected: the lookup is stored on the device and re-applied to every
// multi client the device builds, so pinning survives reconnects.
func (self *DeviceLocal) SetFlowOwnerLookup(lookup FlowOwnerLookup) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.flowOwnerLookup = lookup
	}()
	if multi, ok := self.multiClient(); ok {
		applyFlowOwnerLookup(multi, lookup)
	}
}

// applyFlowOwnerLookup adapts the gomobile interface to the connect func.
func applyFlowOwnerLookup(multi *connect.RemoteUserNatMultiClient, lookup FlowOwnerLookup) {
	if lookup == nil {
		multi.SetFlowOwnerLookup(nil)
		return
	}
	multi.SetFlowOwnerLookup(func(ipPath *connect.IpPath) string {
		return lookup.PinnedFlowAppId(
			int32(ipPath.Version),
			int32(ipPath.Protocol),
			ipPath.SourceIp.String(),
			int32(ipPath.SourcePort),
			ipPath.DestinationIp.String(),
			int32(ipPath.DestinationPort),
		)
	})
}

// MigrateExit hands one exit's movable (established quic) flows to live
// replacements NOW, while the exit is still alive -- the drain-time
// coordinated hand-off, run on demand. Nothing is killed: tcp and anything
// unplaceable keeps working where it is. Returns the number of flows moved,
// -1 when no such exit is in the window.
func (self *DeviceLocal) MigrateExit(clientId *Id) int32 {
	if multi, ok := self.multiClient(); ok {
		return int32(multi.MigrateExit(clientId.toConnectId()))
	}
	return -1
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

// ProbeAllExits fires a qualification probe pass at every exit in the windows
// right now instead of waiting for the background sweep's own schedule, and
// returns how many passes were scheduled. Non-blocking by contract: the passes
// run on their own goroutines behind the same bounded semaphore the prober loop
// uses, so this can be called from the ui thread. Returns 0 while disconnected
// or while provider probing is off (the connect side logs the refusal).
func (self *DeviceLocal) ProbeAllExits() int32 {
	if multi, ok := self.multiClient(); ok {
		return int32(multi.ProbeAllExits())
	}
	return 0
}

// SimulateNetworkChange fires the platform network-change path on demand -- the
// uplink staleness epoch reset and the process-wide transport kick a real
// wifi-to-cellular migration triggers -- so the storm drill the uplink gate
// exists for becomes one tap instead of physically moving between networks. It
// routes through the same production path as NotifyNetworkChange so the drill
// cannot drift from it. No-op while disconnected.
func (self *DeviceLocal) SimulateNetworkChange() {
	if multi, ok := self.multiClient(); ok {
		multi.SimulateNetworkChange()
	}
}

// NotifyNetworkChange is the production entry the android ConnectivityManager
// callback calls when the OS reports the network changed: it rebases the uplink
// staleness epoch and kicks every registered platform transport to drop its
// connection and re-dial immediately over the new path, instead of waiting out
// ping timeouts. No-op while disconnected.
func (self *DeviceLocal) NotifyNetworkChange() {
	if multi, ok := self.multiClient(); ok {
		multi.NotifyNetworkChanged()
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

	// ProbesSent and ProbesAnswered are the provider-qualification probes
	// asked and answered this session; ProvidersQualified counts providers
	// that crossed into the qualified state (transitions, not re-proofs).
	// There is deliberately no failure counter -- a probe failure is not an
	// event about the provider.
	ProbesSent         int64
	ProbesAnswered     int64
	ProvidersQualified int64

	// BusyProbesSent and BusyProbesAcquitted are the busy-flow liveness probes
	// fired at stalled exits and the ones answered inside the budget -- each
	// acquittal is a removal the probe prevented, an exit that was congested
	// rather than dead. SchedulerPausesDetected counts host suspends (doze,
	// freezer, thermal) the pause detector caught, each one a batch of verdicts
	// held rather than executed on a just-resumed phone.
	BusyProbesSent          int64
	BusyProbesAcquitted     int64
	SchedulerPausesDetected int64

	// GroupsFollowed counts new flows a quarantined exit kept under
	// group-follow (its site stayed on its egress ip through the bench);
	// GroupsScattered counts the ones quarantine still turned away -- each a
	// site whose egress ip split on suspicion. A rising scattered count with
	// follow enabled means the benched exits were receive-silent.
	GroupsFollowed  int64
	GroupsScattered int64
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
		ProbesSent:                int64(s.ProbesSent),
		ProbesAnswered:            int64(s.ProbesAnswered),
		ProvidersQualified:        int64(s.ProvidersQualified),
		BusyProbesSent:            int64(s.BusyProbesSent),
		BusyProbesAcquitted:       int64(s.BusyProbesAcquitted),
		SchedulerPausesDetected:   int64(s.SchedulerPausesDetected),
		GroupsFollowed:            int64(s.GroupsFollowed),
		GroupsScattered:           int64(s.GroupsScattered),
	}
}

// ResetReliabilityMetrics zeroes the counters. An A/B run is: reset, set the
// config, drive the same workload, read the metrics back.
func (self *DeviceLocal) ResetReliabilityMetrics() {
	if multi, ok := self.multiClient(); ok {
		multi.ResetReliabilityMetrics()
	}
}
