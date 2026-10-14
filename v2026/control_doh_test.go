// The bootstrap DoH servers of a space (control_doh.go): the values, the url
// check and its error ids, the in-place save, the share, and a space whose api
// is reachable only once a working server is named.
package sdk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
)

// Four v4 bootstrap DoH servers at documentation addresses, as many as a
// regional preset names.
var testControlDohUrlsIpv4 = []string{
	"https://192.0.2.53/dns-query",
	"https://192.0.2.54/dns-query",
	"https://198.51.100.53/dns-query",
	"https://203.0.113.53/dns-query",
}

const testControlDohIpv6Url = "https://[2001:db8::53]/dns-query"

// A StringList of the values, in order.
func testStringList(values ...string) *StringList {
	stringList := NewStringList()
	stringList.addAll(values...)
	return stringList
}

// The servers ride the space's json, and a space without them writes none.
func TestControlDohUrlsJson(t *testing.T) {
	values := NetworkSpaceValues{
		ControlDohUrlsIpv4: testControlDohUrlsIpv4,
		ControlDohUrlsIpv6: []string{testControlDohIpv6Url},
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"control_doh_urls_ipv4":["https://192.0.2.53/dns-query",`) ||
		!strings.Contains(string(encoded), `"control_doh_urls_ipv6":["https://[2001:db8::53]/dns-query"]`) {
		t.Fatalf("json = %s", encoded)
	}
	var decoded NetworkSpaceValues
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.ControlDohUrlsIpv4, testControlDohUrlsIpv4) ||
		!slices.Equal(decoded.ControlDohUrlsIpv6, []string{testControlDohIpv6Url}) {
		t.Fatalf("decoded = %v %v", decoded.ControlDohUrlsIpv4, decoded.ControlDohUrlsIpv6)
	}
	empty, err := json.Marshal(NetworkSpaceValues{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "control_doh") {
		t.Fatalf("a space without bootstrap DoH servers wrote %s", empty)
	}
}

// Every refusal has its error id, and the China preset validates.
func TestValidateControlDohUrl(t *testing.T) {
	for i := range RegionalControlDohUrls("cn").Len() {
		dohUrl := RegionalControlDohUrls("cn").Get(i)
		if errorId := ValidateControlDohUrl(dohUrl); errorId != "" {
			t.Errorf("preset %s = %s", dohUrl, errorId)
		}
	}
	if errorId := ValidateControlDohUrl(testControlDohIpv6Url); errorId != "" {
		t.Errorf("%s = %s", testControlDohIpv6Url, errorId)
	}
	cases := []struct {
		dohUrl  string
		errorId string
	}{
		{dohUrl: "https://dns.example/dns-query", errorId: ControlDohErrorIpRequired},
		{dohUrl: "http://192.0.2.53/dns-query", errorId: ControlDohErrorHttpsRequired},
		{dohUrl: "192.0.2.53", errorId: ControlDohErrorHttpsRequired},
		{dohUrl: "https://192.0.2.53", errorId: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53/dns-query?dns=1", errorId: ControlDohErrorUrlInvalid},
		{dohUrl: "", errorId: ControlDohErrorUrlInvalid},
	}
	for _, c := range cases {
		if errorId := ValidateControlDohUrl(c.dohUrl); errorId != c.errorId {
			t.Errorf("%q = %q, expected %q", c.dohUrl, errorId, c.errorId)
		}
	}
	if presets := RegionalControlDohUrls("us"); presets.Len() != 0 {
		t.Errorf("us presets = %v", presets.getAll())
	}
}

// Saving applies in place -- the space and everything bound to it survive --
// and the strategy's DoH settings list the named servers ahead of the
// defaults, as do the strategies derived from it later. Invalid urls save
// nothing, an unchanged list is no change, and the list survives a restart of
// the manager. Clearing it leaves the defaults alone.
func TestNetworkSpaceSetControlDohUrls(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_control_doh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	defaults := connect.DefaultDnsResolverSettings()

	if dohUrls := networkSpace.GetControlDohUrls(); dohUrls.Len() != 0 {
		t.Fatalf("a new space names %v", dohUrls.getAll())
	}
	if dohUrls := networkSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrls, defaults.RemoteDohUrlsIpv4) {
		t.Fatalf("a new space's servers = %v, expected the defaults", dohUrls)
	}
	strategy := networkSpace.clientStrategy

	// the v4 servers with a v6 server mixed in, blank lines, whitespace and a
	// repeat
	entered := testStringList(append(
		[]string{" ", testControlDohIpv6Url},
		append(slices.Clone(testControlDohUrlsIpv4), " https://192.0.2.53/dns-query ", "")...,
	)...)
	if errorId := networkSpace.SetControlDohUrls(entered); errorId != "" {
		t.Fatal(errorId)
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace || networkSpace.clientStrategy != strategy {
		t.Fatal("saving the bootstrap DoH servers replaced the space")
	}
	if !slices.Equal(networkSpace.GetControlDohUrlsIpv4().getAll(), testControlDohUrlsIpv4) {
		t.Fatalf("v4 = %v", networkSpace.GetControlDohUrlsIpv4().getAll())
	}
	if !slices.Equal(networkSpace.GetControlDohUrlsIpv6().getAll(), []string{testControlDohIpv6Url}) {
		t.Fatalf("v6 = %v", networkSpace.GetControlDohUrlsIpv6().getAll())
	}
	if !slices.Equal(networkSpace.GetControlDohUrls().getAll(), append(slices.Clone(testControlDohUrlsIpv4), testControlDohIpv6Url)) {
		t.Fatalf("all = %v", networkSpace.GetControlDohUrls().getAll())
	}
	expectedIpv4 := append(slices.Clone(testControlDohUrlsIpv4), defaults.RemoteDohUrlsIpv4...)
	expectedIpv6 := append([]string{testControlDohIpv6Url}, defaults.RemoteDohUrlsIpv6...)
	assertDohUrls := func(what string, dohSettings *connect.DohSettings) {
		t.Helper()
		if !slices.Equal(dohSettings.DnsResolverSettings.RemoteDohUrlsIpv4, expectedIpv4) ||
			!slices.Equal(dohSettings.DnsResolverSettings.RemoteDohUrlsIpv6, expectedIpv6) {
			t.Fatalf("%s servers = %v %v", what, dohSettings.DnsResolverSettings.RemoteDohUrlsIpv4, dohSettings.DnsResolverSettings.RemoteDohUrlsIpv6)
		}
	}
	assertDohUrls("the strategy's", networkSpace.clientStrategy.DohSettings())
	// a provider's direct strategies, built after, take them; a hosted
	// device's strategy keeps the built-in servers, since its host would query
	// them (TestHostedClientStrategyRefusesBootstrapDohServers)
	assertDohUrls("the derived", networkSpace.derivedClientStrategySettings().DohSettings)
	hostedStrategy := networkSpace.newHostedClientStrategy(nil)
	if dohUrls := hostedStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrls, defaults.RemoteDohUrlsIpv4) {
		t.Fatalf("a hosted strategy's servers = %v, expected the built-in ones", dohUrls)
	}
	hostedStrategy.Close()

	// invalid urls save nothing
	for _, c := range []struct {
		dohUrl  string
		errorId string
	}{
		{dohUrl: "https://dns.example/dns-query", errorId: ControlDohErrorIpRequired},
		{dohUrl: "http://192.0.2.53/dns-query", errorId: ControlDohErrorHttpsRequired},
	} {
		if errorId := networkSpace.SetControlDohUrls(testStringList("https://192.0.2.53/dns-query", c.dohUrl)); errorId != c.errorId {
			t.Fatalf("%s = %q, expected %q", c.dohUrl, errorId, c.errorId)
		}
	}
	tooMany := NewStringList()
	for i := range connect.ControlDohMaxUrlCount + 1 {
		tooMany.Add(fmt.Sprintf("https://192.0.2.%d/dns-query", i+1))
	}
	if errorId := networkSpace.SetControlDohUrls(tooMany); errorId != ControlDohErrorTooMany {
		t.Fatalf("too many = %q", errorId)
	}
	if !slices.Equal(networkSpace.GetControlDohUrlsIpv4().getAll(), testControlDohUrlsIpv4) {
		t.Fatalf("a refused save changed the servers to %v", networkSpace.GetControlDohUrlsIpv4().getAll())
	}
	assertDohUrls("the strategy's after refused saves", networkSpace.clientStrategy.DohSettings())

	// the same list again, spelled differently, is no change
	if networkSpace.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ControlDohUrlsIpv4 = []string{" HTTPS://192.0.2.53/dns-query", "https://192.0.2.54/dns-query", "https://198.51.100.53/dns-query", "https://203.0.113.53/dns-query"}
		values.ControlDohUrlsIpv6 = []string{testControlDohIpv6Url, ""}
	}) {
		t.Fatal("an unchanged list reported a change")
	}

	networkSpaceManager.Close()
	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if restoredSpace == nil {
		t.Fatal("the space did not survive the restart")
	}
	if !slices.Equal(restoredSpace.GetControlDohUrls().getAll(), networkSpace.GetControlDohUrls().getAll()) {
		t.Fatalf("restored = %v", restoredSpace.GetControlDohUrls().getAll())
	}
	// a space built from stored values starts with them
	assertDohUrls("the restored strategy's", restoredSpace.clientStrategy.DohSettings())

	// clearing leaves the defaults alone
	if errorId := restoredSpace.SetControlDohUrls(nil); errorId != "" {
		t.Fatal(errorId)
	}
	if dohUrls := restoredSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrls, defaults.RemoteDohUrlsIpv4) {
		t.Fatalf("cleared servers = %v, expected the defaults", dohUrls)
	}
	values := restoredSpace.valuesCopy()
	if len(values.ControlDohUrlsIpv4) != 0 || len(values.ControlDohUrlsIpv6) != 0 {
		t.Fatalf("clearing left %v %v", values.ControlDohUrlsIpv4, values.ControlDohUrlsIpv6)
	}
}

// A bootstrap DoH change alone is an in-place change; together with a value
// that needs a rebuild it is not.
func TestOnlyInPlaceValuesChangedIncludesControlDoh(t *testing.T) {
	key := NewNetworkSpaceKey("space.example", "main")
	previous := NetworkSpaceValues{ApiUrl: "https://api.space.example"}
	next := previous
	next.ControlDohUrlsIpv4 = testControlDohUrlsIpv4
	if !onlyInPlaceValuesChanged(key, &previous, &next) {
		t.Fatal("a bootstrap DoH change alone must apply in place")
	}
	ipv6Only := previous
	ipv6Only.ControlDohUrlsIpv6 = []string{testControlDohIpv6Url}
	if !onlyInPlaceValuesChanged(key, &previous, &ipv6Only) {
		t.Fatal("a v6 bootstrap DoH change alone must apply in place")
	}
	rebuilt := next
	rebuilt.ApiUrl = "https://api2.space.example"
	if onlyInPlaceValuesChanged(key, &previous, &rebuilt) {
		t.Fatal("a bootstrap DoH change with an api change must rebuild")
	}
	if onlyInPlaceValuesChanged(key, &previous, &previous) {
		t.Fatal("no change is not an in-place change")
	}
}

// A space whose api name resolves only over DoH, on a network that
// black-holes every default DoH server, cannot reach its api until a working
// bootstrap DoH server is saved. It then reaches it through the running
// space, with no rebuild.
func TestNetworkSpaceControlDohUrlsReachTheApiInPlace(t *testing.T) {
	// the api answers for example.com, which its certificate names
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello api"))
	}))
	defer api.Close()
	apiRoots := x509.NewCertPool()
	apiRoots.AddCert(api.Certificate())
	apiUrl, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}

	// a DoH server: https on the v4 loopback, which its certificate names,
	// answering every address query with the loopback and passing on each
	// name it is asked for
	answer := net.ParseIP("127.0.0.1")
	queried := make(chan string, 64)
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dns-query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var parser dnsmessage.Parser
		header, err := parser.Start(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		question, err := parser.Question()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case queried <- question.Name.String():
		default:
		}
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
		builder.StartQuestions()
		builder.Question(question)
		builder.StartAnswers()
		if question.Type == dnsmessage.TypeA {
			resourceHeader := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60}
			builder.AResource(resourceHeader, dnsmessage.AResource{A: [4]byte(answer.To4())})
		}
		response, err := builder.Finish()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(response)
	}))
	t.Cleanup(dohServer.Close)
	dohRoots := x509.NewCertPool()
	dohRoots.AddCert(dohServer.Certificate())
	// the url, and the address the DoH client dials
	dohUrl := dohServer.URL + "/dns-query"
	dohAddress := dohServer.Listener.Addr().String()
	controlDohSettingsConfigure = func(settings *connect.DohSettings) {
		settings.RequestTimeout = time.Second
		settings.DnsResolverSettings.TlsConfig = &tls.Config{RootCAs: dohRoots}
		settings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				if address == dohAddress {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
	}
	t.Cleanup(func() {
		controlDohSettingsConfigure = nil
	})

	connectSettings := connect.DefaultConnectSettings()
	connectSettings.TlsConfig = &tls.Config{RootCAs: apiRoots}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := newNetworkSpaceWithConnectSettings(
		ctx,
		NetworkSpaceKey{HostName: "example.com", EnvName: "main"},
		NetworkSpaceValues{
			ApiUrl:                   "https://example.com:" + apiUrl.Port(),
			PlatformUrl:              "wss://127.0.0.1:1",
			NetExposeServerIps:       true,
			NetExposeServerHostNames: true,
		},
		"",
		connectSettings,
		false,
	)
	defer networkSpace.close()
	strategy := networkSpace.clientStrategy

	get := func(timeout time.Duration) (string, error) {
		requestCtx, requestCancel := context.WithTimeout(ctx, timeout)
		defer requestCancel()
		body, err := connect.HttpGetWithStrategyRaw(requestCtx, networkSpace.clientStrategy, networkSpace.GetApiUrl()+"/hello", "")
		return string(body), err
	}

	if body, err := get(3 * time.Second); err == nil {
		t.Fatalf("with the default DoH servers black-holed the api must be unreachable, got %q", body)
	}

	if errorId := networkSpace.SetControlDohUrls(testStringList(dohUrl)); errorId != "" {
		t.Fatal(errorId)
	}
	body, err := get(30 * time.Second)
	if err != nil {
		t.Fatalf("through the bootstrap DoH server: %s", err)
	}
	if body != "hello api" {
		t.Fatalf("body = %q", body)
	}
	if networkSpace.clientStrategy != strategy {
		t.Fatal("the strategy was replaced")
	}
	select {
	case name := <-queried:
		if name != "example.com." {
			t.Fatalf("the bootstrap DoH server was asked for %q", name)
		}
	default:
		t.Fatal("the bootstrap DoH server was never asked")
	}
}

// The share's settings block carries the sharer's bootstrap DoH servers: the
// decode lists them for the confirmation, an import that takes the settings
// takes them in place, one that does not leaves them, and a block that names
// none leaves the importer's own.
func TestExtenderViewControllerShareCarriesControlDohUrls(t *testing.T) {
	vc, networkSpace, _ := testExtenderViewController(t)
	if errorId := networkSpace.SetControlDohUrls(testStringList(testControlDohUrlsIpv4...)); errorId != "" {
		t.Fatal(errorId)
	}

	withSettings := vc.BuildShare(true)
	decoded := vc.DecodeShare(withSettings.Text)
	connect.AssertEqual(t, decoded.Ok, true)
	if !slices.Equal(decoded.ControlDohUrls.getAll(), testControlDohUrlsIpv4) {
		t.Fatalf("decoded servers = %v", decoded.ControlDohUrls.getAll())
	}
	plain := vc.BuildShare(false)
	if servers := vc.DecodeShare(plain.Text).ControlDohUrls; servers.Len() != 0 {
		t.Fatalf("a share without settings carries %v", servers.getAll())
	}

	otherVc, otherSpace, otherManager := testExtenderViewController(t)
	otherStrategy := otherSpace.clientStrategy
	connect.AssertEqual(t, otherVc.ImportShare(withSettings.Text, false).Ok, true)
	if servers := otherSpace.GetControlDohUrls(); servers.Len() != 0 {
		t.Fatalf("an import without the settings took %v", servers.getAll())
	}
	connect.AssertEqual(t, otherVc.ImportShare(withSettings.Text, true).Ok, true)
	if !slices.Equal(otherSpace.GetControlDohUrls().getAll(), testControlDohUrlsIpv4) {
		t.Fatalf("imported servers = %v", otherSpace.GetControlDohUrls().getAll())
	}
	if dohUrls := otherSpace.clientStrategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrls[:len(testControlDohUrlsIpv4)], testControlDohUrlsIpv4) {
		t.Fatalf("the importer's strategy servers = %v", dohUrls)
	}
	if otherManager.GetNetworkSpace(otherSpace.GetKey()) != otherSpace || otherSpace.clientStrategy != otherStrategy {
		t.Fatal("an import with settings replaced the space")
	}

	// a settings block that names no servers leaves the importer's own
	if errorId := networkSpace.SetControlDohUrls(nil); errorId != "" {
		t.Fatal(errorId)
	}
	connect.AssertEqual(t, otherVc.ImportShare(vc.BuildShare(true).Text, true).Ok, true)
	if !slices.Equal(otherSpace.GetControlDohUrls().getAll(), testControlDohUrlsIpv4) {
		t.Fatalf("a block without servers changed the importer's to %v", otherSpace.GetControlDohUrls().getAll())
	}
}
