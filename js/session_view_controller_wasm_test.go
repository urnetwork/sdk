//go:build js

package main

import (
	"github.com/urnetwork/sdk"
	"syscall/js"
	"testing"
)

func TestSessionWasmSnapshotHasTypedListsAndMetadata(t *testing.T) {
	list := sdk.NewNetworkSessionInfoList()
	list.Add(&sdk.NetworkSessionInfo{SessionId: sdk.NewId(), LastUsed: &sdk.SessionLastUsed{UnixTime: 1700000000, City: "Chicago", Region: "Illinois", Country: "United States", CountryCode: "us", DeviceType: "android", AppVersion: "1.2.3"}})
	list.Add(&sdk.NetworkSessionInfo{SessionId: sdk.NewId()})
	snapshot := &sdk.ClientSessionSnapshot{Sessions: list, Actions: sdk.NewClientSessionActionList(), Loaded: true, LegacyCoverage: "partial"}
	value := jsJson(snapshot)
	if !value.Get("loaded").Bool() || value.Get("sessions").Length() != 2 || value.Get("Sessions").Type() != js.TypeUndefined {
		t.Fatal("typed snapshot shape differs from generated declaration")
	}
	observed := value.Get("sessions").Index(0).Get("last_used")
	if observed.Get("unix_time").Int() != 1700000000 || observed.Get("city").String() != "Chicago" || observed.Get("region").String() != "Illinois" || observed.Get("country").String() != "United States" || observed.Get("country_code").String() != "us" || observed.Get("device_type").String() != "android" || observed.Get("app_version").String() != "1.2.3" {
		t.Fatal("last-use metadata was lost")
	}
	if !value.Get("sessions").Index(1).Get("last_used").IsNull() {
		t.Fatal("unknown use was fabricated")
	}
}

func TestSessionWasmErrorCarriesTheRevokedSessionFlag(t *testing.T) {
	for _, revoked := range []bool{true, false} {
		snapshot := &sdk.ClientSessionSnapshot{Sessions: sdk.NewNetworkSessionInfoList(), Actions: sdk.NewClientSessionActionList(), Error: &sdk.ClientSessionError{SignInRequired: true, SessionRevoked: revoked}}
		value := jsJson(snapshot).Get("error")
		if !value.Get("sign_in_required").Bool() || value.Get("session_revoked").Type() != js.TypeBoolean || value.Get("session_revoked").Bool() != revoked || value.Get("SessionRevoked").Type() != js.TypeUndefined {
			t.Fatal("the session error's revoked flag differs from the generated declaration")
		}
	}
}
