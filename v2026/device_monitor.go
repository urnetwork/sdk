// Security-policy monitoring is an explicit diagnostic facility. Normal app
// devices do not start it: polling a DeviceRemote performs blocking RPC,
// snapshots the policy maps, and writes logs even while the app is backgrounded.
package sdk

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const securityPolicyMonitorInterval = 30 * time.Second

// securityPolicyMonitorDevice is the narrow internal surface the diagnostic
// monitor needs, which also keeps its opt-in behavior directly testable.
type securityPolicyMonitorDevice interface {
	logger() connect.Logger
	egressSecurityPolicy() securityPolicy
	ingressSecurityPolicy() securityPolicy
}

// securityPolicyMonitor periodically snapshots security decisions for an
// explicitly verbose device. Its output is bounded by result count rather than
// destination cardinality.
type securityPolicyMonitor struct {
	ctx    context.Context
	cancel context.CancelFunc
	device securityPolicyMonitorDevice
}

// newSecurityPolicyMonitor starts diagnostics only when enabled. In particular,
// DeviceRemote must not poll its local service merely because the app object
// exists: foreground/background ownership belongs to app view controllers.
func newSecurityPolicyMonitor(
	ctx context.Context,
	device securityPolicyMonitorDevice,
	enabled bool,
) *securityPolicyMonitor {
	if !enabled {
		return nil
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	securityPolicyMonitor := &securityPolicyMonitor{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	go connect.HandleError(securityPolicyMonitor.run, cancel)
	return securityPolicyMonitor
}

// run snapshots and reports on a fixed diagnostic cadence until canceled.
func (self *securityPolicyMonitor) run() {
	defer self.cancel()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(securityPolicyMonitorInterval):
		}

		printSecurityPolicyStats(
			self.device.logger(),
			"ingress",
			self.device.ingressSecurityPolicy().Stats(false),
		)
		printSecurityPolicyStats(
			self.device.logger(),
			"egress",
			self.device.egressSecurityPolicy().Stats(false),
		)
	}
}

// printSecurityPolicyStats emits one line per result instead of one line per
// historical port. Exact destinations remain available from the stats API.
func printSecurityPolicyStats(log connect.Logger, prefix string, stats connect.SecurityPolicyStats) {
	log.Infof("%s security policy stats:", prefix)
	results := slices.Collect(maps.Keys(stats))
	slices.Sort(results)
	for _, result := range results {
		destinationCounts := stats[result]
		var totalCount uint64
		for _, count := range destinationCounts {
			totalCount += count
		}
		log.Infof(
			"%s[%s] = %d across %d destinations",
			prefix,
			result.String(),
			totalCount,
			len(destinationCounts),
		)
	}
}
