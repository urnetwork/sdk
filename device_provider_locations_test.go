package sdk

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type testing_connectedProviderLocationChangeListener struct {
	changed chan struct{}
}

func (self *testing_connectedProviderLocationChangeListener) ConnectedProviderLocationsChanged() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// Removing a provider must be safe on every device state the ui can reach it
// from: the app can swipe a row while the connection is being torn down, and
// a remote device can have its rpc down.
func TestRemoveConnectedProviderIsSafeWhenDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	listener := &testing_connectedProviderLocationChangeListener{
		changed: make(chan struct{}, 1),
	}
	sub := deviceLocal.AddConnectedProviderLocationChangeListener(listener)
	defer sub.Close()

	// no destination set: there is no multi client to remove from
	deviceLocal.RemoveConnectedProvider(newId(connect.NewId()))
	// a nil id is what an empty/malformed row would produce
	deviceLocal.RemoveConnectedProvider(nil)

	connect.AssertEqual(t, deviceLocal.GetConnectedProviderLocations().Len(), 0)

	// nothing was removed, so no change is reported. (Checked before any
	// destination is set: a destination change legitimately fires this.)
	select {
	case <-listener.changed:
		t.Fatal("a no-op removal fired the change listener")
	case <-time.After(200 * time.Millisecond):
	}

	// Connected: now there IS a multi client, so the removal reaches it. This
	// is where a missing nil guard would dereference the id and take the app
	// down. No provider ever connects here, so nothing is actually removed.
	deviceLocal.SetConnectLocation(&ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	})
	deviceLocal.RemoveConnectedProvider(nil)
	deviceLocal.RemoveConnectedProvider(newId(connect.NewId()))
	deviceLocal.SetConnectLocation(nil)

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(15*time.Second), true)
	deviceRemote.RemoveConnectedProvider(newId(connect.NewId()))
	deviceRemote.RemoveConnectedProvider(nil)

	// and with the rpc down
	deviceLocal.Close()
	deviceRemote.RemoveConnectedProvider(newId(connect.NewId()))
}

// The remote surface derives from the bridged window monitor rather than a
// dedicated rpc. A disconnected remote must retain its last readable list
// (freeze, not drain) so the ui does not blank while the rpc is down.
func TestDeviceRemoteConnectedProviderLocations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	listener := &testing_connectedProviderLocationChangeListener{
		changed: make(chan struct{}, 1),
	}
	sub := deviceRemote.AddConnectedProviderLocationChangeListener(listener)
	defer sub.Close()

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(15*time.Second), true)

	// disconnected: empty, never nil
	locations := deviceRemote.GetConnectedProviderLocations()
	if locations == nil {
		t.Fatal("connected provider locations must never be nil")
	}
	connect.AssertEqual(t, locations.Len(), 0)

	// with the rpc down the last readable list is retained rather than drained
	deviceLocal.Close()
	frozen := deviceRemote.GetConnectedProviderLocations()
	if frozen == nil {
		t.Fatal("a disconnected remote must return the retained list, not nil")
	}
	connect.AssertEqual(t, frozen.Len(), 0)
}

func TestDeriveConnectedProviderLocations(t *testing.T) {
	oldest := time.Now().Add(-10 * time.Minute)
	newer := time.Now().Add(-1 * time.Minute)

	windowA := connect.NewId()
	egressA := connect.NewId()
	windowB := connect.NewId()
	egressB := connect.NewId()
	windowC := connect.NewId()
	windowD := connect.NewId()

	location := &connect.ProviderLocation{
		Country:           "United States",
		CountryCode:       "us",
		Region:            "California",
		City:              "San Francisco",
		RegionCoordinates: &connect.LocationCoordinates{Lat: 37.2, Lon: -119.3},
		CityCoordinates:   &connect.LocationCoordinates{Lat: 37.7749, Lon: -122.4194},
	}

	providerEvents := map[connect.Id]*connect.ProviderEvent{
		windowA: {
			EventTime:      oldest,
			ClientId:       windowA,
			State:          connect.ProviderStateAdded,
			EgressClientId: egressA,
			Location:       location,
		},
		windowB: {
			EventTime:      newer,
			ClientId:       windowB,
			State:          connect.ProviderStateAdded,
			EgressClientId: egressB,
		},
		// not routing-eligible: excluded
		windowC: {
			EventTime:      time.Now(),
			ClientId:       windowC,
			State:          connect.ProviderStateInEvaluation,
			EgressClientId: connect.NewId(),
		},
		// an older device peer: no egress id, no event time. Falls back to
		// the window client id and sorts last (unknown connected-since)
		windowD: {
			ClientId: windowD,
			State:    connect.ProviderStateAdded,
		},
	}

	locations := deriveConnectedProviderLocations(providerEvents)
	if locations.Len() != 3 {
		t.Fatalf("derived %d locations, want 3", locations.Len())
	}

	first := locations.Get(0)
	if first.ClientId.String() != newId(egressA).String() {
		t.Fatal("oldest provider must be first and keyed by its egress client id")
	}
	if first.ConnectedSinceMillis != oldest.UnixMilli() {
		t.Fatalf("connected since = %d, want %d", first.ConnectedSinceMillis, oldest.UnixMilli())
	}
	if !first.HasLocation || !first.HasRegionCoordinates || !first.HasCityCoordinates {
		t.Fatalf("location flags lost: %+v", first)
	}
	if first.Country != "United States" || first.CountryCode != "us" ||
		first.Region != "California" || first.City != "San Francisco" {
		t.Fatalf("location names lost: %+v", first)
	}
	if first.RegionLat != 37.2 || first.RegionLon != -119.3 ||
		first.CityLat != 37.7749 || first.CityLon != -122.4194 {
		t.Fatalf("coordinates lost: %+v", first)
	}

	second := locations.Get(1)
	if second.ClientId.String() != newId(egressB).String() {
		t.Fatal("newer provider must sort after the oldest")
	}
	if second.HasLocation || second.HasRegionCoordinates || second.HasCityCoordinates {
		t.Fatalf("nil location must clear the flags: %+v", second)
	}

	third := locations.Get(2)
	if third.ClientId.String() != newId(windowD).String() {
		t.Fatal("zero egress id must fall back to the window client id")
	}
	if third.ConnectedSinceMillis != 0 {
		t.Fatal("zero event time must surface as 0 connected-since")
	}
}

func TestFixedWindowMonitorEventsCarryConnectedSince(t *testing.T) {
	clientA := connect.NewId()
	clientB := connect.NewId()

	before := time.Now()
	monitor := newFixedWindowMonitor([]connect.Id{clientA, clientB})
	after := time.Now()

	_, providerEvents := monitor.Events()
	if len(providerEvents) != 2 {
		t.Fatalf("fixed monitor events = %d, want 2", len(providerEvents))
	}
	for _, clientId := range []connect.Id{clientA, clientB} {
		event := providerEvents[clientId]
		if event == nil {
			t.Fatal("fixed destination missing from events")
		}
		if event.EgressClientId != clientId {
			t.Fatal("fixed destination id must be its own egress id")
		}
		if event.EventTime.Before(before) || event.EventTime.After(after) {
			t.Fatalf("fixed monitor connected-since %s outside construction window", event.EventTime)
		}
	}

	locations := deriveConnectedProviderLocations(providerEvents)
	if locations.Len() != 2 {
		t.Fatalf("derived %d fixed locations, want 2", locations.Len())
	}
	for i := range locations.Len() {
		location := locations.Get(i)
		if location.HasLocation {
			t.Fatal("fixed destinations have no discovered location")
		}
		if location.ConnectedSinceMillis == 0 {
			t.Fatal("fixed destinations must carry a connected-since time")
		}
	}
}

// the DeviceRemote monitor bridge gobs `connect.ProviderEvent` directly; the
// new detail fields must survive the wire round trip
func TestWindowMonitorEventGobCarriesProviderDetails(t *testing.T) {
	windowClientId := connect.NewId()
	egressClientId := connect.NewId()
	eventTime := time.Now().Add(-time.Minute).Round(0)

	event := &DeviceRemoteWindowMonitorEvent{
		WindowIds: map[connect.Id]bool{connect.NewId(): true},
		WindowExpandEvent: &connect.WindowExpandEvent{
			TargetSize:   4,
			MinSatisfied: true,
		},
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			windowClientId: {
				EventTime:      eventTime,
				ClientId:       windowClientId,
				State:          connect.ProviderStateAdded,
				EgressClientId: egressClientId,
				Location: &connect.ProviderLocation{
					Country:         "Iceland",
					CountryCode:     "is",
					Region:          "Capital Region",
					City:            "Reykjavik",
					CityCoordinates: &connect.LocationCoordinates{Lat: 64.1466, Lon: -21.9426},
				},
			},
		},
		Reset: true,
	}

	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(event); err != nil {
		t.Fatal(err)
	}
	var decoded DeviceRemoteWindowMonitorEvent
	if err := gob.NewDecoder(&buffer).Decode(&decoded); err != nil {
		t.Fatal(err)
	}

	decodedEvent := decoded.ProviderEvents[windowClientId]
	if decodedEvent == nil {
		t.Fatal("provider event lost in gob round trip")
	}
	if !decodedEvent.EventTime.Equal(eventTime) {
		t.Fatalf("event time %s != %s", decodedEvent.EventTime, eventTime)
	}
	if decodedEvent.EgressClientId != egressClientId {
		t.Fatal("egress client id lost in gob round trip")
	}
	location := decodedEvent.Location
	if location == nil || location.CountryCode != "is" || location.City != "Reykjavik" {
		t.Fatalf("location lost in gob round trip: %+v", location)
	}
	if location.CityCoordinates == nil || location.CityCoordinates.Lat != 64.1466 {
		t.Fatalf("city coordinates lost in gob round trip: %+v", location.CityCoordinates)
	}
	if location.RegionCoordinates != nil {
		t.Fatal("absent region coordinates must stay nil through gob")
	}
}
