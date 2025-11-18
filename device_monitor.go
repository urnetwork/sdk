package sdk

import (
	"context"
	"slices"
	"time"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/connect"
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
	go securityPolicyMonitor.run()
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

		printSecurityPolicyStats("ingress", self.device.(device).ingressSecurityPolicy().Stats(false))
		printSecurityPolicyStats("egress", self.device.(device).egressSecurityPolicy().Stats(false))
	}
}

func printSecurityPolicyStats(prefix string, stats connect.SecurityPolicyStats) {
	glog.Infof("%s security policy stats:", prefix)
	for result, destinationCounts := range stats {
		destinations := maps.Keys(destinationCounts)
		slices.SortFunc(destinations, func(a connect.SecurityDestination, b connect.SecurityDestination) int {
			return a.Cmp(b)
		})
		for _, destination := range destinations {
			count := destinationCounts[destination]
			glog.Infof("%s[%s] ->%s = %d", prefix, result.String(), destination.String(), count)
		}
	}
}
