package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The api types are the wire contract the apps read; these pin the json
// names to what the server sends (server/model, server/controller) for the
// fields whose tags once drifted.

func TestFindProviders2DecodeMatchesServer(t *testing.T) {
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// a discovered provider, and a fixed client-id spec provider: neither
		// carries intermediary_ids, which the server omits (reserved)
		fmt.Fprint(w, `{"providers": [
			{
				"client_id": "00000000-0000-0000-0000-000000000041",
				"estimated_bytes_per_second": 5000000,
				"has_estimated_bytes_per_second": true,
				"tier": 2,
				"network_only": true,
				"reputation_failed_names": "bloomberg",
				"ip_family": "dualstack",
				"location": {
					"country": "United States", "country_code": "us",
					"region": "California", "city": "Palo Alto",
					"city_coordinates": {"lat": 37.44, "lon": -122.14}
				}
			},
			{"client_id": "00000000-0000-0000-0000-000000000042", "estimated_bytes_per_second": 0, "has_estimated_bytes_per_second": false, "tier": 0}
		]}`)
	})

	callback, c := connect.NewBlockingApiCallback[*FindProviders2Result](context.Background())
	api.FindProviders2(&FindProviders2Args{Specs: NewProviderSpecList(), Count: 2}, callback)
	r := awaitApiResult(t, c, "FindProviders2 never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if r.Result.ProviderStats == nil || r.Result.ProviderStats.Len() != 2 {
		t.Fatalf("providers = %+v", r.Result.ProviderStats)
	}
	p := r.Result.ProviderStats.Get(0)
	if !p.HasEstimatedBytesPerSecond || p.Tier != 2 || p.IpFamily != IpFamilyDualstack || !p.NetworkOnly {
		t.Errorf("provider = %+v", p)
	}
	if p.IntermediaryIds != nil {
		t.Errorf("intermediary_ids should be absent, got %+v", p.IntermediaryIds)
	}
	if p.Location == nil || p.Location.City != "Palo Alto" || p.Location.CityCoordinates == nil ||
		p.Location.CityCoordinates.Lat != 37.44 || p.Location.RegionCoordinates != nil {
		t.Errorf("location = %+v", p.Location)
	}
	fixed := r.Result.ProviderStats.Get(1)
	if fixed.Location != nil || fixed.IpFamily != "" || fixed.Tier != 0 {
		t.Errorf("fixed provider = %+v", fixed)
	}
}

func TestFindProviders2ArgsEncodeMatchesServer(t *testing.T) {
	hop := NewIdList()
	hop.Add(mustParseTestId(t, "00000000-0000-0000-0000-000000000051"))
	hop.Add(mustParseTestId(t, "00000000-0000-0000-0000-000000000052"))
	excludeDestinations := NewMultiHopIdList()
	excludeDestinations.Add(hop)
	args := &FindProviders2Args{
		Specs:               NewProviderSpecList(),
		Count:               3,
		ForceCount:          true,
		ExcludeClientIds:    NewIdList(),
		ExcludeDestinations: excludeDestinations,
		RankMode:            RankModeSpeed,
		IpFamily:            IpFamilyFilterV6Capable,
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["force_count"] != true || wire["rank_mode"] != "speed" || wire["ip_family"] != "v6-capable" {
		t.Errorf("wire = %s", b)
	}
	dests, _ := wire["exclude_destinations"].([]any)
	if len(dests) != 1 {
		t.Fatalf("exclude_destinations = %s", b)
	}
	if ids, _ := dests[0].([]any); len(ids) != 2 || ids[1] != "00000000-0000-0000-0000-000000000052" {
		t.Errorf("exclude_destinations[0] = %v", dests[0])
	}
}

func TestNetworkUserDecodeUserId(t *testing.T) {
	var user NetworkUser
	if err := json.Unmarshal([]byte(`{"user_id": "00000000-0000-0000-0000-000000000061", "network_name": "n", "auth_types": ["password"]}`), &user); err != nil {
		t.Fatal(err)
	}
	if user.UserId == nil || user.UserId.String() != "00000000-0000-0000-0000-000000000061" {
		t.Errorf("user id = %v (the wire name is user_id)", user.UserId)
	}
}

func TestProxyConfigResultDecodeMatchesServer(t *testing.T) {
	var result AuthNetworkClientResult
	err := json.Unmarshal([]byte(`{
		"by_client_jwt": "jwt",
		"proxy_config_result": {
			"keepalive_seconds": 30,
			"proxy_id": "00000000-0000-0000-0000-000000000071",
			"api_base_url": "https://proxy.example",
			"api_port": 8443,
			"socks_proxy_port": 1080,
			"http_proxy_port": 8080,
			"wg_config": {"wg_proxy_port": 51820, "client_ipv4": "10.0.0.2", "config": "[Interface]"}
		}
	}`), &result)
	if err != nil {
		t.Fatal(err)
	}
	p := result.ProxyConfigResult
	if p == nil || p.SocksProxyPort != 1080 || p.ApiPort != 8443 || p.ApiBaseUrl != "https://proxy.example" ||
		p.ProxyId == nil || p.WgConfig == nil || p.WgConfig.WgProxyPort != 51820 || p.WgConfig.ClientIpv4 != "10.0.0.2" {
		t.Errorf("proxy config result = %+v", p)
	}
}

func TestPointsLeaderboardMeRoundTripsRanked(t *testing.T) {
	var me PointsLeaderboardMe
	if err := json.Unmarshal([]byte(`{"network_id": "00000000-0000-0000-0000-000000000081", "anonymous": true, "rank_points": 3, "position": 3, "points_leaderboard_public": true, "ranked": true}`), &me); err != nil {
		t.Fatal(err)
	}
	if !me.Ranked || !me.PointsLeaderboardPublic || me.Row == nil || me.Row.RankPoints != 3 {
		t.Errorf("me = %+v row = %+v", me, me.Row)
	}
	b, err := json.Marshal(&me)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ranked":true`) {
		t.Errorf("marshal = %s", b)
	}
}

func TestDeleteApiKeyPostsTheId(t *testing.T) {
	var path, body atomic.Value
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.Method + " " + r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	callback, c := connect.NewBlockingApiCallback[*DeleteApiKeyResult](context.Background())
	api.DeleteApiKey(&DeleteApiKeyArgs{Id: mustParseTestId(t, "00000000-0000-0000-0000-000000000091")}, callback)
	r := awaitApiResult(t, c, "DeleteApiKey never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	if got := path.Load(); got != "POST /account/api-key/remove" {
		t.Errorf("request = %v", got)
	}
	if got, _ := body.Load().(string); !strings.Contains(got, `"id":"00000000-0000-0000-0000-000000000091"`) {
		t.Errorf("body = %s", got)
	}
}

func TestRemovedFindRoutesFailWithoutARequest(t *testing.T) {
	var requests atomic.Int32
	api := newTestPaymentApi(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	locationsCallback, lc := connect.NewBlockingApiCallback[*FindLocationsResult](context.Background())
	api.FindLocations(&FindLocationsArgs{Query: "de"}, locationsCallback)
	if r := awaitApiResult(t, lc, "FindLocations never returned"); r.Error != errFindRoutesRemoved {
		t.Errorf("FindLocations error = %v", r.Error)
	}

	providersCallback, pc := connect.NewBlockingApiCallback[*FindProvidersResult](context.Background())
	api.FindProviders(&FindProvidersArgs{Count: 1}, providersCallback)
	if r := awaitApiResult(t, pc, "FindProviders never returned"); r.Error != errFindRoutesRemoved {
		t.Errorf("FindProviders error = %v", r.Error)
	}

	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests reached the server", n)
	}
}

func mustParseTestId(t *testing.T, s string) *Id {
	t.Helper()
	id, err := ParseId(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
