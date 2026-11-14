//go:build !ios_extension && !js

// What an sdk host gives the core (§5.4's SidecarHost, §18.7): the instance's store over StateDir, the app's key store
// through the adapter, and as its token source the Api session that holds the child token once EnrollWithDevice
// minted one in this process (sidecar_host_services.go), none otherwise; the service urls of the app's NetworkSpace;
// and the components a SidecarSettings names, each built by the wrapper that binds it.
//
// Urls follow the space, as the sdk's own do: api and the platform are the space's, and auth, evs, gkm, sockets and dns
// take the space's api url with its api label swapped for the label §18.9's host table gives each service, keeping
// an env's prefix, port and path (api.<host> gives evs.<host>, test-api.<host> test-evs.<host>), as connect's label
// rule takes alt from connect. A test or staging space never reaches production's services (QD-119), and an api url
// with no api label to swap, an address or a single label, refuses rather than guess.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/sidecar"
)

// The core's host for one local Sidecar. Safe for concurrent use.
type sdkSidecarHost struct {
	store    sidecar.SidecarStore
	keyStore sidecar.SidecarKeyStore

	stateLock sync.Mutex
	// the child token's session, nil before EnrollWithDevice made one
	session *sidecarApiSession
}

// The instance's store for the recorded network, or for none before enrollment.
func (self *sdkSidecarHost) Store() sidecar.SidecarStore {
	return self.store
}

// The app's key store, through the adapter.
func (self *sdkSidecarHost) KeyStore() sidecar.SidecarKeyStore {
	return self.keyStore
}

// The child token's session while it holds a token, which the core reads at each generation's start; none
// otherwise, and the core's own refresher holds the token (D-CORE-8).
func (self *sdkSidecarHost) TokenSource() sidecar.SidecarTokenSource {
	self.stateLock.Lock()
	session := self.session
	self.stateLock.Unlock()
	if session == nil || !session.holdsToken() {
		// an untyped nil, so the core sees no source at all
		return nil
	}
	return session
}

// Puts session in place as the host's, answering the one it replaces, nil for none.
func (self *sdkSidecarHost) swapSession(session *sidecarApiSession) *sidecarApiSession {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	previous := self.session
	self.session = session
	return previous
}

// Sets every service url of settings from networkSpace: its api and platform urls, and the other services by the
// host table's labels in place of the api url's.
func applySidecarServiceUrls(networkSpace *NetworkSpace, settings *sidecar.SidecarSettings) error {
	list, err := sidecar.SidecarHostListV1()
	if err != nil {
		return err
	}
	apiLabel, err := list.ServiceLabel(sidecar.SidecarHostServiceApi)
	if err != nil {
		return err
	}
	apiUrl := networkSpace.GetApiUrl()
	urls := map[sidecar.SidecarHostService]string{}
	for _, service := range []sidecar.SidecarHostService{
		sidecar.SidecarHostServiceAuth,
		sidecar.SidecarHostServiceEvs,
		sidecar.SidecarHostServiceGkm,
		sidecar.SidecarHostServiceSockets,
		sidecar.SidecarHostServiceDns,
	} {
		label, err := list.ServiceLabel(service)
		if err != nil {
			return err
		}
		serviceUrl, err := sidecarServiceUrlOf(apiUrl, apiLabel, label)
		if err != nil {
			return err
		}
		urls[service] = serviceUrl
	}
	settings.ApiUrl = apiUrl
	settings.PlatformUrl = networkSpace.GetPlatformUrl()
	settings.AuthUrl = urls[sidecar.SidecarHostServiceAuth]
	settings.EvsUrl = urls[sidecar.SidecarHostServiceEvs]
	settings.GkmUrl = urls[sidecar.SidecarHostServiceGkm]
	settings.SocketsUrl = urls[sidecar.SidecarHostServiceSockets]
	settings.DnsUrl = urls[sidecar.SidecarHostServiceDns]
	return nil
}

// apiUrl with its first label's apiLabel swapped for serviceLabel, an env's "<env>-" prefix, the port and the path
// kept. An api url whose first label is not apiLabel, alone or after a prefix, refuses.
func sidecarServiceUrlOf(apiUrl string, apiLabel string, serviceLabel string) (string, error) {
	parsed, err := url.Parse(apiUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", errors.New("the network space's api url is not an https url")
	}
	first, rest, ok := strings.Cut(parsed.Hostname(), ".")
	if !ok || rest == "" {
		return "", errors.New("the network space's api url has no host to put the sidecar's services under")
	}
	var label string
	switch {
	case first == apiLabel:
		label = serviceLabel
	case strings.HasSuffix(first, "-"+apiLabel):
		label = strings.TrimSuffix(first, apiLabel) + serviceLabel
	default:
		return "", fmt.Errorf("the network space's api host does not start with %s", apiLabel)
	}
	host := label + "." + rest
	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	serviceUrl := &url.URL{Scheme: parsed.Scheme, Host: host, Path: parsed.Path, RawPath: parsed.RawPath}
	return serviceUrl.String(), nil
}

// Builds a component of a local Sidecar from its core, which it attaches to (sidecar.Sidecar.AddComponent). ctx ends
// with the Sidecar. A factory that needs the network the core enrolls in answers errSidecarComponentNotEnrolled while
// there is none, and the Sidecar builds it again once EnrollWithDevice has enrolled it.
type sidecarComponentFactory func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error)

// What a factory answers, wrapped, before the core is enrolled in a network: the component waits for the enrollment
// rather than refuse the Sidecar, since an app opens its Sidecar before its first EnrollWithDevice.
var errSidecarComponentNotEnrolled = errors.New("the component needs an enrolled network")

// The factories the wrappers register by component name (SDK-M8-03's evs and sockets, SDK-M8-04's mail, ...). A name
// with none is refused by NewSidecarLocal: the shell binds no component of its own.
var sidecarComponentFactories = struct {
	stateLock sync.Mutex
	factories map[string]sidecarComponentFactory
}{
	factories: map[string]sidecarComponentFactory{},
}

// Registers the factory of a component name, at init. A name that is not a component's, or a second factory for one,
// is a programming error.
func registerSidecarComponentFactory(name string, factory sidecarComponentFactory) {
	sidecarComponentFactories.stateLock.Lock()
	defer sidecarComponentFactories.stateLock.Unlock()
	if !slices.Contains(sidecarComponentNames, name) || factory == nil || sidecarComponentFactories.factories[name] != nil {
		panic(fmt.Sprintf("sidecar component %q registered twice, unknown or without a factory", name))
	}
	sidecarComponentFactories.factories[name] = factory
}

// The factory of name, if a wrapper registered one.
func sidecarComponentFactoryOf(name string) (sidecarComponentFactory, bool) {
	sidecarComponentFactories.stateLock.Lock()
	defer sidecarComponentFactories.stateLock.Unlock()
	factory, ok := sidecarComponentFactories.factories[name]
	return factory, ok
}
