package main

import (
	"go/types"
	"strings"
	"testing"
)

func TestSessionBindingsExposeTypedSnapshotsAndApiOnlyController(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{"SessionLastUsed", "NetworkSessionInfo", "NetworkSessionInfoList", "NetworkSessionsResult", "NetworkSessionsRevision", "SessionOperationResult", "ClientSessionSnapshot", "ClientSessionAction", "ClientSessionError", "ClientSessionViewController"} {
		g.emitType(testingPreferenceType(t, g, name))
		if !behavioralTypes[name] {
			t.Fatalf("%s lost its typed handle boundary", name)
		}
	}
	listener := testingPreferenceType(t, g, "ClientSessionListener").Type().(*types.Named)
	callback, err := g.callback(listener)
	if err != nil {
		t.Fatal(err)
	}
	if len(callback.methods) != 1 || callback.methods[0].params[0].info.kind != kindHandle || strings.Contains(callback.methods[0].adapter, "cJson(") {
		t.Fatal("session listener reduced typed snapshot to application-parsed JSON")
	}
	for _, getter := range []string{"GetUnixTime", "GetCity", "GetRegion", "GetCountry", "GetCountryCode", "GetDeviceType", "GetAppVersion"} {
		_ = testingPreferenceExport(t, g, "SessionLastUsed", getter)
	}
	g.emitType(testingPreferenceType(t, g, "Api"))
	_ = testingPreferenceExport(t, g, "Api", "OpenClientSessionViewController")
	for _, getter := range []string{"GetLoaded", "GetLoading", "GetRefreshing", "GetActions", "GetBulkAction", "GetCurrentSessionId", "GetLegacyCoverage", "GetSessions"} {
		_ = testingPreferenceExport(t, g, "ClientSessionSnapshot", getter)
	}
	for _, method := range []string{"Start", "Close", "Refresh", "RevokeSession", "RevokeOtherSessions", "SetVisible", "SetForeground"} {
		_ = testingPreferenceExport(t, g, "ClientSessionViewController", method)
	}
}
