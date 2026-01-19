package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/urnetwork/connect/v2026"
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

type NetExtenderAutoConfigure struct {
	DnsIp            string `json:"dns_ip,omitempty"`
	ExtenderHostname string `json:"extender_hostname,omitempty"`
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

	// custom extender
	// this overrides any auto discovered extenders
	NetExtender              *NetExtender              `json:"net_extender,omitempty"`
	NetExtenderAutoConfigure *NetExtenderAutoConfigure `json:"net_extender_auto_configure,omitempty"`
}

func ServiceUrl(key *NetworkSpaceKey, values *NetworkSpaceValues, scheme string, service string) string {
	var hostName string
	if values.MigrationHostName != "" {
		hostName = values.MigrationHostName
	} else {
		hostName = key.HostName
	}

	var serviceHostName string
	switch key.EnvName {
	case "main", "":
		serviceHostName = fmt.Sprintf("%s.%s", service, hostName)
	default:
		serviceHostName = fmt.Sprintf("%s-%s.%s", key.EnvName, service, hostName)
	}

	serviceUrl := fmt.Sprintf("%s://%s", scheme, serviceHostName)
	if values.EnvSecret != "" {
		serviceUrl = fmt.Sprintf("%s/%s", serviceUrl, values.EnvSecret)
	}

	return serviceUrl
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
	ctx    context.Context
	cancel context.CancelFunc

	key         NetworkSpaceKey
	values      NetworkSpaceValues
	storagePath string

	apiUrl      string
	platformUrl string

	clientStrategy  *connect.ClientStrategy
	asyncLocalState *AsyncLocalState
	api             *Api
}

func newNetworkSpace(
	ctx context.Context,
	key NetworkSpaceKey,
	values NetworkSpaceValues,
	storagePath string,
) *NetworkSpace {
	cancelCtx, cancel := context.WithCancel(ctx)

	apiUrl := ServiceUrl(&key, &values, "https", "api")
	platformUrl := ServiceUrl(&key, &values, "wss", "connect")

	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ExposeServerIps = values.NetExposeServerIps
	clientStrategySettings.ExposeServerHostNames = values.NetExposeServerHostNames

	clientStrategy := connect.NewClientStrategy(cancelCtx, clientStrategySettings)

	if values.NetExtender != nil {
		extenderIpSecrets := map[netip.Addr]string{}
		if ip, err := netip.ParseAddr(values.NetExtender.Ip); err == nil {
			extenderIpSecrets[ip] = values.NetExtender.Secret
		}
		clientStrategy.SetCustomExtenders(extenderIpSecrets)
	}

	var asyncLocalState *AsyncLocalState
	if storagePath != "" {
		asyncLocalState = NewAsyncLocalState(storagePath)
	}

	api := newApi(cancelCtx, clientStrategy, apiUrl)

	return &NetworkSpace{
		ctx:    cancelCtx,
		cancel: cancel,

		key:         key,
		values:      values,
		storagePath: storagePath,

		apiUrl:      apiUrl,
		platformUrl: platformUrl,

		clientStrategy:  clientStrategy,
		asyncLocalState: asyncLocalState,
		api:             api,
	}
}

// gomobile:ignore
func NewPlatformNetworkSpace(
	ctx context.Context,
	env string,
	host string,
) *NetworkSpace {
	key := NetworkSpaceKey{
		EnvName:  env,
		HostName: host,
	}
	values := NetworkSpaceValues{}
	return newNetworkSpace(ctx, key, values, "")
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
	byJwt = ""
	return
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

func (self *NetworkSpace) GetNetExtenderAutoConfigure() *NetExtenderAutoConfigure {
	return self.values.NetExtenderAutoConfigure
}

func (self *NetworkSpace) GetSsoGoogle() bool {
	return self.values.SsoGoogle
}

func (self *NetworkSpace) GetAsyncLocalState() *AsyncLocalState {
	return self.asyncLocalState
}

func (self *NetworkSpace) GetApiUrl() string {
	return self.apiUrl
}

func (self *NetworkSpace) GetPlatformUrl() string {
	return self.platformUrl
}

func (self *NetworkSpace) GetApi() *Api {
	return self.api
}

func (self *NetworkSpace) close() {
	self.cancel()
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

	networkSpacesChangeListeners      *connect.CallbackList[NetworkSpacesChangeListener]
	activeNetworkSpaceChangeListeners *connect.CallbackList[ActiveNetworkSpaceChangeListener]
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

func (self *NetworkSpaceManager) load() (returnErr error) {
	if self.storagePath == "" {
		return nil
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		networkSpaceManagerStateBytes, err := os.ReadFile(filepath.Join(self.storagePath, ".network_spaces"))
		if err != nil {
			returnErr = err
			return
		}

		var networkSpaceManagerState networkSpaceManagerState
		err = json.Unmarshal(networkSpaceManagerStateBytes, &networkSpaceManagerState)
		if err != nil {
			returnErr = err
			return
		}

		for _, networkSpace := range self.networkSpaces {
			networkSpace.close()
		}
		self.networkSpaces = map[NetworkSpaceKey]*NetworkSpace{}

		for _, networkSpaceState := range networkSpaceManagerState.NetworkSpaces {
			self.networkSpaces[networkSpaceState.Key] = newNetworkSpace(
				self.ctx,
				networkSpaceState.Key,
				networkSpaceState.Values,
				self.envStoragePath(&networkSpaceState.Key),
			)
		}
		if networkSpaceManagerState.Active != nil {
			if networkSpace, ok := self.networkSpaces[*networkSpaceManagerState.Active]; ok {
				self.activeNetworkSpace = networkSpace
			}
			// else active key not found
		}
	}()
	if returnErr != nil {
		return
	}

	self.activeNetworkSpaceChanged(self.GetActiveNetworkSpace())
	return
}

func (self *NetworkSpaceManager) envStoragePath(key *NetworkSpaceKey) string {
	if self.storagePath == "" {
		return ""
	}
	envStoragePath := filepath.Join(self.storagePath, "network_spaces", key.EnvName)
	if err := os.MkdirAll(envStoragePath, LocalStorageFilePermissions); err != nil {
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

		if self.activeNetworkSpace == networkSpace {
			return
		}

		if networkSpace != nil {
			if _, ok := self.networkSpaces[networkSpace.key]; !ok {
				// does not exist
				return
			}
		}

		self.activeNetworkSpace = networkSpace
		set = true
	}()
	if set {
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

	activeSet := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		copyNetworkSpace := newNetworkSpace(self.ctx, *key, copyValues, self.envStoragePath(key))
		if networkSpace, ok := self.networkSpaces[*key]; ok {
			if self.activeNetworkSpace == networkSpace {
				self.activeNetworkSpace = copyNetworkSpace
				activeSet = true
			}
			networkSpace.close()
		}
		self.networkSpaces[*key] = copyNetworkSpace
	}()
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

		if _, ok := self.networkSpaces[networkSpace.key]; !ok {
			return
		}

		delete(self.networkSpaces, networkSpace.key)
		changed = true
	}()

	if changed {
		self.store()
		self.networkSpacesChanged()
	}
	return changed
}

func (self *NetworkSpaceManager) Close() {
	self.cancel()
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
