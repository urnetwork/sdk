package sdk

import (
	"context"
	"slices"
	"time"

	"maps"

	"github.com/urnetwork/connect/v2026"
)

type securityPolicyMonitor struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device
}

func newSecurityPolicyMonitor(ctx context.Context, device Device) *securityPolicyMonitor {
	cancelCtx, cancel := context.WithCancel(ctx)
	securityPolicyMonitor := &securityPolicyMonitor{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,
	}
	go connect.HandleError(securityPolicyMonitor.run, cancel)
	return securityPolicyMonitor
}

func (self *securityPolicyMonitor) run() {
	defer self.cancel()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}

		printSecurityPolicyStats(deviceLog(self.device), "ingress", self.device.(device).ingressSecurityPolicy().Stats(false))
		printSecurityPolicyStats(deviceLog(self.device), "egress", self.device.(device).egressSecurityPolicy().Stats(false))
	}
}

func printSecurityPolicyStats(log connect.Logger, prefix string, stats connect.SecurityPolicyStats) {
	log.Infof("%s security policy stats:", prefix)
	for result, destinationCounts := range stats {
		destinations := slices.Collect(maps.Keys(destinationCounts))
		slices.SortFunc(destinations, func(a connect.SecurityDestination, b connect.SecurityDestination) int {
			return a.Cmp(b)
		})
		for _, destination := range destinations {
			count := destinationCounts[destination]
			log.Infof("%s[%s] ->%s = %d", prefix, result.String(), destination.String(), count)
		}
	}
}
