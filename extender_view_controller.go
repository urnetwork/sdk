//go:build !ios_extension

package sdk

// extender_view_controller.go — the extender section of the account screen
// (EXTENDER.md K5, K6, K7).
//
// One controller for four things the apps would otherwise each implement:
// the status the panel renders, the three settings of K6, the share payload
// of K7, and the import that reads one back. Encoding, decoding, the foreign
// host rule and the settings write all live here, so android, apple, windows
// and linux each render a payload they never parse themselves.
//
// The status reads the DEVICE, not the space: on ios the packet tunnel
// extension holds the directory whose dials the panel describes, and
// `Device.GetExtenderStatus` is the read-through that crosses to it. The
// settings read and write the space, which both processes share through the
// app group directory and which the extension picks up at its next start.
//
// Errors are localization key ids rather than sentences, so each app maps them
// to its own strings; the apps already carry these two keys.
//
// All methods are safe for concurrent use.

import (
	"context"
	"strings"
	"sync"

	"github.com/urnetwork/connect"
)

// The error ids an import can answer with (K7, K8).
const (
	// The text is not a share payload: a wrong prefix, a bad base64 body, a
	// version this build does not read, or a malformed address.
	ExtenderImportErrorInvalid = "import_extenders_invalid"
	// The payload names another operator's network and the importer did not
	// ask to take its settings.
	ExtenderImportErrorForeignHost = "import_extenders_foreign_host"
)

// The three settings of K6 as the account screen edits them, each with the
// effective value and whether that value is the derived default. An empty
// field means the default, so the ui shows the effective value as a
// placeholder rather than filling the box with it.
type ExtenderSettings struct {
	DnsName        string
	DnsNameDefault bool
	// Never carries the env secret path: the url becomes a multiaddr (F1).
	GossipUrl        string
	GossipUrlDefault bool
	// The manual bootstrap hosts, in the configured order. Empty is a space
	// that bootstraps from dns and the network alone.
	Hosts *StringList
	// The host this space's extender network is keyed by (B2), which a share
	// names and an import is judged against. Empty for a space that runs no
	// extender network at all.
	NetworkHost string
	// The trust anchor in force, hex encoded, and whether it is the bundled
	// table's rather than a configured or imported one (B4).
	RootPublicKeys        *StringList
	RootPublicKeysDefault bool
}

// The payload of a share (K7).
type ExtenderShareResult struct {
	// The one-line text form, `ur-ext:1:` and the base64url body. Empty for a
	// space with no extender network to share.
	Text string
	// Addresses in the payload.
	Count int
	// Whether the operator settings block is included.
	IncludesSettings bool
}

// What a scanned, chosen or pasted payload turns out to be, before anything is
// applied (K7). The apps show this as the import confirmation.
type ExtenderShareDecodeResult struct {
	Ok bool
	// One of the ExtenderImportError ids, empty when Ok.
	Error string
	// The operator network the payload names.
	NetworkHost string
	// True when that network is not this space's, which is the case an import
	// refuses unless the settings are taken too.
	ForeignHost bool
	Count       int
	HasSettings bool
	// The extender dns name the settings block names -- the operator host the
	// importer is asked to confirm before it replaces this space's. Empty
	// without a settings block.
	SettingsHost string
}

// The outcome of an import (K7).
type ExtenderImportResult struct {
	Ok bool
	// One of the ExtenderImportError ids, empty when Ok.
	Error string
	// Addresses the directory did not already know. An address it did keeps
	// everything it has, including where it came from, so a re-import counts
	// zero rather than resetting the set.
	ImportedCount int
}

type ExtenderViewControllerListener interface {
	ExtenderStatusChanged(status *ExtenderStatus)
}

type ExtenderViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	extenderStatusChangedSub Sub

	extenderViewControllerListeners *connect.CallbackList[ExtenderViewControllerListener]
}

func newExtenderViewController(ctx context.Context, device Device) *ExtenderViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ExtenderViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		extenderViewControllerListeners: connect.NewCallbackList[ExtenderViewControllerListener](),
	}
}

// The extender network of the device's space (K5). Never nil.
func (self *ExtenderViewController) GetStatus() *ExtenderStatus {
	return self.device.GetExtenderStatus()
}

func (self *ExtenderViewController) AddStatusListener(
	listener ExtenderViewControllerListener,
) Sub {
	callbackId := self.extenderViewControllerListeners.Add(listener)
	return newSub(func() {
		self.extenderViewControllerListeners.Remove(callbackId)
	})
}

// ExtenderStatusChangeListener. re-emitted to the ui listeners
func (self *ExtenderViewController) ExtenderStatusChanged(status *ExtenderStatus) {
	self.extenderStatusChanged(status)
}

func (self *ExtenderViewController) extenderStatusChanged(status *ExtenderStatus) {
	for _, listener := range self.extenderViewControllerListeners.Get() {
		connect.HandleError(func() {
			listener.ExtenderStatusChanged(status)
		})
	}
}

// The effective settings of the device's space (K6).
func (self *ExtenderViewController) GetSettings() *ExtenderSettings {
	return extenderSettings(self.device.GetNetworkSpace())
}

// SetSettings replaces the three edited values and returns what they resolve
// to (K6). An empty field means the derived default, so clearing a box is how
// a user goes back to it. The space's network client and node restart in
// place; the space, this controller and the device bound to it stay valid.
//
// On ios this writes the app group values the packet tunnel extension reads at
// its next start, which is what the app tells the user.
func (self *ExtenderViewController) SetSettings(
	dnsName string,
	gossipUrl string,
	hosts *StringList,
) *ExtenderSettings {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return extenderSettings(nil)
	}
	var extenderHosts []string
	if hosts != nil {
		extenderHosts = hosts.getAll()
	}
	// trimmed here rather than only where they are read, so a field a user
	// blanked out is stored as empty -- which is what "the default" is -- and
	// the persisted document never carries whitespace
	networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderDnsName = strings.TrimSpace(dnsName)
		values.GossipUrl = strings.TrimSpace(gossipUrl)
		values.ExtenderHosts = extenderHosts
	})
	return extenderSettings(self.device.GetNetworkSpace())
}

// BuildShare renders this space's addresses as the payload of K7: active
// first, then the rest of the usable ones, then the manual addresses, at most
// 48. Keys and records are never shared. `includeSettings` adds the operator
// block, which an importer applies only when it asks to.
func (self *ExtenderViewController) BuildShare(includeSettings bool) *ExtenderShareResult {
	result := &ExtenderShareResult{}
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return result
	}
	values := networkSpace.valuesCopy()
	networkHost := extenderNetworkHostName(&networkSpace.key, &values)
	if networkHost == "" {
		// a space that keys no extender network has nothing to share
		return result
	}
	var rootKeys *connect.ExtenderRootKeySet
	if networkSpace.extenderDirectory != nil {
		// the anchor in force rather than the configured one: a hello answer
		// replaces it, and a share should carry what this install trusts now
		rootKeys = networkSpace.extenderDirectory.RootKeys()
	}
	share := connect.BuildExtenderShare(
		networkSpace.extenderDirectory,
		networkHost,
		networkSpace.GetExtenderDnsName(),
		networkSpace.GetGossipUrl(),
		rootKeys,
		includeSettings,
		connect.ExtenderShareDefaultAddressCount,
	)
	text, err := connect.EncodeExtenderShare(share)
	if err != nil {
		deviceLog(self.device).Infof("[extendervc]share err = %s\n", err)
		return result
	}
	result.Text = text
	result.Count = len(share.Addresses)
	result.IncludesSettings = share.Settings != nil
	return result
}

// DecodeShare reads a scanned, chosen or pasted payload and reports what it
// would do, applying nothing (K7).
func (self *ExtenderViewController) DecodeShare(text string) *ExtenderShareDecodeResult {
	share, err := connect.DecodeExtenderShare(text)
	if err != nil {
		return &ExtenderShareDecodeResult{Error: ExtenderImportErrorInvalid}
	}
	result := &ExtenderShareDecodeResult{
		Ok:          true,
		NetworkHost: share.NetworkHost,
		Count:       len(connect.ExtenderShareAddresses(share)),
		ForeignHost: !self.networkHostAllowed(share.NetworkHost),
	}
	if share.Settings != nil {
		result.HasSettings = true
		result.SettingsHost = share.Settings.DnsName
	}
	return result
}

// ImportShare applies a payload (K7). Every address becomes an unverified
// bootstrap entry sourced `import`, which upgrades the moment a signed record
// naming it arrives over the feed -- that is what bounds a hostile code.
//
// A payload naming another operator's network is refused unless `useSettings`
// is chosen, which also replaces this space's extender dns name, gossip url
// and root keys with the payload's. The first hello over the platform's pinned
// tls replaces the root keys again.
func (self *ExtenderViewController) ImportShare(
	text string,
	useSettings bool,
) *ExtenderImportResult {
	share, err := connect.DecodeExtenderShare(text)
	if err != nil {
		return &ExtenderImportResult{Error: ExtenderImportErrorInvalid}
	}
	// A foreign payload is taken only together with its settings, and only
	// when it has some: `useSettings` is the user confirming a replacement of
	// the dns name, gossip url and anchor, and a payload with no settings
	// block has none to replace. Its addresses would then be unverifiable
	// forever -- this space accepts records for its own network host only.
	if !self.networkHostAllowed(share.NetworkHost) && (!useSettings || share.Settings == nil) {
		return &ExtenderImportResult{Error: ExtenderImportErrorForeignHost}
	}
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return &ExtenderImportResult{Error: ExtenderImportErrorInvalid}
	}
	result := &ExtenderImportResult{
		Ok: true,
		ImportedCount: connect.ApplyExtenderShare(
			networkSpace.extenderDirectory,
			share,
			connect.ExtenderSourceImport,
		),
	}
	// the settings are applied after the addresses so the restarted client
	// finds them already in the directory rather than re-resolving into an
	// empty one
	if useSettings && share.Settings != nil {
		rootPublicKeyHexes := connect.ExtenderShareRootKeyHexes(share)
		networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
			values.ExtenderDnsName = share.Settings.DnsName
			values.GossipUrl = share.Settings.GossipUrl
			values.ExtenderRootPublicKeys = rootPublicKeyHexes
		})
	}
	return result
}

// Reports whether a payload's network host is one this space accepts records
// for (B2): its own namespaces and the host its extender network is keyed by.
// A space that keys no network accepts none, so every payload reads as foreign
// there and an import of one is a deliberate choice.
func (self *ExtenderViewController) networkHostAllowed(networkHost string) bool {
	networkSpace := self.device.GetNetworkSpace()
	if networkSpace == nil {
		return false
	}
	values := networkSpace.valuesCopy()
	return connect.ExtenderNetworkHostAllowed(
		networkHost,
		extenderNetworkHosts(&networkSpace.key, &values)...,
	)
}

// The effective settings of one space, or the empty set when there is no space
// to read.
func extenderSettings(networkSpace *NetworkSpace) *ExtenderSettings {
	settings := &ExtenderSettings{
		Hosts:          NewStringList(),
		RootPublicKeys: NewStringList(),
	}
	if networkSpace == nil {
		return settings
	}
	values := networkSpace.valuesCopy()
	// the default flags read the CONFIGURED value, trimmed: a field holding
	// only whitespace is a blank field, and a ui told otherwise would print
	// the derived default while claiming it was overridden
	configured := func(value string) bool {
		return strings.TrimSpace(value) != ""
	}
	settings.DnsName = ExtenderDnsName(&networkSpace.key, &values)
	settings.DnsNameDefault = !configured(values.ExtenderDnsName)
	settings.GossipUrl = GossipUrl(&networkSpace.key, &values)
	settings.GossipUrlDefault = !configured(values.GossipUrl)
	settings.Hosts.addAll(ExtenderHosts(&values)...)
	settings.NetworkHost = extenderNetworkHostName(&networkSpace.key, &values)
	settings.RootPublicKeys.addAll(ExtenderRootPublicKeys(&networkSpace.key, &values)...)
	settings.RootPublicKeysDefault = true
	for _, rootPublicKey := range values.ExtenderRootPublicKeys {
		if configured(rootPublicKey) {
			settings.RootPublicKeysDefault = false
		}
	}
	return settings
}

func (self *ExtenderViewController) Start() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.extenderStatusChangedSub == nil {
			self.extenderStatusChangedSub = self.device.AddExtenderStatusChangeListener(self)
		}
	}()
	// seed the ui with the current state
	self.extenderStatusChanged(self.GetStatus())
}

func (self *ExtenderViewController) Stop() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.extenderStatusChangedSub != nil {
		self.extenderStatusChangedSub.Close()
		self.extenderStatusChangedSub = nil
	}
}

func (self *ExtenderViewController) Close() {
	deviceLog(self.device).Info("[extendervc]close")
	self.cancel()
	self.Stop()
}
