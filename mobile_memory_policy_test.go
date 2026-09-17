package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestMobileMemoryPolicyAppliesToEveryMobileTarget(t *testing.T) {
	if got := defaultDeviceLocalMemoryTargetByteCountForPlatform(true); got != mobileSteadyMemoryTargetByteCount {
		t.Fatalf("mobile default memory target = %d, want 24 MiB", got)
	}
	if got := defaultDeviceLocalMemoryTargetByteCountForPlatform(false); got != defaultDeviceLocalMemoryTargetByteCount {
		t.Fatalf("server default memory target = %d, want unchanged 20 MiB", got)
	}
	for _, testCase := range []struct {
		name    string
		target  ByteCount
		mobile  bool
		enabled bool
	}{
		{name: "legacy tighter target", target: 20 * 1024 * 1024, mobile: true, enabled: true},
		{name: "24 MiB target", target: mobileSteadyMemoryTargetByteCount, mobile: true, enabled: true},
		// the decoupling: a larger budget scales the caps instead of
		// disabling the policy and restoring desktop sizing
		{name: "one byte above", target: mobileSteadyMemoryTargetByteCount + 1, mobile: true, enabled: true},
		{name: "raised target", target: 32 * 1024 * 1024, mobile: true, enabled: true},
		{name: "desktop", target: mobileSteadyMemoryTargetByteCount, mobile: false, enabled: false},
		{name: "disabled", target: 0, mobile: true, enabled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mobileMemoryPolicyEnabledForPlatform(testCase.target, testCase.mobile); got != testCase.enabled {
				t.Fatalf("policy enabled = %t, want %t", got, testCase.enabled)
			}
		})
	}
}

func TestMobilePackQueueBudgetUsesProviderOffHeadroomWithinHardBounds(t *testing.T) {
	clientShare := ByteCount(168 * 1024 * 1024 / 10)
	providerOffShare := ByteCount(216 * 1024 * 1024 / 10)
	if got := mobilePackQueueBudgetByteCount(clientShare); got != clientShare/10 {
		t.Fatalf("provider-on pack budget = %d, want %d", got, clientShare/10)
	}
	if got := mobilePackQueueBudgetByteCount(providerOffShare); got != mobilePackQueueBudgetMaxByteCount {
		t.Fatalf("provider-off pack budget = %d, want capped %d", got, mobilePackQueueBudgetMaxByteCount)
	}
	if got := mobilePackQueueBudgetByteCount(1); got != mobilePackQueueBudgetMinByteCount {
		t.Fatalf("tiny-share pack budget = %d, want floor %d", got, mobilePackQueueBudgetMinByteCount)
	}

	budget := mobilePackQueueBudgetForPlatform(
		mobileSteadyMemoryTargetByteCount,
		providerOffShare,
		true,
	)
	if budget == nil || budget.TotalByteCount() != mobilePackQueueBudgetMaxByteCount {
		t.Fatalf("mobile pack budget = %v, want %d bytes", budget, mobilePackQueueBudgetMaxByteCount)
	}
	if desktop := mobilePackQueueBudgetForPlatform(
		mobileSteadyMemoryTargetByteCount,
		providerOffShare,
		false,
	); desktop != nil {
		t.Fatal("desktop/server settings unexpectedly gained a pack queue budget")
	}
	// A raised target keeps the budget and scales its ceiling with the target.
	aboveTarget := mobilePackQueueBudgetForPlatform(
		2*mobileSteadyMemoryTargetByteCount,
		providerOffShare,
		true,
	)
	want := mobilePackQueueBudgetByteCountForTarget(
		providerOffShare,
		2*mobileSteadyMemoryTargetByteCount,
	)
	if aboveTarget == nil || aboveTarget.TotalByteCount() != want {
		t.Fatalf("doubled-target pack budget = %v, want %d bytes", aboveTarget, want)
	}
}

func TestMobileLowMemoryClientSettingsBoundOwnership(t *testing.T) {
	if mobilePacketPoolWarmByteCount != 256*1024 {
		t.Fatalf("mobile packet warm set = %d, want 256 KiB", mobilePacketPoolWarmByteCount)
	}
	if mobileClientSequenceBufferMaxCount != 16 {
		t.Fatalf(
			"mobile sequence count = %d, want H3-safe 16-message ceiling",
			mobileClientSequenceBufferMaxCount,
		)
	}
	if mobileH1ReceiveSequenceBufferMaxCount != 64 {
		t.Fatalf(
			"mobile H1 receive sequence count = %d, want 64-message burst ceiling",
			mobileH1ReceiveSequenceBufferMaxCount,
		)
	}
	settings := connect.DefaultClientSettingsWithBufferSize(256)
	settings.ReceiveBufferSettings = connect.DefaultReceiveBufferSettingsWithBufferSize(256)
	// no attached pool: the calibrated receive hold constant is the bound
	// (a pooled client is covered by TestMobileReceiveHoldIsTheAttachedPool)
	settings.ReceiveBufferSettings.ReceiveQueueBudget = nil
	applyMobileLowMemoryClientSettingsForPlatform(
		settings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)

	if got := settings.SendBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("send buffer = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.ForwardBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("forward buffer = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.SendBufferSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("send sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.SendBufferSettings.AckBufferSize; got != mobileClientAckBufferMaxCount {
		t.Fatalf("ack buffer = %d, want %d", got, mobileClientAckBufferMaxCount)
	}
	if got := settings.SendBufferSettings.ResendQueueMinByteCount; got != mobileResendQueueMinByteCount {
		t.Fatalf("resend floor = %d, want %d", got, mobileResendQueueMinByteCount)
	}
	if got := settings.SendBufferSettings.ResendQueueMaxByteCount; got != mobileResendQueueMaxByteCount {
		t.Fatalf("resend max = %d, want %d", got, mobileResendQueueMaxByteCount)
	}
	if got := settings.SendBufferSettings.UnreliableMaximumFlightMessageCount; got != mobileUnreliableFlightMaxMessageCount {
		t.Fatalf("unreliable message flight = %d, want %d", got, mobileUnreliableFlightMaxMessageCount)
	}
	if got := settings.ReceiveBufferSettings.SequenceBufferByteCount; got != mobileReceiveSequenceBufferMaxByteCount {
		t.Fatalf("receive sequence bytes = %d, want %d", got, mobileReceiveSequenceBufferMaxByteCount)
	}
	if got := settings.ReceiveBufferSettings.H1SequenceBufferByteCount; got != mobileH1ReceiveSequenceBufferMaxByteCount {
		t.Fatalf("H1 receive sequence bytes = %d, want %d", got, mobileH1ReceiveSequenceBufferMaxByteCount)
	}
	if got := settings.ReceiveBufferSettings.H1PackHandoffTimeout; got !=
		mobileH1ReceivePackHandoffWaitTimeout {
		t.Fatalf("H1 receive handoff wait = %v, want lossless backpressure", got)
	}
	if got := settings.ReceiveBufferSettings.ReliablePackHandoffTimeout; got !=
		mobileH1ReceivePackHandoffWaitTimeout {
		t.Fatalf("reliable receive handoff wait = %v, want lossless backpressure", got)
	}
	if got := settings.ReceiveBufferSettings.H1AckHandoffTimeout; got != time.Millisecond {
		t.Fatalf("H1 ACK handoff wait = %v, want 1ms", got)
	}
	if got := settings.ReceiveBufferSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("receive sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.ReceiveBufferSettings.H1SequenceBufferSize; got != mobileH1ReceiveSequenceBufferMaxCount {
		t.Fatalf("H1 receive sequence = %d, want %d", got, mobileH1ReceiveSequenceBufferMaxCount)
	}
	if receive := settings.ReceiveBufferSettings; receive.H1SequenceBufferAdaptiveMaxSize != 0 ||
		receive.H1SequenceBufferAdaptiveStepSize != 0 ||
		receive.H1SequenceBufferAdaptiveSaturationThreshold != 0 ||
		receive.H1SequenceBufferAdaptiveSaturationWindow != 0 ||
		receive.H1SequenceBufferAdaptiveMaxByteCount != 0 ||
		receive.H1SequenceBufferAdaptiveStepByteCount != 0 {
		t.Fatalf("rejected adaptive H1 policy remained enabled: %+v", receive)
	}
	if got := settings.ReceiveBufferSettings.ReceiveQueueMinByteCount; got != mobileReceiveQueueMinByteCount {
		t.Fatalf("receive floor = %d, want %d", got, mobileReceiveQueueMinByteCount)
	}
	if got := settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount; got != mobileReceiveQueueMaxByteCount {
		t.Fatalf("receive max = %d, want %d", got, mobileReceiveQueueMaxByteCount)
	}
	if !settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting {
		t.Fatal("mobile receive queue did not enable retained-allocation accounting")
	}
	if !settings.SendBufferSettings.ResendQueueRetainedByteAccounting {
		t.Fatal("mobile resend retained accounting is disabled")
	}
	if settings.ReceiveBufferSettings.PackQueueRetainedByteAccounting {
		t.Fatal("fixed mobile Pack queue unexpectedly enabled adaptive retained accounting")
	}
	if got := settings.ForwardBufferSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("forward sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.ContractManagerSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("contract sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
}

func TestMobileReceiveQueueBudgetIsAggregateAndProviderAware(t *testing.T) {
	target := mobileSteadyMemoryTargetByteCount
	clientShare := target * deviceMemoryRatioClient / deviceMemoryRatioParts
	providerShare := target * deviceMemoryRatioProvider / deviceMemoryRatioParts

	providerOn := mobileReceiveQueueBudgetForPlatform(target, clientShare, true)
	if want := mobileReceiveQueueBudgetByteCount(clientShare); providerOn != want {
		t.Fatalf("provider-on receive budget = %d, want %d", providerOn, want)
	}
	providerOff := mobileReceiveQueueBudgetForPlatform(
		target,
		clientShare+providerShare,
		true,
	)
	if want := mobileReceiveQueueBudgetByteCount(clientShare + providerShare); providerOff != want {
		t.Fatalf(
			"provider-off receive budget = %d, want target-derived %d",
			providerOff,
			want,
		)
	}
	if mobileReceiveQueueBudgetMaxByteCount < providerOff {
		t.Fatalf("provider-off receive budget = %d, exceeds maximum", providerOff)
	}
	if providerOff <= providerOn {
		t.Fatalf(
			"provider-off receive budget = %d, want more than provider-on %d",
			providerOff,
			providerOn,
		)
	}

	desktop := mobileReceiveQueueBudgetForPlatform(target, clientShare, false)
	wantDesktop := max(byteCountFraction(clientShare, 4, 7), ByteCount(1536*1024))
	if desktop != wantDesktop {
		t.Fatalf("desktop receive budget = %d, want unchanged %d", desktop, wantDesktop)
	}
	aboveTarget := mobileReceiveQueueBudgetForPlatform(2*target, clientShare, true)
	if want := mobileReceiveQueueBudgetByteCountForTarget(clientShare, 2*target); aboveTarget != want {
		t.Fatalf("doubled-target receive budget = %d, want target-scaled %d", aboveTarget, want)
	}
	if mobileReceiveQueueMinByteCount != 0 {
		t.Fatalf(
			"per-sequence receive floor = %d, want all reorder bytes charged",
			mobileReceiveQueueMinByteCount,
		)
	}
}

func TestMobileH1AdaptiveDepthIsDisabledAfterRejectedPhysicalArm(t *testing.T) {
	settings := connect.DefaultClientSettingsWithBufferSize(64)
	settings.ReceiveBufferSettings = connect.DefaultReceiveBufferSettingsWithBufferSize(64)
	receive := settings.ReceiveBufferSettings
	receive.H1SequenceBufferAdaptiveMaxSize = 128
	receive.H1SequenceBufferAdaptiveStepSize = 16
	receive.H1SequenceBufferAdaptiveSaturationThreshold = 2
	receive.H1SequenceBufferAdaptiveSaturationWindow = 100 * time.Millisecond
	receive.H1SequenceBufferAdaptiveMaxByteCount = 256 * 1024
	receive.H1SequenceBufferAdaptiveStepByteCount = 32 * 1024
	applyMobileLowMemoryClientSettingsForPlatform(
		settings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)
	if receive.H1SequenceBufferSize != 64 {
		t.Fatalf("explicit H1 depth changed to %d", receive.H1SequenceBufferSize)
	}
	if receive.H1SequenceBufferAdaptiveMaxSize != 0 ||
		receive.H1SequenceBufferAdaptiveStepSize != 0 ||
		receive.H1SequenceBufferAdaptiveSaturationThreshold != 0 ||
		receive.H1SequenceBufferAdaptiveSaturationWindow != 0 ||
		receive.H1SequenceBufferAdaptiveMaxByteCount != 0 ||
		receive.H1SequenceBufferAdaptiveStepByteCount != 0 {
		t.Fatalf("rejected H1 adaptive policy was not cleared: %+v", receive)
	}
}

func TestMobileH1LogicalLanesRequireExplicitLowMemoryH1(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		target     ByteCount
		mobile     bool
		explicitH1 bool
		want       int
	}{
		{
			name:       "explicit H1",
			target:     mobileSteadyMemoryTargetByteCount,
			mobile:     true,
			explicitH1: true,
			want:       mobileH1LogicalDataLaneCount,
		},
		{
			name:       "auto or H3",
			target:     mobileSteadyMemoryTargetByteCount,
			mobile:     true,
			explicitH1: false,
			want:       3,
		},
		{
			name:       "desktop",
			target:     mobileSteadyMemoryTargetByteCount,
			mobile:     false,
			explicitH1: true,
			want:       3,
		},
		{
			// the lane count is sizing: a larger target scales it rather
			// than reverting to the caller's desktop value
			name:       "larger target",
			target:     mobileSteadyMemoryTargetByteCount + 1,
			mobile:     true,
			explicitH1: true,
			want:       mobileH1LogicalDataLaneCount,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			settings := connect.DefaultClientSettings()
			settings.SendBufferSettings.LogicalDataLaneCount = 3
			applyMobileH1PerformanceClientSettingsForPlatform(
				settings,
				testCase.target,
				testCase.mobile,
				testCase.explicitH1,
			)
			if got := settings.SendBufferSettings.LogicalDataLaneCount; got != testCase.want {
				t.Fatalf("logical H1 lanes = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestMobileLowMemoryPlatformTransportAddsOnlyBoundedH1AckLane(t *testing.T) {
	mobileSettings := connect.DefaultPlatformTransportSettings()
	applyMobileLowMemoryPlatformTransportSettingsForPlatform(
		mobileSettings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)
	if got := mobileSettings.H1AckPriorityBufferSize; got != mobileH1AckPriorityBufferSize {
		t.Fatalf("mobile H1 ACK priority buffer = %d, want %d", got, mobileH1AckPriorityBufferSize)
	}

	serverSettings := connect.DefaultPlatformTransportSettings()
	applyMobileLowMemoryPlatformTransportSettingsForPlatform(
		serverSettings,
		mobileSteadyMemoryTargetByteCount,
		false,
	)
	if got := serverSettings.H1AckPriorityBufferSize; got != 0 {
		t.Fatalf("server H1 ACK priority buffer = %d, want disabled", got)
	}

	aboveTargetSettings := connect.DefaultPlatformTransportSettings()
	applyMobileLowMemoryPlatformTransportSettingsForPlatform(
		aboveTargetSettings,
		2*mobileSteadyMemoryTargetByteCount,
		true,
	)
	if got, want := aboveTargetSettings.H1AckPriorityBufferSize, 2*mobileH1AckPriorityBufferSize; got != want {
		t.Fatalf("doubled-target H1 ACK priority buffer = %d, want %d", got, want)
	}
}

func TestMessagePoolMemoryTargetsCapOnlyMobileReturnedBuffers(t *testing.T) {
	const limit = int64(32 * 1024 * 1024)
	packetBytes, largeBytes := messagePoolMemoryTargetsForPlatform(limit, true)
	if packetBytes != int64(mobilePacketPoolCapacityByteCount) {
		t.Fatalf("mobile packet pool capacity = %d, want %d", packetBytes, mobilePacketPoolCapacityByteCount)
	}
	if largeBytes != int64(mobileLargeObjectPoolCapacityByteCount) {
		t.Fatalf("mobile large-object pool capacity = %d, want %d", largeBytes, mobileLargeObjectPoolCapacityByteCount)
	}

	packetBytes, largeBytes = messagePoolMemoryTargetsForPlatform(limit, false)
	if want := limit * memoryTargetRatioPacketPool / memoryTargetRatioParts; packetBytes != want {
		t.Fatalf("server packet pool capacity = %d, want %d", packetBytes, want)
	}
	if want := limit * memoryTargetRatioLargeObjectPool / memoryTargetRatioParts; largeBytes != want {
		t.Fatalf("server large-object pool capacity = %d, want %d", largeBytes, want)
	}
}

func TestMobileLowMemoryMultiClientProfileBoundsLiveSet(t *testing.T) {
	settings := connect.DefaultMultiClientSettings()
	applyMobileLowMemoryMultiClientSettingsForPlatform(
		settings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)
	quality := settings.WindowSizes[connect.WindowTypeQuality]
	if quality.WindowSizeMin != 4 || quality.WindowSizeMax != 4 || quality.WindowSizeHardMax != 4 {
		t.Fatalf("quality window = %+v, want fixed 4", quality)
	}
	speed := settings.WindowSizes[connect.WindowTypeSpeed]
	if speed.WindowSizeMin != 1 || speed.WindowSizeMax != 1 ||
		speed.WindowSizeHardMax != 1 || speed.FixedWindowSize != 1 {
		t.Fatalf("speed window = %+v, want fixed 1", speed)
	}
	if settings.StandingReserve {
		t.Fatal("mobile low-memory profile retained a standing exit")
	}
	if !settings.StrictWindowSizeHardMax {
		t.Fatal("mobile low-memory profile did not enable the hard admission ceiling")
	}
	if got := settings.RemovalReceiveQueueSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("removal queue = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.PacketGroupMaxPacketCount; got != mobilePacketGroupMaxPacketCount {
		t.Fatalf("packet group count = %d, want %d", got, mobilePacketGroupMaxPacketCount)
	}
	if got := settings.PacketGroupMaxByteCount; got != mobilePacketGroupMaxByteCount {
		t.Fatalf("packet group bytes = %d, want %d", got, mobilePacketGroupMaxByteCount)
	}
	if got := settings.TcpSequenceIdleTimeout; got != mobileTcpSequenceIdleTimeout {
		t.Fatalf("tcp idle timeout = %v, want %v", got, mobileTcpSequenceIdleTimeout)
	}
}

func TestMobileLowMemoryMultiClientProfileBoundsPartialSettings(t *testing.T) {
	settings := &connect.MultiClientSettings{}
	applyMobileLowMemoryMultiClientSettingsForPlatform(
		settings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)

	if settings.WindowSizes == nil {
		t.Fatal("mobile policy left a nil window map")
	}
	if got := settings.WindowSizes[connect.WindowTypeQuality].WindowSizeHardMax; got != mobileQualityWindowSize {
		t.Fatalf("partial quality hard max = %d, want %d", got, mobileQualityWindowSize)
	}
	if got := settings.WindowSizes[connect.WindowTypeSpeed].WindowSizeHardMax; got != mobileSpeedWindowSize {
		t.Fatalf("partial speed hard max = %d, want %d", got, mobileSpeedWindowSize)
	}
	if got := settings.PacketGroupMaxPacketCount; got != mobilePacketGroupMaxPacketCount {
		t.Fatalf("unbounded packet group count = %d, want %d", got, mobilePacketGroupMaxPacketCount)
	}
	if got := settings.PacketGroupMaxByteCount; got != mobilePacketGroupMaxByteCount {
		t.Fatalf("unbounded packet group bytes = %d, want %d", got, mobilePacketGroupMaxByteCount)
	}
	if got := settings.TcpSequenceIdleTimeout; got != mobileTcpSequenceIdleTimeout {
		t.Fatalf("partial tcp idle timeout = %v, want %v", got, mobileTcpSequenceIdleTimeout)
	}
}

func TestMobileLowMemoryMultiClientProfilePreservesShorterTcpIdleTimeout(t *testing.T) {
	settings := connect.DefaultMultiClientSettings()
	settings.TcpSequenceIdleTimeout = time.Minute
	applyMobileLowMemoryMultiClientSettingsForPlatform(
		settings,
		mobileSteadyMemoryTargetByteCount,
		true,
	)
	if got := settings.TcpSequenceIdleTimeout; got != time.Minute {
		t.Fatalf("short tcp idle timeout changed to %v", got)
	}
}

func TestMobileMemoryPolicyLeavesServerAndDisabledTargetsUnchanged(t *testing.T) {
	// A larger mobile target is deliberately absent: it now scales the caps
	// instead of restoring the caller's desktop sizing, which is what
	// TestMobileMemoryPolicyCapsAreMonotoneInTheTarget pins.
	for _, testCase := range []struct {
		name   string
		target ByteCount
		mobile bool
	}{
		{name: "server", target: mobileSteadyMemoryTargetByteCount, mobile: false},
		{name: "disabled target", target: 0, mobile: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := connect.DefaultClientSettingsWithBufferSize(256)
			multi := connect.DefaultMultiClientSettings()
			qualityBefore := multi.WindowSizes[connect.WindowTypeQuality]
			tcpIdleBefore := multi.TcpSequenceIdleTimeout
			applyMobileLowMemoryClientSettingsForPlatform(client, testCase.target, testCase.mobile)
			applyMobileLowMemoryMultiClientSettingsForPlatform(multi, testCase.target, testCase.mobile)
			if client.SendBufferSize != 256 {
				t.Fatalf("send buffer changed to %d", client.SendBufferSize)
			}
			if got := multi.WindowSizes[connect.WindowTypeQuality]; got != qualityBefore {
				t.Fatalf("quality window changed from %+v to %+v", qualityBefore, got)
			}
			if !multi.StandingReserve {
				t.Fatal("standing reserve changed outside 24-MiB mobile policy")
			}
			if multi.StrictWindowSizeHardMax {
				t.Fatal("strict hard max changed outside 24-MiB mobile policy")
			}
			if multi.TcpSequenceIdleTimeout != tcpIdleBefore {
				t.Fatalf("tcp idle timeout changed from %v to %v", tcpIdleBefore, multi.TcpSequenceIdleTimeout)
			}
		})
	}
}

// FLIGHTGATEFIX §15 sizing invariants for the mobile ceilings. The 24 MiB
// figure is a crash boundary on iOS, not a target, and the numbers that
// decide throughput and footprint sit in this file while the code that
// spends them sits in connect. These tests pin the relationships so a
// change to either side cannot quietly trickle the tunnel or blow the
// envelope.

// mobileTunnelTypicalMessageByteCount is one Transfer message carrying one
// tunnel IP packet at the product's advertised 1,100-byte MTU; the device
// rig measured about 930 bytes across a download.
const mobileTunnelTypicalMessageByteCount = 930

// The two unreliable ceilings are not the same budget. The byte ceiling is
// the memory the flight may retain and it binds on its own, so the message
// ceiling decides only what share of that memory real messages may use. At
// sixteen messages the direct lane may use about a tenth of the bytes it
// has already been granted, which is why overflow reaches the relay on a
// link that could carry far more. That is a deliberate trade recorded in
// FLIGHTGATEFIX §15.3, which proposes raising the message ceiling because
// doing so retains no additional bytes; this test holds the trade where it
// is and fails if a change makes the lane trickle harder.
func TestMobileUnreliableFlightCeilingsKeepTheirStatedTrade(t *testing.T) {
	admitted := mobileUnreliableFlightMaxMessageCount * mobileTunnelTypicalMessageByteCount
	share := float64(admitted) / float64(mobileUnreliableFlightMaxByteCount)
	t.Logf(
		"mobileUnreliableFlightMaxMessageCount %d at %d bytes admits %d bytes, %.0f%% of the %d-byte mobileUnreliableFlightMaxByteCount",
		mobileUnreliableFlightMaxMessageCount, mobileTunnelTypicalMessageByteCount,
		admitted, 100*share, mobileUnreliableFlightMaxByteCount,
	)
	if share < 0.10 {
		t.Fatalf(
			"mobileUnreliableFlightMaxMessageCount %d admits %.1f%% of the %d bytes "+
				"mobileUnreliableFlightMaxByteCount already grants: at %d bytes a message that is "+
				"%d bytes per round trip, so the direct lane trickles and the relay carries the "+
				"bulk. Raising the message ceiling costs no retained bytes, the byte ceiling still "+
				"binds; see FLIGHTGATEFIX §15.3",
			mobileUnreliableFlightMaxMessageCount, 100*share,
			mobileUnreliableFlightMaxByteCount, mobileTunnelTypicalMessageByteCount, admitted,
		)
	}
	if mobileUnreliableFlightMaxByteCount < mobileTunnelTypicalMessageByteCount {
		t.Fatalf(
			"mobileUnreliableFlightMaxByteCount %d cannot hold one %d-byte message",
			mobileUnreliableFlightMaxByteCount, mobileTunnelTypicalMessageByteCount,
		)
	}
}

// Every mobile budget that retains bytes, summed, against the 24 MiB
// steady-state target. The remainder of the target is the Go runtime,
// goroutine stacks, the gVisor stack and live packet ownership, which this
// file does not size; the device block measured the whole figure at 23.7 to
// 24.4 MiB, so the configured share must stay well inside the envelope. A
// change that pushes these budgets past a third of the target fails here
// rather than on a phone.
func TestMobileRetainedByteBudgetsFitTheSteadyMemoryTarget(t *testing.T) {
	type budget struct {
		name  string
		bytes ByteCount
	}
	budgets := []budget{
		{"mobilePackQueueBudgetMaxByteCount", mobilePackQueueBudgetMaxByteCount},
		{"mobileReceiveQueueBudgetMaxByteCount", mobileReceiveQueueBudgetMaxByteCount},
		{"mobileResendQueueMaxByteCount", mobileResendQueueMaxByteCount},
		{"mobileReceiveQueueMaxByteCount", mobileReceiveQueueMaxByteCount},
		{"mobileUnreliableFlightMaxByteCount", mobileUnreliableFlightMaxByteCount},
		{"mobilePacketPoolCapacityByteCount", mobilePacketPoolCapacityByteCount},
		{"mobileLargeObjectPoolCapacityByteCount", mobileLargeObjectPoolCapacityByteCount},
		{"mobilePacketPoolWarmByteCount", mobilePacketPoolWarmByteCount},
		{
			"removal receive queue (mobileClientSequenceBufferMaxCount at one MTU)",
			ByteCount(mobileClientSequenceBufferMaxCount) * 1500,
		},
		{
			"per-sequence round-trip windows (relay and direct lane, FLIGHTGATEFIX §15.2)",
			mobileRttWindowRetainedByteCount(),
		},
	}
	total := ByteCount(0)
	for _, b := range budgets {
		total += b.bytes
		t.Logf("%-72s %8d bytes", b.name, b.bytes)
	}
	ceiling := mobileSteadyMemoryTargetByteCount / 3
	t.Logf("configured retaining budgets total %d bytes, %.1f%% of the %d-byte target",
		total, 100*float64(total)/float64(mobileSteadyMemoryTargetByteCount),
		mobileSteadyMemoryTargetByteCount)
	if ceiling < total {
		t.Fatalf(
			"the mobile budgets that retain bytes total %d, past %d, a third of the %d-byte "+
				"mobileSteadyMemoryTargetByteCount. The rest of that target is the Go runtime, "+
				"goroutine stacks, gVisor and live packet ownership, and the device block already "+
				"measures the whole figure at 23.7 to 24.4 MiB, so there is no headroom to spend. "+
				"Exceeding the target crashes iOS; lower one of the budgets above",
			total, ceiling, mobileSteadyMemoryTargetByteCount,
		)
	}
}

// mobileRttWindowRetainedByteCount is what the send sequences' round-trip
// windows retain: each sample slot is one ring entry and one minimum-deque
// entry. Every sequence holds one window over every acknowledgement,
// whichever lane carried it; FLIGHTGATEFIX §19 D3 deleted the direct
// lane's own second window, which returned sixteen slots per sequence.
func mobileRttWindowRetainedByteCount() ByteCount {
	const rttWindowSlotByteCount = 40
	send := connect.DefaultSendBufferSettings()
	return ByteCount(mobileClientSequenceBufferMaxCount) *
		ByteCount(send.RttWindowSize) * rttWindowSlotByteCount
}

// One applied snapshot of every cap the mobile memory policy installs, so the
// decoupling can be checked as a whole rather than field by field.
type mobileMemoryPolicySnapshot struct {
	SendBufferSize            int
	ForwardBufferSize         int
	SendSequenceBufferSize    int
	AckBufferSize             int
	ResendQueueMinByteCount   ByteCount
	ResendQueueMaxByteCount   ByteCount
	UnreliableFlightByteCount ByteCount
	UnreliableFlightCount     int
	ReceiveSequenceBufferSize int
	H1ReceiveSequenceSize     int
	SequenceBufferByteCount   ByteCount
	H1SequenceBufferByteCount ByteCount
	ReceiveQueueMinByteCount  ByteCount
	ReceiveQueueMaxByteCount  ByteCount
	// the receive hold cap of a client with an attached pool
	ReceiveQueueMaxByteCountPooled ByteCount
	ForwardSequenceBufferSize      int
	ContractSequenceSize           int
	MultiSequenceBufferSize        int
	RemovalReceiveQueueSize        int
	QualityWindowSize              int
	SpeedWindowSize                int
	H1AckPriorityBufferSize        int
	H1LogicalDataLaneCount         int
	ReceiveQueueBudget             ByteCount
	PackQueueBudget                ByteCount
	// invariants: never sized from the target
	PacketGroupMaxPacketCount int
	PacketGroupMaxByteCount   ByteCount
	RetainedByteAccounting    bool
	StandingReserve           bool
	StrictWindowSizeHardMax   bool
	TcpSequenceIdleTimeout    time.Duration
}

func mobileMemoryPolicySnapshotForTarget(target ByteCount) mobileMemoryPolicySnapshot {
	const hugeCount = 1 << 20
	const hugeByteCount = ByteCount(1) << 40

	settings := connect.DefaultClientSettingsWithBufferSize(256)
	settings.ReceiveBufferSettings = connect.DefaultReceiveBufferSettingsWithBufferSize(256)
	settings.SendBufferSize = hugeCount
	settings.ForwardBufferSize = hugeCount
	send := settings.SendBufferSettings
	send.SequenceBufferSize = hugeCount
	send.AckBufferSize = hugeCount
	send.ResendQueueMinByteCount = hugeByteCount
	send.ResendQueueMaxByteCount = hugeByteCount
	send.UnreliableMaximumFlightByteCount = hugeByteCount
	send.UnreliableMaximumFlightMessageCount = hugeCount
	send.LogicalDataLaneCount = hugeCount
	receive := settings.ReceiveBufferSettings
	receive.SequenceBufferSize = hugeCount
	receive.H1SequenceBufferSize = hugeCount
	receive.SequenceBufferByteCount = hugeByteCount
	receive.H1SequenceBufferByteCount = hugeByteCount
	receive.ReceiveQueueMinByteCount = hugeByteCount
	receive.ReceiveQueueMaxByteCount = hugeByteCount
	// the unpooled arm; the pooled cap is read from the policy's own function
	receive.ReceiveQueueBudget = nil
	settings.ForwardBufferSettings.SequenceBufferSize = hugeCount
	settings.ContractManagerSettings.SequenceBufferSize = hugeCount
	applyMobileLowMemoryClientSettingsForPlatform(settings, target, true)
	applyMobileH1PerformanceClientSettingsForPlatform(settings, target, true, true)

	multi := connect.DefaultMultiClientSettings()
	multi.SequenceBufferSize = hugeCount
	multi.RemovalReceiveQueueSize = hugeCount
	multi.PacketGroupMaxPacketCount = hugeCount
	multi.PacketGroupMaxByteCount = hugeByteCount
	multi.StandingReserve = true
	multi.StrictWindowSizeHardMax = false
	multi.TcpSequenceIdleTimeout = time.Hour
	applyMobileLowMemoryMultiClientSettingsForPlatform(multi, target, true)

	transport := connect.DefaultPlatformTransportSettings()
	applyMobileLowMemoryPlatformTransportSettingsForPlatform(transport, target, true)

	// a fixed share, large enough that the target-derived ceiling always
	// binds: the share must not vary with the target or the comparison
	// measures the share rather than the policy
	clientShareByteCount := ByteCount(1) << 30
	packBudget := ByteCount(0)
	if budget := mobilePackQueueBudgetForPlatform(target, clientShareByteCount, true); budget != nil {
		packBudget = budget.TotalByteCount()
	}

	return mobileMemoryPolicySnapshot{
		SendBufferSize:            settings.SendBufferSize,
		ForwardBufferSize:         settings.ForwardBufferSize,
		SendSequenceBufferSize:    send.SequenceBufferSize,
		AckBufferSize:             send.AckBufferSize,
		ResendQueueMinByteCount:   send.ResendQueueMinByteCount,
		ResendQueueMaxByteCount:   send.ResendQueueMaxByteCount,
		UnreliableFlightByteCount: send.UnreliableMaximumFlightByteCount,
		UnreliableFlightCount:     send.UnreliableMaximumFlightMessageCount,
		ReceiveSequenceBufferSize: receive.SequenceBufferSize,
		H1ReceiveSequenceSize:     receive.H1SequenceBufferSize,
		SequenceBufferByteCount:   receive.SequenceBufferByteCount,
		H1SequenceBufferByteCount: receive.H1SequenceBufferByteCount,
		ReceiveQueueMinByteCount:  receive.ReceiveQueueMinByteCount,
		ReceiveQueueMaxByteCount:  receive.ReceiveQueueMaxByteCount,
		ReceiveQueueMaxByteCountPooled: mobileReceiveQueueMaxByteCountForPool(
			true,
			target,
		),
		ForwardSequenceBufferSize: settings.ForwardBufferSettings.SequenceBufferSize,
		ContractSequenceSize:      settings.ContractManagerSettings.SequenceBufferSize,
		MultiSequenceBufferSize:   multi.SequenceBufferSize,
		RemovalReceiveQueueSize:   multi.RemovalReceiveQueueSize,
		QualityWindowSize:         multi.WindowSizes[connect.WindowTypeQuality].WindowSizeMax,
		SpeedWindowSize:           multi.WindowSizes[connect.WindowTypeSpeed].WindowSizeMax,
		H1AckPriorityBufferSize:   transport.H1AckPriorityBufferSize,
		H1LogicalDataLaneCount:    send.LogicalDataLaneCount,
		ReceiveQueueBudget: mobileReceiveQueueBudgetForPlatform(
			target,
			clientShareByteCount,
			true,
		),
		PackQueueBudget:           packBudget,
		PacketGroupMaxPacketCount: multi.PacketGroupMaxPacketCount,
		PacketGroupMaxByteCount:   multi.PacketGroupMaxByteCount,
		RetainedByteAccounting:    receive.ReceiveQueueRetainedByteAccounting,
		StandingReserve:           multi.StandingReserve,
		StrictWindowSizeHardMax:   multi.StrictWindowSizeHardMax,
		TcpSequenceIdleTimeout:    multi.TcpSequenceIdleTimeout,
	}
}

// Acceptance for decoupling the sizing from the policy gate: at the 24-MiB
// steady target, and at every smaller target the old gate admitted, every
// value is the calibrated constant the gate installed before. So this change
// alters nothing that ships until a device's target is deliberately raised.
func TestMobileMemoryPolicyIsByteIdenticalAtAndBelowTheSteadyTarget(t *testing.T) {
	steady := mobileMemoryPolicySnapshotForTarget(mobileSteadyMemoryTargetByteCount)
	want := mobileMemoryPolicySnapshot{
		SendBufferSize:            mobileClientSequenceBufferMaxCount,
		ForwardBufferSize:         mobileClientSequenceBufferMaxCount,
		SendSequenceBufferSize:    mobileClientSequenceBufferMaxCount,
		AckBufferSize:             mobileClientAckBufferMaxCount,
		ResendQueueMinByteCount:   mobileResendQueueMinByteCount,
		ResendQueueMaxByteCount:   mobileResendQueueMaxByteCount,
		UnreliableFlightByteCount: mobileUnreliableFlightMaxByteCount,
		UnreliableFlightCount:     mobileUnreliableFlightMaxMessageCount,
		ReceiveSequenceBufferSize: mobileClientSequenceBufferMaxCount,
		H1ReceiveSequenceSize:     mobileH1ReceiveSequenceBufferMaxCount,
		SequenceBufferByteCount:   mobileReceiveSequenceBufferMaxByteCount,
		H1SequenceBufferByteCount: mobileH1ReceiveSequenceBufferMaxByteCount,
		ReceiveQueueMinByteCount:  mobileReceiveQueueMinByteCount,
		ReceiveQueueMaxByteCount:  mobileReceiveQueueMaxByteCount,
		// a pooled client's hold cap is the pool ceiling, above the constant
		ReceiveQueueMaxByteCountPooled: mobileReceiveQueueBudgetMaxByteCount,
		ForwardSequenceBufferSize:      mobileClientSequenceBufferMaxCount,
		ContractSequenceSize:           mobileClientSequenceBufferMaxCount,
		MultiSequenceBufferSize:        mobileClientSequenceBufferMaxCount,
		RemovalReceiveQueueSize:        mobileClientSequenceBufferMaxCount,
		QualityWindowSize:              mobileQualityWindowSize,
		SpeedWindowSize:                mobileSpeedWindowSize,
		H1AckPriorityBufferSize:        mobileH1AckPriorityBufferSize,
		H1LogicalDataLaneCount:         mobileH1LogicalDataLaneCount,
		ReceiveQueueBudget:             mobileReceiveQueueBudgetMaxByteCount,
		PackQueueBudget:                mobilePackQueueBudgetMaxByteCount,
		PacketGroupMaxPacketCount:      mobilePacketGroupMaxPacketCount,
		PacketGroupMaxByteCount:        mobilePacketGroupMaxByteCount,
		RetainedByteAccounting:         true,
		StandingReserve:                false,
		StrictWindowSizeHardMax:        true,
		TcpSequenceIdleTimeout:         mobileTcpSequenceIdleTimeout,
	}
	if steady != want {
		t.Fatalf("policy at the steady target = %+v, want the calibrated %+v", steady, want)
	}
	for _, target := range []ByteCount{
		1,
		8 * 1024 * 1024,
		20 * 1024 * 1024,
		mobileSteadyMemoryTargetByteCount - 1,
	} {
		if got := mobileMemoryPolicySnapshotForTarget(target); got != steady {
			t.Fatalf("policy at target %d = %+v, want identical to the steady target", target, got)
		}
	}
}

func TestMobileMemoryPolicyCapsAreMonotoneInTheTarget(t *testing.T) {
	targets := []ByteCount{
		mobileSteadyMemoryTargetByteCount,
		mobileSteadyMemoryTargetByteCount + 1,
		28 * 1024 * 1024,
		32 * 1024 * 1024,
		48 * 1024 * 1024,
		64 * 1024 * 1024,
	}
	sized := func(snapshot mobileMemoryPolicySnapshot) []ByteCount {
		return []ByteCount{
			ByteCount(snapshot.SendBufferSize),
			ByteCount(snapshot.ForwardBufferSize),
			ByteCount(snapshot.SendSequenceBufferSize),
			ByteCount(snapshot.AckBufferSize),
			snapshot.ResendQueueMinByteCount,
			snapshot.ResendQueueMaxByteCount,
			snapshot.UnreliableFlightByteCount,
			ByteCount(snapshot.UnreliableFlightCount),
			ByteCount(snapshot.ReceiveSequenceBufferSize),
			ByteCount(snapshot.H1ReceiveSequenceSize),
			snapshot.SequenceBufferByteCount,
			snapshot.H1SequenceBufferByteCount,
			snapshot.ReceiveQueueMinByteCount,
			snapshot.ReceiveQueueMaxByteCount,
			snapshot.ReceiveQueueMaxByteCountPooled,
			ByteCount(snapshot.ForwardSequenceBufferSize),
			ByteCount(snapshot.ContractSequenceSize),
			ByteCount(snapshot.MultiSequenceBufferSize),
			ByteCount(snapshot.RemovalReceiveQueueSize),
			ByteCount(snapshot.QualityWindowSize),
			ByteCount(snapshot.SpeedWindowSize),
			ByteCount(snapshot.H1AckPriorityBufferSize),
			ByteCount(snapshot.H1LogicalDataLaneCount),
			snapshot.ReceiveQueueBudget,
			snapshot.PackQueueBudget,
		}
	}
	steady := mobileMemoryPolicySnapshotForTarget(mobileSteadyMemoryTargetByteCount)
	previous := sized(steady)
	for _, target := range targets[1:] {
		snapshot := mobileMemoryPolicySnapshotForTarget(target)
		current := sized(snapshot)
		for i := range current {
			if current[i] < previous[i] {
				t.Fatalf(
					"cap %d at target %d = %d, below %d at the smaller target",
					i,
					target,
					current[i],
					previous[i],
				)
			}
		}
		// the invariants never move with the target
		if snapshot.PacketGroupMaxPacketCount != steady.PacketGroupMaxPacketCount ||
			snapshot.PacketGroupMaxByteCount != steady.PacketGroupMaxByteCount ||
			!snapshot.RetainedByteAccounting || snapshot.StandingReserve ||
			!snapshot.StrictWindowSizeHardMax ||
			snapshot.TcpSequenceIdleTimeout != steady.TcpSequenceIdleTimeout {
			t.Fatalf("invariants moved at target %d: %+v", target, snapshot)
		}
	}

	// a doubled target doubles every cap calibrated at the steady target
	doubled := sized(mobileMemoryPolicySnapshotForTarget(2 * mobileSteadyMemoryTargetByteCount))
	for i, value := range sized(steady) {
		if doubled[i] != 2*value {
			t.Fatalf("cap %d at a doubled target = %d, want %d", i, doubled[i], 2*value)
		}
	}
}

// A phone downloading is bound by its own advertised receive hold, which
// connect sends as the smaller of ReceiveQueueMaxByteCount and the attached
// receive pool's live total (receiveWindowAdvertisement). The mobile policy
// used to cap the field at the 768 KiB constant, below the pool the same
// construction attaches, so the phone told the sender it could hold half of
// what it had reserved. After the fix the advertised hold is the pool and
// never more than it, on the iOS shape (20 MiB target inside a 32 MiB process
// budget) and the Android shape (28 inside 40), and the pool itself is
// unchanged, so this is a window raise and not a memory raise: admission
// already stops at the pool. A mobile client with no pool keeps the 768 KiB
// constant, its original protective role.
func TestMobileReceiveHoldIsTheAttachedPool(t *testing.T) {
	defer connect.SetMemoryBudget(0)

	// mirrors receiveWindowAdvertisement in connect/transfer.go
	advertised := func(receive *connect.ReceiveBufferSettings) ByteCount {
		share := receive.ReceiveQueueMaxByteCount
		if receive.ReceiveQueueBudget != nil {
			share = min(share, receive.ReceiveQueueBudget.TotalByteCount())
		}
		return max(0, share)
	}

	for _, shape := range []struct {
		name          string
		target        ByteCount
		processBudget ByteCount
		// the device pool with providing off (the phone default) and on, as
		// applyProvideMemorySharesWithLock sizes it; pinned so a pool change
		// cannot hide behind this test
		poolProviderOff ByteCount
		poolProviderOn  ByteCount
		// the calibrated constant at this target, the no-pool bound
		unpooledHold ByteCount
	}{
		{"iOS", 20 * 1024 * 1024, 32 * 1024 * 1024, 1536 * 1024, 1536 * 1024, 768 * 1024},
		{"Android", 28 * 1024 * 1024, 40 * 1024 * 1024, 1908408, 1792 * 1024, 896 * 1024},
	} {
		t.Run(shape.name, func(t *testing.T) {
			connect.SetMemoryBudget(shape.processBudget)
			defer connect.SetMemoryBudget(0)

			// the construction sequence of newDeviceLocalWithOverrides: the
			// window rule sizes the field from the process share, the device
			// attaches its pool from the client share, the mobile policy runs,
			// and the provide-state resize sets the pool's mobile total
			deviceSettings := &DeviceLocalSettings{
				MemoryTargetByteCount: shape.target,
				AllowProvider:         true,
			}
			_, clientShare, _, providerShare := deviceMemoryShares(deviceSettings)
			settings := connect.DefaultClientSettingsWithBufferSize(256)
			_, pool := deviceLocalTransferBudgets(clientShare)
			settings.ReceiveBufferSettings.ReceiveQueueBudget = pool
			applyMobileLowMemoryClientSettingsForPlatform(settings, shape.target, true)
			pool.SetTotalByteCount(
				mobileReceiveQueueBudgetForPlatform(shape.target, clientShare+providerShare, true),
			)
			receive := settings.ReceiveBufferSettings

			// the pool is unchanged by the hold change
			if got := pool.TotalByteCount(); got != shape.poolProviderOff {
				t.Fatalf("receive pool = %d, want the unchanged %d", got, shape.poolProviderOff)
			}
			// the advertised hold is the pool, and never above it
			if got := advertised(receive); got != shape.poolProviderOff {
				t.Fatalf(
					"advertised hold = %d, want the attached pool %d (field %d)",
					got,
					shape.poolProviderOff,
					receive.ReceiveQueueMaxByteCount,
				)
			}
			if receive.ReceiveQueueMaxByteCount < pool.TotalByteCount() {
				t.Fatalf(
					"receive hold cap %d undercuts the attached pool %d",
					receive.ReceiveQueueMaxByteCount,
					pool.TotalByteCount(),
				)
			}
			// the field is not the pool: the advertisement follows the pool's
			// live total through a provide-mode resize
			pool.SetTotalByteCount(
				mobileReceiveQueueBudgetForPlatform(shape.target, clientShare, true),
			)
			if got := pool.TotalByteCount(); got != shape.poolProviderOn {
				t.Fatalf("provider-on receive pool = %d, want the unchanged %d", got, shape.poolProviderOn)
			}
			if got := advertised(receive); got != shape.poolProviderOn {
				t.Fatalf("advertised hold after the provide-on resize = %d, want the pool %d", got, shape.poolProviderOn)
			}
			// never above the largest total a mobile pool can take
			if ceiling := mobileTargetScaledByteCount(
				mobileReceiveQueueBudgetMaxByteCount,
				shape.target,
			); receive.ReceiveQueueMaxByteCount != ceiling {
				t.Fatalf(
					"pooled receive hold cap = %d, want the pool ceiling %d",
					receive.ReceiveQueueMaxByteCount,
					ceiling,
				)
			}

			// a mobile client with no pool keeps the constant as its bound
			unpooled := connect.DefaultClientSettingsWithBufferSize(256)
			unpooled.ReceiveBufferSettings.ReceiveQueueBudget = nil
			applyMobileLowMemoryClientSettingsForPlatform(unpooled, shape.target, true)
			if got := unpooled.ReceiveBufferSettings.ReceiveQueueMaxByteCount; got != shape.unpooledHold {
				t.Fatalf("unpooled receive hold = %d, want the constant %d", got, shape.unpooledHold)
			}
			if got := advertised(unpooled.ReceiveBufferSettings); got != shape.unpooledHold {
				t.Fatalf("unpooled advertised hold = %d, want %d", got, shape.unpooledHold)
			}
		})
	}

	// the constant itself did not move
	if mobileReceiveQueueMaxByteCount != 768*1024 {
		t.Fatalf("mobileReceiveQueueMaxByteCount = %d, want 768 KiB", mobileReceiveQueueMaxByteCount)
	}
	// and the pooled cap is monotone in the target, like every other cap
	previous := ByteCount(0)
	for _, target := range []ByteCount{
		1,
		20 * 1024 * 1024,
		mobileSteadyMemoryTargetByteCount,
		28 * 1024 * 1024,
		64 * 1024 * 1024,
	} {
		pooled := mobileReceiveQueueMaxByteCountForPool(true, target)
		unpooled := mobileReceiveQueueMaxByteCountForPool(false, target)
		if pooled < previous {
			t.Fatalf("pooled hold cap %d at target %d below %d at a smaller target", pooled, target, previous)
		}
		if pooled < unpooled {
			t.Fatalf("pooled hold cap %d at target %d below the unpooled %d", pooled, target, unpooled)
		}
		previous = pooled
	}
}
