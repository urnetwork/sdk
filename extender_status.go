package sdk

import (
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// extender_status.go — the app-facing extender surface (EXTENDER.md D5, F2).
//
// Two things live here: the role, which decides whether this app is a feed
// client or a full gossip member, and the status the apps render.
//
// The role rule is platform-parameterized (`extenderRoleForPlatform`) so the
// js, mobile and desktop answers are all pinned on one host, exactly as the
// mobile memory policy is. A persisted mode overrides it in either direction.
//
// Status types are plain gomobile-bindable values: ints, int64 millisecond
// times, strings and one exported list. `Carriers` is a comma-separated string
// rather than a list, because a per-row list would be a bound object per row
// on every binding.

// The persisted gossip mode (D5).
const (
	ExtenderGossipModeAuto   = "auto"
	ExtenderGossipModeFeed   = "feed"
	ExtenderGossipModeMember = "member"
)

// The effective role (D5). In this phase both roles take the feed; phase 5a
// gives the member role the gossip node instead.
const (
	ExtenderRoleFeed   = "feed"
	ExtenderRoleMember = "member"
)

// extenderRoleForPlatform is the D5 rule with every platform input explicit.
// A configured mode wins outright. Otherwise the feed role is taken by the
// hosts that cannot afford a mesh: the js build, which has no node at all, and
// a mobile runtime under the low-memory policy (a mobile runtime with a
// process memory budget at or below the steady target). An unset budget is not
// a low-memory host: it is a host that never declared one.
func extenderRoleForPlatform(
	mode string,
	feedOnlyBuild bool,
	mobile bool,
	memoryBudgetByteCount ByteCount,
) string {
	switch NormalExtenderGossipMode(mode) {
	case ExtenderGossipModeFeed:
		return ExtenderRoleFeed
	case ExtenderGossipModeMember:
		return ExtenderRoleMember
	}
	if feedOnlyBuild {
		return ExtenderRoleFeed
	}
	if mobileLowMemoryPolicyEnabledForPlatform(memoryBudgetByteCount, mobile) {
		return ExtenderRoleFeed
	}
	return ExtenderRoleMember
}

// The effective role of this process under `mode`.
func extenderRole(mode string) string {
	return extenderRoleForPlatform(
		mode,
		extenderFeedOnlyBuild,
		mobileRuntime(),
		connect.MemoryBudget(),
	)
}

// One known extender as the apps render it (F2).
type ExtenderInfo struct {
	// Base58 of the extender identity key, empty while the address is
	// unverified.
	Id        string
	Ip        string
	IpVersion int
	// Comma separated, in the record's order: "tcp,quic,dns".
	Carriers    string
	CountryCode string
	// One of active, warning, hold, unverified, revoked, expired.
	State string
	// One of dns, feed, gossip, bootstrap, manual.
	Source string
	// Unix milliseconds, 0 when there has been none.
	LastSuccessTime int64
	LastFailureTime int64
	SuccessCount    int
	FailureCount    int
	// Live dials over this address right now.
	InUse int
	// Unix milliseconds of the record expiry, 0 when unverified.
	ExpireTime int64
}

type ExtenderInfoList struct {
	exportedList[*ExtenderInfo]
}

func NewExtenderInfoList() *ExtenderInfoList {
	return &ExtenderInfoList{
		exportedList: *newExportedList[*ExtenderInfo](),
	}
}

// The whole extender surface of one network space (F2).
type ExtenderStatus struct {
	// feed or member (D5).
	Role          string
	FeedConnected bool
	FeedIp        string
	// The mesh of the member role's node (D1, F2). A feed app reports no mesh.
	GossipConnected bool
	GossipPeerCount int
	KnownCount      int
	ActiveCount     int
	WarningCount    int
	HoldCount       int
	// Unix milliseconds, 0 when there has been no sample.
	LastSampleTime int64
	LastError      string
	Extenders      *ExtenderInfoList
}

type ExtenderStatusChangeListener interface {
	ExtenderStatusChanged(status *ExtenderStatus)
}

// The current status. A space with no extender directory -- a url-only space,
// or one built without storage by a host that disabled discovery -- reports an
// empty status rather than nil, so a caller never has to branch.
func (self *NetworkSpace) GetExtenderStatus() *ExtenderStatus {
	status := &ExtenderStatus{
		Role:      extenderRole(self.GetExtenderGossipMode()),
		Extenders: NewExtenderInfoList(),
	}
	if self.extenderNetworkClient != nil {
		networkStatus := self.extenderNetworkClient.Status()
		status.FeedConnected = networkStatus.FeedConnected
		if networkStatus.FeedIp.IsValid() {
			status.FeedIp = networkStatus.FeedIp.String()
		}
		status.LastSampleTime = extenderStatusTimeMs(networkStatus.LastSampleTime)
		status.LastError = networkStatus.LastError
	}
	extenderNode := self.getExtenderNode()
	status.GossipConnected = extenderNode.gossipConnected()
	status.GossipPeerCount = extenderNode.gossipPeerCount()
	if self.extenderDirectory == nil {
		return status
	}
	snapshot := self.extenderDirectory.Snapshot()
	status.KnownCount = snapshot.KnownCount
	status.ActiveCount = snapshot.ActiveCount
	status.WarningCount = snapshot.WarningCount
	status.HoldCount = snapshot.HoldCount
	for _, entry := range snapshot.Entries {
		extenderInfo := &ExtenderInfo{
			Ip:              entry.Ip.String(),
			IpVersion:       entry.IpVersion,
			Carriers:        strings.Join(entry.Carriers, ","),
			CountryCode:     entry.CountryCode,
			State:           entry.State,
			Source:          entry.Source,
			LastSuccessTime: extenderStatusTimeMs(entry.LastSuccessTime),
			LastFailureTime: extenderStatusTimeMs(entry.LastFailureTime),
			SuccessCount:    entry.SuccessCount,
			FailureCount:    entry.FailureCount,
			InUse:           entry.InUse,
			ExpireTime:      extenderStatusTimeMs(entry.ExpireTime),
		}
		if 0 < len(entry.PublicKey) {
			extenderInfo.Id = base58Encode(entry.PublicKey)
		}
		status.Extenders.Add(extenderInfo)
	}
	return status
}

func extenderStatusTimeMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// The persisted gossip mode of this space (D5). A space with no storage always
// reads auto.
func (self *NetworkSpace) GetExtenderGossipMode() string {
	return extenderGossipMode(self.asyncLocalState)
}

// The persisted mode of one local state, which the space construction reads
// before there is a space to ask.
func extenderGossipMode(asyncLocalState *AsyncLocalState) string {
	if asyncLocalState == nil {
		return ExtenderGossipModeAuto
	}
	return asyncLocalState.GetLocalState().GetExtenderGossipMode()
}

// Persists the gossip mode and publishes the new status, so a ui that changed
// the mode sees the role it now has without asking again.
func (self *NetworkSpace) SetExtenderGossipMode(mode string) {
	mode = NormalExtenderGossipMode(mode)
	if self.asyncLocalState != nil {
		self.asyncLocalState.serialAsync(func() error {
			return self.asyncLocalState.GetLocalState().SetExtenderGossipMode(mode)
		})
	}
	self.extenderStatusChanged()
}

// AddExtenderStatusChangeListener subscribes to the extender status. The
// callbacks are coalesced to at most one per epoch by the space's watch loop,
// which is what keeps a burst of applied records from becoming a burst of ui
// work.
func (self *NetworkSpace) AddExtenderStatusChangeListener(
	listener ExtenderStatusChangeListener,
) Sub {
	callbackId := self.extenderStatusChangeListeners.Add(listener)
	return newSub(func() {
		self.extenderStatusChangeListeners.Remove(callbackId)
	})
}

func (self *NetworkSpace) extenderStatusChanged() {
	status := self.GetExtenderStatus()
	for _, listener := range self.extenderStatusChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ExtenderStatusChanged(status)
		})
	}
}

// watchExtenderStatus emits at most one status per epoch, carrying the
// complete state (F2).
//
// It waits for a change, sleeps the epoch so a burst collapses, and only then
// re-arms and reads. Arming before the epoch would leave the next round
// already triggered by the same burst the snapshot already carries, and would
// emit an identical status one epoch later. See DeviceLocal.watchNetworkPeers
// for the same shape.
func (self *NetworkSpace) watchExtenderStatus() {
	if self.extenderDirectory == nil {
		return
	}
	directoryMonitor := self.extenderDirectory.ChangeMonitor()
	_, directoryUpdate := directoryMonitor.Get()
	var networkUpdate chan struct{}
	if self.extenderNetworkClient != nil {
		_, networkUpdate = self.extenderNetworkClient.StatusMonitor().Get()
	}
	nodeChange := self.extenderNodeMonitor.NotifyChannel()
	nodeUpdate := self.getExtenderNode().statusUpdate()
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-directoryUpdate:
		case <-networkUpdate:
		case <-nodeChange:
		case <-nodeUpdate:
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(extenderStatusEpoch):
		}
		_, directoryUpdate = directoryMonitor.Get()
		if self.extenderNetworkClient != nil {
			_, networkUpdate = self.extenderNetworkClient.StatusMonitor().Get()
		}
		// the node itself is replaced when the provider extender role starts
		// and stops, so the swap is a change and the new node is what the next
		// round waits on (G2)
		nodeChange = self.extenderNodeMonitor.NotifyChannel()
		nodeUpdate = self.getExtenderNode().statusUpdate()
		// contain a panic to the tick: a failed emit must never end the watch,
		// which would silently stop every extender update for the session
		connect.HandleError(self.extenderStatusChanged)
	}
}

// At most one status callback per second (F2).
const extenderStatusEpoch = 1 * time.Second
