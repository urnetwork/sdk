//go:build !ios_extension && !js

// Disclosures for the apps (DESIGN-COMPUTE §15.3, §15.4, §2.8, D-CSURF-1; CTRUST-M11-01, CTRUST-M12-01, CTRUST-M12-02): the functions
// behind Sidecar.Disclosures, AckDisclosure and AckNetworkDisclosure, and the rest of the one view the apps bind for
// every surface that shows a disclosure: the settings list (Account › Settings, every disclosure with its text, version,
// whether it asks and the version acknowledged), a model's line in the model picker and on each message's badge, the
// enclave sheet's and card's provider line, the search card's summary, the support composer's line and its first
// message's whole text, and the indexer's provider line (DESIGN-UI.md T3, T4, T12, S1, S3, B10, B11); and, for an
// Owner or Admin, the network's acknowledgment of each, which the network settings list and each tagged or cloud
// sidecar's card in the team's devices names (TM1).
//
//	Disclosures()              JSON, the core's ComputeDisclosureList: per id its version, revision, grace and the
//	                           instant it took effect, the locale shown, its provider's label in words, whether and
//	                           what it asks before, the summary, "Could be exposed" and "Private", and the version the
//	                           acknowledgments that cover this sidecar hold, whose, whether a use is covered now and
//	                           whether its lines are marked changed, and the network's acknowledgment where the
//	                           network's compute block holds one (who gave it, when, whether it is taken and covers
//	                           now); in the locale SetDisclosureLocale set, else English.
//	                           It remembers per id the version, locale and revision it returned, which the acts below
//	                           take. The core's policy_unavailable error where the device holds no verified policy,
//	                           never an empty list. Locally the core's ComputeDisclosures; remotely
//	                           compute.disclosure.list
//	SetDisclosureLocale(l)     the person's locale as the app knows it, a BCP 47 tag; empty for English
//	AckDisclosure(id)          the person's act in the app's UI after the app showed the whole text: records the version
//	                           Disclosures() last returned for id, refused disclosure_changed where the current one
//	                           differs, and refused where Disclosures() returned none; locally the core's
//	                           AckComputeDisclosure as SidecarActConfirmed, remotely the Confirm method
//	                           compute.disclosure.ack sent confirmed, which the daemon refuses unconfirmed, recording
//	                           nothing. No Harness tool, MCP tool or RPC reaches it
//	OpenDisclosure(id)         the person opened id's whole text, at the version Disclosures() last returned: ends its
//	                           lines' changed mark; locally OpenedComputeDisclosure, remotely compute.disclosure.open
//	                           sent confirmed
//	AckNetworkDisclosure(id)   an Owner's or Admin's act in the app's network settings after the app showed the whole
//	                           text: signs the network's disclosure-ack for the version, locale and revision
//	                           Disclosures() last returned and saves it into the compute block, refused
//	                           disclosure_changed where the current version differs, and refused on any device but
//	                           an Owner's or Admin's approval-capable one that runs the network's config writer;
//	                           locally the core's AckNetworkDisclosure as SidecarActConfirmed, remotely the Confirm
//	                           method compute.disclosure.ack_network sent confirmed
//	RemoveNetworkDisclosure(id) an Owner's or Admin's act taking the network's acknowledgment out, which only
//	                           narrows; locally RemoveNetworkDisclosure, remotely compute.disclosure.remove_network
//	                           sent confirmed
//	NetworkDisclosures()       the network's acknowledgments, read as Disclosures() reads its list, typed, for a tagged
//	                           or cloud sidecar's card: each id's version, who gave it and when, and whether it stands.
//	                           It changes nothing of what Disclosures() last returned, which the acts take, since it
//	                           shows no text
//	AddDisclosuresListener(l)  DisclosuresChanged after each act through this Sidecar (a locale set, an acknowledgment, an
//	                           opening), so the app reads Disclosures() again
//
// The memory these keep lives on the Sidecar (sidecar.go's disclosures field), made at the first call that needs it.
// Nothing here runs in the VPN extension. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The daemon's disclosure methods, as disclosure_daemon.go names them; TestDisclosureControlWireMatchesTheDaemon holds
// them to that source on the legs that build it.
const (
	sidecarControlMethodDisclosureList          = "compute.disclosure.list"
	sidecarControlMethodDisclosureAck           = "compute.disclosure.ack"
	sidecarControlMethodDisclosureOpen          = "compute.disclosure.open"
	sidecarControlMethodDisclosureAckNetwork    = "compute.disclosure.ack_network"
	sidecarControlMethodDisclosureRemoveNetwork = "compute.disclosure.remove_network"
)

// compute.disclosure.list's params, as the daemon reads them strictly.
type sidecarControlDisclosureListParams struct {
	Network string `json:"network,omitempty"`
	Locale  string `json:"locale,omitempty"`
}

// compute.disclosure.ack's and .open's params.
type sidecarControlDisclosureActParams struct {
	Network string `json:"network,omitempty"`
	Id      string `json:"id"`
	Version uint32 `json:"version"`
}

// compute.disclosure.ack_network's params: the id, and the version, locale and revision the person was shown.
type sidecarControlDisclosureNetworkActParams struct {
	Network  string `json:"network,omitempty"`
	Id       string `json:"id"`
	Version  uint32 `json:"version"`
	Locale   string `json:"locale"`
	Revision uint32 `json:"revision"`
}

// compute.disclosure.remove_network's params.
type sidecarControlDisclosureRemoveParams struct {
	Network string `json:"network,omitempty"`
	Id      string `json:"id"`
}

// compute.disclosure.ack's and .open's reply: the refusal's code where the act was refused for its version or for want
// of a policy.
type sidecarControlDisclosureActReply struct {
	Id      string `json:"id"`
	Version uint32 `json:"version,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

var (
	// A Sidecar with neither a local core nor a remote backend: a closed Sidecar.
	errSidecarDisclosuresNoCore = errors.New("disclosures: this Sidecar has no core, local or remote")
	// An act on an id Disclosures() has not returned: the app shows the whole text, from Disclosures(), first.
	errSidecarDisclosureNotShown = errors.New("disclosures: Disclosures() has not returned this id here, so its text was not shown")
)

// A listener for the disclosures' state: told after each act made through this Sidecar, so the app reads Disclosures()
// again.
type SidecarDisclosuresListener interface {
	DisclosuresChanged()
}

// What Disclosures() last returned for one id: the version, locale and revision the app showed.
type sidecarDisclosureShown struct {
	version  uint32
	locale   string
	revision uint32
}

// A Sidecar's disclosure state for its app: the locale SetDisclosureLocale set, what Disclosures() last returned per id,
// and its listeners.
type sidecarDisclosureMemory struct {
	listeners *connect.CallbackList[SidecarDisclosuresListener]

	stateLock sync.Mutex
	locale    string
	idShowns  map[string]*sidecarDisclosureShown
}

// An empty memory.
func newSidecarDisclosureMemory() *sidecarDisclosureMemory {
	return &sidecarDisclosureMemory{
		listeners: connect.NewCallbackList[SidecarDisclosuresListener](),
		idShowns:  map[string]*sidecarDisclosureShown{},
	}
}

// This Sidecar's memory, made at the first call under stateLock and the same one after, the Sidecar open or closed.
func (self *Sidecar) disclosureMemoryOf() *sidecarDisclosureMemory {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.disclosures == nil {
		self.disclosures = newSidecarDisclosureMemory()
	}
	return self.disclosures
}

// The locale Disclosures() renders in.
func (self *sidecarDisclosureMemory) localeNow() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.locale
}

// Sets the locale, answering whether it changed.
func (self *sidecarDisclosureMemory) setLocale(locale string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.locale == locale {
		return false
	}
	self.locale = locale
	return true
}

// Remembers what list showed of each id, replacing what an older list showed.
func (self *sidecarDisclosureMemory) remember(list *sidecar.ComputeDisclosureList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.idShowns = map[string]*sidecarDisclosureShown{}
	for _, view := range list.Disclosures {
		if view != nil {
			self.idShowns[view.Id] = &sidecarDisclosureShown{version: view.Version, locale: view.Locale, revision: view.Revision}
		}
	}
}

// The card's form of the core's network acknowledgment, with the disclosure's current version.
func sidecarNetworkDisclosureOf(network *sidecar.ComputeNetworkDisclosure, current uint32) *SidecarNetworkDisclosure {
	bound := &SidecarNetworkDisclosure{
		Id:             network.Id,
		Version:        int32(network.Version),
		CurrentVersion: int32(current),
		Locale:         network.Locale,
		SignedAtMillis: network.SignedAtMs,
		Taken:          network.Taken,
		Refusal:        network.Refusal,
		Stands:         network.Stands,
		InGrace:        network.InGrace,
		GraceEndMillis: network.GraceEndMs,
	}
	if network.MemberId != (sidecar.Id{}) {
		bound.MemberId = sidecarIdOf(&network.MemberId)
	}
	if network.SidecarId != (sidecar.Id{}) {
		bound.SidecarId = sidecarIdOf(&network.SidecarId)
	}
	return bound
}

// What Disclosures() last returned for id, nil for none.
func (self *sidecarDisclosureMemory) shown(id string) *sidecarDisclosureShown {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if shown, ok := self.idShowns[id]; ok {
		copied := *shown
		return &copied
	}
	return nil
}

// Tells each listener, each call guarded.
func (self *sidecarDisclosureMemory) changed() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.DisclosuresChanged()
		})
	}
}

// The Sidecar's context, Background for a Sidecar made with none.
func sidecarDisclosuresCtx(args *sidecarComputeArgs) context.Context {
	if args.ctx != nil {
		return args.ctx
	}
	return context.Background()
}

// Every disclosure of the newest verified policy, as JSON (the file header).
func sidecarDisclosures(args *sidecarComputeArgs, memory *sidecarDisclosureMemory) (string, error) {
	list, err := sidecarDisclosureList(args, memory)
	if err != nil {
		return "", err
	}
	memory.remember(list)
	data, err := json.Marshal(list)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// The core's list in the memory's locale, local or remote, remembering nothing: the core's policy_unavailable where the
// device holds no verified policy.
func sidecarDisclosureList(args *sidecarComputeArgs, memory *sidecarDisclosureMemory) (*sidecar.ComputeDisclosureList, error) {
	ctx := sidecarDisclosuresCtx(args)
	locale := memory.localeNow()
	var list *sidecar.ComputeDisclosureList
	switch {
	case args.local != nil:
		var err error
		if list, err = args.local.ComputeDisclosures(ctx, locale); err != nil {
			return nil, err
		}
	case args.remote != nil:
		list = &sidecar.ComputeDisclosureList{}
		params := &sidecarControlDisclosureListParams{Network: sidecarSupportNetwork(args.remote), Locale: locale}
		if err := args.remote.call(ctx, sidecarControlMethodDisclosureList, params, false, list); err != nil {
			return nil, err
		}
		switch list.Unavailable {
		case "":
		case sidecar.ComputeDisclosureCodePolicyUnavailable:
			return nil, sidecar.ErrComputeDisclosurePolicyUnavailable
		default:
			return nil, fmt.Errorf("disclosures: the daemon lists none: %s", list.Unavailable)
		}
	default:
		return nil, errSidecarDisclosuresNoCore
	}
	return list, nil
}

// A remote act's reply as the core's error: disclosure_changed, policy_unavailable or disclosure_required.
func sidecarDisclosureRefusal(reply *sidecarControlDisclosureActReply) error {
	switch reply.Refusal {
	case "":
		return nil
	case sidecar.ComputeDisclosureCodeChanged:
		return sidecar.ErrDisclosureChanged
	case sidecar.ComputeDisclosureCodePolicyUnavailable:
		return sidecar.ErrComputeDisclosurePolicyUnavailable
	}
	return fmt.Errorf("%w: %s", sidecar.ErrDisclosureRequired, reply.Refusal)
}

// One of the person's acts on id at the version Disclosures() last returned: local on the core, else remote by the
// Confirm method, sent confirmed. The listeners are told after it lands.
func sidecarDisclosureAct(args *sidecarComputeArgs, memory *sidecarDisclosureMemory, id string, method string,
	local func(ctx context.Context, core *sidecar.Sidecar, version uint32) error) error {
	shown := memory.shown(id)
	if shown == nil {
		return errSidecarDisclosureNotShown
	}
	ctx := sidecarDisclosuresCtx(args)
	switch {
	case args.local != nil:
		if err := local(ctx, args.local, shown.version); err != nil {
			return err
		}
	case args.remote != nil:
		// the person's act in the app's UI, sent confirmed
		reply := &sidecarControlDisclosureActReply{}
		params := &sidecarControlDisclosureActParams{Network: sidecarSupportNetwork(args.remote), Id: id, Version: shown.version}
		if err := args.remote.call(ctx, method, params, true, reply); err != nil {
			return err
		}
		if err := sidecarDisclosureRefusal(reply); err != nil {
			return err
		}
	default:
		return errSidecarDisclosuresNoCore
	}
	memory.changed()
	return nil
}

// The person's acknowledgment of id at the version Disclosures() last returned (the file header).
func ackSidecarDisclosure(args *sidecarComputeArgs, memory *sidecarDisclosureMemory, id string) error {
	return sidecarDisclosureAct(args, memory, id, sidecarControlMethodDisclosureAck, func(ctx context.Context, core *sidecar.Sidecar, version uint32) error {
		// the person's act in the app's UI, confirmed there
		return core.AckComputeDisclosure(ctx, &sidecar.ComputeDisclosureAckArgs{Id: id, Version: version, Origin: sidecar.SidecarActConfirmed})
	})
}

// That the person opened id's whole text at the version Disclosures() last returned (the file header).
func openSidecarDisclosure(args *sidecarComputeArgs, memory *sidecarDisclosureMemory, id string) error {
	return sidecarDisclosureAct(args, memory, id, sidecarControlMethodDisclosureOpen, func(ctx context.Context, core *sidecar.Sidecar, version uint32) error {
		return core.OpenedComputeDisclosure(ctx, id, version, sidecar.SidecarActConfirmed)
	})
}

// An Owner's or Admin's acknowledgment of disclosure id for the network, at the version, locale and revision
// Disclosures() last returned (the file header): locally the core's act as confirmed, remotely the Confirm method sent
// confirmed. The listeners are told after it lands.
func ackSidecarNetworkDisclosure(args *sidecarComputeArgs, memory *sidecarDisclosureMemory, id string) error {
	shown := memory.shown(id)
	if shown == nil {
		return errSidecarDisclosureNotShown
	}
	ctx := sidecarDisclosuresCtx(args)
	switch {
	case args.local != nil:
		_, err := args.local.AckNetworkDisclosure(ctx, &sidecar.ComputeNetworkDisclosureAckArgs{
			Id: id, Version: shown.version, Locale: shown.locale, Revision: shown.revision, Origin: sidecar.SidecarActConfirmed,
		})
		if err != nil {
			return err
		}
	case args.remote != nil:
		// the person's act in the app's network settings, sent confirmed
		reply := &sidecarControlDisclosureActReply{}
		params := &sidecarControlDisclosureNetworkActParams{
			Network: sidecarSupportNetwork(args.remote), Id: id, Version: shown.version, Locale: shown.locale, Revision: shown.revision,
		}
		if err := args.remote.call(ctx, sidecarControlMethodDisclosureAckNetwork, params, true, reply); err != nil {
			return err
		}
		if err := sidecarDisclosureRefusal(reply); err != nil {
			return err
		}
	default:
		return errSidecarDisclosuresNoCore
	}
	memory.changed()
	return nil
}

// An Owner's or Admin's removal of the network's acknowledgment of id, which only narrows (the file header).
func removeSidecarNetworkDisclosure(args *sidecarComputeArgs, memory *sidecarDisclosureMemory, id string) error {
	ctx := sidecarDisclosuresCtx(args)
	switch {
	case args.local != nil:
		if _, err := args.local.RemoveNetworkDisclosure(ctx, &sidecar.ComputeNetworkDisclosureRemoveArgs{Id: id, Origin: sidecar.SidecarActConfirmed}); err != nil {
			return err
		}
	case args.remote != nil:
		reply := &sidecarControlDisclosureActReply{}
		params := &sidecarControlDisclosureRemoveParams{Network: sidecarSupportNetwork(args.remote), Id: id}
		if err := args.remote.call(ctx, sidecarControlMethodDisclosureRemoveNetwork, params, true, reply); err != nil {
			return err
		}
		if err := sidecarDisclosureRefusal(reply); err != nil {
			return err
		}
	default:
		return errSidecarDisclosuresNoCore
	}
	memory.changed()
	return nil
}

// The person's locale, as the app knows it, a BCP 47 tag such as de-AT; empty for English. Disclosures() renders each
// text in it where the policy has it, matched by tag and then by language, else English, one locale whole (§2.8).
func (self *Sidecar) SetDisclosureLocale(locale string) {
	trace(func() {
		memory := self.disclosureMemoryOf()
		if memory.setLocale(locale) {
			memory.changed()
		}
	})
}

// That the person opened disclosure id's whole text in the app, which ends its lines' "changed" mark (§2.8, §15.4):
// the person's act in the app's UI, at the version Disclosures() last returned.
func (self *Sidecar) OpenDisclosure(id string) error {
	return traceWithError(func() error {
		return openSidecarDisclosure(self.computeArgs(), self.disclosureMemoryOf(), id)
	})
}

// Takes the network's acknowledgment of disclosure id out of its compute block, an Owner's or Admin's act in the app's
// network settings, which only narrows (§2.8, CO-41).
func (self *Sidecar) RemoveNetworkDisclosure(id string) error {
	return traceWithError(func() error {
		return removeSidecarNetworkDisclosure(self.computeArgs(), self.disclosureMemoryOf(), id)
	})
}

// The network's acknowledgments of the newest verified policy's disclosures, each with who gave it, when, and whether
// it stands, in the policy's order: what a tagged or cloud sidecar's card names of the disclosures it relies on, and
// what an Owner's or Admin's network settings show (§2.8, §15.4). It shows no text, so it changes nothing the acts
// take; the core's policy_unavailable where the device holds no verified policy.
func (self *Sidecar) NetworkDisclosures() (*SidecarNetworkDisclosureList, error) {
	return traceWithReturnError(func() (*SidecarNetworkDisclosureList, error) {
		read, err := sidecarDisclosureList(self.computeArgs(), self.disclosureMemoryOf())
		if err != nil {
			return nil, err
		}
		list := NewSidecarNetworkDisclosureList()
		for _, view := range read.Disclosures {
			if view != nil && view.Network != nil {
				list.Add(sidecarNetworkDisclosureOf(view.Network, view.Version))
			}
		}
		return list, nil
	})
}

// Adds a listener for the disclosures' state (SidecarDisclosuresListener); Close on the answer removes it.
func (self *Sidecar) AddDisclosuresListener(listener SidecarDisclosuresListener) Sub {
	memory := self.disclosureMemoryOf()
	callbackId := memory.listeners.Add(listener)
	return newSub(func() {
		memory.listeners.Remove(callbackId)
	})
}
