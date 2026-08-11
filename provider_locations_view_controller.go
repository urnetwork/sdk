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

// ProviderLocationsViewController owns the provider-locations globe's
// selection and its scroll wheel, so every app shares one behavior instead of
// hand-rolling it per platform.
//
// The wheel is the plottable providers (the ones with coordinates) ordered
// west to east by longitude relative to the providers' spherical centroid:
// each provider is ranked by its signed longitude offset from the centroid,
// so the wheel's two ends fall on the meridian opposite the data's center —
// the farthest place from the providers. A raw longitude sort would instead
// cut at the +/-180 antimeridian and split a cluster that straddles it (a
// provider at -178 would sort to the far west end even though it sits just
// east of one at +175).
//
// Scrolling does not cycle: `StepSelection` clamps at the wheel's ends, so
// stepping past the extreme west or east sticks there instead of teleporting
// the long way round the globe. The apps translate their gestures (drag
// travel past a hysteresis threshold, scroll-wheel notches) into step counts;
// everything after that — order, clamping, selection — lives here.
//
// The selection is the egress client id as a string. It is not restricted to
// plottable providers (the list lets the user select a row with no
// coordinates); a step from such a selection starts at the wheel's west end.
// Two rules keep it pointed at something real, so the screen never rests on
// nothing while providers are connected:
//
//   - with nothing selected, the LONGEST CONNECTED provider is selected (the
//     window's first entry — the sdk sorts it oldest-connected first);
//   - when the selected provider leaves the window (the user removed it, or it
//     rotated out at the window's client lifetime), the selection moves to the
//     nearest provider connected LONGER than it, falling back to the nearest
//     younger one. Handing it to an older neighbor keeps the selection on a row
//     that has been there all along rather than on a newcomer.
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

// providerWindow is the current connect window as the selection needs it: the
// providers in the sdk's order, the set for membership tests, and the globe's
// wheel.
type providerWindow struct {
	// every provider's egress client id, longest connected first
	order []string
	// the same ids as a set, for "is this provider still connected"
	clientIds map[string]bool
	// the plottable providers, west to east about their centroid (see the
	// ProviderLocationsViewController comment)
	wheel []string
}

// longestConnected is the provider that has been connected the longest — the
// window's first entry — or "" when nothing is connected.
func (self providerWindow) longestConnected() string {
	if len(self.order) == 0 {
		return ""
	}
	return self.order[0]
}

// successorTo is who inherits the selection when `clientId` goes away: the
// nearest provider connected longer than it that `present` still reports, else
// the nearest younger one. "" when nothing survives or the id is not in this
// window.
func (self providerWindow) successorTo(clientId string, present func(string) bool) string {
	index := slices.Index(self.order, clientId)
	if index < 0 {
		return ""
	}
	for i := index - 1; 0 <= i; i -= 1 {
		if present(self.order[i]) {
			return self.order[i]
		}
	}
	for i := index + 1; i < len(self.order); i += 1 {
		if present(self.order[i]) {
			return self.order[i]
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
// real: an older neighbor when the selected provider has left, the longest
// connected provider when nothing is selected. Split from the listener so the
// state transition is exercised directly by tests, with no device to stand up.
func (self *ProviderLocationsViewController) setLocations(locations *ConnectedProviderLocationList) {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := self.window
		self.window = deriveProviderWindow(locations)
		next := self.selectedClientId
		if next != "" && !self.window.clientIds[next] {
			// removed by the user, or rotated out of the window
			next = previous.successorTo(next, func(clientId string) bool {
				return self.window.clientIds[clientId]
			})
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
// moves to the next older provider up front, rather than after the window
// round trip, so the ui never blinks through "nothing selected".
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

// selectSuccessorTo hands the selection to the next older provider when
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

// deriveProviderWindow maps the current window locations to what the selection
// needs: the providers in the sdk's order (longest connected first), the same
// ids as a set, and the wheel — the plottable providers ordered west to east
// by signed longitude offset from their spherical centroid. The wheel sort is
// stable, so providers sharing a longitude keep the list's oldest-first
// duration order.
func deriveProviderWindow(locations *ConnectedProviderLocationList) providerWindow {
	type plot struct {
		clientId string
		lon      float64
		lat      float64
	}

	order := []string{}
	clientIds := map[string]bool{}
	plots := []plot{}
	if locations == nil {
		// both device implementations return an empty list rather than nil, but
		// a nil here would be a crash rather than an empty globe
		return providerWindow{order: order, clientIds: clientIds, wheel: []string{}}
	}
	for i := 0; i < locations.Len(); i += 1 {
		location := locations.Get(i)
		if location == nil || location.ClientId == nil {
			continue
		}
		clientId := location.ClientId.String()
		if clientIds[clientId] {
			// the same provider twice would give the selection two homes
			continue
		}
		order = append(order, clientId)
		clientIds[clientId] = true
		if lat, lon, ok := plotCoordinates(location); ok {
			plots = append(plots, plot{clientId: clientId, lon: lon, lat: lat})
		}
	}

	lonLats := make([][2]float64, len(plots))
	for i, p := range plots {
		lonLats[i] = [2]float64{p.lon, p.lat}
	}
	centroidLon := centroidLongitude(lonLats)
	slices.SortStableFunc(plots, func(a, b plot) int {
		return cmp.Compare(
			signedLonDelta(a.lon, centroidLon),
			signedLonDelta(b.lon, centroidLon),
		)
	})

	wheel := make([]string, len(plots))
	for i, p := range plots {
		wheel[i] = p.clientId
	}
	return providerWindow{order: order, clientIds: clientIds, wheel: wheel}
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
