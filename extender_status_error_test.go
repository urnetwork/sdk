//go:build !ios && !android && !js

package sdk

import (
	"net/netip"
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
//
// The failed dial's text stays in the status only until the next pass, which finds
// the address on hold and says there is no candidate. So the refresh loop is held
// between passes (PassAfter), and the record goes in only once the cold pass has
// parked: a wake that lands while a pass is out is carried into another pass at once
// (connect's refresh loop subscribes before it looks), so a record applied and woken
// for during the cold pass is dialed by that pass, and its error replaced by the next
// before anything reads it.
func TestExtenderStatusLastErrorNamesNoAddress(t *testing.T) {
	testSeedEnableBlockedNetworkClient(t, false)
	extenderSeedLoopDisabled.Store(true)
	parks := make(chan time.Duration, 16)
	previousConfigure := extenderNetworkClientConfigure
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		previousConfigure(settings)
		settings.PassAfter = func(wait time.Duration) <-chan time.Time {
			select {
			case parks <- wait:
			default:
			}
			// never fires: only a wake runs the next pass
			return nil
		}
	}
	t.Cleanup(func() {
		extenderNetworkClientConfigure = previousConfigure
	})
	testSeedInstallEmbedded(t, "")
	dialer := newTestSeedDialer()
	testSeedInstallConnectSettings(t, dialer, nil)

	space := newTestSeedSpace(t, testSeedStoragePath(t))
	networkClient := space.getExtenderNetworkClient()
	if networkClient == nil {
		t.Fatal("the space runs no network client")
	}
	requireParked := func(what string) {
		t.Helper()
		select {
		case <-parks:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the refresh loop did not park", what)
		}
	}

	requireParked("the cold pass")
	// the constant the status says while there is no candidate
	if lastError := space.GetExtenderStatus().LastError; lastError != "no extender candidate" {
		t.Fatalf("the cold pass's status: %q", lastError)
	}

	now := time.Now()
	record := testSeedRecord(t, testSeedRecordSpec{
		index:      0,
		issueTime:  now.Add(-time.Hour),
		expireTime: now.Add(24 * time.Hour),
	})
	if _, err := space.extenderDirectory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	networkClient.Wake()
	requireParked("the pass that dials the record")

	address := netip.AddrPortFrom(testSeedExtenderIp(0), 443).String()
	if !dialer.dialedAddress(address) {
		t.Fatalf("the pass did not dial the record: %v", dialer.dialed())
	}
	lastError := space.GetExtenderStatus().LastError
	if !strings.Contains(lastError, "refused") {
		t.Fatalf("the status is not about the dial: %q", lastError)
	}
	if strings.Contains(lastError, testSeedExtenderIp(0).String()) {
		t.Fatalf("the status names the address: %q", lastError)
	}
}
