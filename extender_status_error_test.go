//go:build !ios && !android && !js

package sdk

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Root cause guarded: the feed's failed dial went into the status as it came, and a
// failed dial names the address it dialed, so an extender's address reached what the
// apps read and put in feedback (connect/EXTENDER.md, the note on what a client's
// errors say). The recorded dialer refuses each dial the way a real one does, naming
// the address.
func TestExtenderStatusLastErrorNamesNoAddress(t *testing.T) {
	testSeedEnableBlockedNetworkClient(t, false)
	extenderSeedLoopDisabled.Store(true)
	// the failed dial's text stays in the status until the next pass, and that pass
	// finds the address on hold and says something else: a long backoff keeps it
	// there to be read
	previousConfigure := extenderNetworkClientConfigure
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		previousConfigure(settings)
		settings.MinBackoff = time.Minute
		settings.MaxBackoff = time.Minute
	}
	t.Cleanup(func() {
		extenderNetworkClientConfigure = previousConfigure
	})
	testSeedInstallEmbedded(t, "")
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)

	space := newTestSeedSpace(t, testSeedStoragePath(t))
	now := time.Now()
	record := testSeedRecord(t, testSeedRecordSpec{
		index:      0,
		issueTime:  now.Add(-time.Hour),
		expireTime: now.Add(24 * time.Hour),
	})
	if _, err := space.extenderDirectory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	networkClient := space.getExtenderNetworkClient()
	if networkClient == nil {
		t.Fatal("the space runs no network client")
	}

	networkClient.Wake()

	lastError := ""
	testSeedWaitForTimeout(t, 30*time.Second, func() bool {
		lastError = space.GetExtenderStatus().LastError
		// past the constant the status says while there is no candidate
		return lastError != "" && lastError != "no extender candidate"
	})
	if !strings.Contains(lastError, "refused") {
		t.Fatalf("the status is not about the dial: %q", lastError)
	}
	if strings.Contains(lastError, testSeedExtenderIp(0).String()) {
		t.Fatalf("the status names the address: %q", lastError)
	}
}
