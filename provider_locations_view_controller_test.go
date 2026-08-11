package sdk

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func testing_expectNear(t *testing.T, expected float64, actual float64, tolerance float64) {
	t.Helper()
	if math.Abs(expected-actual) > tolerance {
		t.Fatalf("expected %f within %f, got %f", expected, tolerance, actual)
	}
}

func testing_plottableLocation(clientId *Id, lon float64, lat float64) *ConnectedProviderLocation {
	return &ConnectedProviderLocation{
		ClientId:           clientId,
		HasLocation:        true,
		HasCityCoordinates: true,
		CityLat:            lat,
		CityLon:            lon,
	}
}

// The wheel order is west to east as seen from the providers' centroid, so a
// cluster straddling the +/-180 antimeridian stays contiguous: the -175
// provider is the EASTERN end, where a raw longitude sort would make it the
// western one.
func TestWheelOrderKeepsADateLineClusterContiguous(t *testing.T) {
	at160 := newId(connect.NewId())
	at170 := newId(connect.NewId())
	atMinus175 := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(atMinus175, -175, 0))
	locations.Add(testing_plottableLocation(at160, 160, 0))
	locations.Add(testing_plottableLocation(at170, 170, 0))

	window := deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.clientIds), 3)
	connect.AssertEqual(t, window.wheel, []string{
		at160.String(),
		at170.String(),
		atMinus175.String(),
	})
}

// Only plottable providers ride the wheel, the city centroid wins over the
// region centroid, and providers sharing a longitude keep the list's
// oldest-first order (the sort is stable).
func TestWheelOrderPlotsAndTieBreaks(t *testing.T) {
	cityWins := newId(connect.NewId())
	regionOnly := newId(connect.NewId())
	unplottable := newId(connect.NewId())
	sharedOlder := newId(connect.NewId())
	sharedYounger := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	// city at lon 10 must beat region at lon 50, sorting this row first
	locations.Add(&ConnectedProviderLocation{
		ClientId:             cityWins,
		HasLocation:          true,
		HasCityCoordinates:   true,
		CityLon:              10,
		HasRegionCoordinates: true,
		RegionLon:            50,
	})
	locations.Add(&ConnectedProviderLocation{
		ClientId:             regionOnly,
		HasLocation:          true,
		HasRegionCoordinates: true,
		RegionLon:            20,
	})
	// listed but never plotted
	locations.Add(&ConnectedProviderLocation{ClientId: unplottable})
	locations.Add(testing_plottableLocation(sharedOlder, 30, 0))
	locations.Add(testing_plottableLocation(sharedYounger, 30, 0))

	window := deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.clientIds), 5)
	connect.AssertEqual(t, window.clientIds[unplottable.String()], true)
	// the window keeps the sdk's order, longest connected first, whether or not
	// a provider can be plotted
	connect.AssertEqual(t, window.order, []string{
		cityWins.String(),
		regionOnly.String(),
		unplottable.String(),
		sharedOlder.String(),
		sharedYounger.String(),
	})
	connect.AssertEqual(t, window.wheel, []string{
		cityWins.String(),
		regionOnly.String(),
		sharedOlder.String(),
		sharedYounger.String(),
	})
}

// An empty window and (defensively) a nil list are an empty wheel, not a crash.
// A row whose client id is missing is skipped rather than keyed on "".
func TestWheelOrderToleratesEmptyAndMalformedInput(t *testing.T) {
	window := deriveProviderWindow(nil)
	connect.AssertEqual(t, len(window.clientIds), 0)
	connect.AssertEqual(t, len(window.wheel), 0)
	connect.AssertEqual(t, len(window.order), 0)

	locations := NewConnectedProviderLocationList()
	window = deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.clientIds), 0)
	connect.AssertEqual(t, len(window.wheel), 0)

	// no client id: nothing can select it, so it is neither listed nor plotted
	locations.Add(&ConnectedProviderLocation{
		HasLocation:        true,
		HasCityCoordinates: true,
	})
	window = deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.clientIds), 0)
	connect.AssertEqual(t, len(window.wheel), 0)
	connect.AssertEqual(t, len(window.order), 0)
}

func TestCentroidLongitudeIsTheHorizontalMeanDirection(t *testing.T) {
	// a single point is its own centroid
	testing_expectNear(t, 20, centroidLongitude([][2]float64{{20, 50}}), 1e-9)
	// two points straddling the date line average to 180, not 0
	testing_expectNear(
		t,
		180,
		math.Abs(centroidLongitude([][2]float64{{170, 0}, {-170, 0}})),
		1e-6,
	)
	// latitude weights: the near-pole point pulls less than the equatorial
	// one, so the centroid sits east of the plain longitude average of 5
	testing_expectNear(t, 6.6704, centroidLongitude([][2]float64{{0, 60}, {10, 0}}), 1e-3)
	// an empty set has no direction; resolves to 0
	testing_expectNear(t, 0, centroidLongitude(nil), 0)
}

func TestSignedLonDeltaIsTheOffsetFromTheReference(t *testing.T) {
	testing_expectNear(t, 0, signedLonDelta(10, 10), 0)
	// 170 is 20 degrees west of -170 across the date line, not 340 east
	testing_expectNear(t, -20, signedLonDelta(170, -170), 1e-9)
	testing_expectNear(t, 20, signedLonDelta(-170, 170), 1e-9)
	// both antipodes of the reference land at -180, the far-west end
	testing_expectNear(t, -180, signedLonDelta(180, 0), 1e-9)
	testing_expectNear(t, -180, signedLonDelta(-180, 0), 1e-9)
}

// The wheel does not wrap: stepping past an end sticks there instead of
// teleporting the long way round the globe.
func TestClampWheelIndexSticksAtBothEnds(t *testing.T) {
	connect.AssertEqual(t, clampWheelIndex(2, 1, 3), 2)
	connect.AssertEqual(t, clampWheelIndex(0, -1, 3), 0)
	connect.AssertEqual(t, clampWheelIndex(0, 1, 3), 1)
	// a multi-step drag clamps to the end it runs into
	connect.AssertEqual(t, clampWheelIndex(0, 7, 3), 2)
	connect.AssertEqual(t, clampWheelIndex(2, -7, 3), 0)
	connect.AssertEqual(t, clampWheelIndex(0, 1, 0), -1)
}

func TestStepWheelSelection(t *testing.T) {
	wheel := []string{"west", "mid", "east"}

	// an empty wheel never steps
	_, ok := stepWheelSelection(nil, "", 1)
	connect.AssertEqual(t, ok, false)

	// nothing selected (or a selection without coordinates): the first step
	// lands on the wheel's west end, whatever the direction
	for _, selected := range []string{"", "not-on-the-wheel"} {
		for _, steps := range []int{1, -1, 5} {
			next, ok := stepWheelSelection(wheel, selected, steps)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, next, "west")
		}
	}

	next, ok := stepWheelSelection(wheel, "west", 1)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, next, "mid")

	next, ok = stepWheelSelection(wheel, "east", -2)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, next, "west")

	// a fast drag past the end clamps to it
	next, ok = stepWheelSelection(wheel, "west", 7)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, next, "east")

	// sticking at an end is not a change
	_, ok = stepWheelSelection(wheel, "east", 1)
	connect.AssertEqual(t, ok, false)
	_, ok = stepWheelSelection(wheel, "west", -3)
	connect.AssertEqual(t, ok, false)
}

// The exported stepping API over a populated wheel. The wheel normally comes
// from the device's window; here it is set directly so the controller's own
// behavior — clamping at the ends and reporting only real changes — is tested
// without standing up a connection.
func TestStepSelectionClampsAtTheWheelEnds(t *testing.T) {
	vc := &ProviderLocationsViewController{
		window: providerWindow{
			order:     []string{"west", "mid", "east"},
			clientIds: map[string]bool{"west": true, "mid": true, "east": true},
			wheel:     []string{"west", "mid", "east"},
		},
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}

	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	// nothing selected: the first step lands on the west end
	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "west")
	connect.AssertEqual(t, changes, 1)

	// a zero step is not a step
	vc.StepSelection(0)
	connect.AssertEqual(t, changes, 1)

	// stepping west from the west end sticks there and reports nothing
	vc.StepSelection(-1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "west")
	connect.AssertEqual(t, changes, 1)

	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "mid")
	connect.AssertEqual(t, changes, 2)

	// a fast drag past the east end clamps to it rather than wrapping west
	vc.StepSelection(9)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "east")
	connect.AssertEqual(t, changes, 3)

	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "east")
	connect.AssertEqual(t, changes, 3)

	// and back the other way, clamping at the west end
	vc.StepSelection(-9)
	connect.AssertEqual(t, vc.GetSelectedClientId(), "west")
	connect.AssertEqual(t, changes, 4)
}

// testing_window builds a window from ids in the sdk's order (longest
// connected first), spread over distinct longitudes so every one is plottable.
func testing_window(t *testing.T, ids ...*Id) *ConnectedProviderLocationList {
	t.Helper()
	locations := NewConnectedProviderLocationList()
	for i, id := range ids {
		locations.Add(testing_plottableLocation(id, float64(10*i), 0))
	}
	return locations
}

// With nothing selected, the provider that has been connected the longest —
// the window's first entry — is selected, so the screen never opens on an
// empty selection.
func TestTheLongestConnectedProviderIsSelectedByDefault(t *testing.T) {
	oldest := newId(connect.NewId())
	middle := newId(connect.NewId())
	youngest := newId(connect.NewId())

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	// an empty window has nothing to select
	vc.setLocations(NewConnectedProviderLocationList())
	connect.AssertEqual(t, vc.GetSelectedClientId(), "")
	connect.AssertEqual(t, changes, 0)

	vc.setLocations(testing_window(t, oldest, middle, youngest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 1)

	// an explicit selection survives later window updates
	vc.SetSelectedClientId(youngest.String())
	connect.AssertEqual(t, changes, 2)
	vc.setLocations(testing_window(t, oldest, middle, youngest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), youngest.String())
	connect.AssertEqual(t, changes, 2)

	// clearing falls back to the default rather than resting on nothing
	vc.SetSelectedClientId("")
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 3)
}

// When the selected provider leaves the window the selection moves to the next
// OLDER provider — the neighbor that has been connected longer.
func TestSelectionMovesToTheNextOlderProviderWhenItsProviderLeaves(t *testing.T) {
	oldest := newId(connect.NewId())
	middle := newId(connect.NewId())
	youngest := newId(connect.NewId())

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(testing_window(t, oldest, middle, youngest))

	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	vc.SetSelectedClientId(youngest.String())
	connect.AssertEqual(t, changes, 1)

	// the youngest goes away: the selection lands on the one connected next
	// longest, not on the oldest and not on nothing
	vc.setLocations(testing_window(t, oldest, middle))
	connect.AssertEqual(t, vc.GetSelectedClientId(), middle.String())
	connect.AssertEqual(t, changes, 2)

	// and again, down to the oldest
	vc.setLocations(testing_window(t, oldest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 3)

	// the last one leaving is the only way back to no selection
	vc.setLocations(NewConnectedProviderLocationList())
	connect.AssertEqual(t, vc.GetSelectedClientId(), "")
	connect.AssertEqual(t, changes, 4)
}

// The oldest provider has no older neighbor, so the selection falls to the
// nearest younger one — which is the new longest connected.
func TestRemovingTheOldestSelectedProviderFallsToTheNextYoungest(t *testing.T) {
	oldest := newId(connect.NewId())
	middle := newId(connect.NewId())
	youngest := newId(connect.NewId())

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(testing_window(t, oldest, middle, youngest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())

	vc.setLocations(testing_window(t, middle, youngest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), middle.String())
}

// Removing the selected provider moves the selection immediately, without
// waiting for the window round trip, so the ui never blinks through "nothing
// selected". (RemoveProvider itself also calls the device; this is the
// selection half of it.)
func TestRemovingTheSelectedProviderMovesTheSelectionUpFront(t *testing.T) {
	oldest := newId(connect.NewId())
	middle := newId(connect.NewId())
	youngest := newId(connect.NewId())

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(testing_window(t, oldest, middle, youngest))

	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	// removing a provider that is not selected leaves the selection alone
	vc.selectSuccessorTo(youngest.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 0)

	vc.SetSelectedClientId(middle.String())
	connect.AssertEqual(t, changes, 1)

	vc.selectSuccessorTo(middle.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 2)

	// removing the oldest with nothing older left falls to the next younger
	vc.selectSuccessorTo(oldest.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), middle.String())
	connect.AssertEqual(t, changes, 3)
}

// Several providers rotating out at once: the selection walks back to the
// nearest SURVIVING older provider, skipping the ones that also went.
func TestSelectionSkipsProvidersThatLeftAlongsideIt(t *testing.T) {
	oldest := newId(connect.NewId())
	alsoLeft := newId(connect.NewId())
	selected := newId(connect.NewId())

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(testing_window(t, oldest, alsoLeft, selected))
	vc.SetSelectedClientId(selected.String())

	vc.setLocations(testing_window(t, oldest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
}

type testing_selectedProviderLocationCounter func()

func (self testing_selectedProviderLocationCounter) SelectedProviderLocationChanged() {
	self()
}

type testing_selectedProviderLocationChangeListener struct {
	changed chan struct{}
}

func (self *testing_selectedProviderLocationChangeListener) SelectedProviderLocationChanged() {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// The exported surface on a real device: selection set/get/step and the
// change listener, on the manager's open/close lifecycle.
func TestProviderLocationsViewControllerSelection(t *testing.T) {
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

	vc := deviceLocal.OpenProviderLocationsViewController()

	listener := &testing_selectedProviderLocationChangeListener{
		changed: make(chan struct{}, 1),
	}
	sub := vc.AddSelectedProviderLocationChangeListener(listener)
	defer sub.Close()

	expectChange := func() {
		select {
		case <-listener.changed:
		case <-time.After(1 * time.Second):
			t.Fatal("expected a selection change")
		}
	}
	expectNoChange := func() {
		select {
		case <-listener.changed:
			t.Fatal("unexpected selection change")
		case <-time.After(200 * time.Millisecond):
		}
	}

	connect.AssertEqual(t, vc.GetSelectedClientId(), "")

	vc.SetSelectedClientId("provider-1")
	expectChange()
	connect.AssertEqual(t, vc.GetSelectedClientId(), "provider-1")

	// re-selecting the same provider is not a change
	vc.SetSelectedClientId("provider-1")
	expectNoChange()

	// disconnected: the wheel is empty, so a step changes nothing
	vc.StepSelection(1)
	expectNoChange()
	connect.AssertEqual(t, vc.GetSelectedClientId(), "provider-1")
	vc.StepSelection(0)
	expectNoChange()

	vc.SetSelectedClientId("")
	expectChange()
	connect.AssertEqual(t, vc.GetSelectedClientId(), "")

	deviceLocal.CloseProviderLocationsViewController(vc)
}
