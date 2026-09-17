//go:build ios || android || js

package sdk

import (
	"context"
	"reflect"
	"testing"
)

// The builds that carry no extender role (EXTENDER.md G1, N2). Everything the
// provider extender surface answers on a phone or in the browser is the
// unsupported status, so an app hides the row rather than drawing a toggle
// that starts nothing (N1).
//
// The shared half of this is pinned on every platform by
// TestExtenderProvideStubAnswersTheUnsupportedStatus; this is the stub itself.
func TestExtenderProvideMobileStubIsUnsupported(t *testing.T) {
	if extenderProvideSupported {
		t.Fatal("a build with no extender role reported the role supported")
	}

	// the role is never built here, whatever it is asked for
	extender, err := newDeviceLocalExtender(context.Background(), &deviceLocalExtenderSettings{})
	if extender != nil || err == nil {
		t.Fatal("a build with no extender role built one")
	}
	if extender.statusUpdate() != nil {
		t.Fatal("a build with no extender role published status updates")
	}

	// and what it reports is the unsupported status, whatever the setting and
	// the provide state say
	status := extender.status().withState(true, true)
	if status.Supported {
		t.Fatal("the stub reported the role supported")
	}
	if status.State != ExtenderProvideStateOff || status.ErrorCase != "" {
		t.Fatalf("state = %q, %q, expected off", status.State, status.ErrorCase)
	}
	if !reflect.DeepEqual(status, unsupportedExtenderProvideStatus()) {
		t.Fatalf("the stub's status is %+v", status)
	}
}

// No role, so nothing is relayed and there is no series to show (O2).
func TestExtenderProvideMobileStubReportsNoStats(t *testing.T) {
	extender, err := newDeviceLocalExtender(context.Background(), &deviceLocalExtenderSettings{})
	if extender != nil || err == nil {
		t.Fatal("a build with no extender role built one")
	}
	if stats := extender.stats(); stats != nil {
		t.Fatalf("the stub reported stats %+v", stats)
	}
}
