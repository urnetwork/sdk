package sdk

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Every distribution the sdk exports, with a device type it pairs with: what
// each app build passes to NewClientInfoWithDistribution.
var testClientDistributionDeviceTypes = []struct {
	distribution string
	deviceType   string
}{
	{distribution: ClientDistributionAppStore, deviceType: "ios"},
	{distribution: ClientDistributionAppStore, deviceType: "macos"},
	{distribution: ClientDistributionMacDirect, deviceType: "macos"},
	{distribution: ClientDistributionPlay, deviceType: "android"},
	{distribution: ClientDistributionAndroidDirect, deviceType: "android"},
	{distribution: ClientDistributionSolanaDapp, deviceType: "android"},
	{distribution: ClientDistributionEthosDapp, deviceType: "android"},
	{distribution: ClientDistributionWindows, deviceType: "windows"},
	{distribution: ClientDistributionLinux, deviceType: "linux"},
	{distribution: ClientDistributionLinuxAppimage, deviceType: "linux"},
	{distribution: ClientDistributionLinuxFlatpak, deviceType: "linux"},
	{distribution: ClientDistributionWeb, deviceType: "web"},
	{distribution: ClientDistributionExtensionChrome, deviceType: "web"},
	{distribution: ClientDistributionExtensionFirefox, deviceType: "web"},
}

// The api keeps each build's distribution with its device type, and puts it in
// what every request and the connect auth send (connectClientInfo).
func TestClientInfoKeepsEachDistribution(t *testing.T) {
	_, api := newTestApiForURL(t, "http://127.0.0.1:1")
	for _, c := range testClientDistributionDeviceTypes {
		api.SetClientInfo(NewClientInfoWithDistribution(c.deviceType, "2026.11.11-1", c.distribution))
		info := api.GetClientInfo()
		if info.Distribution != c.distribution || info.DeviceType != c.deviceType || info.AppVersion != "2026.11.11-1" || info.SdkVersion != Version || info.Version != 1 {
			t.Errorf("%s/%s: %+v", c.deviceType, c.distribution, info)
		}
		if sent := api.connectClientInfo(); sent.Distribution != c.distribution || sent.DeviceType != c.deviceType {
			t.Errorf("%s/%s: sends %+v", c.deviceType, c.distribution, sent)
		}
	}
}

// An app that names no distribution reports it unknown, as every app did
// before the field; an unknown value, or one that does not pair with the
// device type, is dropped and the rest is kept.
func TestClientInfoUnknownDistributionIsDropped(t *testing.T) {
	_, api := newTestApiForURL(t, "http://127.0.0.1:1")
	cases := []struct {
		info       *ClientInfo
		deviceType string
	}{
		{info: NewClientInfo("macos", "1"), deviceType: "macos"},
		{info: NewClientInfoWithDistribution("macos", "1", ""), deviceType: "macos"},
		{info: NewClientInfoWithDistribution("macos", "1", "dmg"), deviceType: "macos"},
		{info: NewClientInfoWithDistribution("ios", "1", ClientDistributionMacDirect), deviceType: "ios"},
		{info: NewClientInfoWithDistribution("android", "1", ClientDistributionAppStore), deviceType: "android"},
		{info: NewClientInfoWithDistribution("windows", "1", ClientDistributionLinux), deviceType: "windows"},
	}
	for _, c := range cases {
		api.SetClientInfo(c.info)
		if info := api.GetClientInfo(); info.Distribution != "" || info.DeviceType != c.deviceType || info.AppVersion != "1" {
			t.Errorf("%+v: %+v", c.info, info)
		}
	}
}

// The sign-up request carries the build's distribution in X-UR-ClientInfo:
// the server records the account's onboarding platform from it, and it is
// all that tells the App Store Mac from the direct-download Mac.
func TestClientInfoDistributionReachesTheSignUpRequest(t *testing.T) {
	observed := make(chan connect.ClientInfo, 1)
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/network-create" {
			select {
			case observed <- connect.ClientInfoFromHeader(r.Header):
			default:
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	api.SetClientInfo(NewClientInfoWithDistribution("macos", "2026.11.11-1", ClientDistributionMacDirect))
	done := make(chan error, 1)
	api.NetworkCreate(&NetworkCreateArgs{UserName: "synthetic", Terms: true}, connect.NewApiCallback(func(_ *NetworkCreateResult, err error) {
		done <- err
	}))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("network create did not return")
	}
	select {
	case info := <-observed:
		if info != (connect.ClientInfo{Version: 1, DeviceType: "macos", AppVersion: "2026.11.11-1", SdkVersion: Version, Distribution: ClientDistributionMacDirect}) {
			t.Fatal(info)
		}
	default:
		t.Fatal("the sign-up request did not reach the server")
	}
}

// The web sdk's distribution table (js/src/client_info.ts), which the
// extension builds its X-UR-ClientInfo from, names exactly these values.
func TestClientDistributionTsMatchesGo(t *testing.T) {
	source, err := os.ReadFile("js/src/client_info.ts")
	if err != nil {
		t.Fatal(err)
	}
	table := regexp.MustCompile(`(?s)export const ClientDistribution = \{(.*?)\} as const;`).FindSubmatch(source)
	if table == nil {
		t.Fatal("js/src/client_info.ts has no ClientDistribution table")
	}
	tsValues := []string{}
	for _, match := range regexp.MustCompile(`\w+: "([a-z_]+)",`).FindAllSubmatch(table[1], -1) {
		tsValues = append(tsValues, string(match[1]))
	}
	goValues := []string{}
	for _, c := range testClientDistributionDeviceTypes {
		if !slices.Contains(goValues, c.distribution) {
			goValues = append(goValues, c.distribution)
		}
	}
	slices.Sort(tsValues)
	slices.Sort(goValues)
	if !slices.Equal(tsValues, goValues) {
		t.Fatalf("ts %v, go %v", tsValues, goValues)
	}
}
