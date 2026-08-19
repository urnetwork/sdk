package sdk

import (
	"bytes"
	"context"
	"fmt"
	"time"

	// "io"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

const AsyncQueueSize = 32

const LocalStorageDirectoryPermissions = 0700
const LocalStorageFilePermissions = 0600

type ByJwt struct {
	UserId      *Id
	NetworkName string
	NetworkId   *Id
	GuestMode   bool
	Pro         bool
}

type LocalState struct {
	ctx    context.Context
	cancel context.CancelFunc

	localStorageDir string
	authStateLock   sync.Mutex

	// providerPriorsRetention is stamped into every saved provider-priors
	// envelope (see persistedProviderPriors.Retention) and defaults to
	// providerPriorsStaleAfter; 0 means unlimited. Unexported field, not a
	// gomobile boundary -- there is no exported setter for it in this task.
	providerPriorsRetention time.Duration
}

// setRefreshedByJwt persists a token rotation without changing the device
// instance that is already paired with a running local/remote device. Some
// hosted processes intentionally seed only by_jwt + instance_id, so deriving a
// new instance through SetByClientJwt would recreate the original reconnect
// bug. The caller supplies the immutable instance of the live Device instead.
// New login/device identity still goes through SetByJwt + SetByClientJwt, and
// logout still clears the instance.
func (self *LocalState) setRefreshedByJwt(byJwt string, instanceId *Id) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if instanceId == nil {
			return false, errors.New("cannot persist refreshed JWT without a device instance")
		}
		instanceIdString := instanceId.String()
		if state.ByJwt == byJwt && state.ByClientJwt == byJwt &&
			state.InstanceId == instanceIdString {
			return false, nil
		}
		state.ByJwt = byJwt
		state.ByClientJwt = byJwt
		state.InstanceId = instanceIdString
		return true, nil
	})
}

func newLocalState(ctx context.Context, localStorageHome string) *LocalState {
	// FIXME local storage dir is always a sub dir of the passed dir
	// localStorageHome/.by
	localStorageDir := filepath.Join(localStorageHome, ".by")
	err := os.MkdirAll(localStorageDir, LocalStorageDirectoryPermissions)
	if err != nil {
		panic(err)
	}
	cancelCtx, cancel := context.WithCancel(ctx)

	localState := &LocalState{
		ctx:                     cancelCtx,
		cancel:                  cancel,
		localStorageDir:         localStorageDir,
		providerPriorsRetention: providerPriorsStaleAfter,
	}
	// Best-effort eager migration. Reads remain compatible with legacy state if
	// the first write cannot complete (for example, a temporarily read-only
	// filesystem), and the next authenticated mutation retries the migration.
	localState.authStateLock.Lock()
	_, _ = localState.loadAuthStateLocked()
	localState.authStateLock.Unlock()
	return localState
}

func (self *LocalState) GetByJwt() string {
	state, err := self.loadAuthState()
	if err != nil {
		return ""
	}
	return state.ByJwt
}

func (self *LocalState) ParseByJwt() (*ByJwt, error) {
	byJwtStr := self.GetByJwt()
	if byJwtStr == "" {
		return nil, errors.New("Not found.")
	}

	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byJwtStr, gojwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	claims := token.Claims.(gojwt.MapClaims)

	byJwt := &ByJwt{}

	if userIdStr, ok := claims["user_id"]; ok {
		if userId, err := ParseId(userIdStr.(string)); err == nil {
			byJwt.UserId = userId
		}
	}
	if networkName, ok := claims["network_name"]; ok {
		byJwt.NetworkName = networkName.(string)
	}
	if networkIdStr, ok := claims["network_id"]; ok {
		if networkId, err := ParseId(networkIdStr.(string)); err == nil {
			byJwt.NetworkId = networkId
		}
	}
	if guestMode, ok := claims["guest_mode"]; ok {
		byJwt.GuestMode = guestMode.(bool)
	}

	if isPro, ok := claims["pro"]; ok {
		byJwt.Pro = isPro.(bool)
	}

	return byJwt, nil
}

// clears `byClientJwt` and `instanceId`
func (self *LocalState) SetByJwt(byJwt string) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		if state.ByJwt == byJwt {
			return false, nil
		}
		state.ByJwt = byJwt
		state.ByClientJwt = ""
		state.InstanceId = ""
		return true, nil
	})
}

func (self *LocalState) GetByClientJwt() string {
	state, err := self.loadAuthState()
	if err != nil {
		return ""
	}
	return state.ByClientJwt
}

// if `byClientJwt` is set, sets a new `instanceId`; othewwise, clears `instanceId`
func (self *LocalState) SetByClientJwt(byClientJwt string) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		// Equality is a no-op only when the paired instance is coherent too. An
		// interrupted/legacy installation can retain the client JWT while losing
		// its instance; treating that as unchanged recreates the reconnect bug on
		// every launch instead of repairing it once.
		if state.ByClientJwt == byClientJwt &&
			((byClientJwt == "" && state.InstanceId == "") ||
				(byClientJwt != "" && state.InstanceId != "")) {
			return false, nil
		}
		state.ByClientJwt = byClientJwt
		if byClientJwt == "" {
			state.InstanceId = ""
		} else {
			state.InstanceId = newId(connect.NewId()).String()
		}
		return true, nil
	})
}

func (self *LocalState) GetInstanceId() *Id {
	state, err := self.loadAuthState()
	if err != nil || state.InstanceId == "" {
		return nil
	}
	instanceId, err := ParseId(state.InstanceId)
	if err != nil {
		return nil
	}
	return instanceId
}

func (self *LocalState) SetInstanceId(instanceId *Id) error {
	return self.updateAuthState(func(state *persistedLocalAuthState) (bool, error) {
		instanceIdString := ""
		if instanceId != nil {
			instanceIdString = instanceId.String()
		}
		if state.InstanceId == instanceIdString {
			return false, nil
		}
		state.InstanceId = instanceIdString
		return true, nil
	})
}

// auto, always, never
func (self *LocalState) SetProvideMode(provideMode ProvideMode) error {
	path := filepath.Join(self.localStorageDir, ".provide_mode")
	provideModeBytes := []byte(fmt.Sprintf("%d", provideMode))
	return os.WriteFile(path, provideModeBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetProvideMode() ProvideMode {
	path := filepath.Join(self.localStorageDir, ".provide_mode")
	if provideModeBytes, err := os.ReadFile(path); err == nil {
		var provideMode ProvideMode
		if _, err := fmt.Sscanf(string(provideModeBytes), "%d", &provideMode); err == nil {
			return provideMode
		}
	}
	return ProvideModeNone
}

// wifi, cell, etc
func (self *LocalState) SetProvideNetworkMode(provideNetworkMode ProvideNetworkMode) error {
	path := filepath.Join(self.localStorageDir, ".provide_network_mode")
	provideNetworkModeBytes := []byte(fmt.Sprintf("%s", provideNetworkMode))
	return os.WriteFile(path, provideNetworkModeBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetProvideNetworkMode() ProvideNetworkMode {
	path := filepath.Join(self.localStorageDir, ".provide_network_mode")
	if provideNetworkModeBytes, err := os.ReadFile(path); err == nil {
		var provideNetworkMode ProvideNetworkMode
		if _, err := fmt.Sscanf(string(provideNetworkModeBytes), "%s", &provideNetworkMode); err == nil {
			return provideNetworkMode
		}
	}
	return ProvideNetworkModeWiFi
}

func (self *LocalState) SetRouteLocal(routeLocal bool) error {
	path := filepath.Join(self.localStorageDir, ".route_local-2")
	routeLocalBytes := []byte(fmt.Sprintf("%t", routeLocal))
	return os.WriteFile(path, routeLocalBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetRouteLocal() bool {
	path := filepath.Join(self.localStorageDir, ".route_local-2")
	if routeLocalBytes, err := os.ReadFile(path); err == nil {
		var routeLocal bool
		if _, err := fmt.Sscanf(string(routeLocalBytes), "%t", &routeLocal); err == nil {
			return routeLocal
		}
	}
	return true
}

func (self *LocalState) SetBlockerEnabled(blockerEnabled bool) error {
	path := filepath.Join(self.localStorageDir, ".blocker_enabled")
	blockerEnabledBytes := []byte(fmt.Sprintf("%t", blockerEnabled))
	return os.WriteFile(path, blockerEnabledBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetBlockerEnabled() bool {
	path := filepath.Join(self.localStorageDir, ".blocker_enabled")
	if blockerEnabledBytes, err := os.ReadFile(path); err == nil {
		var blockerEnabled bool
		if _, err := fmt.Sscanf(string(blockerEnabledBytes), "%t", &blockerEnabled); err == nil {
			return blockerEnabled
		}
	}
	return false
}

func (self *LocalState) SetConnectLocation(connectLocation *ConnectLocation) error {
	path := filepath.Join(self.localStorageDir, ".connect_location")
	if connectLocation == nil {
		os.Remove(path)
		return nil
	} else {
		connectLocationBytes, err := json.Marshal(connectLocation)
		if err != nil {
			return err
		}
		return os.WriteFile(path, connectLocationBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetConnectLocation() *ConnectLocation {
	path := filepath.Join(self.localStorageDir, ".connect_location")
	if connectLocationBytes, err := os.ReadFile(path); err == nil {
		var connectLocation ConnectLocation
		if err := json.Unmarshal(connectLocationBytes, &connectLocation); err == nil {
			return &connectLocation
		}
	}
	return nil
}

func (self *LocalState) SetBlockActionOverrides(blockActionOverrides *BlockActionOverrideList) error {
	path := filepath.Join(self.localStorageDir, ".block_action_overrides")
	if blockActionOverrides == nil {
		os.Remove(path)
		return nil
	} else {
		blockActionOverridesBytes, err := json.Marshal(blockActionOverrides)
		if err != nil {
			return err
		}
		return os.WriteFile(path, blockActionOverridesBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetBlockActionOverrides() *BlockActionOverrideList {
	path := filepath.Join(self.localStorageDir, ".block_action_overrides")
	if blockActionOverridesBytes, err := os.ReadFile(path); err == nil {
		blockActionOverrides := NewBlockActionOverrideList()
		if err := json.Unmarshal(blockActionOverridesBytes, blockActionOverrides); err == nil {
			return blockActionOverrides
		}
	}
	return nil
}

func (self *LocalState) SetDnsResolverSettings(dnsResolverSettings *DnsResolverSettings) error {
	path := filepath.Join(self.localStorageDir, ".dns_resolver_settings")
	if dnsResolverSettings == nil {
		os.Remove(path)
		return nil
	} else {
		dnsResolverSettingsBytes, err := json.Marshal(dnsResolverSettings)
		if err != nil {
			return err
		}
		return os.WriteFile(path, dnsResolverSettingsBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) setTransportSettings(filename string, settings *TransportSettings, provider bool) error {
	path := filepath.Join(self.localStorageDir, filename)
	if settings == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	settingsBytes, err := json.Marshal(normalizeTransportSettings(settings, provider))
	if err != nil {
		return err
	}
	return os.WriteFile(path, settingsBytes, LocalStorageFilePermissions)
}

func (self *LocalState) getTransportSettings(filename string, provider bool) *TransportSettings {
	settingsBytes, err := os.ReadFile(filepath.Join(self.localStorageDir, filename))
	if err != nil {
		return nil
	}
	var settings TransportSettings
	if err := json.Unmarshal(settingsBytes, &settings); err != nil {
		return nil
	}
	return normalizeTransportSettings(&settings, provider)
}

// SetTransportSettings persists the client carrier policy for the next process.
func (self *LocalState) SetTransportSettings(settings *TransportSettings) error {
	return self.setTransportSettings(".transport_settings", settings, false)
}

// GetTransportSettings returns nil when no valid client policy is stored.
func (self *LocalState) GetTransportSettings() *TransportSettings {
	return self.getTransportSettings(".transport_settings", false)
}

// SetProviderTransportSettings persists the provider carrier policy separately
// so changing one direction never silently changes the other.
func (self *LocalState) SetProviderTransportSettings(settings *TransportSettings) error {
	return self.setTransportSettings(".provider_transport_settings", settings, true)
}

// GetProviderTransportSettings returns nil when no valid provider policy is stored.
func (self *LocalState) GetProviderTransportSettings() *TransportSettings {
	return self.getTransportSettings(".provider_transport_settings", true)
}

// dohServerScoresStaleAfter discards a persisted DoH server score snapshot older than this:
// server rankings are fairly stable, but a weeks-old snapshot should not bias a fresh session.
const dohServerScoresStaleAfter = 7 * 24 * time.Hour

// persistedDohServerScores is the on-disk form of the per-DoH-server success scores
// (connect.DohSettings.ServerStatsSeed), stamped with the save time for staleness.
type persistedDohServerScores struct {
	SavedAt time.Time          `json:"saved_at"`
	Scores  map[string]float64 `json:"scores"`
}

// GetDohServerScores returns the persisted per-DoH-server success scores from the last
// session (nil if none, unreadable, or stale), used to seed the resolver fan-out order so
// the first lookups after launch pick the known-fastest server. See
// connect.DohSettings.ServerStatsSeed.
func (self *LocalState) getDohServerScores() map[string]float64 {
	path := filepath.Join(self.localStorageDir, ".doh_server_scores")
	scoresBytes, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var persisted persistedDohServerScores
	if err := json.Unmarshal(scoresBytes, &persisted); err != nil {
		return nil
	}
	if persisted.SavedAt.IsZero() || dohServerScoresStaleAfter < time.Since(persisted.SavedAt) {
		return nil
	}
	return persisted.Scores
}

// SetDohServerScores persists the per-DoH-server success scores (nil/empty removes).
func (self *LocalState) setDohServerScores(scores map[string]float64) error {
	path := filepath.Join(self.localStorageDir, ".doh_server_scores")
	if len(scores) == 0 {
		os.Remove(path)
		return nil
	}
	scoresBytes, err := json.Marshal(&persistedDohServerScores{
		SavedAt: time.Now(),
		Scores:  scores,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, scoresBytes, LocalStorageFilePermissions)
}

// providerPriorsStaleAfter discards a persisted provider-priors snapshot
// (connect.ProviderPriors.Snapshot) older than this by default: provider
// identities churn over weeks, and a stale snapshot would bias a fresh
// session's placement toward exits that are no longer representative. The
// owner chose a 90-day default with an "unlimited" opt-out -- see
// persistedProviderPriors.Retention, which is what getProviderPriors
// actually honors (a zero Retention skips the staleness check entirely),
// so a snapshot's own saved retention survives a later change to this
// constant.
const providerPriorsStaleAfter = 90 * 24 * time.Hour

// persistedProviderPriors is the on-disk form of the coarse per-provider
// routing memory (connect.ProviderPriors.Snapshot), stamped with the save
// time and the retention window in force at save time.
type persistedProviderPriors struct {
	SavedAt   time.Time                        `json:"saved_at"`
	Retention time.Duration                    `json:"retention"`
	Priors    map[string]connect.ProviderPrior `json:"priors"`
}

// getProviderPriors returns the persisted provider priors from the last
// session (nil if none, unreadable, malformed, or stale), used to seed
// connect.ProviderPriors so routing bias survives a restart. A zero
// Retention on the saved envelope means unlimited: the staleness check is
// skipped entirely.
func (self *LocalState) getProviderPriors() map[string]connect.ProviderPrior {
	path := filepath.Join(self.localStorageDir, ".provider_priors")
	priorsBytes, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var persisted persistedProviderPriors
	if err := json.Unmarshal(priorsBytes, &persisted); err != nil {
		return nil
	}
	// IsZero is a VALIDITY check on the envelope (was SavedAt even set?),
	// separate from and evaluated before the retention/age comparison below
	// -- it does not conflict with "Retention == 0 skips the staleness
	// check." Without it, syntactically valid but truncated/tampered JSON
	// missing saved_at reads as SavedAt's zero value, and with Retention
	// also 0 (unlimited) the staleness comparison never even runs, so
	// corrupt data would be trusted forever in exactly the configuration
	// meant to keep real data forever. Mirrors getDohServerScores's
	// `persisted.SavedAt.IsZero() || <stale>` guard.
	if persisted.SavedAt.IsZero() {
		return nil
	}
	if persisted.Retention != 0 && persisted.Retention < time.Since(persisted.SavedAt) {
		return nil
	}
	return persisted.Priors
}

// setProviderPriors persists the coarse per-provider routing memory
// (nil/empty removes), stamping the envelope with self.providerPriorsRetention
// (defaults to providerPriorsStaleAfter; 0 means unlimited).
func (self *LocalState) setProviderPriors(priors map[string]connect.ProviderPrior) error {
	path := filepath.Join(self.localStorageDir, ".provider_priors")
	if len(priors) == 0 {
		os.Remove(path)
		return nil
	}
	priorsBytes, err := json.Marshal(&persistedProviderPriors{
		SavedAt:   time.Now(),
		Retention: self.providerPriorsRetention,
		Priors:    priors,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, priorsBytes, LocalStorageFilePermissions)
}

// localStatePriorsStore implements connect.PriorsStore by delegating to the
// provider-priors dot-file above, so a connect.ProviderPriors instance can
// persist across restarts. Mirrors localStateWindowIdentityStore's shape
// (see window_identity_store.go): a thin adapter holding just the LocalState
// it delegates to.
type localStatePriorsStore struct {
	localState *LocalState
}

func newLocalStatePriorsStore(localState *LocalState) *localStatePriorsStore {
	return &localStatePriorsStore{localState: localState}
}

func (self *localStatePriorsStore) Load() map[string]connect.ProviderPrior {
	return self.localState.getProviderPriors()
}

func (self *localStatePriorsStore) Save(priors map[string]connect.ProviderPrior) error {
	return self.localState.setProviderPriors(priors)
}

// var _ connect.PriorsStore = ... is a compile-time assertion that
// localStatePriorsStore satisfies connect.PriorsStore. Nothing in shipped
// code assigns this adapter to the interface yet (its consumer, the multi
// client wiring, lands in a later task), so without this line the compiler
// never checks the signatures line up -- only a manual read confirmed it
// here. Kept even though there is no other precedent for this pattern in
// the package, specifically because this implementation ships ahead of its
// consumer.
var _ connect.PriorsStore = (*localStatePriorsStore)(nil)

func (self *LocalState) GetDnsResolverSettings() *DnsResolverSettings {
	path := filepath.Join(self.localStorageDir, ".dns_resolver_settings")
	if dnsResolverSettingsBytes, err := os.ReadFile(path); err == nil {
		var dnsResolverSettings DnsResolverSettings
		if err := json.Unmarshal(dnsResolverSettingsBytes, &dnsResolverSettings); err == nil {
			return &dnsResolverSettings
		}
	}
	return nil
}

func (self *LocalState) SetDefaultLocation(connectLocation *ConnectLocation) error {
	path := filepath.Join(self.localStorageDir, ".default_location")
	if connectLocation == nil {
		os.Remove(path)
		return nil
	} else {
		defaultLocationBytes, err := json.Marshal(connectLocation)
		if err != nil {
			return err
		}
		return os.WriteFile(path, defaultLocationBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetDefaultLocation() *ConnectLocation {
	path := filepath.Join(self.localStorageDir, ".default_location")
	if connectLocationBytes, err := os.ReadFile(path); err == nil {
		var connectLocation ConnectLocation
		if err := json.Unmarshal(connectLocationBytes, &connectLocation); err == nil {
			return &connectLocation
		}
	}
	return nil
}

func (self *LocalState) SetProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) error {
	path := filepath.Join(self.localStorageDir, ".provide_secret_keys")
	if provideSecretKeyList == nil {
		os.Remove(path)
		return nil
	} else {
		provideSecretKeysBytes, err := json.Marshal(provideSecretKeyList)
		if err != nil {
			return err
		}
		return os.WriteFile(path, provideSecretKeysBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetProvideSecretKeys() *ProvideSecretKeyList {
	path := filepath.Join(self.localStorageDir, ".provide_secret_keys")
	if connectLocationBytes, err := os.ReadFile(path); err == nil {
		var provideSecretKeys ProvideSecretKeyList
		if err := json.Unmarshal(connectLocationBytes, &provideSecretKeys); err == nil {
			return &provideSecretKeys
		}
	}
	return nil
}

type deviceLocalKeyMaterialStorage struct {
	ClientKeySeed            []byte `json:"client_key_seed,omitempty"`
	ProvideTlsCertificatePem []byte `json:"provide_tls_certificate_pem,omitempty"`
	ProvideTlsPrivateKeyPem  []byte `json:"provide_tls_private_key_pem,omitempty"`
}

func (self *LocalState) SetDeviceLocalKeyMaterial(keyMaterial *DeviceLocalKeyMaterial) error {
	path := filepath.Join(self.localStorageDir, ".device_local_key_material")
	if keyMaterial == nil || keyMaterial.IsEmpty() {
		os.Remove(path)
		return nil
	}

	keyMaterialBytes, err := json.Marshal(deviceLocalKeyMaterialStorage{
		ClientKeySeed:            keyMaterial.GetClientKeySeed(),
		ProvideTlsCertificatePem: keyMaterial.GetProvideTlsCertificatePem(),
		ProvideTlsPrivateKeyPem:  keyMaterial.GetProvideTlsPrivateKeyPem(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, keyMaterialBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetDeviceLocalKeyMaterial() *DeviceLocalKeyMaterial {
	path := filepath.Join(self.localStorageDir, ".device_local_key_material")
	if keyMaterialBytes, err := os.ReadFile(path); err == nil {
		var keyMaterial deviceLocalKeyMaterialStorage
		if err := json.Unmarshal(keyMaterialBytes, &keyMaterial); err == nil {
			deviceLocalKeyMaterial := NewDeviceLocalKeyMaterial(
				keyMaterial.ClientKeySeed,
				keyMaterial.ProvideTlsCertificatePem,
				keyMaterial.ProvideTlsPrivateKeyPem,
			)
			if !deviceLocalKeyMaterial.IsEmpty() {
				return deviceLocalKeyMaterial
			}
		}
	}
	return nil
}

func (self *LocalState) SetCanShowRatingDialog(canShowRatingDialog bool) error {
	path := filepath.Join(self.localStorageDir, ".can_show_rating_dialog")
	canShowRatingDialogBytes, err := json.Marshal(canShowRatingDialog)
	if err != nil {
		return err
	}
	return os.WriteFile(path, canShowRatingDialogBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetCanShowRatingDialog() bool {
	path := filepath.Join(self.localStorageDir, ".can_show_rating_dialog")
	if canShowRatingDialogBytes, err := os.ReadFile(path); err == nil {
		var canShowRatingDialog bool
		if err := json.Unmarshal(canShowRatingDialogBytes, &canShowRatingDialog); err == nil {
			return canShowRatingDialog
		}
	}
	return true
}

func (self *LocalState) SetIntroFunnelLastPrompted() error {

	now := time.Now()

	path := filepath.Join(self.localStorageDir, ".can_prompt_intro_funnel")
	lastPromptedBytes, err := json.Marshal(now)
	if err != nil {
		return err
	}
	return os.WriteFile(path, lastPromptedBytes, LocalStorageFilePermissions)
}

func (self *LocalState) SetCanPromptIntroFunnel(canPrompt bool) error {
	path := filepath.Join(self.localStorageDir, ".can_prompt_intro_funnel")
	canPromptBytes, err := json.Marshal(canPrompt)
	if err != nil {
		return err
	}
	return os.WriteFile(path, canPromptBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetCanPromptIntroFunnel() bool {
	path := filepath.Join(self.localStorageDir, ".can_prompt_intro_funnel")

	if intoFunnelTimeLastPromptedBytes, err := os.ReadFile(path); err == nil {
		var canPrompt bool
		if err := json.Unmarshal(intoFunnelTimeLastPromptedBytes, &canPrompt); err == nil {
			return canPrompt
		}

		var intoFunnelTimeLastPrompted time.Time
		if err := json.Unmarshal(intoFunnelTimeLastPromptedBytes, &intoFunnelTimeLastPrompted); err == nil {

			now := time.Now().UTC()

			timePassed := now.Sub(intoFunnelTimeLastPrompted)

			return timePassed.Hours() > 24*5

		}
	}
	return true
}

func (self *LocalState) SetCanRefer(canRefer bool) error {
	path := filepath.Join(self.localStorageDir, ".can_refer")
	canReferBytes, err := json.Marshal(canRefer)
	if err != nil {
		return err
	}
	return os.WriteFile(path, canReferBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetCanRefer() bool {
	path := filepath.Join(self.localStorageDir, ".can_refer")
	if canReferBytes, err := os.ReadFile(path); err == nil {
		var canRefer bool
		if err := json.Unmarshal(canReferBytes, &canRefer); err == nil {
			return canRefer
		}
	}
	return false
}

func (self *LocalState) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) error {
	path := filepath.Join(self.localStorageDir, ".vpn_interface_while_offline")
	vpnInterfaceWhileOfflineBytes, err := json.Marshal(vpnInterfaceWhileOffline)
	if err != nil {
		return err
	}
	return os.WriteFile(path, vpnInterfaceWhileOfflineBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetVpnInterfaceWhileOffline() bool {
	path := filepath.Join(self.localStorageDir, ".vpn_interface_while_offline")
	if vpnInterfaceWhileOfflineBytes, err := os.ReadFile(path); err == nil {
		var vpnInterfaceWhileOffline bool
		if err := json.Unmarshal(vpnInterfaceWhileOfflineBytes, &vpnInterfaceWhileOffline); err == nil {
			return vpnInterfaceWhileOffline
		}
	}
	return false
}

func (self *LocalState) SetProvideControlMode(mode ProvideControlMode) error {
	path := filepath.Join(self.localStorageDir, ".provide_control_mode")
	provideControlModeBytes, err := json.Marshal(mode)
	if err != nil {
		return err
	}
	return os.WriteFile(path, provideControlModeBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetProvideControlMode() ProvideControlMode {
	path := filepath.Join(self.localStorageDir, ".provide_control_mode")
	if provideControlModeBytes, err := os.ReadFile(path); err == nil {
		var provideControlMode ProvideControlMode
		if err := json.Unmarshal(provideControlModeBytes, &provideControlMode); err == nil {
			return provideControlMode
		}
	}
	// providing is opt-in: a user with no stored choice defaults to never
	return ProvideControlModeNever
}

func (self *LocalState) SetPerformanceProfile(profile *PerformanceProfile) error {
	path := filepath.Join(self.localStorageDir, ".performance_profile")
	if profile == nil {
		os.Remove(path)
		return nil
	} else {
		profileBytes, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		// App view models reconstruct equivalent value objects while
		// resuming. Avoid an identical filesystem write independently of the
		// live-device no-op guards; exact JSON equality preserves the caller's
		// persisted representation while removing needless flash I/O.
		if currentBytes, readErr := os.ReadFile(path); readErr == nil &&
			bytes.Equal(currentBytes, profileBytes) {
			return nil
		}
		return os.WriteFile(path, profileBytes, LocalStorageFilePermissions)
	}
}

func (self *LocalState) GetPerformanceProfile() *PerformanceProfile {
	path := filepath.Join(self.localStorageDir, ".performance_profile")
	if performanceProfileBytes, err := os.ReadFile(path); err == nil {
		var performanceProfile PerformanceProfile
		if err := json.Unmarshal(performanceProfileBytes, &performanceProfile); err == nil {
			return &performanceProfile
		}
	}
	return nil
}

// SetRoutingTier persists the RoutingTier dial (see routing_tier.go), the
// same shape as SetPerformanceProfile: a plain JSON-encoded value in its own
// dotfile under the local storage dir. Stored as a bare int, matching the
// gomobile-safe type SetRoutingTier takes on DeviceLocal.
func (self *LocalState) SetRoutingTier(tier int) error {
	path := filepath.Join(self.localStorageDir, ".routing_tier")
	tierBytes, err := json.Marshal(tier)
	if err != nil {
		return err
	}
	return os.WriteFile(path, tierBytes, LocalStorageFilePermissions)
}

// GetRoutingTier reads back the persisted tier. Unset or unreadable (fresh
// install, corrupt file) both read as RoutingTierOff -- the fail-safe
// default that matches RoutingTier's zero value.
func (self *LocalState) GetRoutingTier() int {
	path := filepath.Join(self.localStorageDir, ".routing_tier")
	if tierBytes, err := os.ReadFile(path); err == nil {
		var tier int
		if err := json.Unmarshal(tierBytes, &tier); err == nil {
			return tier
		}
	}
	return int(RoutingTierOff)
}

func (self *LocalState) SetAllowForeground(allowForeground bool) error {
	path := filepath.Join(self.localStorageDir, ".allow_foreground")
	allowForegroundBytes, err := json.Marshal(allowForeground)
	if err != nil {
		return err
	}
	return os.WriteFile(path, allowForegroundBytes, LocalStorageFilePermissions)
}

func (self *LocalState) GetAllowForeground() bool {
	path := filepath.Join(self.localStorageDir, ".allow_foreground")
	if allowForegroundBytes, err := os.ReadFile(path); err == nil {
		var allowForeground bool
		if err := json.Unmarshal(allowForegroundBytes, &allowForeground); err == nil {
			return allowForeground
		}
	}
	return false
}

// clears all auth tokens
//
// This also wipes .provider_priors (RemoveAll on the whole localStorageDir
// below), so a logout drops the persisted routing memory along with
// everything else per-space -- no separate deletion needed here.
func (self *LocalState) Logout() error {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return errors.Join(
		os.RemoveAll(self.localStorageDir),
		os.MkdirAll(self.localStorageDir, LocalStorageDirectoryPermissions),
	)
}

func (self *LocalState) Close() {
	self.cancel()
}

type CommitCallback interface {
	Complete(success bool)
}

type singleResultCallback[R any] interface {
	Result(result R, ok bool)
}

type GetByJwtCallback interface {
	singleResultCallback[string]
}

type ParseByJwtCallback interface {
	singleResultCallback[*ByJwt]
}

type GetByClientJwtCallback interface {
	singleResultCallback[string]
}

type GetInstanceIdCallback interface {
	singleResultCallback[*Id]
}

type AsyncLocalState struct {
	ctx    context.Context
	cancel context.CancelFunc

	localState *LocalState

	jobs chan *job
}

func NewAsyncLocalState(localStorageHome string) *AsyncLocalState {
	cancelCtx, cancel := context.WithCancel(context.Background())

	localState := newLocalState(cancelCtx, localStorageHome)

	asyncLocalState := &AsyncLocalState{
		ctx:        cancelCtx,
		cancel:     cancel,
		localState: localState,
		jobs:       make(chan *job, AsyncQueueSize),
	}
	go connect.HandleError(asyncLocalState.run)

	return asyncLocalState
}

func (self *AsyncLocalState) run() {
	defer func() {
		self.cancel()

		// drain the jobs
		func() {
			for {
				select {
				case job, ok := <-self.jobs:
					if !ok {
						return
					}
					for _, callback := range job.callbacks {
						callback.Complete(false)
					}
				}
			}
		}()
	}()
	for {
		select {
		case <-self.ctx.Done():
			return
		case job, ok := <-self.jobs:
			if !ok {
				return
			}
			func() {
				defer func() {
					if err := recover(); err != nil {
						for _, callback := range job.callbacks {
							callback.Complete(false)
						}
					}
				}()
				err := job.work()
				for _, callback := range job.callbacks {
					success := err == nil
					callback.Complete(success)
				}
			}()
		}
	}
}

func (self *AsyncLocalState) serialAsync(work func() error, callbacks ...CommitCallback) {
	job := &job{
		work:      work,
		callbacks: callbacks,
	}
	select {
	case <-self.ctx.Done():
		for _, callback := range callbacks {
			callback.Complete(false)
		}
	case self.jobs <- job:
	}
}

// get the sync local state
func (self *AsyncLocalState) GetLocalState() *LocalState {
	return self.localState
}

func (self *AsyncLocalState) GetByJwt(callback GetByJwtCallback) {
	self.serialAsync(func() error {
		byJwt := self.localState.GetByJwt()
		callback.Result(byJwt, byJwt != "")
		return nil
	})
}

func (self *AsyncLocalState) ParseByJwt(callback ParseByJwtCallback) {
	self.serialAsync(func() error {
		byJwt, err := self.localState.ParseByJwt()
		if err == nil {
			callback.Result(byJwt, true)
		} else {
			callback.Result(nil, false)
		}
		return nil
	})
}

// clears the clientjwt and instanceid if differnet
func (self *AsyncLocalState) SetByJwt(byJwt string, callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.SetByJwt(byJwt)
	}, callback)
}

func (self *AsyncLocalState) GetByClientJwt(callback GetByClientJwtCallback) {
	self.serialAsync(func() error {
		byClientJwt := self.localState.GetByClientJwt()
		callback.Result(byClientJwt, byClientJwt != "")
		return nil
	})
}

func (self *AsyncLocalState) SetByClientJwt(byClientJwt string, callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.SetByClientJwt(byClientJwt)
	}, callback)
}

func (self *AsyncLocalState) GetInstanceId(callback GetInstanceIdCallback) {
	self.serialAsync(func() error {
		instanceId := self.localState.GetInstanceId()
		callback.Result(instanceId, instanceId != nil)
		return nil
	})
}

func (self *AsyncLocalState) Logout(callback CommitCallback) {
	self.serialAsync(func() error {
		return self.localState.Logout()
	}, callback)
}

func (self *AsyncLocalState) Close() {
	self.cancel()
	close(self.jobs)
}

type job struct {
	work      func() error
	callbacks []CommitCallback
}
