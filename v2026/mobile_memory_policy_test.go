package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestMobileLowMemoryPolicyUsesTwentyFourMiBBoundary(t *testing.T) {
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
		{name: "one byte above", target: mobileSteadyMemoryTargetByteCount + 1, mobile: true, enabled: false},
		{name: "desktop", target: mobileSteadyMemoryTargetByteCount, mobile: false, enabled: false},
		{name: "disabled", target: 0, mobile: true, enabled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mobileLowMemoryPolicyEnabledForPlatform(testCase.target, testCase.mobile); got != testCase.enabled {
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
	if aboveTarget := mobilePackQueueBudgetForPlatform(
		mobileSteadyMemoryTargetByteCount+1,
		providerOffShare,
		true,
	); aboveTarget != nil {
		t.Fatal("non-low-memory mobile settings unexpectedly gained a pack queue budget")
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
	if got := settings.ReceiveBufferSettings.H1PackHandoffTimeout; got != 10*time.Millisecond {
		t.Fatalf("H1 receive handoff wait = %v, want 10ms", got)
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
	if got := settings.ReceiveBufferSettings.ReceiveQueueMinByteCount; got != mobileReceiveQueueMinByteCount {
		t.Fatalf("receive floor = %d, want %d", got, mobileReceiveQueueMinByteCount)
	}
	if got := settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount; got != mobileReceiveQueueMaxByteCount {
		t.Fatalf("receive max = %d, want %d", got, mobileReceiveQueueMaxByteCount)
	}
	if !settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting {
		t.Fatal("mobile receive queue did not enable retained-allocation accounting")
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
	if providerOff != mobileReceiveQueueBudgetMaxByteCount {
		t.Fatalf(
			"provider-off receive budget = %d, want bounded maximum %d",
			providerOff,
			mobileReceiveQueueBudgetMaxByteCount,
		)
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
	aboveTarget := mobileReceiveQueueBudgetForPlatform(target+1, clientShare, true)
	if aboveTarget != wantDesktop {
		t.Fatalf("above-target receive budget = %d, want unchanged %d", aboveTarget, wantDesktop)
	}
	if mobileReceiveQueueMinByteCount != 0 {
		t.Fatalf(
			"per-sequence receive floor = %d, want all reorder bytes charged",
			mobileReceiveQueueMinByteCount,
		)
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
		mobileSteadyMemoryTargetByteCount+1,
		true,
	)
	if got := aboveTargetSettings.H1AckPriorityBufferSize; got != 0 {
		t.Fatalf("above-target H1 ACK priority buffer = %d, want disabled", got)
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

func TestMobileLowMemoryPolicyLeavesServerAndLargerTargetsUnchanged(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		target ByteCount
		mobile bool
	}{
		{name: "server", target: mobileSteadyMemoryTargetByteCount, mobile: false},
		{name: "larger mobile target", target: 32 * 1024 * 1024, mobile: true},
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
