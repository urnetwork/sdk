//go:build !ios_extension

package sdk

import (
	"cmp"
	"context"
	"math"
	"slices"
	"sync"

	"github.com/urnetwork/connect"
)

// fired whenever the selected provider changes: an explicit selection, a
// scroll step, or the selected provider leaving the window. Consumers re-read
// `GetSelectedClientId`.
type SelectedProviderLocationChangeListener interface {
	SelectedProviderLocationChanged()
}

// ProviderLocationsViewController owns the provider-locations screen's shared
// behavior — the display order, the selection, and the globe's scroll wheel —
// so every app shares one behavior instead of hand-rolling it per platform.
//
// DISPLAY ORDER is west to east about the providers' spherical centroid: each
// plottable provider is ranked by its signed longitude offset from the
// centroid, so the order's two ends fall on the meridian opposite the data's
// center — the farthest place from the providers. A raw longitude sort would
// instead cut at the +/-180 antimeridian and split a cluster that straddles it
// (a provider at -178 would sort to the far west end even though it sits just
// east of one at +175). Providers with no coordinates have no place on the
// globe, so they follow, in the sdk's own order.
//
// `GetProviderLocations` returns the window in that order and is what the apps
// render their list from. The list therefore reads left to right in the same
// order the globe steps through — sorting the list by connected duration
// instead put the rows in an order the globe's wheel did not follow.
//
// The WHEEL is the plottable head of that order. Scrolling does not cycle:
// `StepSelection` clamps at the wheel's ends, so stepping past the extreme
// west or east sticks there instead of teleporting the long way round the
// globe. The apps translate their gestures (drag travel past a hysteresis
// threshold, scroll-wheel notches) into step counts; everything after that —
// order, clamping, selection — lives here.
//
// The SELECTION is the egress client id as a string. It is not restricted to
// plottable providers (the list lets the user select a row with no
// coordinates); a step from such a selection starts at the wheel's west end.
// Two rules keep it pointed at something real, so the screen never rests on
// nothing while providers are connected:
//
//   - with nothing selected, the LONGEST CONNECTED provider is selected (the
//     window's first entry — the sdk sorts it oldest-connected first);
//   - when the selected provider leaves the window (the user removed it, or it
//     rotated out at the window's client lifetime), the selection moves to the
//     NEAREST surviving provider — the smallest great-circle distance from the
//     one that left. The globe is centered on the selection, so handing it to
//     the nearest provider moves the globe the shortest way to the dot beside
//     the one that just disappeared, instead of throwing it across the world.
//
// "" therefore means only "no providers are connected".
type ProviderLocationsViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	window           providerWindow
	selectedClientId string

	locationsChangedSub Sub

	selectedListeners *connect.CallbackList[SelectedProviderLocationChangeListener]
}

// providerWindow is the current connect window as the screen needs it: the
// providers in display order, an index over them, the globe's wheel, and the
// longest connected provider.
type providerWindow struct {
	// every provider in display order: the plottable ones west to east about
	// their centroid, then the ones with no coordinates (see the
	// ProviderLocationsViewController comment)
	order []*ConnectedProviderLocation
	// each provider's position in `order`, keyed by egress client id; doubles
	// as the set for "is this provider still connected"
	index map[string]int
	// the plottable head of `order` as client ids — the globe's wheel
	wheel []string
	// the provider that has been connected the longest, which is the first
	// entry of the sdk's own (oldest-connected-first) order, not of `order`
	longest string
}

// has reports whether the provider is in this window.
func (self providerWindow) has(clientId string) bool {
	_, ok := self.index[clientId]
	return ok
}

// longestConnected is the provider that has been connected the longest, or ""
// when nothing is connected.
func (self providerWindow) longestConnected() string {
	return self.longest
}

// successorTo is who inherits the selection when `clientId` goes away: the
// NEAREST provider `present` still reports, by great-circle distance from the
// one that left. Ties keep the earlier provider in display order.
//
// Distance needs coordinates at both ends, so when the departing provider has
// none — or nothing that survives has any — it falls back to the neighbor in
// display order: the entry before it, else the one after. That is the nearest
// row on screen, which is the same idea one dimension down.
//
// "" when nothing survives or the id is not in this window.
func (self providerWindow) successorTo(clientId string, present func(string) bool) string {
	index, ok := self.index[clientId]
	if !ok {
		return ""
	}
	if lat, lon, plottable := plotCoordinates(self.order[index]); plottable {
		nearest := ""
		nearestDistance := math.MaxFloat64
		for i, location := range self.order {
			if i == index {
				continue
			}
			otherLat, otherLon, otherPlottable := plotCoordinates(location)
			if !otherPlottable {
				continue
			}
			otherClientId := location.ClientId.String()
			if !present(otherClientId) {
				continue
			}
			if distance := angularDistance(lat, lon, otherLat, otherLon); distance < nearestDistance {
				nearest = otherClientId
				nearestDistance = distance
			}
		}
		if nearest != "" {
			return nearest
		}
	}
	for i := index - 1; 0 <= i; i -= 1 {
		if id := self.order[i].ClientId.String(); present(id) {
			return id
		}
	}
	for i := index + 1; i < len(self.order); i += 1 {
		if id := self.order[i].ClientId.String(); present(id) {
			return id
		}
	}
	return ""
}

func newProviderLocationsViewController(ctx context.Context, device Device) *ProviderLocationsViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &ProviderLocationsViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		window:           deriveProviderWindow(nil),
		selectedClientId: "",

		selectedListeners: connect.NewCallbackList[SelectedProviderLocationChangeListener](),
	}
	vc.locationsChangedSub = device.AddConnectedProviderLocationChangeListener(vc)
	vc.ConnectedProviderLocationsChanged()
	return vc
}

// ConnectedProviderLocationChangeListener
func (self *ProviderLocationsViewController) ConnectedProviderLocationsChanged() {
	self.setLocations(self.device.GetConnectedProviderLocations())
}

// setLocations rebuilds the window and re-points the selection at something
// real: the nearest surviving provider when the selected one has left, the
// longest connected provider when nothing is selected. Split from the listener
// so the state transition is exercised directly by tests, with no device to
// stand up.
func (self *ProviderLocationsViewController) setLocations(locations *ConnectedProviderLocationList) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.window
		self.window = deriveProviderWindow(locations)
		next := self.selectedClientId
		if next != "" && !self.window.has(next) {
			// removed by the user, or rotated out of the window. The previous
			// window is what still knows where the departing provider was, so
			// the nearest surviving one is measured from there.
			next = previous.successorTo(next, self.window.has)
		}
		if next == "" {
			next = self.window.longestConnected()
		}
		if next != self.selectedClientId {
			self.selectedClientId = next
			changed = true
		}
	}()
	if changed {
		self.selectedProviderLocationChanged()
	}
}

// RemoveProvider drops the provider from the connection and stops it being
// re-discovered for the rest of this connection (see
// `Device.RemoveConnectedProvider`). When it is the selected one the selection
// moves to the nearest provider up front, rather than after the window round
// trip, so the ui never blinks through "nothing selected".
//
// The caller still trims its own row list optimistically: that is a rendering
// choice, and the sdk only reports the removal once the window drops the
// client.
func (self *ProviderLocationsViewController) RemoveProvider(clientId string) {
	if clientId == "" {
		return
	}
	id, err := ParseId(clientId)
	if err != nil {
		// a malformed id removes nothing, so it must not move the selection
		deviceLog(self.device).Info("[plvc]remove bad client id %s", clientId)
		return
	}
	self.selectSuccessorTo(clientId)
	// outside the lock: this can re-enter through the window change listener
	self.device.RemoveConnectedProvider(id)
}

// selectSuccessorTo hands the selection to the nearest other provider when
// `clientId` is the one selected, ahead of the window reporting it gone. A
// no-op for any other provider.
func (self *ProviderLocationsViewController) selectSuccessorTo(clientId string) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.selectedClientId != clientId {
			return
		}
		// every other provider in the window is still connected
		next := self.window.successorTo(clientId, func(other string) bool {
			return other != clientId
		})
		if next != self.selectedClientId {
			self.selectedClientId = next
			changed = true
		}
	}()
	if changed {
		self.selectedProviderLocationChanged()
	}
}

// GetProviderLocations returns the connected providers in display order — the
// same providers `Device.GetConnectedProviderLocations` reports, ordered west
// to east about their centroid with the ones that have no coordinates after
// them. Apps render their list from this rather than from the device, so the
// rows read left to right in the order the globe steps through, and the order
// can never disagree with the selection: both come from one window snapshot.
//
// Re-read it on `ConnectedProviderLocationChangeListener`. This controller
// subscribes to that same listener when it is opened, and `connect.CallbackList`
// fires callbacks in subscription order, so the snapshot an app reads from its
// own callback is already the new one — PROVIDED the app opens the controller
// BEFORE it registers its own connected-provider listener. An app that
// registers first reads a window one notify behind (the next notify corrects
// it); this is not derived on read because `setLocations` needs the previous
// window to find where a departing provider was.
func (self *ProviderLocationsViewController) GetProviderLocations() *ConnectedProviderLocationList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	locations := NewConnectedProviderLocationList()
	locations.addAll(self.window.order...)
	return locations
}

// GetSelectedClientId returns the selected provider's egress client id, or ""
// when nothing is selected.
func (self *ProviderLocationsViewController) GetSelectedClientId() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.selectedClientId
}

// SetSelectedClientId selects a provider explicitly (a tap on a dot or a list
// row). "" falls back to the longest connected provider rather than clearing:
// with providers connected, the screen always has one selected.
func (self *ProviderLocationsViewController) SetSelectedClientId(clientId string) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		next := clientId
		if next == "" {
			next = self.window.longestConnected()
		}
		if next != self.selectedClientId {
			self.selectedClientId = next
			changed = true
		}
	}()
	if changed {
		self.selectedProviderLocationChanged()
	}
}

// StepSelection moves the selection `steps` providers along the wheel:
// positive steps east, negative west, clamped at the wheel's ends. With no
// plottable selection yet, the first step lands on the wheel's west end.
// Sticking at an end is not a change, so no update is reported for it.
func (self *ProviderLocationsViewController) StepSelection(steps int32) {
	if steps == 0 {
		return
	}
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		next, ok := stepWheelSelection(self.window.wheel, self.selectedClientId, int(steps))
		if ok {
			self.selectedClientId = next
			changed = true
		}
	}()
	if changed {
		self.selectedProviderLocationChanged()
	}
}

func (self *ProviderLocationsViewController) selectedProviderLocationChanged() {
	for _, listener := range self.selectedListeners.Get() {
		connect.HandleError(func() {
			listener.SelectedProviderLocationChanged()
		})
	}
}

func (self *ProviderLocationsViewController) AddSelectedProviderLocationChangeListener(listener SelectedProviderLocationChangeListener) Sub {
	callbackId := self.selectedListeners.Add(listener)
	return newSub(func() {
		self.selectedListeners.Remove(callbackId)
	})
}

func (self *ProviderLocationsViewController) Start() {}

func (self *ProviderLocationsViewController) Stop() {}

func (self *ProviderLocationsViewController) Close() {
	deviceLog(self.device).Info("[plvc]close")

	self.cancel()
	self.locationsChangedSub.Close()
}

// deriveProviderWindow maps the current window locations to what the screen
// needs: the providers in display order, their positions in it, the wheel, and
// the longest connected provider.
//
// Display order is the plottable providers sorted by signed longitude offset
// from their spherical centroid, then the ones with no coordinates. Both sorts
// are stable, so providers sharing a longitude — and every unplottable one —
// keep the sdk's oldest-connected-first order.
func deriveProviderWindow(locations *ConnectedProviderLocationList) providerWindow {
	plottable := []*ConnectedProviderLocation{}
	unplottable := []*ConnectedProviderLocation{}
	lonLats := [][2]float64{}
	longest := ""
	seen := map[string]bool{}
	// both device implementations return an empty list rather than nil, but a
	// nil here would be a crash rather than an empty globe
	if locations != nil {
		for i := 0; i < locations.Len(); i += 1 {
			location := locations.Get(i)
			if location == nil || location.ClientId == nil {
				continue
			}
			clientId := location.ClientId.String()
			if seen[clientId] {
				// the same provider twice would give the selection two homes
				continue
			}
			seen[clientId] = true
			if longest == "" {
				// the sdk sorts the window oldest-connected first
				longest = clientId
			}
			if lat, lon, ok := plotCoordinates(location); ok {
				plottable = append(plottable, location)
				lonLats = append(lonLats, [2]float64{lon, lat})
			} else {
				unplottable = append(unplottable, location)
			}
		}
	}

	centroidLon := centroidLongitude(lonLats)
	slices.SortStableFunc(plottable, func(a, b *ConnectedProviderLocation) int {
		_, aLon, _ := plotCoordinates(a)
		_, bLon, _ := plotCoordinates(b)
		return cmp.Compare(
			signedLonDelta(aLon, centroidLon),
			signedLonDelta(bLon, centroidLon),
		)
	})

	order := make([]*ConnectedProviderLocation, 0, len(plottable)+len(unplottable))
	order = append(order, plottable...)
	order = append(order, unplottable...)
	index := map[string]int{}
	wheel := make([]string, len(plottable))
	for i, location := range order {
		clientId := location.ClientId.String()
		index[clientId] = i
		if i < len(plottable) {
			wheel[i] = clientId
		}
	}
	return providerWindow{order: order, index: index, wheel: wheel, longest: longest}
}

// plotCoordinates is the provider's dot position on the globe: the city
// centroid when known, else the region centroid. ok is false when the
// provider has neither — it is listed but never plotted.
func plotCoordinates(location *ConnectedProviderLocation) (lat float64, lon float64, ok bool) {
	if location.HasCityCoordinates {
		return location.CityLat, location.CityLon, true
	}
	if location.HasRegionCoordinates {
		return location.RegionLat, location.RegionLon, true
	}
	return 0, 0, false
}

// stepWheelSelection resolves one scroll step: the client id selected after
// moving `steps` from the current selection, clamped at the wheel's ends.
// ok is false when nothing changes — an empty wheel, or a step that only
// runs into the end it is already at.
func stepWheelSelection(wheel []string, selectedClientId string, steps int) (string, bool) {
	if len(wheel) == 0 {
		return "", false
	}
	// "" or a selection without coordinates is not on the wheel: -1
	current := slices.Index(wheel, selectedClientId)
	var next int
	if current < 0 {
		// the first step lands on the wheel's west end
		next = 0
	} else {
		next = clampWheelIndex(current, steps, len(wheel))
	}
	if next == current {
		return "", false
	}
	return wheel[next], true
}

// clampWheelIndex advances an index by `steps`, sticking at both ends. The
// wheel is cut at the meridian opposite the providers' centroid (see
// `centroidLongitude`), so its ends are the extreme west and east of the
// providers as seen from their center: stepping past an end stays there
// instead of teleporting the long way round the globe. Returns -1 for an
// empty wheel.
func clampWheelIndex(index int, steps int, count int) int {
	if count <= 0 {
		return -1
	}
	return min(max(index+steps, 0), count-1)
}

// centroidLongitude is the longitude, in degrees [-180, 180], of the
// spherical centroid of the given {lon, lat} points: unit vectors summed,
// the horizontal direction of the sum taken. Latitude only weights — a point
// near a pole pulls the centroid's longitude less than one near the equator,
// matching how little its own longitude means there. An empty or perfectly
// balanced set (a zero sum, where no direction is meaningful) resolves to 0
// deterministically (atan2(0, 0) == 0).
func centroidLongitude(lonLats [][2]float64) float64 {
	x := 0.0
	y := 0.0
	for _, lonLat := range lonLats {
		lambda := lonLat[0] * math.Pi / 180
		phi := lonLat[1] * math.Pi / 180
		x += math.Cos(phi) * math.Cos(lambda)
		y += math.Cos(phi) * math.Sin(lambda)
	}
	return math.Atan2(y, x) * 180 / math.Pi
}

// angularDistance is the great-circle distance between two {lat, lon} points
// given in degrees, as radians of arc — a pure ranking quantity here, so the
// earth's radius never enters. Haversine rather than acos of the unit-vector
// dot product: two providers in neighboring cities are a very small angle
// apart, exactly where acos loses its precision.
func angularDistance(latDeg1 float64, lonDeg1 float64, latDeg2 float64, lonDeg2 float64) float64 {
	lat1 := latDeg1 * math.Pi / 180
	lat2 := latDeg2 * math.Pi / 180
	halfDeltaLat := (lat2 - lat1) / 2
	halfDeltaLon := (lonDeg2 - lonDeg1) * math.Pi / 360
	sinHalfLat := math.Sin(halfDeltaLat)
	sinHalfLon := math.Sin(halfDeltaLon)
	a := sinHalfLat*sinHalfLat + math.Cos(lat1)*math.Cos(lat2)*sinHalfLon*sinHalfLon
	// rounding can push a just past 1 for antipodal points, where Asin is NaN
	return 2 * math.Asin(math.Sqrt(min(1, a)))
}

// signedLonDelta is the signed east-west offset of `lonDeg` from
// `referenceDeg`, normalized to [-180, 180): negative is west of the
// reference, positive east. Sorting providers by this offset from their
// `centroidLongitude` is the wheel order — west to east as seen from the
// providers' center, with the cut (the wheel's two ends) on the meridian
// opposite the centroid, the farthest place from the data. A point exactly
// opposite the reference lands at -180, the far-west end.
func signedLonDelta(lonDeg float64, referenceDeg float64) float64 {
	delta := math.Mod(lonDeg-referenceDeg, 360)
	if delta < -180 {
		delta += 360
	} else if delta >= 180 {
		delta -= 360
	}
	return delta
}
