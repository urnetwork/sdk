//go:build !ios && !android && !js

package sdk

import (
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The bounded pinging of the provider extender role (connect/GEOMAP.md §2.1,
// D26): the peer sample its pinger takes and the cap on the active records of
// the space's directory while it runs.

// The role's peer sample: zero keeps the connect default, a negative value
// pings every peer, and a positive one is the sample size.
func TestDeviceLocalProviderExtenderPeerSampleSizePassesThrough(t *testing.T) {
	defaultSampleSize := connect.DefaultExtenderPeerPingerSettings().PeerSampleSize
	cases := []struct {
		peerSampleSize       int
		expectPeerSampleSize int
	}{
		{peerSampleSize: 0, expectPeerSampleSize: defaultSampleSize},
		{peerSampleSize: -1, expectPeerSampleSize: 0},
		{peerSampleSize: 16, expectPeerSampleSize: 16},
	}
	for _, c := range cases {
		var stateLock sync.Mutex
		peerSampleSize := -2
		fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
			settings.PeerSampleSize = c.peerSampleSize
			settings.ConfigurePeerPinger = func(pingerSettings *connect.ExtenderPeerPingerSettings) {
				stateLock.Lock()
				defer stateLock.Unlock()
				peerSampleSize = pingerSettings.PeerSampleSize
			}
		})
		fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
			return status.Enabled && status.Listening
		})
		stateLock.Lock()
		gotPeerSampleSize := peerSampleSize
		stateLock.Unlock()
		if gotPeerSampleSize != c.expectPeerSampleSize {
			t.Errorf("sample size %d reached the pinger as %d, expected %d", c.peerSampleSize, gotPeerSampleSize, c.expectPeerSampleSize)
		}
		fixture.device.SetProvideExtender(false)
	}
}

// The role's cap on the space directory's active records holds while the role
// runs and gives way to the space's own when it stops: zero leaves the
// space's, a negative value keeps every record, a positive one is the cap.
func TestDeviceLocalProviderExtenderDirectoryCapWhileRunning(t *testing.T) {
	defaultCap := connect.DefaultExtenderDirectorySettings().MaxActiveRecordCount
	cases := []struct {
		maxActiveRecordCount       int
		expectMaxActiveRecordCount int
	}{
		{maxActiveRecordCount: 0, expectMaxActiveRecordCount: defaultCap},
		{maxActiveRecordCount: -1, expectMaxActiveRecordCount: 0},
		{maxActiveRecordCount: 16, expectMaxActiveRecordCount: 16},
	}
	for _, c := range cases {
		fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
			settings.MaxActiveRecordCount = c.maxActiveRecordCount
		})
		fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
			return status.Enabled && status.Listening
		})
		directory := fixture.networkSpace.extenderDirectory
		if got := directory.MaxActiveRecordCount(); got != c.expectMaxActiveRecordCount {
			t.Errorf("cap %d holds as %d while the role runs, expected %d", c.maxActiveRecordCount, got, c.expectMaxActiveRecordCount)
		}
		// the role stops synchronously, and the space's own cap is back
		fixture.device.SetProvideExtender(false)
		if got := directory.MaxActiveRecordCount(); got != defaultCap {
			t.Errorf("cap %d left the space at %d once the role stopped, expected %d", c.maxActiveRecordCount, got, defaultCap)
		}
	}
}
