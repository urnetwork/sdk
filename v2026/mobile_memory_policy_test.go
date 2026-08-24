package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestMobileLowMemoryClientSettingsBoundOwnership(t *testing.T) {
	if mobileClientSequenceBufferMaxCount != 16 {
		t.Fatalf(
			"mobile sequence count = %d, want measured 16-message ceiling",
			mobileClientSequenceBufferMaxCount,
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
	if got := settings.SendBufferSettings.AckBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("ack buffer = %d, want %d", got, mobileClientSequenceBufferMaxCount)
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
	if got := settings.ReceiveBufferSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("receive sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.ReceiveBufferSettings.ReceiveQueueMinByteCount; got != mobileReceiveQueueMinByteCount {
		t.Fatalf("receive floor = %d, want %d", got, mobileReceiveQueueMinByteCount)
	}
	if got := settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount; got != mobileReceiveQueueMaxByteCount {
		t.Fatalf("receive max = %d, want %d", got, mobileReceiveQueueMaxByteCount)
	}
	if got := settings.ForwardBufferSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("forward sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
	}
	if got := settings.ContractManagerSettings.SequenceBufferSize; got != mobileClientSequenceBufferMaxCount {
		t.Fatalf("contract sequence = %d, want %d", got, mobileClientSequenceBufferMaxCount)
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
	if quality.WindowSizeMin != 3 || quality.WindowSizeMax != 3 || quality.WindowSizeHardMax != 3 {
		t.Fatalf("quality window = %+v, want fixed 3", quality)
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
				t.Fatal("standing reserve changed outside 20-MiB mobile policy")
			}
			if multi.StrictWindowSizeHardMax {
				t.Fatal("strict hard max changed outside 20-MiB mobile policy")
			}
			if multi.TcpSequenceIdleTimeout != tcpIdleBefore {
				t.Fatalf("tcp idle timeout changed from %v to %v", tcpIdleBefore, multi.TcpSequenceIdleTimeout)
			}
		})
	}
}
