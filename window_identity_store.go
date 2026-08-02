package sdk

// Window identity persistence for the apps (PACKETRESEARCH1 §12).
//
// PROXYDRAIN1.md §3.5 introduced `connect.MultiClientIdentityStore` so a
// restarted cloud-proxy container reuses its window client identities and the
// egress providers' NAT flows resume. This file brings the same mechanism to
// the locally-owned devices: the identities are persisted in the device's
// local storage, and an app relaunch that reconnects to the SAME destination
// specs reuses them — skipping one AuthNetworkClient api round trip per
// window client at exactly the moment the user is waiting for the window to
// form, and letting still-live provider NAT flows resume after a brief kill.
//
// Scoping guards (all enforced on load):
// - owner: identities belong to one device client id; a different login
//   discards them.
// - destination specs: restored identities are dialed FIRST by the window
//   expansion, so identities recorded while connected to location A must
//   never steer a connect to location B. The store snapshot is stamped with a
//   fingerprint of the connect specs, and a mismatch discards it.
// - ttl: a stale snapshot's providers have likely churned; discard rather
//   than spend the first window dials on them.
//
// With a store configured, the generator deliberately skips the shutdown
// remove-client calls (see ApiMultiClientGenerator.RemoveClientArgs) so the
// identities remain reusable; identities that are never reused are cleaned up
// by the server's idle client reap.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// windowIdentitiesStaleAfter discards a persisted window identity snapshot older than this.
// Within it, a relaunch reuses identities (auth skip; NAT flows additionally resume within the
// provider's own flow lifetime); beyond it the last session's providers have likely churned.
const windowIdentitiesStaleAfter = 4 * time.Hour

// persistedWindowIdentity is the on-disk form of one connect.WindowClientIdentity.
type persistedWindowIdentity struct {
	ClientId   string `json:"client_id"`
	ByJwt      string `json:"by_jwt"`
	InstanceId string `json:"instance_id"`
	// Destination is the multi-hop id as an ordered id string list.
	Destination []string `json:"destination"`
}

// persistedWindowIdentities is the on-disk snapshot: the owning device client id and the
// connect-spec fingerprint scope the identities; SavedAt ages them out.
type persistedWindowIdentities struct {
	OwnerClientId    string                     `json:"owner_client_id"`
	SpecsFingerprint string                     `json:"specs_fingerprint"`
	SavedAt          time.Time                  `json:"saved_at"`
	Identities       []*persistedWindowIdentity `json:"identities"`
}

// providerSpecsFingerprint canonically fingerprints a connect destination (the provider
// specs): each spec json-serialized, sorted, joined. Stable across reorderings of the same
// specs; different for any changed spec.
func providerSpecsFingerprint(specs []*connect.ProviderSpec) string {
	parts := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec == nil {
			continue
		}
		if specBytes, err := json.Marshal(spec); err == nil {
			parts = append(parts, string(specBytes))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "|")
}

// localStateWindowIdentityStore implements connect.MultiClientIdentityStore over the device's
// local storage. The device stamps the current connect's spec fingerprint (SetSpecsFingerprint)
// before each generator is built; load and store are scoped to it.
type localStateWindowIdentityStore struct {
	localState    *LocalState
	ownerClientId connect.Id

	mutex            sync.Mutex
	specsFingerprint string
}

func newLocalStateWindowIdentityStore(localState *LocalState, ownerClientId connect.Id) *localStateWindowIdentityStore {
	return &localStateWindowIdentityStore{
		localState:    localState,
		ownerClientId: ownerClientId,
	}
}

// SetSpecsFingerprint scopes subsequent loads and stores to the given connect specs. Called
// by the device under its state lock before the generator for those specs is created.
func (self *localStateWindowIdentityStore) SetSpecsFingerprint(specsFingerprint string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.specsFingerprint = specsFingerprint
}

func (self *localStateWindowIdentityStore) path() string {
	return filepath.Join(self.localState.localStorageDir, ".window_identities")
}

// StoreWindowClientIdentities implements connect.MultiClientIdentityStore: replace the
// snapshot with the full live set (called by the generator's async writer on every change).
func (self *localStateWindowIdentityStore) StoreWindowClientIdentities(identities []*connect.WindowClientIdentity) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	persisted := &persistedWindowIdentities{
		OwnerClientId:    self.ownerClientId.String(),
		SpecsFingerprint: self.specsFingerprint,
		SavedAt:          time.Now(),
	}
	for _, identity := range identities {
		if identity == nil {
			continue
		}
		destination := []string{}
		for _, id := range identity.Destination.Ids() {
			destination = append(destination, id.String())
		}
		persisted.Identities = append(persisted.Identities, &persistedWindowIdentity{
			ClientId:    identity.ClientId.String(),
			ByJwt:       identity.ByJwt,
			InstanceId:  identity.InstanceId.String(),
			Destination: destination,
		})
	}

	if identitiesBytes, err := json.Marshal(persisted); err == nil {
		os.WriteFile(self.path(), identitiesBytes, LocalStorageFilePermissions)
	}
}

// LoadWindowClientIdentities implements connect.MultiClientIdentityStore: the persisted
// identities, or nil when the snapshot is missing, unreadable, stale, or scoped to a
// different owner or destination specs.
func (self *localStateWindowIdentityStore) LoadWindowClientIdentities() []*connect.WindowClientIdentity {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	identitiesBytes, err := os.ReadFile(self.path())
	if err != nil {
		return nil
	}
	var persisted persistedWindowIdentities
	if err := json.Unmarshal(identitiesBytes, &persisted); err != nil {
		return nil
	}
	if persisted.OwnerClientId != self.ownerClientId.String() {
		return nil
	}
	if persisted.SpecsFingerprint != self.specsFingerprint {
		return nil
	}
	if persisted.SavedAt.IsZero() || windowIdentitiesStaleAfter < time.Since(persisted.SavedAt) {
		return nil
	}

	identities := []*connect.WindowClientIdentity{}
	for _, p := range persisted.Identities {
		clientId, err := connect.ParseId(p.ClientId)
		if err != nil {
			continue
		}
		instanceId, err := connect.ParseId(p.InstanceId)
		if err != nil {
			continue
		}
		destinationIds := []connect.Id{}
		ok := true
		for _, idStr := range p.Destination {
			id, err := connect.ParseId(idStr)
			if err != nil {
				ok = false
				break
			}
			destinationIds = append(destinationIds, id)
		}
		if !ok || p.ByJwt == "" {
			continue
		}
		destination, err := connect.NewMultiHopId(destinationIds...)
		if err != nil {
			continue
		}
		identities = append(identities, &connect.WindowClientIdentity{
			ClientId:    clientId,
			ByJwt:       p.ByJwt,
			InstanceId:  instanceId,
			Destination: destination,
		})
	}
	return identities
}
