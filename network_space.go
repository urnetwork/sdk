package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/connect"
)

// a network space is a set of server and app configurations
// sequence of setting up a device:
// 1. network space creates api
// 2. api creates device
// use `UpdateNetworkSpace` to create a new network space

func NormalEnvName(envName string) string {
	switch envName {
	case "":
		return "main"
	default:
		return strings.ToLower(envName)
	}
}

type NetExtender struct {
	Ip     string `json:"ip"`
	Secret string `json:"secret"`
}

type ExportNetworkSpace struct {
	Key    *NetworkSpaceKey    `json:"key,omitempty"`
	Values *NetworkSpaceValues `json:"values,omitempty"`
}

type NetworkSpaceKey struct {
	HostName string `json:"host_name,omitempty"`
	EnvName  string `json:"env_name,omitempty"`
}

func NewNetworkSpaceKey(hostName string, envName string) *NetworkSpaceKey {
	return &NetworkSpaceKey{
		HostName: hostName,
		EnvName:  NormalEnvName(envName),
	}
}

type NetworkSpaceValues struct {
	EnvSecret                string `json:"env_secret,omitempty"`
	Bundled                  bool   `json:"bundled,omitempty"`
	NetExposeServerIps       bool   `json:"net_expose_server_ips,omitempty"`
	NetExposeServerHostNames bool   `json:"net_expose_server_host_names,omitempty"`
	LinkHostName             string `json:"link_host_name,omitempty"`
	MigrationHostName        string `json:"migration_host_name,omitempty"`
	Store                    string `json:"store,omitempty"`
	Wallet                   string `json:"wallet,omitempty"`
	SsoGoogle                bool   `json:"sso_google,omitempty"`

	// Optional absolute service endpoints. When empty, URLs are derived from HostName/EnvName
	// via ServiceUrl, matching main-branch behavior:
	//   api:     https://api.<host>
	//   connect: wss://connect.<host>
	// no trailing slash: api.go builds paths as fmt.Sprintf("%s/path", apiUrl).
	//
	// When set, these overrides are used exactly as provided — the caller is
	// responsible for any EnvSecret query parameter. ServiceUrl's automatic
	// EnvSecret appending does not apply to explicit overrides.
	ApiUrl      string `json:"api_url,omitempty"`
	PlatformUrl string `json:"platform_url,omitempty"`

	// UR protocol chain overrides (vault, coordinator, operator id, rpc)
	SnChain *SnChainSettings `json:"sn_chain,omitempty"`

	// custom extender
	// this overrides any auto discovered extenders
	NetExtender *NetExtender `json:"net_extender,omitempty"`

	// Extender network overrides (EXTENDER.md F1). Empty derives each from
	// HostName/EnvName exactly as ServiceUrl does:
	//   extender dns: extender.<host>
	//   gossip:       wss://gossip.<host>
	// ExtenderRootPublicKeys is the trust anchor before first contact (B4);
	// empty takes the bundled table for the host, and a hello answer replaces
	// whatever is in force.
	ExtenderDnsName        string   `json:"extender_dns_name,omitempty"`
	GossipUrl              string   `json:"gossip_url,omitempty"`
	ExtenderRootPublicKeys []string `json:"extender_root_public_keys,omitempty"`
}

// The space host a service name is derived under: the migration host while one
// is configured, else the key host. Every derived name follows it, so a
// migration moves the whole namespace at once.
func spaceHostName(key *NetworkSpaceKey, values *NetworkSpaceValues) string {
	if values.MigrationHostName != "" {
		return values.MigrationHostName
	}
	return key.HostName
}

// The host name of one service under a space, with the env prefix rule: a
// non-main env prefixes the service label rather than the host.
func ServiceHostName(key *NetworkSpaceKey, values *NetworkSpaceValues, service string) string {
	hostName := spaceHostName(key, values)
	switch key.EnvName {
	case "main", "":
		return fmt.Sprintf("%s.%s", service, hostName)
	default:
		return fmt.Sprintf("%s-%s.%s", key.EnvName, service, hostName)
	}
}

func ServiceUrl(key *NetworkSpaceKey, values *NetworkSpaceValues, scheme string, service string) string {
	serviceHostName := ServiceHostName(key, values, service)

	serviceUrl := fmt.Sprintf("%s://%s", scheme, serviceHostName)
	if values.EnvSecret != "" {
		serviceUrl = fmt.Sprintf("%s/%s", serviceUrl, values.EnvSecret)
	}

	return serviceUrl
}

// networkSpaceDohDomains returns every service namespace owned by a network
// space. During a hostname migration both namespaces remain protected because
// persisted URLs and in-flight clients can legitimately use either one.
func networkSpaceDohDomains(key *NetworkSpaceKey, values *NetworkSpaceValues) []string {
	domains := make([]string, 0, 2)
	if hostName := strings.TrimSpace(key.HostName); hostName != "" {
		domains = append(domains, hostName)
	}
	if migrationHostName := strings.TrimSpace(values.MigrationHostName); migrationHostName != "" {
		domains = append(domains, migrationHostName)
	}
	return domains
}

func ConnectLinkUrl(key *NetworkSpaceKey, values *NetworkSpaceValues, target string) string {
	var linkHostName string
	if values.LinkHostName != "" {
		linkHostName = values.LinkHostName
	} else {
		linkHostName = key.HostName
	}

	return fmt.Sprintf("%s://%s/c?%s", "https", linkHostName, target)
}

type NetworkSpace struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	key         NetworkSpaceKey
	values      NetworkSpaceValues
	storagePath string

	apiUrl      string
	platformUrl string

	clientStrategy *connect.ClientStrategy
	// clientStrategySettings is what clientStrategy was built from. The
	// provider's family-pinned transports seed their direct-only strategies
	// from it (connect.NewDirectClientStrategy), so a pinned dial carries the
	// same tls, resolver and logging configuration as the shared strategy
	// minus the extenders and proxy.
	clientStrategySettings *connect.ClientStrategySettings
	asyncLocalState        *AsyncLocalState
	api                    *Api
	// the space's dial logger, carried so the manager's one-time control ip
	// family restore lands on the same log as the dials it governs
	log connect.Logger

	// The extender directory of this space (EXTENDER.md E1, F1). Nil for a
	// url-only space, which names its endpoints outright and has nothing to
	// discover.
	extenderDirectory *connect.ExtenderDirectory
	// The refresh loop that fills the directory (E3). Nil for a space whose
	// host is not a real dns name, which has nothing to resolve or sample.
	extenderNetworkClient *connect.ExtenderNetworkClient
	// The gossip node of the member role (D1, D5). Nil in the feed role, on
	// the js build, and for a space with nothing to join.
	extenderNode                  *spaceExtenderNode
	extenderStatusChangeListeners *connect.CallbackList[ExtenderStatusChangeListener]
}

func newNetworkSpace(
	ctx context.Context,
	key NetworkSpaceKey,
	values NetworkSpaceValues,
	storagePath string,
) *NetworkSpace {
	return newNetworkSpaceWithConnectSettings(ctx, key, values, storagePath, connect.DefaultConnectSettings())
}

func newNetworkSpaceWithConnectSettings(
	ctx context.Context,
	key NetworkSpaceKey,
	values NetworkSpaceValues,
	storagePath string,
	connectSettings *connect.ConnectSettings,
) *NetworkSpace {
	cancelCtx, cancel := context.WithCancel(ctx)

	// Prefer explicit overrides when set; otherwise derive from HostName/EnvName.
	// no trailing slash: api.go builds paths as fmt.Sprintf("%s/path", apiUrl),
	// so a trailing slash here would double up to "//path" and 404 every request.
	apiUrl := strings.TrimRight(values.ApiUrl, "/")
	if apiUrl == "" {
		apiUrl = ServiceUrl(&key, &values, "https", "api")
	}
	platformUrl := strings.TrimRight(values.PlatformUrl, "/")
	if platformUrl == "" {
		platformUrl = ServiceUrl(&key, &values, "wss", "connect")
	}

	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ConnectSettings = *connectSettings
	// the network space logger rides `ConnectSettings.Log`. this silences the
	// shared client strategy, which a per-device `DisableLogging` cannot reach.
	clientStrategySettings.Log = connectSettings.Log
	clientStrategySettings.ExposeServerIps = values.NetExposeServerIps
	clientStrategySettings.ExposeServerHostNames = values.NetExposeServerHostNames
	clientStrategySettings.InternalDohDomains = networkSpaceDohDomains(&key, &values)

	var asyncLocalState *AsyncLocalState
	if storagePath != "" {
		asyncLocalState = NewAsyncLocalState(storagePath)
	}

	// the directory is built before the strategy so the strategy's extender
	// dialers come from it (E2)
	extenderDirectory := newSpaceExtenderDirectory(cancelCtx, &key, &values, asyncLocalState, clientStrategySettings.Log)
	clientStrategySettings.ExtenderDirectory = extenderDirectory

	clientStrategy := connect.NewClientStrategy(cancelCtx, clientStrategySettings)

	if values.NetExtender != nil {
		extenderIpSecrets := map[netip.Addr]string{}
		if ip, err := netip.ParseAddr(values.NetExtender.Ip); err == nil {
			extenderIpSecrets[ip] = values.NetExtender.Secret
		}
		clientStrategy.SetCustomExtenders(extenderIpSecrets)
	}

	api := newApi(cancelCtx, clientStrategy, apiUrl)

	networkSpace := &NetworkSpace{
		ctx:    cancelCtx,
		cancel: cancel,

		key:         key,
		values:      values,
		storagePath: storagePath,

		apiUrl:      apiUrl,
		platformUrl: platformUrl,

		clientStrategy:                clientStrategy,
		clientStrategySettings:        clientStrategySettings,
		asyncLocalState:               asyncLocalState,
		api:                           api,
		log:                           clientStrategySettings.ConnectSettings.Log,
		extenderDirectory:             extenderDirectory,
		extenderStatusChangeListeners: connect.NewCallbackList[ExtenderStatusChangeListener](),
	}
	// the role decides what fills the directory: the feed role holds the
	// subscribe stream, the member role runs the node instead (D5)
	role := extenderRole(extenderGossipMode(asyncLocalState))
	networkSpace.extenderNetworkClient = newSpaceExtenderNetworkClient(
		cancelCtx,
		&key,
		&values,
		role,
		clientStrategy,
		extenderDirectory,
		apiUrl,
		clientStrategySettings.Log,
	)
	networkSpace.extenderNode = newSpaceExtenderNode(
		cancelCtx,
		&key,
		&values,
		role,
		extenderDirectory,
		networkSpace.extenderNetworkClient,
		asyncLocalState,
		clientStrategySettings,
		clientStrategySettings.Log,
	)
	// no rescue handler: the watch already contains a panic to one tick, and a
	// failure to render status must never tear the space down
	go connect.HandleError(networkSpace.watchExtenderStatus)
	return networkSpace
}

// The space's extender directory (E1, F1). It persists under the space's own
// storage; without storage it runs in memory, which is what a headless or
// ephemeral host gets.
func newSpaceExtenderDirectory(
	ctx context.Context,
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	asyncLocalState *AsyncLocalState,
	log connect.Logger,
) *connect.ExtenderDirectory {
	settings := connect.DefaultExtenderDirectorySettings()
	settings.Log = log
	settings.NetworkHosts = networkSpaceDohDomains(key, values)
	if asyncLocalState != nil {
		settings.Store = newLocalStateExtenderStore(asyncLocalState.GetLocalState())
	}
	directory := connect.NewExtenderDirectory(ctx, settings)
	if rootPublicKeyHexes := ExtenderRootPublicKeys(key, values); 0 < len(rootPublicKeyHexes) {
		if keySet, err := connect.NewExtenderRootKeySetFromHex(rootPublicKeyHexes...); err == nil {
			directory.SetRootKeys(keySet)
		}
	}
	return directory
}

// extenderNetworkClientEnabled is a process-wide switch for the extender
// network client. Production never changes it; the sdk test binary turns it
// off in TestMain so a unit test that constructs a production-host space does
// not resolve or dial anything, and the extender tests that need a client
// build one explicitly.
var extenderNetworkClientEnabled = true

// The space's extender network client (E3), or nil when there is nothing for
// it to do. It runs only for a space whose extender dns name is a real dns
// name: a derived name under a single-label host such as `test` or `custom`
// resolves to nothing, and a client there would be a failure loop on every
// launch.
func newSpaceExtenderNetworkClient(
	ctx context.Context,
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	role string,
	clientStrategy *connect.ClientStrategy,
	directory *connect.ExtenderDirectory,
	apiUrl string,
	log connect.Logger,
) *connect.ExtenderNetworkClient {
	if !extenderNetworkClientEnabled || !extenderNetworkClientRuns(key, values) {
		return nil
	}
	return connect.NewExtenderNetworkClient(
		ctx,
		clientStrategy,
		directory,
		spaceExtenderNetworkClientSettings(key, values, role, apiUrl, log),
	)
}

// The settings of one space's network client (E3, D5).
func spaceExtenderNetworkClientSettings(
	key *NetworkSpaceKey,
	values *NetworkSpaceValues,
	role string,
	apiUrl string,
	log connect.Logger,
) *connect.ExtenderNetworkClientSettings {
	settings := connect.DefaultExtenderNetworkClientSettings()
	settings.Log = log
	settings.ExtenderDnsName = ExtenderDnsName(key, values)
	settings.ApiUrl = apiUrl
	// every app takes the one-shot sample at start; only the feed role keeps
	// the subscribe stream open, because the member role hears the same
	// records from the mesh instead (D5)
	settings.Subscribe = role == ExtenderRoleFeed
	return settings
}

// Reports whether a space has an extender dns name worth resolving (F1): both
// the derived name and the space host it came from must be real dns names.
func extenderNetworkClientRuns(key *NetworkSpaceKey, values *NetworkSpaceValues) bool {
	return isDottedHostName(ExtenderDnsName(key, values)) &&
		isDottedHostName(spaceHostName(key, values))
}

// Reports whether a name is a dns name with at least two labels, which is what
// separates a real space host from the single-label `test` and `custom` hosts
// the tests and the url-only spaces use.
func isDottedHostName(hostName string) bool {
	hostName = strings.TrimSuffix(strings.TrimSpace(hostName), ".")
	if hostName == "" || net.ParseIP(hostName) != nil {
		return false
	}
	labels := strings.Split(hostName, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
		for _, c := range label {
			switch {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return true
}

//gomobile:noexport
func NewPlatformNetworkSpace(
	ctx context.Context,
	env string,
	host string,
	connectSettings *connect.ConnectSettings,
) *NetworkSpace {
	key := NetworkSpaceKey{
		EnvName:  env,
		HostName: host,
	}
	values := NetworkSpaceValues{
		NetExposeServerIps:       true,
		NetExposeServerHostNames: true,
	}
	return newNetworkSpaceWithConnectSettings(ctx, key, values, "", connectSettings)
}

func testing_newNetworkSpace(ctx context.Context) (networkSpace *NetworkSpace, byJwt string, returnErr error) {
	key := NetworkSpaceKey{
		HostName: "test",
		EnvName:  "test",
	}
	values := NetworkSpaceValues{
		Bundled:                  true,
		NetExposeServerIps:       true,
		NetExposeServerHostNames: true,
	}
	storagePath, err := os.MkdirTemp("", "networkspace")
	if err != nil {
		returnErr = err
		return
	}

	networkSpace = newNetworkSpace(
		ctx,
		key,
		values,
		storagePath,
	)
	// AsyncLocalState owns a background-context worker rather than inheriting
	// the NetworkSpace context. Tests historically canceled ctx but never
	// closed that worker, retaining one goroutine and its temporary storage per
	// test invocation. Tie both test-only resources to the supplied lifetime.
	context.AfterFunc(ctx, func() {
		if networkSpace.asyncLocalState != nil {
			_ = networkSpace.asyncLocalState.CloseAndWait(context.Background())
		}
		_ = os.RemoveAll(storagePath)
	})
	byJwt = ""
	return
}

// Testing_NewNetworkSpaceWithUrls builds a NetworkSpace that targets explicit
// api and platform urls instead of deriving them from a host/env via ServiceUrl.
// This lets integration tests point the SDK at local servers, e.g.
// apiUrl="https://127.0.0.1:8083" and platformUrl="wss://127.0.0.1:8080".
//
// Only the "normal" dialer is enabled (no resilient tls fragmentation/reorder)
// to keep the loopback tls handshake deterministic. Pair this with a
// connectSettings whose TlsConfig has InsecureSkipVerify=true so the SDK accepts
// the local self-signed certificates (every dialer honors ConnectSettings.TlsConfig).
//
//gomobile:noexport
func Testing_NewNetworkSpaceWithUrls(
	ctx context.Context,
	apiUrl string,
	platformUrl string,
	connectSettings *connect.ConnectSettings,
) *NetworkSpace {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ConnectSettings = *connectSettings
	clientStrategySettings.Log = connectSettings.Log
	clientStrategySettings.ExposeServerIps = true
	clientStrategySettings.ExposeServerHostNames = true
	// only the direct tls dialer; the resilient dialers fragment/reorder the tls
	// ClientHello, which is unnecessary (and an extra failure mode) on loopback
	clientStrategySettings.EnableNormal = true
	clientStrategySettings.EnableResilient = false

	return NewNetworkSpaceWithUrls(ctx, apiUrl, platformUrl, clientStrategySettings)
}

// NewNetworkSpaceWithUrls creates a production, storage-less NetworkSpace for
// explicit API/connect URLs and a full client-strategy configuration. It is
// the server/headless counterpart to NewPlatformNetworkSpace: callers can
// retain proxy, TLS, resolver, timeout, and logging settings while using SDK
// DeviceLocal and the API-owned JWT refresh lifecycle.
//
//gomobile:noexport
func NewNetworkSpaceWithUrls(
	ctx context.Context,
	apiUrl string,
	platformUrl string,
	clientStrategySettings *connect.ClientStrategySettings,
) *NetworkSpace {
	cancelCtx, cancel := context.WithCancel(ctx)
	if clientStrategySettings == nil {
		clientStrategySettings = connect.DefaultClientStrategySettings()
	}

	key := NetworkSpaceKey{
		HostName: "custom",
		EnvName:  "custom",
	}
	values := NetworkSpaceValues{
		ApiUrl:                   strings.TrimRight(apiUrl, "/"),
		PlatformUrl:              strings.TrimRight(platformUrl, "/"),
		NetExposeServerIps:       clientStrategySettings.ExposeServerIps,
		NetExposeServerHostNames: clientStrategySettings.ExposeServerHostNames,
	}
	clientStrategy := connect.NewClientStrategy(cancelCtx, clientStrategySettings)
	api := newApi(cancelCtx, clientStrategy, values.ApiUrl)

	// no extender directory and no network client: a url-only space names its
	// endpoints outright, so there is no space host to derive an extender dns
	// name from and nothing to discover (F1)
	return &NetworkSpace{
		ctx:    cancelCtx,
		cancel: cancel,

		key:         key,
		values:      values,
		storagePath: "",

		apiUrl:      values.ApiUrl,
		platformUrl: values.PlatformUrl,

		clientStrategy:                clientStrategy,
		clientStrategySettings:        clientStrategySettings,
		asyncLocalState:               nil,
		api:                           api,
		extenderStatusChangeListeners: connect.NewCallbackList[ExtenderStatusChangeListener](),
	}
}

// NewUrlsNetworkSpace builds a storage-less NetworkSpace targeting explicit api
// and platform urls. Used by JS/wasm DeviceRemote constructors; their selected
// rpc dialer (direct browser websocket or extension byte transport) is supplied
// separately. The NetworkSpace mainly carries API state and a fallback client
// strategy, which extension-backed remotes explicitly disable.
func NewUrlsNetworkSpace(apiUrl string, platformUrl string) *NetworkSpace {
	return Testing_NewNetworkSpaceWithUrls(
		context.Background(),
		apiUrl,
		platformUrl,
		connect.DefaultConnectSettings(),
	)
}

func (self *NetworkSpace) GetKey() *NetworkSpaceKey {
	// make a copy
	key := self.key
	return &key
}

func (self *NetworkSpace) ServiceUrl(scheme string, service string) string {
	return ServiceUrl(&self.key, &self.values, scheme, service)
}

func (self *NetworkSpace) ConnectLinkUrl(target string) string {
	return ConnectLinkUrl(&self.key, &self.values, target)
}

func (self *NetworkSpace) GetHostName() string {
	return self.key.HostName
}

func (self *NetworkSpace) GetEnvName() string {
	return self.key.EnvName
}

func (self *NetworkSpace) GetEnvSecret() string {
	return self.values.EnvSecret
}

func (self *NetworkSpace) GetBundled() bool {
	return self.values.Bundled
}

func (self *NetworkSpace) GetNetExposeServerIps() bool {
	return self.values.NetExposeServerIps
}

func (self *NetworkSpace) GetNetExposeServerHostNames() bool {
	return self.values.NetExposeServerHostNames
}

func (self *NetworkSpace) GetLinkHostName() string {
	return self.values.LinkHostName
}

func (self *NetworkSpace) GetMigrationHostName() string {
	return self.values.MigrationHostName
}

func (self *NetworkSpace) GetStore() string {
	return self.values.Store
}

func (self *NetworkSpace) GetWallet() string {
	return self.values.Wallet
}

func (self *NetworkSpace) GetNetExtender() *NetExtender {
	return self.values.NetExtender
}

// The resolved extender dns name (F1): the configured override, else
// `extender.<host>` under the env prefix rule.
func (self *NetworkSpace) GetExtenderDnsName() string {
	return ExtenderDnsName(&self.key, &self.values)
}

// The resolved gossip url (F1): the configured override, else
// `wss://gossip.<host>` under the env prefix rule.
func (self *NetworkSpace) GetGossipUrl() string {
	return GossipUrl(&self.key, &self.values)
}

// The resolved extender root public keys (F1, B4): the configured override,
// else the bundled table for this host.
func (self *NetworkSpace) GetExtenderRootPublicKeys() *StringList {
	rootPublicKeys := NewStringList()
	rootPublicKeys.addAll(ExtenderRootPublicKeys(&self.key, &self.values)...)
	return rootPublicKeys
}

// The resolved extender dns name of a space.
func ExtenderDnsName(key *NetworkSpaceKey, values *NetworkSpaceValues) string {
	if extenderDnsName := strings.TrimSpace(values.ExtenderDnsName); extenderDnsName != "" {
		return extenderDnsName
	}
	return ServiceHostName(key, values, "extender")
}

// The resolved gossip url of a space. The env secret rides it exactly as it
// rides the api and platform urls.
func GossipUrl(key *NetworkSpaceKey, values *NetworkSpaceValues) string {
	if gossipUrl := strings.TrimSpace(values.GossipUrl); gossipUrl != "" {
		return strings.TrimRight(gossipUrl, "/")
	}
	return ServiceUrl(key, values, "wss", "gossip")
}

// The resolved extender root public keys of a space.
func ExtenderRootPublicKeys(key *NetworkSpaceKey, values *NetworkSpaceValues) []string {
	rootPublicKeys := []string{}
	for _, rootPublicKey := range values.ExtenderRootPublicKeys {
		if rootPublicKey = strings.TrimSpace(rootPublicKey); rootPublicKey != "" {
			rootPublicKeys = append(rootPublicKeys, rootPublicKey)
		}
	}
	if 0 < len(rootPublicKeys) {
		return rootPublicKeys
	}
	return bundledExtenderRootPublicKeys(spaceHostName(key, values))
}

func (self *NetworkSpace) GetSsoGoogle() bool {
	return self.values.SsoGoogle
}

func (self *NetworkSpace) GetAsyncLocalState() *AsyncLocalState {
	return self.asyncLocalState
}

func (self *NetworkSpace) GetConfiguredApiUrl() string {
	return self.values.ApiUrl
}

func (self *NetworkSpace) GetConfiguredPlatformUrl() string {
	return self.values.PlatformUrl
}

func (self *NetworkSpace) GetApiUrl() string {
	return self.apiUrl
}

func (self *NetworkSpace) GetPlatformUrl() string {
	return self.platformUrl
}

// The family-pinned service urls (IPV6.md A9). The platform runs the
// provider's two family-pinned transports against connect-v4 and connect-v6
// (see connect.HeaderIpFamily); the api forms exist for diagnostics. Derived
// from the resolved urls by inserting the suffix on the service label, so
// `wss://connect.bringyour.com/secret` becomes
// `wss://connect-v4.bringyour.com/secret` and `g2-connect` becomes
// `g2-connect-v4`. Empty when the url has no dotted hostname to suffix — an
// ip literal, `localhost`, or a label that already carries a family suffix —
// in which case the pinned transports are disabled (HasPlatformFamilyUrls).

func (self *NetworkSpace) GetPlatformUrlV4() string {
	return familyServiceUrl(self.platformUrl, 4)
}

func (self *NetworkSpace) GetPlatformUrlV6() string {
	return familyServiceUrl(self.platformUrl, 6)
}

func (self *NetworkSpace) GetApiUrlV4() string {
	return familyServiceUrl(self.apiUrl, 4)
}

func (self *NetworkSpace) GetApiUrlV6() string {
	return familyServiceUrl(self.apiUrl, 6)
}

// HasPlatformFamilyUrls reports whether both family-pinned platform urls
// derive, which is the precondition for running the pinned transports.
func (self *NetworkSpace) HasPlatformFamilyUrls() bool {
	return self.GetPlatformUrlV4() != "" && self.GetPlatformUrlV6() != ""
}

// familyServiceUrl derives the family-pinned form of a resolved service url
// for ip version 4 or 6, or "" when there is no service label to suffix. The
// scheme, port and path (the env secret) are preserved verbatim.
func familyServiceUrl(serviceUrl string, ipVersion int) string {
	if ipVersion != 4 && ipVersion != 6 {
		return ""
	}
	serviceUrl = strings.TrimSpace(serviceUrl)
	if serviceUrl == "" {
		return ""
	}
	parsedUrl, err := url.Parse(serviceUrl)
	if err != nil || parsedUrl.Host == "" {
		return ""
	}
	hostName := parsedUrl.Hostname()
	if net.ParseIP(hostName) != nil {
		// an ip literal has no label to suffix
		return ""
	}
	label, domain, ok := strings.Cut(hostName, ".")
	if !ok || label == "" || domain == "" {
		return ""
	}
	if strings.HasSuffix(label, "-v4") || strings.HasSuffix(label, "-v6") {
		// already family-pinned by the operator: not a dual-stack space
		return ""
	}
	familyHostName := fmt.Sprintf("%s-v%d.%s", label, ipVersion, domain)
	if port := parsedUrl.Port(); port != "" {
		parsedUrl.Host = net.JoinHostPort(familyHostName, port)
	} else {
		parsedUrl.Host = familyHostName
	}
	return parsedUrl.String()
}

func (self *NetworkSpace) GetApi() *Api {
	return self.api
}

// SetControlIpFamilyPolicy sets the control-plane address family policy for
// this process and records it, so a relaunch comes back up under it.
//
// The entry point a developer ui uses when there is no Device -- signed out,
// or with the tunnel down. With a Device, use Device.SetControlIpFamilyPolicy
// instead: on ios that also carries the policy into the packet tunnel
// extension, which is the process that dials while the tunnel is up.
func (self *NetworkSpace) SetControlIpFamilyPolicy(policy int) {
	clamped := clampIpFamilyPolicy(policy)
	SetControlIpFamilyPolicy(clamped)
	if self.asyncLocalState != nil {
		self.asyncLocalState.serialAsync(func() error {
			return self.asyncLocalState.GetLocalState().SetControlIpFamilyPolicy(clamped)
		})
	}
}

// restoreControlIpFamilyPolicy applies this space's persisted control-plane ip
// family policy to this process, and reports whether there was one to apply.
//
// Only the manager calls this, and only once per manager (see
// `NetworkSpaceManager.restoreControlIpFamilyPolicyOnce`): the policy is
// process-global while the persisted copy is per-space, so restoring from
// every constructed space would let whichever space happened to be built last
// decide what the process dials under.
//
// The bool is what the manager's guard is spent on. A space with no local
// storage, or one that has never had a policy written, changes nothing here --
// and a guard spent on it would be a guard the space that DOES have a policy
// never gets.
func (self *NetworkSpace) restoreControlIpFamilyPolicy() bool {
	if self.asyncLocalState == nil {
		return false
	}
	_, applied := applyPersistedControlIpFamilyPolicy(self.asyncLocalState.GetLocalState(), self.log)
	return applied
}

func (self *NetworkSpace) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		_ = self.api.CloseAndWait(context.Background())
		// the network client is joined before the directory it writes into,
		// and both before the local state the directory saves through
		if self.extenderNetworkClient != nil {
			self.extenderNetworkClient.Close()
		}
		// the node writes into the directory too, so it is joined before it
		self.extenderNode.Close()
		if self.extenderDirectory != nil {
			self.extenderDirectory.Close()
		}
		if self.asyncLocalState != nil {
			_ = self.asyncLocalState.CloseAndWait(context.Background())
		}
		self.clientStrategy.Close()
	})
}

// Releases the API, local-state worker, and shared client strategy owned by an
// explicitly constructed headless network space. Manager-owned spaces are
// closed by their manager.
//
//gomobile:noexport
func (self *NetworkSpace) Close() {
	self.close()
}

func (self *NetworkSpace) ToJson() (string, error) {
	exportNetworkSpace := &ExportNetworkSpace{
		Key:    &self.key,
		Values: &self.values,
	}
	networkSpaceJsonBytes, err := json.Marshal(exportNetworkSpace)
	if err != nil {
		return "", err
	}
	return string(networkSpaceJsonBytes), nil
}

type NetworkSpaceUpdate interface {
	Update(values *NetworkSpaceValues)
}

type NetworkSpacesChangeListener interface {
	NetworkSpacesChanged()
}

type ActiveNetworkSpaceChangeListener interface {
	ActiveNetworkSpaceChanged(networkSpace *NetworkSpace)
}

type networkSpaceManagerState struct {
	NetworkSpaces []*networkSpaceState `json:"network_spaces"`
	Active        *NetworkSpaceKey     `json:"active,omitempty"`
}

type networkSpaceState struct {
	Key    NetworkSpaceKey    `json:"key"`
	Values NetworkSpaceValues `json:"values"`
}

type NetworkSpaceManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	storagePath string

	stateLock          sync.Mutex
	networkSpaces      map[NetworkSpaceKey]*NetworkSpace
	activeNetworkSpace *NetworkSpace
	closed             bool

	networkSpacesChangeListeners      *connect.CallbackList[NetworkSpacesChangeListener]
	activeNetworkSpaceChangeListeners *connect.CallbackList[ActiveNetworkSpaceChangeListener]

	// the control ip family policy is restored from ONE space, ONCE per
	// manager. See restoreControlIpFamilyPolicyOnce.
	//
	// Its own lock, not stateLock: `load` and `updateNetworkSpace` both call
	// the restore while holding stateLock, and sync.Mutex is not reentrant.
	controlIpFamilyPolicyRestoreLock sync.Mutex
	controlIpFamilyPolicyRestored    bool
}

// restoreControlIpFamilyPolicyOnce applies networkSpace's persisted
// control-plane ip family policy to this process, at most once per manager.
//
// Two properties this has to hold, and neither survives restoring at
// NetworkSpace construction:
//
//   - the ACTIVE space wins. The runtime policy is process-global while the
//     persisted copy lives under each space's own local storage. `load`
//     constructs every stored space before it selects the active one, so a
//     per-construction restore hands the process whichever space came last in
//     the stored slice. With a second api host configured alongside the
//     production one that is routinely the wrong space.
//   - a runtime set is not undone. `updateNetworkSpace` rebuilds a space on
//     every launch and on every space import, so a per-construction
//     restore re-imposes the persisted value over one an embedder had just set
//     through `SetControlIpFamilyPolicy`, with nothing in the logs to say why.
//
// Called with the space this manager is bound to: the active one wherever
// there is a selection, and otherwise the space just created or imported. The
// second case is the ios packet tunnel extension, which imports its space and
// never selects an active one -- gating purely on the active space would leave
// the extension dialing under Auto until the app's device rpc reached it.
//
// The guard is spent only when a policy was ACTUALLY applied. A space with
// nothing persisted applies nothing, so letting it spend the guard would let
// it decide the process's policy by silence: with a second api host
// configured alongside the production one, the bundled space with no policy is
// routinely the first one the restore sees, and it would leave the space that
// does have one unable to restore it for the rest of the session.
//
// Once a policy IS applied the guard is closed for good, which is the half
// this must not lose: `updateNetworkSpace` rebuilds a space on every launch
// and every space import, and a second apply there would re-impose the
// persisted value over one an embedder had just set through
// `SetControlIpFamilyPolicy`.
func (self *NetworkSpaceManager) restoreControlIpFamilyPolicyOnce(networkSpace *NetworkSpace) {
	if networkSpace == nil {
		return
	}

	self.controlIpFamilyPolicyRestoreLock.Lock()
	defer self.controlIpFamilyPolicyRestoreLock.Unlock()

	if self.controlIpFamilyPolicyRestored {
		return
	}
	self.controlIpFamilyPolicyRestored = networkSpace.restoreControlIpFamilyPolicy()
}

func NewNetworkSpaceManagerNoStorage() *NetworkSpaceManager {
	return NewNetworkSpaceManager("")
}

func NewNetworkSpaceManager(storagePath string) *NetworkSpaceManager {
	ctx := context.Background()

	return newNetworkSpaceManagerWithContext(ctx, storagePath)
}

func newNetworkSpaceManagerWithContext(ctx context.Context, storagePath string) *NetworkSpaceManager {
	cancelCtx, cancel := context.WithCancel(ctx)

	networkSpaceManager := &NetworkSpaceManager{
		ctx:                               cancelCtx,
		cancel:                            cancel,
		storagePath:                       storagePath,
		networkSpaces:                     map[NetworkSpaceKey]*NetworkSpace{},
		activeNetworkSpace:                nil,
		networkSpacesChangeListeners:      connect.NewCallbackList[NetworkSpacesChangeListener](),
		activeNetworkSpaceChangeListeners: connect.NewCallbackList[ActiveNetworkSpaceChangeListener](),
	}
	networkSpaceManager.load()
	return networkSpaceManager
}

func (self *NetworkSpaceManager) store() error {
	if self.storagePath == "" {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	networkSpaceStates := []*networkSpaceState{}
	for key, networkSpace := range self.networkSpaces {
		networkSpaceState := &networkSpaceState{
			Key:    key,
			Values: networkSpace.values,
		}
		networkSpaceStates = append(networkSpaceStates, networkSpaceState)
	}

	var activeKey *NetworkSpaceKey
	if self.activeNetworkSpace != nil {
		activeKey = &self.activeNetworkSpace.key
	}

	networkSpaceManagerState := &networkSpaceManagerState{
		NetworkSpaces: networkSpaceStates,
		Active:        activeKey,
	}

	networkSpaceManagerStateBytes, err := json.Marshal(networkSpaceManagerState)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(self.storagePath, ".network_spaces"), networkSpaceManagerStateBytes, LocalStorageFilePermissions)
}

func (self *NetworkSpaceManager) load() error {
	if self.storagePath == "" {
		return nil
	}

	networkSpaceManagerStateBytes, err := os.ReadFile(filepath.Join(self.storagePath, ".network_spaces"))
	if err != nil {
		return err
	}
	var storedState networkSpaceManagerState
	if err := json.Unmarshal(networkSpaceManagerStateBytes, &storedState); err != nil {
		return err
	}

	replacementNetworkSpaces := map[NetworkSpaceKey]*NetworkSpace{}
	replacedNetworkSpaces := []*NetworkSpace{}
	for _, networkSpaceState := range storedState.NetworkSpaces {
		replacement := newNetworkSpace(
			self.ctx,
			networkSpaceState.Key,
			networkSpaceState.Values,
			self.envStoragePath(&networkSpaceState.Key),
		)
		if replaced := replacementNetworkSpaces[networkSpaceState.Key]; replaced != nil {
			replacedNetworkSpaces = append(replacedNetworkSpaces, replaced)
		}
		replacementNetworkSpaces[networkSpaceState.Key] = replacement
	}
	var replacementActiveNetworkSpace *NetworkSpace
	if storedState.Active != nil {
		replacementActiveNetworkSpace = replacementNetworkSpaces[*storedState.Active]
	}

	previousNetworkSpaces := func() []*NetworkSpace {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		previous := slices.Collect(maps.Values(self.networkSpaces))
		self.networkSpaces = replacementNetworkSpaces
		self.activeNetworkSpace = replacementActiveNetworkSpace

		// AFTER the selection above, and still inside the manager constructor:
		// no listener can be registered yet and no Device exists, so nothing
		// has been able to make an api request. That is the whole point of
		// restoring here rather than at Device construction -- on a relaunch
		// the login call is the first request out, and for the user this
		// setting exists for it is the call that hangs.
		self.restoreControlIpFamilyPolicyOnce(self.activeNetworkSpace)

		return previous
	}()
	for _, networkSpace := range append(previousNetworkSpaces, replacedNetworkSpaces...) {
		networkSpace.close()
	}

	self.activeNetworkSpaceChanged(self.GetActiveNetworkSpace())
	return nil
}

func (self *NetworkSpaceManager) envStoragePath(key *NetworkSpaceKey) string {
	if self.storagePath == "" {
		return ""
	}
	// include host so multiple network servers can coexist without sharing auth state
	safeHost := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(key.HostName)
	if safeHost == "" || safeHost == "." || safeHost == ".." {
		safeHost = "default"
	}
	safeEnv := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(key.EnvName)
	if safeEnv == "" || safeEnv == "." || safeEnv == ".." {
		safeEnv = "default"
	}
	envStoragePath := filepath.Join(self.storagePath, "network_spaces", safeHost, safeEnv)

	// Best-effort migration: before host-scoped storage existed, state lived at
	// `network_spaces/<env>` (no host segment). If an install still has state
	// there and nothing has been written to the new host-scoped path yet, move
	// it over so upgrading users aren't silently signed out. This only ever
	// fires once - after the first successful rename, the legacy path no
	// longer exists for any subsequent host to (incorrectly) inherit.
	if _, err := os.Stat(envStoragePath); os.IsNotExist(err) {
		legacyEnvStoragePath := filepath.Join(self.storagePath, "network_spaces", safeEnv)
		if legacyInfo, err := os.Stat(legacyEnvStoragePath); err == nil && legacyInfo.IsDir() {
			if err := os.MkdirAll(filepath.Dir(envStoragePath), LocalStorageDirectoryPermissions); err == nil {
				// ignore errors - this is best-effort, and the normal
				// MkdirAll below still guarantees envStoragePath exists
				_ = os.Rename(legacyEnvStoragePath, envStoragePath)
			}
		}
	}

	if err := os.MkdirAll(envStoragePath, LocalStorageDirectoryPermissions); err != nil {
		panic(err)
	}
	return envStoragePath
}

func (self *NetworkSpaceManager) AddNetworkSpacesChangeListener(listener NetworkSpacesChangeListener) Sub {
	callbackId := self.networkSpacesChangeListeners.Add(listener)
	return newSub(func() {
		self.networkSpacesChangeListeners.Remove(callbackId)
	})
}

func (self *NetworkSpaceManager) networkSpacesChanged() {
	for _, listener := range self.networkSpacesChangeListeners.Get() {
		connect.HandleError(func() {
			listener.NetworkSpacesChanged()
		})
	}
}

func (self *NetworkSpaceManager) AddActiveNetworkSpaceChangeListener(listener ActiveNetworkSpaceChangeListener) Sub {
	callbackId := self.activeNetworkSpaceChangeListeners.Add(listener)
	return newSub(func() {
		self.activeNetworkSpaceChangeListeners.Remove(callbackId)
	})
}

func (self *NetworkSpaceManager) activeNetworkSpaceChanged(networkSpace *NetworkSpace) {
	for _, listener := range self.activeNetworkSpaceChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ActiveNetworkSpaceChanged(networkSpace)
		})
	}
}

func (self *NetworkSpaceManager) GetActiveNetworkSpace() *NetworkSpace {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.activeNetworkSpace
}

func (self *NetworkSpaceManager) SetActiveNetworkSpace(networkSpace *NetworkSpace) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.closed {
			return
		}
		if self.activeNetworkSpace == networkSpace {
			return
		}

		if networkSpace != nil {
			currentNetworkSpace, ok := self.networkSpaces[networkSpace.key]
			if !ok || currentNetworkSpace != networkSpace {
				return
			}
		}

		self.activeNetworkSpace = networkSpace
		set = true
	}()
	if set {
		// the first selection is a restore point too: a fresh install, or one
		// whose `.network_spaces` was unreadable, has no active space when the
		// manager is built and gets one here, still before any api request.
		// It is also the point an in-session space switch reaches, so a space
		// whose policy has never been restored still gets to restore it.
		// The once guard is what keeps a re-selection made AFTER a policy was
		// applied from re-imposing a persisted one over one set at runtime.
		self.restoreControlIpFamilyPolicyOnce(self.GetActiveNetworkSpace())
		self.store()
		self.activeNetworkSpaceChanged(self.GetActiveNetworkSpace())
	}
}

func (self *NetworkSpaceManager) GetNetworkSpaces() *NetworkSpaceList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	networkSpaceList := NewNetworkSpaceList()
	for _, networkSpace := range self.networkSpaces {
		networkSpaceList.Add(networkSpace)
	}
	return networkSpaceList
}

func (self *NetworkSpaceManager) GetNetworkSpace(key *NetworkSpaceKey) *NetworkSpace {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.networkSpaces[*key]
}

func (self *NetworkSpaceManager) UpdateNetworkSpace(key *NetworkSpaceKey, callback NetworkSpaceUpdate) *NetworkSpace {
	return self.updateNetworkSpace(key, callback.Update)
}

// UpdateNetworkSpaceValues sets the network space for key to values and returns
// it. Unlike UpdateNetworkSpace, it takes the values directly instead of via a
// mutation callback, so it maps cleanly across the c abi where callback
// arguments cannot be mutated in place (the cgo desktop SDK). A nil values
// leaves the existing values unchanged.
func (self *NetworkSpaceManager) UpdateNetworkSpaceValues(key *NetworkSpaceKey, values *NetworkSpaceValues) *NetworkSpace {
	return self.updateNetworkSpace(key, func(v *NetworkSpaceValues) {
		if values != nil {
			*v = *values
		}
	})
}

func (self *NetworkSpaceManager) updateNetworkSpace(key *NetworkSpaceKey, callback func(values *NetworkSpaceValues)) *NetworkSpace {
	var copyValues NetworkSpaceValues

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if networkSpace, ok := self.networkSpaces[*key]; ok {
			copyValues = networkSpace.values
		}
	}()

	callback(&copyValues)

	copyNetworkSpace := newNetworkSpace(self.ctx, *key, copyValues, self.envStoragePath(key))
	activeSet := false
	installed := false
	var previousNetworkSpace *NetworkSpace
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.closed {
			return
		}
		if networkSpace, ok := self.networkSpaces[*key]; ok {
			previousNetworkSpace = networkSpace
			if self.activeNetworkSpace == networkSpace {
				self.activeNetworkSpace = copyNetworkSpace
				activeSet = true
			}
		}
		self.networkSpaces[*key] = copyNetworkSpace
		installed = true

		// only when this manager has no active space at all -- the ios packet
		// tunnel extension, which imports its space and never selects one, and
		// the app path where `.network_spaces` was missing or unreadable and
		// the bundled space is created right here. With an active space
		// selected the restore has already run against it and the once guard
		// makes this a no-op, which is what stops every launch and every
		// space import from re-imposing a persisted policy.
		if self.activeNetworkSpace == nil {
			self.restoreControlIpFamilyPolicyOnce(copyNetworkSpace)
		}
	}()
	if !installed {
		copyNetworkSpace.close()
		return nil
	}
	if previousNetworkSpace != nil {
		previousNetworkSpace.close()
	}
	self.store()
	self.networkSpacesChanged()
	if activeSet {
		self.activeNetworkSpaceChanged(self.GetActiveNetworkSpace())
	}
	return self.GetNetworkSpace(key)
}

func (self *NetworkSpaceManager) RemoveNetworkSpace(networkSpace *NetworkSpace) bool {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		// cannot remove active or bundled
		if self.activeNetworkSpace == networkSpace || networkSpace.values.Bundled {
			return
		}

		currentNetworkSpace, ok := self.networkSpaces[networkSpace.key]
		if !ok || currentNetworkSpace != networkSpace {
			return
		}

		delete(self.networkSpaces, networkSpace.key)
		changed = true
	}()

	if changed {
		networkSpace.close()
		self.store()
		self.networkSpacesChanged()
	}
	return changed
}

func (self *NetworkSpaceManager) Close() {
	self.cancel()
	networkSpaces := func() []*NetworkSpace {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.closed {
			return nil
		}
		self.closed = true
		networkSpaces := slices.Collect(maps.Values(self.networkSpaces))
		self.networkSpaces = map[NetworkSpaceKey]*NetworkSpace{}
		self.activeNetworkSpace = nil
		return networkSpaces
	}()
	for _, networkSpace := range networkSpaces {
		networkSpace.close()
	}
}

func (self *NetworkSpaceManager) ImportNetworkSpaceFromJson(networkSpaceJson string) (*NetworkSpace, error) {
	exportNetworkSpace := ExportNetworkSpace{}
	err := json.Unmarshal([]byte(networkSpaceJson), &exportNetworkSpace)
	if err != nil {
		return nil, err
	}
	networkSpace := self.updateNetworkSpace(exportNetworkSpace.Key, func(values *NetworkSpaceValues) {
		*values = *exportNetworkSpace.Values
	})
	return networkSpace, nil
}
