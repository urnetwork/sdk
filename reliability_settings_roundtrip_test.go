package sdk

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Every connect reliability field crosses the sdk struct and back unchanged.
// GetReliabilitySettings -> change a field -> SetReliabilitySettings replaces
// the multi client's whole override, so a field the sdk struct does not carry
// is turned off by any write through it. The hosted proxy writes its
// evaluation pool multiple this way, and the write turned off every hosted
// window's outcome watchdog, its uplink staleness hold and its light
// classifier.
func TestReliabilitySettingsRoundTripCarriesEveryConnectField(t *testing.T) {
	settings := &connect.ReliabilitySettings{}
	value := reflect.ValueOf(settings).Elem()
	durationType := reflect.TypeOf(time.Duration(0))
	for i := range value.NumField() {
		field := value.Field(i)
		// distinct per field, and whole milliseconds, the sdk's duration unit
		seed := int64(i + 1)
		switch {
		case field.Type() == durationType:
			field.SetInt(int64(time.Duration(seed) * time.Millisecond))
		case field.Kind() == reflect.Bool:
			field.SetBool(true)
		case field.CanInt():
			field.SetInt(seed)
		case field.Kind() == reflect.Float64:
			field.SetFloat(float64(seed) + 0.5)
		default:
			t.Fatalf("connect.ReliabilitySettings.%s is a %s, which this test cannot fill", value.Type().Field(i).Name, field.Type())
		}
	}

	roundTrip := reflect.ValueOf(reliabilitySettingsFromConnect(settings).toConnect()).Elem()
	for i := range value.NumField() {
		want := value.Field(i).Interface()
		if got := roundTrip.Field(i).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("connect.ReliabilitySettings.%s = %v after the sdk round trip, want %v", value.Type().Field(i).Name, got, want)
		}
	}
}

// The shipped connect defaults survive the round trip, among them the window
// outcome watchdog (a silent rebuild after 45 s with no provider added, the
// failed latch 45 s later), the 60 s uplink staleness hold and the light
// classifier.
func TestReliabilitySettingsRoundTripKeepsTheConnectDefaults(t *testing.T) {
	defaults := connect.ReliabilitySettingsFrom(connect.DefaultMultiClientSettings())
	if defaults.WindowOutcomeDeadline <= 0 || defaults.WindowOutcomeRebuildDeadline <= 0 || defaults.UplinkStalenessMaxHold <= 0 || !defaults.LightClassifier {
		t.Fatalf("the connect defaults no longer turn on the watchdog, the uplink hold and the light classifier: %+v", defaults)
	}
	connect.AssertEqual(t, reliabilitySettingsFromConnect(defaults).toConnect(), defaults)
}
