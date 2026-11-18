//go:build !ios_extension && !js

// Remote search's requester seams on an app host (§18.4's remote, setting and notice rows; §18.7's foreground and
// control rows; SDK-M10-03; R-SDK-14, D-SDK-26, D-SDK-27, Q-SDK-6, I-19, R-CTX-37): the indexers that may answer this
// device, this network's mode and a notice's answer, bound over CORE-M10-03's Sidecar.GetSearchIndexers,
// SetSearchRemote and AcknowledgeSearchIndexer. Asking, eligibility, failover and merging are the core's (SEARCH's
// SearchRemoteClient, the engine an app host installs); this layer maps fields (D-SDK-22).
//
//	mode      on (the default), own (this member's indexers only) or off (the fallback only), per device and network,
//	          kept in §5.9's non-derived search state with the notice answers: never in the config repo, and never set
//	          through MCP, since SetRemote is the app's call for the person's setting and no control method carries it
//	notice    AcknowledgeIndexer(memberId, allow) is a notice's answer: allow lets that member's indexers be asked;
//	          false sets own, as "Only my devices" does, in the same write; a nil memberId clears every answer ("reset
//	          notices"), the mode staying. The copy is the app's: a page's progress names the indexer a notice waits
//	          on (RemoteBy), with its device name, its member (a tagged device's sponsor) and the repos it served
//	indexers  GetIndexers lists those that may answer this device, own first, each with its state, whether its
//	          member's notice was allowed and the repos it served in the last Search.RemoteServedTimeout (24 h)
//	states    a page's Remote state, its FallbackReason and an indexer's State map one for one from the core's
//	          names (§18.5, DESIGN-DETAILS 7.2.4): a name the core gives that §18.5 does not is read as none, or for
//	          an indexer as unknown, never passed on
//	labels    "verified by <device>" is a hit's By, the answering indexer's config device name, as the core merged it
//	never     a remote-mode Sidecar never asks, its daemon holding all (§18.7's control row): on the remote backend
//	          GetIndexers lists none, and SetRemote and AcknowledgeIndexer answer SEARCH_REFUSED with nothing sent to
//	          the daemon; a browser Worker's search (sdk/js) asks none either, and the js leg builds none of this
//
// Nothing here logs, and a refusal answers its code's fixed text (I-13, I-19). Safe for concurrent use.

package sdk

import (
	"context"

	"github.com/urnetwork/sidecar"
)

// §18.4's remote modes, as SetRemote takes them and SidecarSearchStatus.Remote names them. Format values: apps read
// them. SidecarSearchRemoteOff is also a query's setting that keeps one search on the device.
const (
	SidecarSearchRemoteOn  = "on"
	SidecarSearchRemoteOwn = "own"
)

// §18.5's remote states, as SidecarSearchProgress.Remote names them; "" while no repo of the search had an indexer.
const (
	SidecarSearchRemoteStateNotice   = "notice"
	SidecarSearchRemoteStateAsking   = "asking"
	SidecarSearchRemoteStateAnswered = "answered"
	SidecarSearchRemoteStatePartial  = "partial"
	SidecarSearchRemoteStateFallback = "fallback"
	SidecarSearchRemoteStateOff      = "off"
)

// §18.5's fallback reasons, as SidecarSearchProgress.FallbackReason names them; "" while no repo fell back.
const (
	SidecarSearchFallbackNone        = "none"
	SidecarSearchFallbackOffline     = "offline"
	SidecarSearchFallbackBusy        = "busy"
	SidecarSearchFallbackRefused     = "refused"
	SidecarSearchFallbackUnreachable = "unreachable"
	SidecarSearchFallbackOff         = "off"
	SidecarSearchFallbackNotice      = "notice"
)

// §18.5's indexer states, as SidecarIndexer.State names them.
const (
	SidecarIndexerOnline      = "online"
	SidecarIndexerUnreachable = "unreachable"
	SidecarIndexerBusy        = "busy"
	SidecarIndexerUnknown     = "unknown"
)

// The core's remote states by §18.5's names, one for one (DESIGN-DETAILS 7.2.4's table).
var sidecarSearchRemoteStates = map[string]string{
	sidecar.SearchRemoteStateNotice:   SidecarSearchRemoteStateNotice,
	sidecar.SearchRemoteStateAsking:   SidecarSearchRemoteStateAsking,
	sidecar.SearchRemoteStateAnswered: SidecarSearchRemoteStateAnswered,
	sidecar.SearchRemoteStatePartial:  SidecarSearchRemoteStatePartial,
	sidecar.SearchRemoteStateFallback: SidecarSearchRemoteStateFallback,
	sidecar.SearchRemoteStateOff:      SidecarSearchRemoteStateOff,
}

// The core's fallback reasons by §18.5's names, one for one.
var sidecarSearchFallbackReasons = map[string]string{
	sidecar.SearchFallbackNone:        SidecarSearchFallbackNone,
	sidecar.SearchFallbackOffline:     SidecarSearchFallbackOffline,
	sidecar.SearchFallbackBusy:        SidecarSearchFallbackBusy,
	sidecar.SearchFallbackRefused:     SidecarSearchFallbackRefused,
	sidecar.SearchFallbackUnreachable: SidecarSearchFallbackUnreachable,
	sidecar.SearchFallbackOff:         SidecarSearchFallbackOff,
	sidecar.SearchFallbackNotice:      SidecarSearchFallbackNotice,
}

// The indexer states the core names, by §18.5's names.
var sidecarIndexerStates = map[string]string{
	SidecarIndexerOnline:      SidecarIndexerOnline,
	SidecarIndexerUnreachable: SidecarIndexerUnreachable,
	SidecarIndexerBusy:        SidecarIndexerBusy,
	SidecarIndexerUnknown:     SidecarIndexerUnknown,
}

// §18.5's remote state of the core's: "" for none, and for a name §18.5 does not give.
func sidecarSearchRemoteStateOf(state string) string {
	return sidecarSearchRemoteStates[state]
}

// §18.5's fallback reason of the core's: "" for none, and for a name §18.5 does not give.
func sidecarSearchFallbackReasonOf(reason string) string {
	return sidecarSearchFallbackReasons[reason]
}

// §18.5's indexer state of the core's: unknown for none, and for a name §18.5 does not give.
func sidecarIndexerStateOf(state string) string {
	if bound, ok := sidecarIndexerStates[state]; ok {
		return bound
	}
	return SidecarIndexerUnknown
}

// The indexers that may answer this device, own first.
type SidecarIndexerList struct {
	exportedList[*SidecarIndexer]
}

// An empty list.
func NewSidecarIndexerList() *SidecarIndexerList {
	return &SidecarIndexerList{
		exportedList: *newExportedList[*SidecarIndexer](),
	}
}

// The indexers that may answer this device, own first (§18.5's GetIndexers), each with its state, whether its member's
// notice was allowed and the repos it served in the last 24 h: none on a host that asks no indexer, as a remote-mode
// Sidecar (§18.7's control row), while no remote client runs, or once the Sidecar closed. Never nil.
func (self *SidecarSearch) GetIndexers() *SidecarIndexerList {
	list, err := traceWithReturn(func() *SidecarIndexerList {
		list := NewSidecarIndexerList()
		ctx, err := self.callCtx()
		if err != nil {
			return list
		}
		ctx, cancel := context.WithTimeout(ctx, self.settings.StatusTimeout)
		defer cancel()
		indexers, err := self.backend.indexers(ctx)
		if err != nil {
			return list
		}
		list.addAll(indexers...)
		return list
	})
	if err != nil || list == nil {
		return NewSidecarIndexerList()
	}
	return list
}

// Sets this network's remote mode (§18.4's setting row): on, own or off, kept in §5.9's search state, which DropCache
// and cache eviction keep. Answers callback once: SEARCH_REFUSED on a host that asks no indexer, as a remote-mode
// Sidecar, with nothing sent; SEARCH_INVALID for any other mode.
func (self *SidecarSearch) SetRemote(mode string, callback SidecarResultCallback) {
	sidecarSearchAnswer(func() *SidecarResult {
		ctx, err := self.callCtx()
		if err != nil {
			return sidecarSearchResultOf(err)
		}
		return sidecarSearchResultOf(self.backend.setRemote(ctx, mode))
	}, callback)
}

// A notice's answer (§18.4's notice row): allow lets memberId's indexers be asked, as [Search] does; false sets this
// network's mode to own, as [Only my devices] does; a nil memberId clears every answer ("reset notices"), the mode
// staying. Answers callback once: SEARCH_REFUSED on a host that asks no indexer, with nothing sent.
func (self *SidecarSearch) AcknowledgeIndexer(memberId *Id, allow bool, callback SidecarResultCallback) {
	sidecarSearchAnswer(func() *SidecarResult {
		ctx, err := self.callCtx()
		if err != nil {
			return sidecarSearchResultOf(err)
		}
		return sidecarSearchResultOf(self.backend.acknowledgeIndexer(ctx, sidecarSearchRepoIdOf(memberId), allow))
	}, callback)
}

// See sidecarSearchBackend: the core's list, its Acknowledged and ServedRepoIds the core's search state's.
func (self *sidecarSearchCoreBackend) indexers(ctx context.Context) ([]*SidecarIndexer, error) {
	indexers := self.local.GetSearchIndexers()
	bound := make([]*SidecarIndexer, 0, len(indexers))
	for index := range indexers {
		bound = append(bound, sidecarIndexerOf(&indexers[index]))
	}
	return bound, nil
}

// See sidecarSearchBackend: the core refuses on a host without the requester side and a mode it does not know.
func (self *sidecarSearchCoreBackend) setRemote(ctx context.Context, mode string) error {
	return self.local.SetSearchRemote(mode)
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) acknowledgeIndexer(ctx context.Context, memberId *sidecar.Id, allow bool) error {
	return self.local.AcknowledgeSearchIndexer(memberId, allow)
}

// See sidecarSearchBackend: a remote-mode Sidecar asks no indexer, its daemon holding all (§18.7's control row).
func (self *sidecarSearchRemoteBackend) indexers(ctx context.Context) ([]*SidecarIndexer, error) {
	return nil, nil
}

// See sidecarSearchBackend: refused, and nothing reaches the daemon, whose mode is not a remote-mode app's to set.
func (self *sidecarSearchRemoteBackend) setRemote(ctx context.Context, mode string) error {
	return sidecar.ErrSidecarSearchRefused
}

// See sidecarSearchBackend: refused, and nothing reaches the daemon.
func (self *sidecarSearchRemoteBackend) acknowledgeIndexer(ctx context.Context, memberId *sidecar.Id, allow bool) error {
	return sidecar.ErrSidecarSearchRefused
}
