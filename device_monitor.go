// Security-policy monitoring is an explicit diagnostic facility. Normal app
// devices do not start it: polling a DeviceRemote performs blocking RPC,
// snapshots the policy maps, and writes logs even while the app is backgrounded.
package sdk

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
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
	ctx       context.Context
	cancel    context.CancelFunc
	device    securityPolicyMonitorDevice
	started   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
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
		ctx:     cancelCtx,
		cancel:  cancel,
		device:  device,
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(securityPolicyMonitor.done)
		connect.HandleError(securityPolicyMonitor.run, cancel)
	}()
	return securityPolicyMonitor
}

// run snapshots and reports on a fixed diagnostic cadence until canceled.
func (self *securityPolicyMonitor) run() {
	defer self.cancel()
	close(self.started)

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
		for _, policy := range []struct {
			prefix string
			policy securityPolicy
		}{
			{prefix: "ingress", policy: self.device.ingressSecurityPolicy()},
			{prefix: "egress", policy: self.device.egressSecurityPolicy()},
		} {
			if reasons, ok := policy.policy.(securityPolicyReasons); ok {
				printSecurityPolicyReasons(self.device.logger(), policy.prefix, reasons.Reasons(false))
			}
		}
	}
}

func (self *securityPolicyMonitor) Close() {
	self.closeOnce.Do(self.cancel)
}

func (self *securityPolicyMonitor) CloseAndWait(ctx context.Context) error {
	self.Close()
	select {
	case <-self.done:
		return nil
	case <-ctx.Done():
		select {
		case <-self.done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// securityPolicyReasonTopPorts is how many ports each reason line names.
const securityPolicyReasonTopPorts = 3

// printSecurityPolicyReasons emits one line per verdict reason with its total,
// its port count, and its busiest ports. Like the result lines, the output is
// bounded by the number of reasons, not by port cardinality.
func printSecurityPolicyReasons(log connect.Logger, prefix string, reasons connect.SecurityPolicyReasonStats) {
	log.Infof("%s security policy reasons:", prefix)
	reasonKeys := slices.Collect(maps.Keys(reasons))
	slices.Sort(reasonKeys)
	for _, reason := range reasonKeys {
		destinationCounts := reasons[reason]
		var totalCount uint64
		for _, count := range destinationCounts {
			totalCount += count
		}
		destinations := slices.Collect(maps.Keys(destinationCounts))
		slices.SortFunc(destinations, func(a connect.SecurityDestination, b connect.SecurityDestination) int {
			if destinationCounts[a] != destinationCounts[b] {
				if destinationCounts[b] < destinationCounts[a] {
					return -1
				}
				return 1
			}
			return a.Cmp(b)
		})
		topPorts := []string{}
		for _, destination := range destinations[:min(len(destinations), securityPolicyReasonTopPorts)] {
			topPorts = append(topPorts, fmt.Sprintf("%s/%d=%d", destination.Protocol.String(), destination.Port, destinationCounts[destination]))
		}
		log.Infof(
			"%s[%s] = %d across %d ports (top %s)",
			prefix,
			reason.String(),
			totalCount,
			len(destinationCounts),
			strings.Join(topPorts, " "),
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
