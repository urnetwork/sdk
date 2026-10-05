package main

import (
	"testing"

	"github.com/urnetwork/sdk"
)

// An export whose result is an error id answers "" when the call succeeded,
// so it can never answer NULL: the c++ wrapper takes NULL as "", and the apps
// read "" as saved or valid. A call that could not run answers
// URNET_ERROR_ID_INTERNAL. The id is written out here because it is abi: the
// apps mirror the literal (gen/gen.go errorIdInternal).
const testingErrorIdInternal = "internal_error"

// errorIdAnswer calls an error-id export with callExportNil's arguments and
// reads its answer.
func errorIdAnswer(t *testing.T, fn any, args ...any) string {
	t.Helper()
	answer := callExportNil(t, fn, args...)[0]
	if answer.IsNil() {
		t.Fatal("the export answered NULL, which the c++ wrapper reads as \"\", success")
	}
	text := goStringAt(answer.UnsafePointer())
	callExportNil(t, urnet_free_string, answer.UnsafePointer())
	return text
}

// The check of one url answers the sdk's own verdict: "" for a url the space
// can query and the refusal's id otherwise.
func TestErrorIdExportAnswersTheSdkVerdict(t *testing.T) {
	for _, row := range []struct {
		dohUrl  string
		errorId string
	}{
		{dohUrl: "https://223.5.5.5/dns-query", errorId: ""},
		{dohUrl: "http://223.5.5.5/dns-query", errorId: sdk.ControlDohErrorHttpsRequired},
	} {
		dohUrl := cString(row.dohUrl)
		got := errorIdAnswer(t, urnet_validate_control_doh_url, dohUrl)
		cStringFree(dohUrl)
		if got != row.errorId {
			t.Errorf("%s answered %q, want %q", row.dohUrl, got, row.errorId)
		}
	}
}

// Settings json that does not decode is never checked, so it must not read
// as settings that validate.
func TestErrorIdExportAnswersInternalErrorForJsonThatDoesNotDecode(t *testing.T) {
	settings := cString(`{"enabled": true, "port": "443"`)
	defer cStringFree(settings)
	if got := errorIdAnswer(t, urnet_validate_vless_settings, settings); got != testingErrorIdInternal {
		t.Errorf("validate_vless_settings answered %q for json that does not decode", got)
	}

	space := newHandle(&sdk.NetworkSpace{})
	defer handleRelease(space)
	if got := errorIdAnswer(t, urnet_network_space_set_vless_settings, space, settings); got != testingErrorIdInternal {
		t.Errorf("set_vless_settings answered %q for json that does not decode", got)
	}
	dohUrls := cString(`["https://223.5.5.5/dns-query"`)
	defer cStringFree(dohUrls)
	if got := errorIdAnswer(t, urnet_network_space_set_control_doh_urls, space, dohUrls); got != testingErrorIdInternal {
		t.Errorf("set_control_doh_urls answered %q for json that does not decode", got)
	}
}

// A space handle that does not resolve saves nothing: the null handle, and one
// that was never issued.
func TestErrorIdExportAnswersInternalErrorForAHandleThatDoesNotResolve(t *testing.T) {
	const unknown = uint64(1) << 62
	for _, space := range []uint64{0, unknown} {
		if got := errorIdAnswer(t, urnet_network_space_set_vless_settings, space, nil); got != testingErrorIdInternal {
			t.Errorf("set_vless_settings on space handle %d answered %q", space, got)
		}
		if got := errorIdAnswer(t, urnet_network_space_set_control_doh_urls, space, nil); got != testingErrorIdInternal {
			t.Errorf("set_control_doh_urls on space handle %d answered %q", space, got)
		}
	}
}

// A panic in the sdk call is recovered so it cannot unwind into C, and what
// it answers is the internal id, not the zero result. A nil space behind a
// live handle panics on its first field read.
func TestErrorIdExportAnswersInternalErrorForARecoveredPanic(t *testing.T) {
	space := newHandle((*sdk.NetworkSpace)(nil))
	defer handleRelease(space)
	if got := errorIdAnswer(t, urnet_network_space_set_vless_settings, space, nil); got != testingErrorIdInternal {
		t.Errorf("set_vless_settings answered %q after a panic", got)
	}
	if got := errorIdAnswer(t, urnet_network_space_set_control_doh_urls, space, nil); got != testingErrorIdInternal {
		t.Errorf("set_control_doh_urls answered %q after a panic", got)
	}
}
