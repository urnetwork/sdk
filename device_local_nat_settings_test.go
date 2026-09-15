// Device nat profiles retain the legacy process-budget policy when their
// owner disables per-device sizing, including all protocol caps and udp reap.
package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Explicit state changes reproduce the device/server profile boundary without
// transport activity or scheduler timing. The original 24 MiB udp cap is 192.
func TestProviderLocalUserNatSettingsMemoryProfiles(t *testing.T) {
	previousBudget := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previousBudget) })

	type flowLimits struct {
		udpUserLimit    int
		udpGlobalLimit  int
		tcpUserLimit    int
		tcpGlobalLimit  int
		icmpUserLimit   int
		icmpGlobalLimit int
		udpIdleTimeout  time.Duration
	}
	cases := []struct {
		memoryTargetByteCount ByteCount
		memoryBudgetByteCount ByteCount
		limits                flowLimits
	}{
		{
			memoryBudgetByteCount: 24 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 192, udpGlobalLimit: 768,
				tcpUserLimit: 96, tcpGlobalLimit: 192,
				icmpUserLimit: 48, icmpGlobalLimit: 96,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryTargetByteCount: -1,
			memoryBudgetByteCount: 24 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 192, udpGlobalLimit: 768,
				tcpUserLimit: 96, tcpGlobalLimit: 192,
				icmpUserLimit: 48, icmpGlobalLimit: 96,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryBudgetByteCount: 1,
			limits: flowLimits{
				udpUserLimit: 64, udpGlobalLimit: 256,
				tcpUserLimit: 32, tcpGlobalLimit: 64,
				icmpUserLimit: 16, icmpGlobalLimit: 32,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryBudgetByteCount: 64 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 512, udpGlobalLimit: 2048,
				tcpUserLimit: 256, tcpGlobalLimit: 512,
				icmpUserLimit: 128, icmpGlobalLimit: 256,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryBudgetByteCount: 128 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 512, udpGlobalLimit: 2048,
				tcpUserLimit: 256, tcpGlobalLimit: 512,
				icmpUserLimit: 128, icmpGlobalLimit: 256,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			limits: flowLimits{udpIdleTimeout: 300 * time.Second},
		},
		{
			memoryBudgetByteCount: -1,
			limits:                flowLimits{udpIdleTimeout: 300 * time.Second},
		},
		{
			memoryTargetByteCount: 4 * 1024 * 1024,
			memoryBudgetByteCount: 24 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 256, udpGlobalLimit: 614,
				tcpUserLimit: 256, tcpGlobalLimit: 512,
				icmpUserLimit: 64, icmpGlobalLimit: 128,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryTargetByteCount: 4 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 256, udpGlobalLimit: 614,
				tcpUserLimit: 256, tcpGlobalLimit: 512,
				icmpUserLimit: 64, icmpGlobalLimit: 128,
				udpIdleTimeout: 60 * time.Second,
			},
		},
		{
			memoryTargetByteCount: 128 * 1024 * 1024,
			memoryBudgetByteCount: 24 * 1024 * 1024,
			limits: flowLimits{
				udpUserLimit: 4915, udpGlobalLimit: 19660,
				tcpUserLimit: 1638, tcpGlobalLimit: 3276,
				icmpUserLimit: 256, icmpGlobalLimit: 512,
				udpIdleTimeout: 60 * time.Second,
			},
		},
	}
	for _, c := range cases {
		connect.SetMemoryBudget(c.memoryBudgetByteCount)
		log := connect.NewNoopLogger()
		settings := providerLocalUserNatSettings(c.memoryTargetByteCount, log)
		got := flowLimits{
			udpUserLimit:    settings.UdpBufferSettings.UserLimit,
			udpGlobalLimit:  settings.UdpBufferSettings.GlobalLimit,
			tcpUserLimit:    settings.TcpBufferSettings.UserLimit,
			tcpGlobalLimit:  settings.TcpBufferSettings.GlobalLimit,
			icmpUserLimit:   settings.IcmpBufferSettings.UserLimit,
			icmpGlobalLimit: settings.IcmpBufferSettings.GlobalLimit,
			udpIdleTimeout:  settings.UdpBufferSettings.IdleTimeout,
		}
		if got != c.limits {
			t.Errorf("target=%d budget=%d: limits=%+v, want %+v",
				c.memoryTargetByteCount, c.memoryBudgetByteCount, got, c.limits)
		}
		if settings.Log != log {
			t.Errorf("target=%d budget=%d: device logger was not retained",
				c.memoryTargetByteCount, c.memoryBudgetByteCount)
		}
	}
}
