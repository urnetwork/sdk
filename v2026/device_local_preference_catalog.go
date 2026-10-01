// The closed user-preference catalog reuses existing per-space records. It is
// not authentication, native tunnel intent, or a general storage registry.
package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

const localPreferenceUnknownMessage = "unknown local preference"

// Ordering applies transport/security before any providing or consuming path.
// Non-manual provide control owns its derived mode; Load applies raw mode only
// in Manual. Missing records keep caller defaults.
var localPreferenceCatalog = []struct {
	name     string
	file     string
	optional bool
}{
	{name: "log-verbosity", file: ".log_verbosity", optional: true},
	{name: "control-ip-family-policy", file: controlIpFamilyPolicyFileName},
	{name: "transport-settings", file: ".transport_settings"},
	{name: "provider-transport-settings", file: ".provider_transport_settings"},
	{name: "performance-profile", file: ".performance_profile"},
	{name: "route-local", file: ".route_local-2"},
	{name: "blocker-enabled", file: ".blocker_enabled"},
	{name: "block-action-overrides", file: ".block_action_overrides"},
	{name: "dns-resolver-settings", file: ".dns_resolver_settings"},
	{name: "routing-tier", file: ".routing_tier"},
	{name: "vpn-interface-while-offline", file: ".vpn_interface_while_offline"},
	{name: "allow-foreground", file: ".allow_foreground"},
	{name: "can-show-rating-dialog", file: ".can_show_rating_dialog", optional: true},
	{name: "can-prompt-intro-funnel", file: ".can_prompt_intro_funnel", optional: true},
	{name: "can-refer", file: ".can_refer", optional: true},
	{name: "provide-network-mode", file: ".provide_network_mode"},
	{name: "provide-mode", file: ".provide_mode"},
	{name: "provide-control-mode", file: ".provide_control_mode"},
}

// Unknown input never becomes a filesystem name or a successful absence.
func localPreferenceFile(name string) (string, bool) {
	for _, preference := range localPreferenceCatalog {
		if preference.name == name {
			return preference.file, true
		}
	}
	return "", false
}

func knownLocalPreference(name string) bool {
	if name == "connect-location" || name == "default-location" {
		return true
	}
	_, known := localPreferenceFile(name)
	return known
}

// Presence is meaningful only with an empty error for this fixed catalog name.
func (self *DeviceLocalLoadResult) GetHasPreference(name string) bool {
	switch name {
	case "connect-location":
		return self.hasConnectLocation
	case "default-location":
		return self.hasDefaultLocation
	default:
		return self.preferencePresent[name]
	}
}

// Unknown names yield a fixed error without echoing caller input. Optional
// errors are not absence and never authorize a default or policy replacement.
func (self *DeviceLocalLoadResult) GetPreferenceError(name string) string {
	if !knownLocalPreference(name) {
		return localPreferenceUnknownMessage
	}
	if name == "default-location" {
		return self.defaultError
	}
	return self.preferenceErrors[name]
}

// Logs only fixed names after owned locks release. Required failures identify
// the blocked branch; optional unavailability remains distinct from absence.
// Arbitrary underlying error text never reaches the diagnostic sink.
func (self *DeviceLocal) logLocalPreferenceLoad(result *DeviceLocalLoadResult, err error) {
	if self.log == nil {
		return
	}
	if err != nil {
		preference, stage := "preferences", "apply"
		switch err.Error() {
		case localPreferencesUnsupportedMessage:
			preference, stage = "store", "unsupported"
		case localPreferencesClosedMessage:
			preference, stage = "owner", "closed"
		case localAuthSnapshotSupersededMessage:
			preference, stage = "auth-owner", "superseded"
		case "read preference auth owner":
			preference, stage = "auth-owner", "read"
		case "load connect location":
			preference, stage = "connect-location", "read"
		default:
			for _, entry := range localPreferenceCatalog {
				if err.Error() == "load "+entry.name {
					preference, stage = entry.name, "read"
					break
				}
				if err.Error() == "apply "+entry.name {
					preference, stage = entry.name, "apply"
					break
				}
			}
		}
		self.log.Warningf("[local-state] load result=failed preference=%s stage=%s", preference, stage)
		return
	}
	if result != nil {
		var unavailable []string
		if result.defaultError != "" {
			unavailable = append(unavailable, "default-location")
		}
		for _, entry := range localPreferenceCatalog {
			if result.preferenceErrors[entry.name] != "" {
				unavailable = append(unavailable, entry.name)
			}
		}
		if len(unavailable) != 0 {
			self.log.Warningf("[local-state] load result=completed optional_unavailable=%s", strings.Join(unavailable, ","))
		}
	}
}

// A read-only, decoded catalog observation held until final owner admission.
type localPreferenceObservation struct {
	name  string
	value any
}

// Caller holds the current accepted auth boundary. Required policy failures
// stop before any preference/route adoption, even with no current consumer:
// providing, local routing, DNS and permission paths can still be affected.
func (self *LocalState) loadPreferenceCatalogWithLock(result *DeviceLocalLoadResult) ([]localPreferenceObservation, error) {
	var observations []localPreferenceObservation
	for _, preference := range localPreferenceCatalog {
		data, present, err := self.readPreferenceBytesWithLock(preference.file)
		var value any
		if err == nil && present {
			value, err = decodeLocalPreference(preference.name, data)
		}
		if err != nil {
			stage := "load " + preference.name
			if !preference.optional {
				return nil, localStorageStageError(stage, err)
			}
			result.preferenceErrors[preference.name] = stage
			continue
		}
		result.preferencePresent[preference.name] = present
		if present {
			observations = append(observations, localPreferenceObservation{name: preference.name, value: value})
		}
	}
	return observations, nil
}

// Mirrors checked location read semantics, including readable symlinks and
// absent leaf versus unavailable parent, without treating read failure as nil.
func (self *LocalState) readPreferenceBytesWithLock(name string) ([]byte, bool, error) {
	if self.ctx.Err() != nil {
		return nil, false, errors.New("preference storage owner is closed")
	}
	path := filepath.Join(self.localStorageDir, name)
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, errors.New("inspect saved preference")
		}
		parent, parentErr := os.Stat(self.localStorageDir)
		if parentErr != nil || !parent.IsDir() {
			return nil, false, errors.New("saved preference directory is unavailable")
		}
		return nil, false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return nil, false, errors.New("resolve saved preference")
		}
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("saved preference is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, errors.New("read saved preference")
	}
	return data, true, nil
}

// Preserve record encodings, including the intro-funnel's legacy timestamp and
// existing scalar/enum values. Scalar records require complete tokens: fmt's
// scanners can accept arbitrary booleans or a valid prefix of damaged state.
func decodeLocalPreference(name string, data []byte) (any, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		switch name {
		case "can-show-rating-dialog", "can-prompt-intro-funnel", "can-refer":
			// Legacy JSON boolean UI records accepted null as false.
		default:
			// Existing critical setters remove optional records or write a
			// complete value/empty list; null is not their reset encoding.
			return nil, errors.New("null saved preference")
		}
	}
	switch name {
	case "provide-mode", "log-verbosity", "control-ip-family-policy":
		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, errors.New("invalid saved integer preference")
		}
		return value, nil
	case "provide-network-mode":
		fields := strings.Fields(string(data))
		if len(fields) != 1 {
			return nil, errors.New("invalid saved network preference")
		}
		return fields[0], nil
	case "route-local", "blocker-enabled":
		switch strings.ToLower(strings.TrimSpace(string(data))) {
		case "true", "t", "1":
			return true, nil
		case "false", "f", "0":
			return false, nil
		default:
			return nil, errors.New("invalid saved boolean preference")
		}
	case "can-show-rating-dialog", "can-refer", "vpn-interface-while-offline", "allow-foreground":
		var value bool
		err := json.Unmarshal(data, &value)
		return value, err
	case "can-prompt-intro-funnel":
		var value bool
		if err := json.Unmarshal(data, &value); err == nil {
			return value, nil
		}
		var lastPrompted time.Time
		if err := json.Unmarshal(data, &lastPrompted); err != nil {
			return nil, err
		}
		return time.Since(lastPrompted) > 5*24*time.Hour, nil
	case "provide-control-mode":
		var value string
		err := json.Unmarshal(data, &value)
		return value, err
	case "routing-tier":
		var value int
		err := json.Unmarshal(data, &value)
		return value, err
	case "performance-profile":
		var value PerformanceProfile
		err := json.Unmarshal(data, &value)
		return &value, err
	case "transport-settings", "provider-transport-settings":
		var value TransportSettings
		err := json.Unmarshal(data, &value)
		return normalizeTransportSettings(&value, name == "provider-transport-settings"), err
	case "dns-resolver-settings":
		var value DnsResolverSettings
		err := json.Unmarshal(data, &value)
		return &value, err
	case "block-action-overrides":
		value := NewBlockActionOverrideList()
		if err := json.Unmarshal(data, value); err != nil {
			return nil, err
		}
		for _, override := range value.getAll() {
			if override == nil {
				return nil, errors.New("invalid saved block action override")
			}
		}
		return value, nil
	default:
		return nil, errors.New(localPreferenceUnknownMessage)
	}
}

// Copies mutable inputs before either serialization or live installation.
func (self *DeviceLocal) ownLocalCatalogValue(name string, value any) any {
	switch name {
	case "performance-profile":
		return clonePerformanceProfile(self.hostedSafePerformanceProfile(value.(*PerformanceProfile)))
	case "transport-settings":
		return normalizeTransportSettings(value.(*TransportSettings), false)
	case "provider-transport-settings":
		return normalizeTransportSettings(value.(*TransportSettings), true)
	case "dns-resolver-settings":
		return cloneDnsResolverSettings(value.(*DnsResolverSettings))
	case "block-action-overrides":
		return self.ownBlockActionOverrides(value.(*BlockActionOverrideList))
	case "log-verbosity":
		return clampLogVerbosity(value.(int))
	case "control-ip-family-policy":
		return clampIpFamilyPolicy(value.(int))
	default:
		return value
	}
}

// The same format as the public legacy LocalState setter, but with an atomic
// commit and checked removal error. It runs inside existing owner serialization.
func (self *LocalState) saveCatalogPreferenceWithLock(name string, value any) error {
	file, known := localPreferenceFile(name)
	if !known {
		return errors.New(localPreferenceUnknownMessage)
	}
	var data []byte
	var err error
	switch name {
	case "provide-mode", "log-verbosity", "control-ip-family-policy":
		data = []byte(fmt.Sprintf("%d", value.(int)))
	case "provide-network-mode":
		data = []byte(value.(string))
	case "route-local", "blocker-enabled":
		data = []byte(fmt.Sprintf("%t", value.(bool)))
	case "performance-profile":
		if value.(*PerformanceProfile) != nil {
			data, err = json.Marshal(value)
		}
	default:
		data, err = json.Marshal(value)
	}
	if err != nil {
		return errors.New("encode saved preference")
	}
	return self.writePreferenceBytesWithLock(file, data)
}

// Existing void setters report through immutable result/getter/listener. RPC
// calls this returned-error boundary rather than guessing from the last result.
func (self *DeviceLocal) setLocalCatalogPreference(name string, value any) error {
	notify, err := self.setLocalCatalogPreferenceDeferred(name, value)
	if notify != nil {
		notify()
	}
	return err
}

// Sync retains returned work until its outer service lock is released.
func (self *DeviceLocal) setLocalCatalogPreferenceDeferred(name string, value any) (func(), error) {
	if _, known := localPreferenceFile(name); !known {
		return nil, errors.New(localPreferenceUnknownMessage)
	}
	if name == "dns-resolver-settings" && value.(*DnsResolverSettings) == nil {
		// Historical nil DNS setter is no command, not a request to disable.
		return nil, nil
	}
	value = self.ownLocalCatalogValue(name, value)
	return self.mutateLocalPreferenceDeferred(name, func(localState *LocalState) error {
		return localState.saveCatalogPreferenceWithLock(name, value)
	}, func() (func(), error) {
		if self.autoSave && (name == "control-ip-family-policy" || name == "log-verbosity") {
			return self.applyOwnedGlobalPreferenceWithLock(name, value.(int))
		}
		return self.applyLocalCatalogPreferenceWithLock(name, value)
	})
}

// These two side effects belong to the process, not the old DeviceLocal.
// Re-admit immediately around their fixed callback-free scalar stores so an
// admitted old operation cannot overwrite a replacement owner's global value.
// No transport construction, arbitrary setter, logging or notification occurs
// within this auth boundary. Off-mode explicit setters retain their old path.
func (self *DeviceLocal) applyOwnedGlobalPreferenceWithLock(name string, value int) (func(), error) {
	if self.testingBeforeGlobalPreferenceApply != nil {
		self.testingBeforeGlobalPreferenceApply(name)
	}
	err := self.withOwnedPreferenceStore(func(*LocalState) error {
		switch name {
		case "control-ip-family-policy":
			SetControlIpFamilyPolicy(value)
			return nil
		case "log-verbosity":
			// Flag registration is immutable at runtime, as required by the
			// existing verbosity reader/writer. Reject custom flag callbacks;
			// the known glog implementation only locks and updates its cache/level.
			levelFlag := flag.Lookup("v")
			if levelFlag == nil {
				return errors.New("unsupported local verbosity flag")
			}
			if _, ok := levelFlag.Value.(*glog.Level); !ok {
				return errors.New("unsupported local verbosity flag")
			}
			return SetLogVerbosity(value)
		default:
			return errors.New(localPreferenceUnknownMessage)
		}
	})
	return nil, err
}

// Load calls only apply, never the saving setter, even with autosave enabled.
func (self *DeviceLocal) applyLocalCatalogPreferenceWithLock(name string, value any) (func(), error) {
	switch name {
	case "performance-profile":
		return self.applyPerformanceProfileWithLock(value.(*PerformanceProfile))
	case "provide-mode":
		return self.applyProvideModeWithLock(value.(int))
	case "provide-control-mode":
		return self.applyProvideControlModeWithLock(value.(string))
	case "provide-network-mode":
		return self.applyProvideNetworkModeWithLock(value.(string))
	case "route-local":
		return self.applyRouteLocalWithLock(value.(bool))
	case "blocker-enabled":
		return self.applyBlockerEnabledWithLock(value.(bool))
	case "block-action-overrides":
		return self.applyBlockActionOverridesWithLock(value.(*BlockActionOverrideList))
	case "dns-resolver-settings":
		return self.applyDnsResolverSettingsWithLock(value.(*DnsResolverSettings))
	case "transport-settings":
		return self.applyTransportSettingsWithLock(value.(*TransportSettings))
	case "provider-transport-settings":
		return self.applyProviderTransportSettingsWithLock(value.(*TransportSettings))
	case "routing-tier":
		return self.applyRoutingTierWithLock(value.(int))
	case "log-verbosity":
		return self.applyLogVerbosityWithLock(value.(int))
	case "control-ip-family-policy":
		return self.applyControlIpFamilyPolicyWithLock(value.(int))
	case "vpn-interface-while-offline":
		return self.applyVpnInterfaceWhileOfflineWithLock(value.(bool))
	case "allow-foreground":
		return self.applyAllowForegroundWithLock(value.(bool))
	case "can-show-rating-dialog":
		return self.applyCanShowRatingDialogWithLock(value.(bool))
	case "can-prompt-intro-funnel":
		return self.applyCanPromptIntroFunnelWithLock(value.(bool))
	case "can-refer":
		return self.applyCanReferWithLock(value.(bool))
	default:
		return nil, errors.New(localPreferenceUnknownMessage)
	}
}

// Reuse the existing canonical deep-copy conversion; no RPC is performed.
// Invalid ids are skipped and later equal ids replace earlier ones, as before.
func (self *DeviceLocal) ownBlockActionOverrides(overrides *BlockActionOverrideList) *BlockActionOverrideList {
	owned := NewBlockActionOverrideList()
	for _, override := range toBlockActionOverrideList(newBlockActionOverridesRpc(overrides)).getAll() {
		owned.Add(self.hostedSafeBlockActionOverride(override))
	}
	return owned
}

// Add/remove are read-modify-write preference operations. Compute their final
// value under preference serialization before either saving or installing it.
func (self *DeviceLocal) changeBlockActionOverride(override *BlockActionOverride, removeId *Id) error {
	if override == nil && removeId == nil {
		return nil
	}
	if override != nil && override.OverrideId == nil {
		return nil
	}
	var added *BlockActionOverrideRpc
	if override != nil {
		added = newBlockActionOverrideRpc(override)
	}
	var id *Id
	if removeId != nil {
		id = newId(removeId.toConnectId())
	}
	var final *BlockActionOverrideList
	prepare := func() {
		if final != nil {
			return
		}
		current := newBlockActionOverridesRpc(self.GetBlockActionOverrides())
		if added != nil {
			current = removeBlockActionOverrideRpc(current, added.OverrideId)
			current = append(current, added)
		} else {
			current = removeBlockActionOverrideRpc(current, id.toConnectId())
		}
		final = self.ownBlockActionOverrides(toBlockActionOverrideList(current))
	}
	notify, err := self.mutateLocalPreferenceDeferred("block-action-overrides", func(localState *LocalState) error {
		prepare()
		return localState.saveCatalogPreferenceWithLock("block-action-overrides", final)
	}, func() (func(), error) {
		prepare()
		return self.applyBlockActionOverridesWithLock(final)
	})
	if notify != nil {
		notify()
	}
	return err
}
