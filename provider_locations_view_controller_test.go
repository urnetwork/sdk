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

// testing_orderIds is the window's display order as client id strings.
func testing_orderIds(window providerWindow) []string {
	ids := []string{}
	for _, location := range window.order {
		ids = append(ids, location.ClientId.String())
	}
	return ids
}

// The display order is west to east as seen from the providers' centroid, so a
// cluster straddling the +/-180 antimeridian stays contiguous: the -175
// provider is the EASTERN end, where a raw longitude sort would make it the
// western one.
func TestDisplayOrderKeepsADateLineClusterContiguous(t *testing.T) {
	at160 := newId(connect.NewId())
	at170 := newId(connect.NewId())
	atMinus175 := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(atMinus175, -175, 0))
	locations.Add(testing_plottableLocation(at160, 160, 0))
	locations.Add(testing_plottableLocation(at170, 170, 0))

	window := deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.index), 3)
	connect.AssertEqual(t, window.wheel, []string{
		at160.String(),
		at170.String(),
		atMinus175.String(),
	})
	// the wheel is the plottable head of the display order, so with every
	// provider plottable they are the same list
	connect.AssertEqual(t, testing_orderIds(window), window.wheel)
	// the longest connected provider is the window's first entry whatever the
	// display order does with it
	connect.AssertEqual(t, window.longestConnected(), atMinus175.String())
}

// The list is the wheel plus the providers that have no place on the globe:
// plottable ones west to east, then the unplottable ones. The city centroid
// wins over the region centroid, and providers sharing a longitude keep the
// sdk's oldest-first order (the sort is stable).
func TestDisplayOrderPlotsAndTieBreaks(t *testing.T) {
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
	connect.AssertEqual(t, len(window.index), 5)
	connect.AssertEqual(t, window.has(unplottable.String()), true)
	connect.AssertEqual(t, testing_orderIds(window), []string{
		cityWins.String(),
		regionOnly.String(),
		sharedOlder.String(),
		sharedYounger.String(),
		unplottable.String(),
	})
	connect.AssertEqual(t, window.wheel, []string{
		cityWins.String(),
		regionOnly.String(),
		sharedOlder.String(),
		sharedYounger.String(),
	})
	connect.AssertEqual(t, window.index[sharedYounger.String()], 3)
	connect.AssertEqual(t, window.longestConnected(), cityWins.String())
}

// An empty window and (defensively) a nil list are an empty wheel, not a crash.
// A row whose client id is missing is skipped rather than keyed on "".
func TestDisplayOrderToleratesEmptyAndMalformedInput(t *testing.T) {
	window := deriveProviderWindow(nil)
	connect.AssertEqual(t, len(window.index), 0)
	connect.AssertEqual(t, len(window.wheel), 0)
	connect.AssertEqual(t, len(window.order), 0)
	connect.AssertEqual(t, window.longestConnected(), "")

	locations := NewConnectedProviderLocationList()
	window = deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.index), 0)
	connect.AssertEqual(t, len(window.wheel), 0)

	// no client id: nothing can select it, so it is neither listed nor plotted
	locations.Add(&ConnectedProviderLocation{
		HasLocation:        true,
		HasCityCoordinates: true,
	})
	window = deriveProviderWindow(locations)
	connect.AssertEqual(t, len(window.index), 0)
	connect.AssertEqual(t, len(window.wheel), 0)
	connect.AssertEqual(t, len(window.order), 0)
	connect.AssertEqual(t, window.longestConnected(), "")
}

// The list an app renders is the controller's, in display order — not the
// device's, which is sorted by connected duration.
func TestGetProviderLocationsIsInDisplayOrder(t *testing.T) {
	east := newId(connect.NewId())
	west := newId(connect.NewId())
	unplottable := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(east, 40, 0))
	locations.Add(&ConnectedProviderLocation{ClientId: unplottable})
	locations.Add(testing_plottableLocation(west, -40, 0))

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	connect.AssertEqual(t, vc.GetProviderLocations().Len(), 0)

	vc.setLocations(locations)
	ordered := vc.GetProviderLocations()
	connect.AssertEqual(t, ordered.Len(), 3)
	connect.AssertEqual(t, ordered.Get(0).ClientId.String(), west.String())
	connect.AssertEqual(t, ordered.Get(1).ClientId.String(), east.String())
	connect.AssertEqual(t, ordered.Get(2).ClientId.String(), unplottable.String())
	// the default selection is still the longest connected provider, wherever
	// the display order puts it
	connect.AssertEqual(t, vc.GetSelectedClientId(), east.String())
}

// Great-circle distance in radians of arc, used only to rank successors.
func TestAngularDistance(t *testing.T) {
	testing_expectNear(t, 0, angularDistance(37.8, -122.4, 37.8, -122.4), 0)
	// a quarter turn along the equator, and from the equator to the pole
	testing_expectNear(t, math.Pi/2, angularDistance(0, 0, 0, 90), 1e-12)
	testing_expectNear(t, math.Pi/2, angularDistance(0, 0, 90, 0), 1e-12)
	// antipodes: a is exactly 1 (or a hair past it), which must not be NaN
	testing_expectNear(t, math.Pi, angularDistance(0, 0, 0, 180), 1e-9)
	testing_expectNear(t, math.Pi, angularDistance(45, 10, -45, -170), 1e-9)
	// a degree of longitude shrinks with the cosine of the latitude: at 60N it
	// is half of the degree of latitude it would be at the equator
	testing_expectNear(t, math.Pi/360, angularDistance(60, 0, 60, 1), 1e-5)
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
	west := newId(connect.NewId())
	mid := newId(connect.NewId())
	east := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(west, -20, 0))
	locations.Add(testing_plottableLocation(mid, 0, 0))
	locations.Add(testing_plottableLocation(east, 20, 0))

	vc := &ProviderLocationsViewController{
		window:            deriveProviderWindow(locations),
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}

	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	// nothing selected: the first step lands on the west end
	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), west.String())
	connect.AssertEqual(t, changes, 1)

	// a zero step is not a step
	vc.StepSelection(0)
	connect.AssertEqual(t, changes, 1)

	// stepping west from the west end sticks there and reports nothing
	vc.StepSelection(-1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), west.String())
	connect.AssertEqual(t, changes, 1)

	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), mid.String())
	connect.AssertEqual(t, changes, 2)

	// a fast drag past the east end clamps to it rather than wrapping west
	vc.StepSelection(9)
	connect.AssertEqual(t, vc.GetSelectedClientId(), east.String())
	connect.AssertEqual(t, changes, 3)

	vc.StepSelection(1)
	connect.AssertEqual(t, vc.GetSelectedClientId(), east.String())
	connect.AssertEqual(t, changes, 3)

	// and back the other way, clamping at the west end
	vc.StepSelection(-9)
	connect.AssertEqual(t, vc.GetSelectedClientId(), west.String())
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

// When the selected provider leaves the window the selection moves to the
// NEAREST surviving provider, so the globe travels the short way to the dot
// beside the one that just disappeared.
func TestSelectionMovesToTheNearestProviderWhenItsProviderLeaves(t *testing.T) {
	// spread 10 degrees apart, so each one's nearest neighbor is unambiguous
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

	// the easternmost goes away: the selection lands on its western neighbor,
	// not on the far end of the wheel and not on nothing
	vc.setLocations(testing_window(t, oldest, middle))
	connect.AssertEqual(t, vc.GetSelectedClientId(), middle.String())
	connect.AssertEqual(t, changes, 2)

	// and again, down to the last one standing
	vc.setLocations(testing_window(t, oldest))
	connect.AssertEqual(t, vc.GetSelectedClientId(), oldest.String())
	connect.AssertEqual(t, changes, 3)

	// the last one leaving is the only way back to no selection
	vc.setLocations(NewConnectedProviderLocationList())
	connect.AssertEqual(t, vc.GetSelectedClientId(), "")
	connect.AssertEqual(t, changes, 4)
}

// Nearest is measured on the globe, not in the list: the provider next to the
// departing one in display order can be much further away than one several
// rows off, because the order only ranks longitude while distance also counts
// latitude.
func TestTheNearestSuccessorIsNotAlwaysTheNeighboringRow(t *testing.T) {
	selected := newId(connect.NewId())
	adjacentButFar := newId(connect.NewId())
	distantRowButNear := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(selected, 0, 0))
	// 5 degrees east but 80 degrees north: the very next row, ~80 degrees away
	locations.Add(testing_plottableLocation(adjacentButFar, 5, 80))
	// two rows along the equator, 30 degrees away
	locations.Add(testing_plottableLocation(distantRowButNear, 30, 0))

	window := deriveProviderWindow(locations)
	connect.AssertEqual(t, testing_orderIds(window), []string{
		selected.String(),
		adjacentButFar.String(),
		distantRowButNear.String(),
	})

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(locations)
	vc.SetSelectedClientId(selected.String())

	vc.selectSuccessorTo(selected.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), distantRowButNear.String())
}

// Two providers exactly as far away: the one earlier in display order wins, so
// the hand-off is the same every time rather than map-iteration roulette.
func TestEquallyNearSuccessorsResolveByDisplayOrder(t *testing.T) {
	west := newId(connect.NewId())
	selected := newId(connect.NewId())
	east := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(selected, 0, 0))
	locations.Add(testing_plottableLocation(east, 10, 0))
	locations.Add(testing_plottableLocation(west, -10, 0))

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(locations)
	vc.SetSelectedClientId(selected.String())

	vc.selectSuccessorTo(selected.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), west.String())
}

// A provider with no coordinates has no distance to anything, and neither does
// a window where nothing plottable is left. Both fall back to the neighboring
// row — the entry before it in display order, else the one after.
func TestSuccessorFallsBackToTheNeighboringRowWithoutCoordinates(t *testing.T) {
	west := newId(connect.NewId())
	east := newId(connect.NewId())
	unplottable := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(east, 10, 0))
	locations.Add(&ConnectedProviderLocation{ClientId: unplottable})
	locations.Add(testing_plottableLocation(west, -10, 0))

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	// display order is west, east, unplottable
	vc.setLocations(locations)
	vc.SetSelectedClientId(unplottable.String())

	// the departing provider has no position to measure from: the row above it
	vc.selectSuccessorTo(unplottable.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), east.String())

	// nothing plottable survives, so the same fallback applies from the other
	// end: no row above, so the row below
	firstRowOnly := NewConnectedProviderLocationList()
	firstRowOnly.Add(testing_plottableLocation(west, -10, 0))
	firstRowOnly.Add(&ConnectedProviderLocation{ClientId: unplottable})
	vc.setLocations(firstRowOnly)
	vc.SetSelectedClientId(west.String())
	vc.selectSuccessorTo(west.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), unplottable.String())
}

// Removing the selected provider moves the selection immediately, without
// waiting for the window round trip, so the ui never blinks through "nothing
// selected". (RemoveProvider itself also calls the device; this is the
// selection half of it.)
func TestRemovingTheSelectedProviderMovesTheSelectionUpFront(t *testing.T) {
	west := newId(connect.NewId())
	middle := newId(connect.NewId())
	east := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	// west is the longest connected, so it is also the default selection
	locations.Add(testing_plottableLocation(west, 0, 0))
	locations.Add(testing_plottableLocation(middle, 30, 0))
	locations.Add(testing_plottableLocation(east, 35, 0))

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(locations)

	changes := 0
	sub := vc.AddSelectedProviderLocationChangeListener(
		testing_selectedProviderLocationCounter(func() { changes += 1 }),
	)
	defer sub.Close()

	// removing a provider that is not selected leaves the selection alone
	vc.selectSuccessorTo(east.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), west.String())
	connect.AssertEqual(t, changes, 0)

	vc.SetSelectedClientId(middle.String())
	connect.AssertEqual(t, changes, 1)

	// east is 5 degrees away, west is 30
	vc.selectSuccessorTo(middle.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), east.String())
	connect.AssertEqual(t, changes, 2)

	vc.selectSuccessorTo(east.String())
	connect.AssertEqual(t, vc.GetSelectedClientId(), middle.String())
	connect.AssertEqual(t, changes, 3)
}

// Several providers rotating out at once: the selection lands on the nearest
// SURVIVOR, skipping the ones that also went — even when one of those was
// closer than anything left.
func TestSelectionSkipsProvidersThatLeftAlongsideIt(t *testing.T) {
	farSurvivor := newId(connect.NewId())
	alsoLeft := newId(connect.NewId())
	selected := newId(connect.NewId())

	locations := NewConnectedProviderLocationList()
	locations.Add(testing_plottableLocation(farSurvivor, -60, 0))
	locations.Add(testing_plottableLocation(alsoLeft, 25, 0))
	locations.Add(testing_plottableLocation(selected, 30, 0))

	vc := &ProviderLocationsViewController{
		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.setLocations(locations)
	vc.SetSelectedClientId(selected.String())

	survivors := NewConnectedProviderLocationList()
	survivors.Add(testing_plottableLocation(farSurvivor, -60, 0))
	vc.setLocations(survivors)
	connect.AssertEqual(t, vc.GetSelectedClientId(), farSurvivor.String())
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
