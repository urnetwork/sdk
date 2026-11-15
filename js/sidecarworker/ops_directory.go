//go:build sidecar_http || !js

// directory_view, RS2's team console on the page Worker (§18.8's pages and messages rows, §16.11's console row,
// D-SDK-28, I-20): the team directory's current items as this device's own verified copy folds them, each a view built
// field by field from the root's item and its stub (sidecar.DirectoryStubOf), never the root's item passed through.
//
//	sync       a view syncs the copy first, as a desktop does at each view (§16.11's desktops row), unless a sync of it
//	           succeeded within DirectoryViewCoalesce, so the pages of one listing read one copy; a sync that fails
//	           answers its refusal and the next view syncs again. Nothing else syncs on a page Worker: it runs no
//	           schedule of its own while no page asks
//	list       the filter's fields are DirectoryFilter's, applied by DirectoryClient.List over the folded copy alone,
//	           so nothing of a filter or a query reaches evs (I-19); the items come in the copy's order, active first
//	           within each kind, then the latest renewed, DirectoryMaxItemCount a page, and the reply names where the
//	           next starts
//	one item   announcement_id names one item (DirectoryClient.Get), private markers included; where two writers use
//	           the id the reply is ambiguous until the request names the writer
//	who        each item's member is the one this device's state names, its signer's or a tagged signer's sponsor,
//	           never the id the writer wrote, and a page shows its own config's name for it (config_view)
//	prose      titles, descriptions, claim text and handoff notes are written automatically or to persuade, so they
//	           are untrusted whoever wrote them (D-MCP-29). They cross only in UntrustedText, by field name, for the
//	           page to show a person labeled as untrusted (§16.11's people row: the apps show prose to people), never
//	           as an instruction, a link target or an agent's input; an untrusted writer's identifiers cross the same
//	           way. Identifiers holds identifier fields only while their writer is trusted
//	nothing    a page Worker is read_only and admin_refused: this operation files no request or decision, announces,
//	filed      withdraws or revokes nothing, and the seam it reads, WorkerDirectory, has no call that would
//
// What a directory holds is a run's who, what and where and the repos and workers announced (I-20); a view shows
// nothing beyond the item.

package sidecarworker

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// A kind's name in a request and a view.
const (
	directoryKindRun    = "run"
	directoryKindWorker = "worker"
	directoryKindRepo   = "repo"
)

// The kind a request names, false for none of run, worker or repo.
func directoryKindNamed(name string) (sidecar.DirectoryAnnouncementKind, bool) {
	switch name {
	case directoryKindRun:
		return sidecar.DirectoryAnnouncementKindRun, true
	case directoryKindWorker:
		return sidecar.DirectoryAnnouncementKindWorker, true
	case directoryKindRepo:
		return sidecar.DirectoryAnnouncementKindRepo, true
	}
	return 0, false
}

// What directory_view's fields must hold together: a writer names an announcement's, and an announcement id is the
// whole request, taking no filter and no page.
func checkDirectoryViewFields(fields *requestFields) error {
	if fields.writerSidecarId != nil && fields.announcementId == nil {
		return errors.New("a writer names the writer of an announcement id")
	}
	if fields.announcementId != nil && (fields.kind != 0 || fields.memberId != nil || fields.repoIdFilter != nil || fields.activeOnly ||
		fields.query != "" || fields.includePrivate || fields.after != 0) {
		return errors.New("an announcement id takes no filter and no page")
	}
	return nil
}

// The request's filter as List takes it.
func (self *requestFields) directoryFilter() *sidecar.DirectoryFilter {
	return &sidecar.DirectoryFilter{
		Kind:           self.kind,
		MemberId:       self.memberId,
		RepoId:         self.repoIdFilter,
		ActiveOnly:     self.activeOnly,
		Query:          self.query,
		IncludePrivate: self.includePrivate,
	}
}

// The team directory's current items, a page at a time, or one item by its id.
func runDirectoryView(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	if worker.args.Directory == nil {
		return refusalReply("", CodeUnavailable)
	}
	directory, err := worker.args.Directory()
	if err != nil {
		return errorReply(err)
	}
	if directory == nil {
		return refusalReply("", CodeUnavailable)
	}
	directoryId, ok := directory.DirectoryId()
	if !ok {
		return refusalReply("", CodeNoDirectory)
	}
	if err := worker.syncDirectory(ctx, directory, directoryId); err != nil {
		return errorReply(err)
	}
	if err := step.step(ctx); err != nil {
		return errorReply(err)
	}
	var items []*sidecar.DirectoryItem
	if fields.announcementId != nil {
		item, err := directory.Get(*fields.announcementId, fields.writerSidecarId)
		if err != nil {
			return errorReply(err)
		}
		items = []*sidecar.DirectoryItem{item}
	} else if items, err = directory.List(fields.directoryFilter()); err != nil {
		return errorReply(err)
	}
	view := &DirectoryView{
		DirectoryId: directoryId.String(),
		ItemCount:   len(items),
		Items:       []*DirectoryItemView{},
	}
	for i := fields.after; i < len(items); i++ {
		if err := step.step(ctx); err != nil {
			return errorReply(err)
		}
		if worker.settings.DirectoryMaxItemCount <= len(view.Items) {
			view.Next = strconv.Itoa(i)
			break
		}
		itemView, err := directoryItemViewOf(items[i])
		if err != nil {
			return errorReply(err)
		}
		view.Items = append(view.Items, itemView)
	}
	return &Reply{Ok: true, Directory: view}
}

// Syncs directory's copy unless a sync of directoryId succeeded within DirectoryViewCoalesce, remembering when one that
// succeeds started. A sync that fails leaves nothing remembered, so the next view syncs again; a clock that went back
// syncs.
func (self *Worker) syncDirectory(ctx context.Context, directory WorkerDirectory, directoryId sidecar.Id) error {
	startedAt := self.now()
	due := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.syncedAt.IsZero() || self.syncedDirectoryId != directoryId {
			return true
		}
		elapsed := startedAt.Sub(self.syncedAt)
		return elapsed < 0 || self.settings.DirectoryViewCoalesce <= elapsed
	}()
	if !due {
		return nil
	}
	if err := directory.Sync(ctx); err != nil {
		return err
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.syncedDirectoryId = directoryId
		self.syncedAt = startedAt
	}()
	return nil
}

// A page of the team directory's current items.
type DirectoryView struct {
	DirectoryId string `json:"directory_id"`
	// How many items the filter kept, or 1 for one item by its id.
	ItemCount int                  `json:"item_count"`
	Items     []*DirectoryItemView `json:"items"`
	// The index of the next page's first item, as directory_view's after takes it; empty when this page is the last.
	Next string `json:"next,omitempty"`
}

// One current item of the team directory as the console shows it to a person: what this device's own verified state
// attests, the identifier fields when their writer is trusted, and everything else as untrusted text (§16.11's
// classes and taint rows).
type DirectoryItemView struct {
	AnnouncementId string `json:"announcement_id"`
	// run, worker or repo.
	Kind string `json:"kind"`
	// Raised at each content change, decimal.
	Version string `json:"version"`
	// active, stale, ended, handed_off or private.
	Display string `json:"display"`
	// The sidecar that signed the standing version, whose own stream carried it, and evs's time for that push.
	SignerSidecarId string `json:"signer_sidecar_id"`
	PushTimeMillis  int64  `json:"push_time_millis,omitempty"`
	// Whose the item is in this device's state: the signer's member, or a tagged or ephemeral signer's sponsor; empty
	// where none stands, which AutogroupAdmin then shows as autogroup:admin (§16.11's who row).
	MemberId       string `json:"member_id,omitempty"`
	AutogroupAdmin bool   `json:"autogroup_admin,omitempty"`
	// The signer is a member's own device, untagged.
	MemberDevice bool `json:"member_device,omitempty"`
	// The writing run was tainted: its text is untrusted twice over (D-MCP-29).
	Tainted       bool  `json:"tainted,omitempty"`
	StartedMillis int64 `json:"started_millis,omitempty"`
	RenewedMillis int64 `json:"renewed_millis,omitempty"`
	// Runs and workers; a repo item carries none.
	ExpiresMillis int64 `json:"expires_millis,omitempty"`
	// A run's agent and its run; a repo item's repo, or the repo a run is in.
	AgentId string `json:"agent_id,omitempty"`
	RunId   string `json:"run_id,omitempty"`
	RepoId  string `json:"repo_id,omitempty"`
	// A worker's keyed tag ids (§7.9), which config_view names.
	TagIds []string `json:"tag_ids,omitempty"`
	// A repo item's kind (general, record, ...) and the roles it offers (deposit, reader, writer).
	RepoKind string   `json:"repo_kind,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	// A repo's private marker, or a run in a repo with no live announcement: no name shown.
	Private bool `json:"private,omitempty"`
	// A worker's sidecar is ephemeral.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// A worker's or a repo's requests: open or closed.
	Requests string `json:"requests,omitempty"`
	// A run's claims, their attested parts; their text and targets are fields of UntrustedText or Identifiers.
	Claims []*DirectoryClaimView `json:"claims,omitempty"`
	// A run's handoff, its attested parts; its agent label and note are fields of Identifiers or UntrustedText.
	Handoff *DirectoryHandoffView `json:"handoff,omitempty"`
	// member or untrusted: whether Identifiers holds the identifier fields.
	IdentifierTrust string `json:"identifier_trust"`
	// The identifier fields by name, only when their trust is member: labels, harness, model, project, branch, topics,
	// claim targets, service names, and the writer's member and host labels.
	Identifiers map[string]string `json:"identifiers,omitempty"`
	// A worker's services: each one's kind, and its name when its writer is trusted.
	Services []*DirectoryServiceView `json:"services,omitempty"`
	// The fields a query matched, by name, never by their text.
	Matches []string `json:"matches,omitempty"`
	// Untrusted text for a person to read, by field name: prose whoever wrote it, and an untrusted writer's
	// identifiers. The page shows each labeled as untrusted, never as an instruction, a link or an agent's input.
	UntrustedText map[string]string `json:"untrusted_text,omitempty"`
}

// One claim of a run, as its item carries it.
type DirectoryClaimView struct {
	ClaimId string `json:"claim_id"`
	// active, released or handed_off.
	State         string `json:"state"`
	ExpiresMillis int64  `json:"expires_millis,omitempty"`
}

// A run's handoff.
type DirectoryHandoffView struct {
	// The member it was handed to, empty for an agent's.
	ToMemberId string `json:"to_member_id,omitempty"`
	Millis     int64  `json:"millis,omitempty"`
}

// One service a worker announces.
type DirectoryServiceView struct {
	// socket, tls_host or bridge_port.
	Kind string `json:"kind"`
	// Withheld with the other identifiers when their trust is untrusted; its text is then in UntrustedText.
	Name string `json:"name,omitempty"`
}

// item's view: the stub's attested and identifier fields as they are, everything it holds back as untrusted text,
// and the attested fields the stub does not carry. Nothing of the root's item crosses but these fields.
func directoryItemViewOf(item *sidecar.DirectoryItem) (*DirectoryItemView, error) {
	stub, err := sidecar.DirectoryStubOf(item, true)
	if err != nil {
		return nil, err
	}
	announcement := item.Announcement
	who := announcement.GetWho()
	what := announcement.GetWhat()
	where := announcement.GetWhere()
	view := &DirectoryItemView{
		AnnouncementId:  stub.AnnouncementId,
		Kind:            stub.Kind,
		Version:         strconv.FormatUint(stub.Version, 10),
		Display:         stub.Display,
		SignerSidecarId: stub.SidecarId,
		PushTimeMillis:  item.PushTimeMillis,
		MemberId:        stub.MemberId,
		AutogroupAdmin:  item.AutogroupAdmin,
		MemberDevice:    item.MemberDevice,
		Tainted:         stub.Tainted,
		StartedMillis:   announcement.GetStartedMillis(),
		RenewedMillis:   stub.RenewedMillis,
		ExpiresMillis:   stub.ExpiresMillis,
		AgentId:         optionalIdString(who.GetAgentId()),
		RunId:           stub.RunId,
		RepoId:          stub.RepoId,
		Private:         where.GetPrivate(),
		Ephemeral:       where.GetEphemeral(),
		Requests:        stub.Requests,
		IdentifierTrust: stub.IdentifierTrust,
		Identifiers:     stub.Identifiers,
		Matches:         slices.Clone(stub.Matches),
		UntrustedText:   stub.Untrusted,
	}
	for _, tagId := range who.GetTagIds() {
		if text := optionalIdString(tagId); text != "" {
			view.TagIds = append(view.TagIds, text)
		}
	}
	if sidecar.DirectoryAnnouncementKind(announcement.GetKind()) == sidecar.DirectoryAnnouncementKindRepo {
		view.RepoKind = repoKindName(protocol.RepoKind(what.GetRepoKind()))
		for _, role := range what.GetRoles() {
			view.Roles = append(view.Roles, repoGrantRoleName(protocol.RepoGrantRole(role)))
		}
	}
	for _, claim := range announcement.GetClaims() {
		view.Claims = append(view.Claims, &DirectoryClaimView{
			ClaimId:       optionalIdString(claim.GetClaimId()),
			State:         directoryClaimStateName(sidecar.DirectoryClaimState(claim.GetState())),
			ExpiresMillis: claim.GetExpiresMillis(),
		})
	}
	if handoff := announcement.GetHandoff(); handoff != nil {
		view.Handoff = &DirectoryHandoffView{ToMemberId: optionalIdString(handoff.GetToMemberId()), Millis: handoff.GetMillis()}
	}
	for _, service := range stub.Services {
		view.Services = append(view.Services, &DirectoryServiceView{Kind: service.Kind, Name: service.Name})
	}
	return view, nil
}

// A claim state's name.
func directoryClaimStateName(state sidecar.DirectoryClaimState) string {
	switch state {
	case sidecar.DirectoryClaimStateActive:
		return "active"
	case sidecar.DirectoryClaimStateReleased:
		return "released"
	case sidecar.DirectoryClaimStateHandedOff:
		return "handed_off"
	}
	return "unknown"
}

// A repo kind's name (§11.12).
func repoKindName(kind protocol.RepoKind) string {
	switch kind {
	case protocol.RepoKind_REPO_KIND_GENERAL:
		return "general"
	case protocol.RepoKind_REPO_KIND_RECORD:
		return "record"
	case protocol.RepoKind_REPO_KIND_CONFIG:
		return "config"
	case protocol.RepoKind_REPO_KIND_DIRECTORY:
		return "directory"
	case protocol.RepoKind_REPO_KIND_TEAM_RECORD:
		return "team_record"
	case protocol.RepoKind_REPO_KIND_MAILCFG:
		return "mailcfg"
	case protocol.RepoKind_REPO_KIND_VAULT:
		return "vault"
	case protocol.RepoKind_REPO_KIND_MAILBOX:
		return "mailbox"
	case protocol.RepoKind_REPO_KIND_SECRET:
		return "secret"
	}
	return "unknown"
}

// A grant role's name (plan-RECORD-small-readings #4).
func repoGrantRoleName(role protocol.RepoGrantRole) string {
	switch role {
	case protocol.RepoGrantRole_REPO_GRANT_ROLE_DEPOSIT:
		return "deposit"
	case protocol.RepoGrantRole_REPO_GRANT_ROLE_READER:
		return "reader"
	case protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER:
		return "writer"
	}
	return "unknown"
}
