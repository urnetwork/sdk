//go:build !ios_extension && !js

// D-TRUST-9's zone watch on sdk hosts (QD-1384; the core's dns_watch_component.go): the core's zone watch, whose pass
// about every 6 h asks two public DoH resolvers about this device's published names and mail hosts and alarms on
// records it did not request. An sdk host publishes through its tls/ app listeners alone (sidecar_tls.go), with no
// publish of its own, and the core's watch watches the names they serve beside any publish's, so a desktop app's
// tls/ Listen joins the watch's next pass and its Close leaves it.
//
// The host class decides it, as it decides D-TLS-8's CT watch (sidecar_tls.go): a class that stays up, desktop's, runs
// it, as ur daemon does for each network it hosts; ios and android run the sidecar role only while the app is open
// (owner decision 197), where a pass would seldom come, so they run none and leave it to the network's daemons, as they
// leave the CT watch. The composition adds it beside the core's dns component, whatever components the app names, as
// ur daemon adds it beside that component, so no name of its own joins SidecarSettings.Components (§18.5's bound
// list): an app on a class that stays up cannot leave it out.

package sdk

import (
	"github.com/urnetwork/sidecar"
)

// The zone watch's component for a host of hostClass on settings, nil taking the core's defaults; nil on a
// foreground-only class, which runs none.
func newSidecarDnsWatchComponent(local *sidecar.Sidecar, hostClass *sidecarHostClass, settings *sidecar.DnsWatchComponentSettings) sidecar.SidecarComponent {
	if hostClass.foregroundOnly {
		return nil
	}
	return local.NewDnsWatchComponent(settings)
}
