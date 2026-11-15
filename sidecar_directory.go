//go:build !ios_extension && !js

// SidecarDirectory (§18.5, §18.4's directory row, D-SDK-28, R-SDK-12; SDK-M6-03): an app's surface on its network's
// team directory (§16.11), each call a thin mapping onto the core's DirectoryClient. Nothing here adds directory
// logic: the fetch order, the metered cap, the fold, the filter, a query's match, the redaction, the limits and every
// refusal are the core's, and the stubs that part attested and identifier fields from prose are the core's
// DirectoryStubOf and DirectoryRequestStubOf.
//
//	view       a phone fetches announcements only while a List's Sub lives. The first List opens the core's
//	           DirectoryView, every List while it is open shares it, and closing the last Sub closes it, which drops
//	           everything it fetched. With no Sub open nothing is fetched, Get answers nil, and no timer runs
//	coverage   each page a listener hears names how much of the directory the view holds: the announcing sidecars
//	           fetched and left, the bytes left, and waiting=metered while the metered cap holds it. A partial view is
//	           never reported complete, and no page is delivered before the view's first fetch has answered, so none
//	           reads as an empty directory
//	continue   a List whose filter sets AllowMetered lifts the cap on the open view, which goes on from where it
//	           stopped and reads nothing twice; so does an unmetered link, once the host reports it (setUnmetered,
//	           which SDK-M10-02's SetConditions calls). A link nobody reported counts as metered (§18.4)
//	refresh    while the view is open it is fetched again every ViewRefreshInterval, which lists what moved since
//	prose      titles, descriptions, claims, notes and reasons are written automatically or to persuade, so they are
//	           untrusted whoever wrote them. They cross only in UntrustedText, by field name, for an app to show a
//	           person as untrusted text: never in a field an app may act on, link from or pass to an agent. An
//	           untrusted writer's identifiers cross the same way
//	person     AnnounceRepo and RevokeRepo are a person's acts in the app's UI, so they go to the core, and on the
//	           remote backend to the daemon, as confirmed (D-CORE-13). SetRuns, SetWorker and Request are the person's
//	           own too; a request is filed as this person's and passes the core's gate
//	revoke     RevokeRepo's result reads hidden: the private marker hides the announcement at once, while the erasure
//	           waits on the directory's erasure runner, and SidecarEvent directory_erasure names one still pending
//	           past 24 h
//	remote     a remote-mode Sidecar forwards to §5.8's ur directory methods: directory.list for a view,
//	           directory.announce and directory.revoke for the repo acts. §5.8 has no method yet for a member's runs
//	           value by project, the worker setting, filing a request or listing requests (RECORD-M6-23's), so those
//	           answer unavailable there and sign nothing
//
// GetDirectory answers nil outside a team network. Every callback answers once, on a Go thread; a listener hears its
// pages in order on a Go thread of its own, the newest replacing one it has not taken yet, since each page is the
// whole view. Nothing here logs an announcement's text, a repo's name or a request's reason (I-13). Safe for
// concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// What an app's directory calls are held to.
type sidecarDirectorySettings struct {
	// How often an open view is fetched again, which lists what moved since its last listing: 30 s, the core's
	// coalescing of a view's syncs (§16.11). 0 fetches only when a List opens or continues the view.
	ViewRefreshInterval time.Duration
	// The bound of one act: SetRuns, SetWorker, Request, AnnounceRepo, RevokeRepo. 30 s.
	CallTimeout time.Duration
	// The bound of GetRequests, which answers on the caller's thread: 10 s.
	ReadTimeout time.Duration
	// The bound of the remote backend's question whether its daemon's network has a directory, which GetDirectory
	// asks on the caller's thread: 2 s.
	StandingTimeout time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarDirectorySettings() *sidecarDirectorySettings {
	return &sidecarDirectorySettings{
		ViewRefreshInterval: 30 * time.Second,
		CallTimeout:         30 * time.Second,
		ReadTimeout:         10 * time.Second,
		StandingTimeout:     2 * time.Second,
	}
}

// The values the bound types name. Format values: apps read them.
const (
	// SidecarDirectoryFilter.Kind and SidecarAnnouncement.Kind.
	SidecarDirectoryKindRun    = "run"
	SidecarDirectoryKindWorker = "worker"
	SidecarDirectoryKindRepo   = "repo"
	// SidecarAccessRequest.TargetKind.
	SidecarAccessTargetRepo   = "repo"
	SidecarAccessTargetWorker = "worker"
	SidecarAccessTargetSocket = "socket"
	// SidecarAccessRequest.Access.
	SidecarAccessDeposit = "deposit"
	SidecarAccessReader  = "reader"
	SidecarAccessWriter  = "writer"
	SidecarAccessDial    = "dial"
	// GetRequests' views.
	SidecarAccessRequestsMine     = "mine"
	SidecarAccessRequestsIncoming = "incoming"
	// SidecarRepoAnnouncement.Requests.
	SidecarDirectoryRequestsOpen   = "open"
	SidecarDirectoryRequestsClosed = "closed"
	// RevokeRepo's result: the announcement is hidden at once, its erasure pending on the runner.
	SidecarDirectoryRevokeHidden = "hidden"
)

var (
	// The Sidecar closed, or the call's context ended, before an answer.
	errSidecarDirectoryUnavailable = errors.New("the directory is unavailable: the sidecar closed or the call was cancelled")
	// The network has no team directory now: it is personal, or its directory does not stand yet.
	errSidecarDirectoryNone = errors.New("this network has no team directory")
	// The core has no RevokeRepo yet (RECORD-M6-20): nothing was signed, and the announcement stands.
	errSidecarDirectoryRevokeUnbuilt = errors.New("revoking a repo's announcement is not built in the core yet (RECORD-M6-20): nothing was signed")
	// §5.8 has no control method for the call (RECORD-M6-23): nothing was sent.
	errSidecarDirectoryRemoteMethod = errors.New("the daemon's control mode has no method for this directory call yet: nothing was sent")
)

// What List keeps, and whether the person allowed the view's metered bytes. json in the C ABI and js.
type SidecarDirectoryFilter struct {
	// run, worker or repo; empty for every kind.
	Kind string `json:"kind,omitempty"`
	// Only this member's items.
	MemberId *Id `json:"member_id,omitempty"`
	// Only a run in this repo, or this repo's own item.
	RepoId *Id `json:"repo_id,omitempty"`
	// Only items shown active now.
	ActiveOnly bool `json:"active_only,omitempty"`
	// Words each of which must appear in some text field of an item. It is matched on this device and never reaches
	// the service (I-19).
	Query string `json:"query,omitempty"`
	// Private markers too.
	IncludePrivate bool `json:"include_private,omitempty"`
	// The person allowed this view's metered bytes: the view goes on past the metered cap from where it stopped.
	AllowMetered bool `json:"allow_metered,omitempty"`
}

// One named text field.
type SidecarDirectoryField struct {
	// The field's name in the announcement or request, as the core's stubs name it.
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Named text fields, by name.
type SidecarDirectoryFieldList struct {
	exportedList[*SidecarDirectoryField]
}

// An empty list.
func NewSidecarDirectoryFieldList() *SidecarDirectoryFieldList {
	return &SidecarDirectoryFieldList{
		exportedList: *newExportedList[*SidecarDirectoryField](),
	}
}

// One service a worker announces.
type SidecarDirectoryService struct {
	// socket, tls_host or bridge_port.
	Kind string `json:"kind"`
	// Its name, only when the item's identifiers are trusted.
	Name string `json:"name,omitempty"`
}

// A worker's services.
type SidecarDirectoryServiceList struct {
	exportedList[*SidecarDirectoryService]
}

// An empty list.
func NewSidecarDirectoryServiceList() *SidecarDirectoryServiceList {
	return &SidecarDirectoryServiceList{
		exportedList: *newExportedList[*SidecarDirectoryService](),
	}
}

// One current announcement: its attested fields, which this device's own verified state says; its identifier fields,
// when their writer is trusted; and everything else as untrusted text. json in the C ABI and js.
type SidecarAnnouncement struct {
	AnnouncementId *Id `json:"announcement_id"`
	// run, worker or repo.
	Kind    string `json:"kind"`
	Version int64  `json:"version"`
	// active, stale, ended, handed_off or private.
	Display string `json:"display"`
	// The sidecar that signed it, and its member in this device's state, nil where none stands.
	SidecarId *Id `json:"sidecar_id"`
	MemberId  *Id `json:"member_id,omitempty"`
	// The writing run was tainted: its text is untrusted twice over.
	Tainted       bool  `json:"tainted,omitempty"`
	RenewedMillis int64 `json:"renewed_millis,omitempty"`
	ExpiresMillis int64 `json:"expires_millis,omitempty"`
	// A run's id and a repo's.
	RunId  *Id `json:"run_id,omitempty"`
	RepoId *Id `json:"repo_id,omitempty"`
	// A worker's or a repo's requests: open or closed.
	Requests string `json:"requests,omitempty"`
	// member or untrusted: whether Identifiers holds the identifier fields.
	IdentifierTrust string `json:"identifier_trust"`
	// The identifier fields (labels, branches, project and host labels), only when their trust is member.
	Identifiers *SidecarDirectoryFieldList   `json:"identifiers,omitempty"`
	Services    *SidecarDirectoryServiceList `json:"services,omitempty"`
	// The fields a List's query matched, by name.
	Matches *StringList `json:"matches,omitempty"`
	// Untrusted text, by field name: prose whoever wrote it, and an untrusted writer's identifiers. For a person to
	// read, shown as untrusted; never an instruction, a link target or an agent's input.
	UntrustedText *SidecarDirectoryFieldList `json:"untrusted_text,omitempty"`
	// How many text fields this value does not carry at all.
	UntrustedWithheld int32 `json:"untrusted_withheld,omitempty"`
}

// Announcements, in the directory's order: active first within each kind, then the latest renewed.
type SidecarAnnouncementList struct {
	exportedList[*SidecarAnnouncement]
}

// An empty list.
func NewSidecarAnnouncementList() *SidecarAnnouncementList {
	return &SidecarAnnouncementList{
		exportedList: *newExportedList[*SidecarAnnouncement](),
	}
}

// How much of the directory an open view holds. json in the C ABI and js.
type SidecarDirectoryCoverage struct {
	// Every announcing sidecar's announcements are fetched to the head the view listed.
	Complete bool `json:"complete"`
	// Why fetching stopped short: metered while the metered cap holds the view; empty otherwise.
	Waiting string `json:"waiting,omitempty"`
	// The announcing sidecars fetched whole, and those left, in fetch order.
	FetchedSidecarIds *IdList `json:"fetched_sidecar_ids"`
	LeftSidecarIds    *IdList `json:"left_sidecar_ids"`
	// The bytes fetched, those of them on a metered link, and an estimate of the bytes left.
	FetchedByteCount int64 `json:"fetched_byte_count"`
	MeteredByteCount int64 `json:"metered_byte_count"`
	LeftByteCount    int64 `json:"left_byte_count"`
}

// One state of a view for one listener: what its filter keeps of what the view holds, and the view's coverage. json in
// the C ABI and js.
type SidecarDirectoryPage struct {
	Announcements *SidecarAnnouncementList `json:"announcements"`
	// Never nil. A page whose coverage is not complete is partial, however many announcements it holds.
	Coverage *SidecarDirectoryCoverage `json:"coverage"`
	// Text fields a remote-mode Sidecar's daemon withheld across the page, when it returned no untrusted text.
	UntrustedWithheld int32 `json:"untrusted_withheld,omitempty"`
	// Why the last fetch or listing failed, for a person; empty when it did not. The page still holds what the view
	// held before.
	Message string `json:"message,omitempty"`
}

// App-implemented: called on a Go thread with each state of the view, in order.
type SidecarDirectoryListener interface {
	DirectoryChanged(page *SidecarDirectoryPage)
}

// One decision on a request.
type SidecarAccessDecision struct {
	DeciderSidecarId *Id `json:"decider_sidecar_id"`
	// approved, denied or cancelled.
	Outcome string `json:"outcome"`
	// Whether its decider was eligible for the target, by this device's state.
	Eligible           bool  `json:"eligible"`
	GrantExpiresMillis int64 `json:"grant_expires_millis,omitempty"`
	// Its note, as untrusted text.
	UntrustedText *SidecarDirectoryFieldList `json:"untrusted_text,omitempty"`
}

// A request's decisions, in push order.
type SidecarAccessDecisionList struct {
	exportedList[*SidecarAccessDecision]
}

// An empty list.
func NewSidecarAccessDecisionList() *SidecarAccessDecisionList {
	return &SidecarAccessDecisionList{
		exportedList: *newExportedList[*SidecarAccessDecision](),
	}
}

// An access request: what Request files, and what GetRequests answers. json in the C ABI and js.
type SidecarAccessRequest struct {
	// What is asked for: repo, worker or socket, with the one field its kind names.
	TargetKind string `json:"target_kind"`
	// repo: the repo.
	RepoId *Id `json:"repo_id,omitempty"`
	// worker: the worker's sidecar, and the TCP ports asked of it; none asks for its announced bridge ports (owner
	// decision 179).
	WorkerSidecarId *Id      `json:"worker_sidecar_id,omitempty"`
	TcpPorts        *IntList `json:"tcp_ports,omitempty"`
	// socket: the service's name token (§9.4), as its announcement or a refused dial names it.
	SocketToken []byte `json:"socket_token,omitempty"`
	// repo: deposit, reader or writer. worker and socket: dial, which empty also means.
	Access string `json:"access,omitempty"`
	// The grant's expiry asked, in unix milliseconds; 0 for the approver's default.
	GrantExpiresMillis int64 `json:"grant_expires_millis,omitempty"`
	// Request: how long the request itself stands, in milliseconds; 0 for the core's default.
	LifetimeMillis int64 `json:"lifetime_millis,omitempty"`
	// Request: why, as the person wrote it (prose, at most 512 B). GetRequests never sets it: a filed request's
	// reason comes back in UntrustedText.
	Reason string `json:"reason,omitempty"`
	// Request: the announcement the person acted on, if any.
	AnnouncementId *Id `json:"announcement_id,omitempty"`

	// The rest is GetRequests' alone; Request ignores it.
	RequestId          *Id `json:"request_id,omitempty"`
	RequesterSidecarId *Id `json:"requester_sidecar_id,omitempty"`
	// pending, approved, denied, cancelled or expired.
	State string `json:"state,omitempty"`
	// Whom the grant is for, and whether that is a member rather than one sidecar.
	SubjectId       *Id  `json:"subject_id,omitempty"`
	SubjectIsMember bool `json:"subject_is_member,omitempty"`
	// When the request itself ends, in unix milliseconds.
	NotAfterMillis int64 `json:"not_after_millis,omitempty"`
	// person, agent or tagged_sidecar, and an agent's or tagged sidecar's endorsement: device, presence or harness.
	FiledBy     string `json:"filed_by,omitempty"`
	Endorsement string `json:"endorsement,omitempty"`
	// The filing run was tainted.
	Tainted bool `json:"tainted,omitempty"`
	// This sidecar filed it; this device decides it now (through SidecarApprovals).
	Mine      bool                       `json:"mine,omitempty"`
	Decidable bool                       `json:"decidable,omitempty"`
	Decisions *SidecarAccessDecisionList `json:"decisions,omitempty"`
	// Its reason, as untrusted text.
	UntrustedText *SidecarDirectoryFieldList `json:"untrusted_text,omitempty"`
}

// Requests, the older first.
type SidecarAccessRequestList struct {
	exportedList[*SidecarAccessRequest]
}

// An empty list.
func NewSidecarAccessRequestList() *SidecarAccessRequestList {
	return &SidecarAccessRequestList{
		exportedList: *newExportedList[*SidecarAccessRequest](),
	}
}

// What AnnounceRepo writes of a repo. json in the C ABI and js.
type SidecarRepoAnnouncement struct {
	// Prose: the repo's name for people, at most 128 B once redacted.
	DisplayName string `json:"display_name,omitempty"`
	// Prose: what the repo holds, at most 512 B once redacted. Not named Description, which an Objective-C binding
	// refuses beside NSObject's; the json name is description.
	RepoDescription string `json:"description,omitempty"`
	// Labels of [a-z0-9-]{1,32}, at most 8.
	Topics *StringList `json:"topics,omitempty"`
	// The roles offered, of deposit, reader and writer; none offers the repo kind's default.
	Roles *StringList `json:"roles,omitempty"`
	// open or closed; empty for the repo kind's default.
	Requests string `json:"requests,omitempty"`
}

// One listing of a view: the items a filter keeps, the view's coverage, and what a daemon withheld.
type sidecarDirectoryViewPage struct {
	items             []*sidecar.DirectoryItem
	coverage          *sidecar.DirectoryCoverage
	untrustedWithheld int
}

// One open view, as SidecarDirectory asks it: *sidecar.DirectoryView through sidecarDirectoryCoreView, the daemon's
// directory.list, or a test's.
type sidecarDirectoryViewBackend interface {
	// The link changed: metered or not.
	setMetered(metered bool)
	// The person allowed this view's metered bytes.
	allowMetered()
	// Fetches what the view has left, until it is whole or the metered cap holds it; with nothing left, what moved.
	fetch(ctx context.Context) error
	// The items filter keeps of what the view holds now, and its coverage.
	list(ctx context.Context, filter *sidecar.DirectoryFilter) (*sidecarDirectoryViewPage, error)
	// Drops everything the view holds.
	close()
}

// What SidecarDirectory asks of its network's directory: the core's DirectoryClient through
// sidecarDirectoryCoreBackend, the daemon's control methods through sidecarDirectoryRemoteBackend, or a test's.
type sidecarDirectoryBackend interface {
	// Whether the network has a team directory now.
	standing() bool
	// sidecar.DirectoryClient.View.
	openView(args *sidecar.DirectoryViewArgs) (sidecarDirectoryViewBackend, error)
	// sidecar.DirectoryClient.SetRuns.
	setRuns(ctx context.Context, projectKey string, claims sidecar.DirectoryRunClaims) error
	// sidecar.DirectoryClient.SetWorker of the device's on or off alone.
	setWorker(ctx context.Context, enabled bool) error
	// sidecar.DirectoryClient.Request, answering the request's id.
	request(ctx context.Context, ask *sidecar.DirectoryRequestAsk) (sidecar.Id, error)
	// sidecar.DirectoryClient.GetRequests, as stubs that hold their prose apart.
	requests(ctx context.Context, filter *sidecar.DirectoryRequestFilter) ([]*sidecar.DirectoryRequestStub, error)
	// sidecar.DirectoryClient.AnnounceRepo, answering what the person should hear.
	announceRepo(ctx context.Context, args *sidecar.DirectoryRepoAnnounceArgs) ([]string, error)
	// §16.11's revoke row (RECORD-M6-20's DirectoryClient.RevokeRepo): the private marker over the repo's item and
	// its erasure_request, the person having confirmed it.
	revokeRepo(ctx context.Context, repoId sidecar.Id) error
}

// sidecarDirectoryViewBackend over the core's view.
type sidecarDirectoryCoreView struct {
	view *sidecar.DirectoryView
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryCoreView) setMetered(metered bool) {
	self.view.SetMetered(metered)
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryCoreView) allowMetered() {
	self.view.AllowMetered()
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryCoreView) fetch(ctx context.Context) error {
	return self.view.Fetch(ctx)
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryCoreView) list(ctx context.Context, filter *sidecar.DirectoryFilter) (*sidecarDirectoryViewPage, error) {
	page := self.view.List(filter)
	return &sidecarDirectoryViewPage{items: page.Items, coverage: page.Coverage}, nil
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryCoreView) close() {
	self.view.Close()
}

// sidecarDirectoryBackend over the core. The client is asked for at each call, since a network gains its directory,
// and the core its authorization state, after the Sidecar opens.
type sidecarDirectoryCoreBackend struct {
	local *sidecar.Sidecar
}

// The core's client, or errSidecarDirectoryNone outside a team network.
func (self *sidecarDirectoryCoreBackend) client() (*sidecar.DirectoryClient, error) {
	client := self.local.Directory()
	if client == nil {
		return nil, errSidecarDirectoryNone
	}
	return client, nil
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) standing() bool {
	return self.local.Directory() != nil
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) openView(args *sidecar.DirectoryViewArgs) (sidecarDirectoryViewBackend, error) {
	client, err := self.client()
	if err != nil {
		return nil, err
	}
	view, err := client.View(args)
	if err != nil {
		return nil, err
	}
	return &sidecarDirectoryCoreView{view: view}, nil
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) setRuns(ctx context.Context, projectKey string, claims sidecar.DirectoryRunClaims) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	return client.SetRuns(ctx, projectKey, claims)
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) setWorker(ctx context.Context, enabled bool) error {
	client, err := self.client()
	if err != nil {
		return err
	}
	return client.SetWorker(ctx, &sidecar.DirectoryWorkerSetting{On: &enabled})
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) request(ctx context.Context, ask *sidecar.DirectoryRequestAsk) (sidecar.Id, error) {
	client, err := self.client()
	if err != nil {
		return sidecar.Id{}, err
	}
	filed, err := client.Request(ctx, ask)
	if err != nil {
		return sidecar.Id{}, err
	}
	return filed.RequestId, nil
}

// See sidecarDirectoryBackend. The stubs carry their prose apart (include), for a person to read.
func (self *sidecarDirectoryCoreBackend) requests(ctx context.Context, filter *sidecar.DirectoryRequestFilter) ([]*sidecar.DirectoryRequestStub, error) {
	client, err := self.client()
	if err != nil {
		return nil, err
	}
	views, err := client.GetRequests(ctx, filter)
	if err != nil {
		// the core answers this sidecar's own beside some errors: none is passed on as the whole answer
		return nil, err
	}
	stubs := []*sidecar.DirectoryRequestStub{}
	for _, view := range views {
		stub, err := sidecar.DirectoryRequestStubOf(view, true)
		if err != nil {
			return nil, err
		}
		stubs = append(stubs, stub)
	}
	return stubs, nil
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryCoreBackend) announceRepo(ctx context.Context, args *sidecar.DirectoryRepoAnnounceArgs) ([]string, error) {
	client, err := self.client()
	if err != nil {
		return nil, err
	}
	write, err := client.AnnounceRepo(ctx, args)
	if err != nil {
		return nil, err
	}
	return write.Warnings, nil
}

// See sidecarDirectoryBackend. The core has no RevokeRepo yet, so this signs nothing and says so.
func (self *sidecarDirectoryCoreBackend) revokeRepo(ctx context.Context, repoId sidecar.Id) error {
	if _, err := self.client(); err != nil {
		return err
	}
	return errSidecarDirectoryRevokeUnbuilt
}

// §5.8's ur directory methods a remote-mode Sidecar calls, as CORE-M6-02 named them; sidecar_directory_test.go holds
// them equal to the daemon's.
const (
	sidecarControlMethodDirectoryList     = "directory.list"
	sidecarControlMethodDirectoryAnnounce = "directory.announce"
	sidecarControlMethodDirectoryRevoke   = "directory.revoke"
)

// directory.list's untrusted argument that returns prose.
const sidecarControlDirectoryUntrustedInclude = "include"

// directory.list's params.
type sidecarControlDirectoryListParams struct {
	Network   string      `json:"network,omitempty"`
	Kind      string      `json:"kind,omitempty"`
	MemberId  *sidecar.Id `json:"member_id,omitempty"`
	RepoId    *sidecar.Id `json:"repo_id,omitempty"`
	Active    bool        `json:"active,omitempty"`
	Query     string      `json:"query,omitempty"`
	Untrusted string      `json:"untrusted,omitempty"`
}

// One item of directory.list's reply, as far as this file reads it.
type sidecarControlDirectoryListItem struct {
	Kind            string          `json:"kind"`
	AnnouncementId  sidecar.Id      `json:"announcement_id"`
	WriterSidecarId sidecar.Id      `json:"writer_sidecar_id"`
	Display         string          `json:"display"`
	MemberId        *sidecar.Id     `json:"member_id,omitempty"`
	AutogroupAdmin  bool            `json:"autogroup_admin,omitempty"`
	MemberDevice    bool            `json:"member_device,omitempty"`
	Tainted         bool            `json:"tainted,omitempty"`
	Matches         []string        `json:"matches,omitempty"`
	Announcement    json.RawMessage `json:"announcement"`
}

// directory.list's reply.
type sidecarControlDirectoryListReply struct {
	Items     []*sidecarControlDirectoryListItem `json:"items"`
	Untrusted bool                               `json:"untrusted"`
	Withheld  int                                `json:"withheld"`
}

// directory.announce's params.
type sidecarControlDirectoryAnnounceParams struct {
	Network     string     `json:"network,omitempty"`
	RepoId      sidecar.Id `json:"repo_id"`
	DisplayName string     `json:"display_name,omitempty"`
	Description string     `json:"description,omitempty"`
	Topics      []string   `json:"topics,omitempty"`
	Roles       []uint32   `json:"roles,omitempty"`
	Requests    string     `json:"requests,omitempty"`
}

// directory.announce's reply.
type sidecarControlDirectoryAnnounceReply struct {
	Warnings []string `json:"warnings,omitempty"`
}

// directory.revoke's params.
type sidecarControlDirectoryRevokeParams struct {
	Network string     `json:"network,omitempty"`
	RepoId  sidecar.Id `json:"repo_id"`
}

// sidecarDirectoryBackend over a daemon's control mode. It holds nothing of the directory: every listing is the
// daemon's own copy, read through directory.list.
type sidecarDirectoryRemoteBackend struct {
	remote   sidecarBackend
	settings *sidecarDirectorySettings
	// set once the daemon has answered a listing: its network has a directory
	stands atomic.Bool
}

// The network the pairing bound, as a control call's network argument.
func (self *sidecarDirectoryRemoteBackend) network() string {
	if named, ok := self.remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// The daemon's listing for filter, with its untrusted text where the daemon returns it: a daemon that cannot read the
// caller's process returns none (§5.8), and the listing is then asked for as stubs, their withheld fields counted.
func (self *sidecarDirectoryRemoteBackend) listItems(ctx context.Context, filter *sidecar.DirectoryFilter) (*sidecarControlDirectoryListReply, error) {
	params := &sidecarControlDirectoryListParams{
		Network:   self.network(),
		MemberId:  filter.MemberId,
		RepoId:    filter.RepoId,
		Active:    filter.ActiveOnly,
		Query:     filter.Query,
		Untrusted: sidecarControlDirectoryUntrustedInclude,
	}
	if filter.Kind != 0 {
		params.Kind = sidecarDirectoryKindNames[filter.Kind]
	}
	reply := &sidecarControlDirectoryListReply{}
	err := self.remote.call(ctx, sidecarControlMethodDirectoryList, params, false, reply)
	var refusal *sidecarControlError
	if errors.As(err, &refusal) {
		params.Untrusted = ""
		reply = &sidecarControlDirectoryListReply{}
		err = self.remote.call(ctx, sidecarControlMethodDirectoryList, params, false, reply)
	}
	if err != nil {
		return nil, err
	}
	self.stands.Store(true)
	return reply, nil
}

// See sidecarDirectoryBackend. Asked on the caller's thread, within StandingTimeout, until the daemon has answered a
// listing once.
func (self *sidecarDirectoryRemoteBackend) standing() bool {
	if self.stands.Load() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.StandingTimeout)
	defer cancel()
	_, err := self.listItems(ctx, &sidecar.DirectoryFilter{Kind: sidecar.DirectoryAnnouncementKindRepo})
	return err == nil
}

// See sidecarDirectoryBackend. The view holds nothing: each listing asks the daemon.
func (self *sidecarDirectoryRemoteBackend) openView(args *sidecar.DirectoryViewArgs) (sidecarDirectoryViewBackend, error) {
	return &sidecarDirectoryRemoteView{backend: self}, nil
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryRemoteBackend) setRuns(ctx context.Context, projectKey string, claims sidecar.DirectoryRunClaims) error {
	// directory.claims chooses the calling process's project, which an app has none of
	return errSidecarDirectoryRemoteMethod
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryRemoteBackend) setWorker(ctx context.Context, enabled bool) error {
	return errSidecarDirectoryRemoteMethod
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryRemoteBackend) request(ctx context.Context, ask *sidecar.DirectoryRequestAsk) (sidecar.Id, error) {
	return sidecar.Id{}, errSidecarDirectoryRemoteMethod
}

// See sidecarDirectoryBackend.
func (self *sidecarDirectoryRemoteBackend) requests(ctx context.Context, filter *sidecar.DirectoryRequestFilter) ([]*sidecar.DirectoryRequestStub, error) {
	return nil, errSidecarDirectoryRemoteMethod
}

// See sidecarDirectoryBackend. Sent as confirmed: the call is the person's act in the app's UI (D-CORE-13).
func (self *sidecarDirectoryRemoteBackend) announceRepo(ctx context.Context, args *sidecar.DirectoryRepoAnnounceArgs) ([]string, error) {
	params := &sidecarControlDirectoryAnnounceParams{
		Network:     self.network(),
		RepoId:      args.RepoId,
		DisplayName: args.DisplayName,
		Description: args.Description,
		Topics:      args.Topics,
	}
	if args.Roles != nil {
		params.Roles = []uint32{}
		for _, role := range args.Roles {
			params.Roles = append(params.Roles, uint32(role))
		}
	}
	switch args.Requests {
	case sidecar.DirectoryRequestsOpen:
		params.Requests = SidecarDirectoryRequestsOpen
	case sidecar.DirectoryRequestsClosed:
		params.Requests = SidecarDirectoryRequestsClosed
	}
	reply := &sidecarControlDirectoryAnnounceReply{}
	if err := self.remote.call(ctx, sidecarControlMethodDirectoryAnnounce, params, true, reply); err != nil {
		return nil, err
	}
	return reply.Warnings, nil
}

// See sidecarDirectoryBackend. Sent as confirmed, as announceRepo is.
func (self *sidecarDirectoryRemoteBackend) revokeRepo(ctx context.Context, repoId sidecar.Id) error {
	params := &sidecarControlDirectoryRevokeParams{Network: self.network(), RepoId: repoId}
	return self.remote.call(ctx, sidecarControlMethodDirectoryRevoke, params, true, nil)
}

// A remote-mode Sidecar's view: the daemon's copy, which the daemon syncs at each view (§16.11), so nothing is fetched
// or metered here and every listing is whole. The daemon's listing takes no private markers.
type sidecarDirectoryRemoteView struct {
	backend *sidecarDirectoryRemoteBackend
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryRemoteView) setMetered(metered bool) {
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryRemoteView) allowMetered() {
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryRemoteView) fetch(ctx context.Context) error {
	return nil
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryRemoteView) list(ctx context.Context, filter *sidecar.DirectoryFilter) (*sidecarDirectoryViewPage, error) {
	if filter == nil {
		filter = &sidecar.DirectoryFilter{}
	}
	reply, err := self.backend.listItems(ctx, filter)
	if err != nil {
		return nil, err
	}
	page := &sidecarDirectoryViewPage{
		items:    []*sidecar.DirectoryItem{},
		coverage: &sidecar.DirectoryCoverage{Complete: true},
	}
	if !reply.Untrusted {
		page.untrustedWithheld = reply.Withheld
	}
	for _, replyItem := range reply.Items {
		if replyItem == nil {
			continue
		}
		kind, ok := sidecarDirectoryKindOf(replyItem.Kind)
		if !ok || kind == 0 {
			return nil, fmt.Errorf("the daemon listed an item of kind %q", replyItem.Kind)
		}
		announcement := &protocol.Announcement{}
		if err := protojson.Unmarshal(replyItem.Announcement, announcement); err != nil {
			return nil, errors.New("the daemon listed an item this sdk cannot read")
		}
		key := sidecar.DirectoryItemKey{Kind: kind, AnnouncementId: replyItem.AnnouncementId}
		if kind != sidecar.DirectoryAnnouncementKindRepo {
			// a repo's item folds across its admins, so its key names no writer
			key.WriterSidecarId = replyItem.WriterSidecarId
		}
		page.items = append(page.items, &sidecar.DirectoryItem{
			Key:             key,
			Announcement:    announcement,
			Display:         sidecar.DirectoryItemDisplay(replyItem.Display),
			SignerSidecarId: replyItem.WriterSidecarId,
			MemberId:        replyItem.MemberId,
			AutogroupAdmin:  replyItem.AutogroupAdmin,
			MemberDevice:    replyItem.MemberDevice,
			Tainted:         replyItem.Tainted,
			Matches:         replyItem.Matches,
		})
	}
	return page, nil
}

// See sidecarDirectoryViewBackend.
func (self *sidecarDirectoryRemoteView) close() {
}

// The kinds by the names apps and the daemon use.
var sidecarDirectoryKindNames = map[sidecar.DirectoryAnnouncementKind]string{
	sidecar.DirectoryAnnouncementKindRun:    SidecarDirectoryKindRun,
	sidecar.DirectoryAnnouncementKindWorker: SidecarDirectoryKindWorker,
	sidecar.DirectoryAnnouncementKindRepo:   SidecarDirectoryKindRepo,
}

// The kind name names, 0 for the empty name's every kind; false for any other name.
func sidecarDirectoryKindOf(name string) (sidecar.DirectoryAnnouncementKind, bool) {
	if name == "" {
		return 0, true
	}
	for kind, kindName := range sidecarDirectoryKindNames {
		if kindName == name {
			return kind, true
		}
	}
	return 0, false
}

// The core's filter of an app's; an error for a kind it does not name. A nil filter keeps every item but private
// markers.
func sidecarDirectoryFilterOf(filter *SidecarDirectoryFilter) (*sidecar.DirectoryFilter, error) {
	if filter == nil {
		return &sidecar.DirectoryFilter{}, nil
	}
	kind, ok := sidecarDirectoryKindOf(filter.Kind)
	if !ok {
		return nil, fmt.Errorf("a directory filter's kind is run, worker or repo, not %q", filter.Kind)
	}
	coreFilter := &sidecar.DirectoryFilter{
		Kind:           kind,
		ActiveOnly:     filter.ActiveOnly,
		Query:          filter.Query,
		IncludePrivate: filter.IncludePrivate,
	}
	if filter.MemberId != nil {
		memberId := sidecar.Id(filter.MemberId.id)
		coreFilter.MemberId = &memberId
	}
	if filter.RepoId != nil {
		repoId := sidecar.Id(filter.RepoId.id)
		coreFilter.RepoId = &repoId
	}
	return coreFilter, nil
}

// fields as a list, by name; nil for none.
func sidecarDirectoryFieldsOf(fields map[string]string) *SidecarDirectoryFieldList {
	if len(fields) == 0 {
		return nil
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	list := NewSidecarDirectoryFieldList()
	for _, name := range names {
		list.Add(&SidecarDirectoryField{Name: name, Value: fields[name]})
	}
	return list
}

// The bound id of an id's bytes, nil unless they are one.
func sidecarDirectoryIdOfBytes(idBytes []byte) *Id {
	id, err := sidecar.IdFromBytes(idBytes)
	if err != nil || id == (sidecar.Id{}) {
		return nil
	}
	return newId([16]byte(id))
}

// The bound id an id's text names, nil unless it names one.
func sidecarDirectoryIdOfText(text string) *Id {
	if text == "" {
		return nil
	}
	id, err := sidecar.ParseId(text)
	if err != nil {
		return nil
	}
	return newId([16]byte(id))
}

// The bound announcement of an item, through the core's stub: every field the stub holds as attested or as a trusted
// identifier crosses as one, and everything the stub holds back crosses only as untrusted text.
func sidecarAnnouncementOf(item *sidecar.DirectoryItem) (*SidecarAnnouncement, error) {
	stub, err := sidecar.DirectoryStubOf(item, true)
	if err != nil {
		return nil, err
	}
	bound := &SidecarAnnouncement{
		AnnouncementId:    newId([16]byte(item.Key.AnnouncementId)),
		Kind:              stub.Kind,
		Version:           int64(stub.Version),
		Display:           stub.Display,
		SidecarId:         newId([16]byte(item.SignerSidecarId)),
		MemberId:          sidecarIdOf(item.MemberId),
		Tainted:           stub.Tainted,
		RenewedMillis:     stub.RenewedMillis,
		ExpiresMillis:     stub.ExpiresMillis,
		RunId:             sidecarDirectoryIdOfText(stub.RunId),
		RepoId:            sidecarDirectoryIdOfText(stub.RepoId),
		Requests:          stub.Requests,
		IdentifierTrust:   stub.IdentifierTrust,
		Identifiers:       sidecarDirectoryFieldsOf(stub.Identifiers),
		UntrustedText:     sidecarDirectoryFieldsOf(stub.Untrusted),
		UntrustedWithheld: int32(stub.UntrustedWithheld),
	}
	if 0 < len(stub.Services) {
		bound.Services = NewSidecarDirectoryServiceList()
		for _, service := range stub.Services {
			bound.Services.Add(&SidecarDirectoryService{Kind: service.Kind, Name: service.Name})
		}
	}
	if 0 < len(stub.Matches) {
		bound.Matches = NewStringList()
		bound.Matches.addAll(stub.Matches...)
	}
	return bound, nil
}

// The bound coverage of a view's; a view that reports none is not complete.
func sidecarDirectoryCoverageOf(coverage *sidecar.DirectoryCoverage) *SidecarDirectoryCoverage {
	bound := &SidecarDirectoryCoverage{
		FetchedSidecarIds: NewIdList(),
		LeftSidecarIds:    NewIdList(),
	}
	if coverage == nil {
		return bound
	}
	bound.Complete = coverage.Complete
	bound.Waiting = coverage.Waiting
	bound.FetchedByteCount = coverage.FetchedByteCount
	bound.MeteredByteCount = coverage.MeteredByteCount
	bound.LeftByteCount = coverage.LeftByteCount
	for _, sidecarId := range coverage.FetchedSidecarIds {
		bound.FetchedSidecarIds.Add(newId([16]byte(sidecarId)))
	}
	for _, sidecarId := range coverage.LeftSidecarIds {
		bound.LeftSidecarIds.Add(newId([16]byte(sidecarId)))
	}
	return bound
}

// The bound request of a stub.
func sidecarAccessRequestOf(stub *sidecar.DirectoryRequestStub) *SidecarAccessRequest {
	bound := &SidecarAccessRequest{
		TargetKind:         stub.TargetKind,
		Access:             stub.Access,
		GrantExpiresMillis: stub.GrantExpiresMillis,
		RequestId:          sidecarDirectoryIdOfText(stub.RequestId),
		RequesterSidecarId: sidecarDirectoryIdOfText(stub.RequesterSidecarId),
		State:              stub.State,
		SubjectId:          sidecarDirectoryIdOfText(stub.Subject),
		SubjectIsMember:    stub.SubjectIsMember,
		NotAfterMillis:     stub.NotAfterMillis,
		FiledBy:            stub.FiledBy,
		Endorsement:        stub.Endorsement,
		Tainted:            stub.Tainted,
		Mine:               stub.Mine,
		Decidable:          stub.Decidable,
		UntrustedText:      sidecarDirectoryFieldsOf(stub.Untrusted),
	}
	switch stub.TargetKind {
	case SidecarAccessTargetRepo:
		bound.RepoId = sidecarDirectoryIdOfText(stub.Target)
	case SidecarAccessTargetWorker:
		bound.WorkerSidecarId = sidecarDirectoryIdOfText(stub.Target)
	}
	if 0 < len(stub.TcpPorts) {
		bound.TcpPorts = NewIntList()
		for _, port := range stub.TcpPorts {
			bound.TcpPorts.Add(int(port))
		}
	}
	if 0 < len(stub.Decisions) {
		bound.Decisions = NewSidecarAccessDecisionList()
		for _, decision := range stub.Decisions {
			bound.Decisions.Add(&SidecarAccessDecision{
				DeciderSidecarId:   sidecarDirectoryIdOfText(decision.DeciderSidecarId),
				Outcome:            decision.Outcome,
				Eligible:           decision.Eligible,
				GrantExpiresMillis: decision.GrantExpiresMillis,
				UntrustedText:      sidecarDirectoryFieldsOf(decision.Untrusted),
			})
		}
	}
	return bound
}

// The roles an app names, as the core's.
var sidecarAccessRoles = map[string]protocol.RepoGrantRole{
	SidecarAccessDeposit: protocol.RepoGrantRole_REPO_GRANT_ROLE_DEPOSIT,
	SidecarAccessReader:  protocol.RepoGrantRole_REPO_GRANT_ROLE_READER,
	SidecarAccessWriter:  protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER,
}

// What the core is asked to file for request, as this person's own. Only the shape is checked here, that the target's
// kind names its one field and an access the kind takes; whether the target exists, takes requests and stands under
// the requester's limits is the core's to say.
func sidecarDirectoryAskOf(request *SidecarAccessRequest) (*sidecar.DirectoryRequestAsk, error) {
	if request == nil {
		return nil, errors.New("no request")
	}
	if request.GrantExpiresMillis < 0 || request.LifetimeMillis < 0 {
		return nil, errors.New("a request's expiry and lifetime are not negative")
	}
	ask := &sidecar.DirectoryRequestAsk{
		Target:             &protocol.RequestTarget{},
		GrantExpiresMillis: request.GrantExpiresMillis,
		NotAfter:           time.Duration(request.LifetimeMillis) * time.Millisecond,
		Reason:             request.Reason,
		FiledBy:            sidecar.DirectoryFiledByPerson,
	}
	if request.AnnouncementId != nil {
		announcementId := sidecar.Id(request.AnnouncementId.id)
		ask.AnnouncementId = &announcementId
	}
	dial := func() error {
		if request.Access != "" && request.Access != SidecarAccessDial {
			return fmt.Errorf("a %s request asks dial, not %q", request.TargetKind, request.Access)
		}
		ask.Role = uint32(protocol.AclAction_ACL_ACTION_DIAL)
		return nil
	}
	switch request.TargetKind {
	case SidecarAccessTargetRepo:
		role, ok := sidecarAccessRoles[request.Access]
		if !ok || request.RepoId == nil {
			return nil, errors.New("a repo request names its repo and deposit, reader or writer")
		}
		ask.Target.RepoId = request.RepoId.Bytes()
		ask.Role = uint32(role)
	case SidecarAccessTargetWorker:
		if request.WorkerSidecarId == nil {
			return nil, errors.New("a worker request names the worker's sidecar")
		}
		if err := dial(); err != nil {
			return nil, err
		}
		ask.Target.SidecarId = request.WorkerSidecarId.Bytes()
		if request.TcpPorts != nil {
			for _, port := range request.TcpPorts.getAll() {
				if port <= 0 || 65535 < port {
					return nil, fmt.Errorf("no TCP port %d", port)
				}
				ask.Target.TcpPorts = append(ask.Target.TcpPorts, uint32(port))
			}
		}
	case SidecarAccessTargetSocket:
		if len(request.SocketToken) == 0 {
			return nil, errors.New("a socket request names the service's name token")
		}
		if err := dial(); err != nil {
			return nil, err
		}
		ask.Target.SocketToken = slices.Clone(request.SocketToken)
	default:
		return nil, fmt.Errorf("a request's target is a repo, a worker or a socket, not %q", request.TargetKind)
	}
	return ask, nil
}

// What the core is asked to write for repoId's announcement, as the person's confirmed act.
func sidecarDirectoryAnnounceArgsOf(repoId *Id, announcement *SidecarRepoAnnouncement) (*sidecar.DirectoryRepoAnnounceArgs, error) {
	if repoId == nil {
		return nil, errors.New("an announcement names its repo")
	}
	if announcement == nil {
		return nil, errors.New("no announcement")
	}
	args := &sidecar.DirectoryRepoAnnounceArgs{
		RepoId:      sidecar.Id(repoId.id),
		DisplayName: announcement.DisplayName,
		Description: announcement.RepoDescription,
		Confirmed:   true,
	}
	if announcement.Topics != nil {
		args.Topics = announcement.Topics.getAll()
	}
	if announcement.Roles != nil && 0 < announcement.Roles.Len() {
		args.Roles = []protocol.RepoGrantRole{}
		for _, name := range announcement.Roles.getAll() {
			role, ok := sidecarAccessRoles[name]
			if !ok {
				return nil, fmt.Errorf("a repo offers deposit, reader or writer, not %q", name)
			}
			args.Roles = append(args.Roles, role)
		}
	}
	switch announcement.Requests {
	case "":
	case SidecarDirectoryRequestsOpen:
		args.Requests = sidecar.DirectoryRequestsOpen
	case SidecarDirectoryRequestsClosed:
		args.Requests = sidecar.DirectoryRequestsClosed
	default:
		return nil, fmt.Errorf("requests is open or closed, not %q", announcement.Requests)
	}
	return args, nil
}

// One List: its filter, its listener and the pages on their way to it.
type sidecarDirectoryListing struct {
	ctx      context.Context
	cancel   context.CancelFunc
	filter   *sidecar.DirectoryFilter
	listener SidecarDirectoryListener
	// the newest page not yet taken
	pages chan *SidecarDirectoryPage
	// set by close: no listener call starts after it
	closed atomic.Bool
}

// A listing whose delivery runs until ctx ends or close.
func newSidecarDirectoryListing(ctx context.Context, filter *sidecar.DirectoryFilter, listener SidecarDirectoryListener) *sidecarDirectoryListing {
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &sidecarDirectoryListing{
		ctx:      cancelCtx,
		cancel:   cancel,
		filter:   filter,
		listener: listener,
		pages:    make(chan *SidecarDirectoryPage, 1),
	}
	go self.run()
	return self
}

// Leaves page for the listener, in place of one it has not taken: each page is the whole view, so the older says
// nothing the newer does not. Called by the view's one goroutine.
func (self *sidecarDirectoryListing) offer(page *SidecarDirectoryPage) {
	for {
		select {
		case self.pages <- page:
			return
		default:
		}
		select {
		case <-self.pages:
		default:
		}
	}
}

// Delivers pages in order until ctx ends.
func (self *sidecarDirectoryListing) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case page := <-self.pages:
			if self.closed.Load() {
				return
			}
			trace(func() {
				self.listener.DirectoryChanged(page)
			})
		}
	}
}

// Ends delivery without waiting, so a listener may call it: no listener call starts after it returns.
func (self *sidecarDirectoryListing) close() {
	self.closed.Store(true)
	self.cancel()
}

// The view one or more Lists hold open.
type sidecarDirectoryOpenView struct {
	ctx     context.Context
	cancel  context.CancelFunc
	backend sidecarDirectoryViewBackend
	// asks the view's goroutine for a fetch and a page to every listing
	kick chan struct{}
	// closed once the goroutine has closed the backend's view
	done chan struct{}
}

// Asks for a fetch, coalesced with one already asked for.
func (self *sidecarDirectoryOpenView) fetchSoon() {
	select {
	case self.kick <- struct{}{}:
	default:
	}
}

// One network's team directory for an app. Safe for concurrent use.
type SidecarDirectory struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  sidecarDirectoryBackend
	settings *sidecarDirectorySettings

	stateLock sync.Mutex
	// the host reported an unmetered link; a link nobody reported counts as metered
	unmetered bool
	// the open view, nil while no List's Sub lives
	view *sidecarDirectoryOpenView
	// the open view's listings
	listings map[*sidecarDirectoryListing]bool
	// what the open view held at its last page, by announcement id, for Get; an id two writers use is held as nil
	announcements map[sidecar.Id]*SidecarAnnouncement
	closed        bool
}

// A SidecarDirectory on backend until ctx ends or close.
func newSidecarDirectory(ctx context.Context, backend sidecarDirectoryBackend, settings *sidecarDirectorySettings) *SidecarDirectory {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarDirectory{
		ctx:      cancelCtx,
		cancel:   cancel,
		backend:  backend,
		settings: settings,
		listings: map[*sidecarDirectoryListing]bool{},
	}
}

// This Sidecar's team directory (§18.5): nil outside a team network, which is a personal network, a team whose
// directory does not stand yet, and a core that holds no verified state of its network yet. On the remote backend
// the daemon is asked, on this thread and within a short bound, until it has answered once.
func (self *Sidecar) GetDirectory() *SidecarDirectory {
	directory, err := traceWithReturn(func() *SidecarDirectory {
		local, remote := self.backends()
		existing, settings, backend, closed := func() (*SidecarDirectory, *sidecarDirectorySettings, sidecarDirectoryBackend, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.directory, self.directorySettings, self.directoryBackend, self.closed
		}()
		if closed {
			return nil
		}
		if existing != nil {
			if !existing.backend.standing() {
				return nil
			}
			return existing
		}
		if settings == nil {
			settings = defaultSidecarDirectorySettings()
		}
		if backend == nil {
			switch {
			case local != nil:
				backend = &sidecarDirectoryCoreBackend{local: local}
			case remote != nil:
				backend = &sidecarDirectoryRemoteBackend{remote: remote, settings: settings}
			default:
				return nil
			}
		}
		// asked with no lock held: the remote backend's answer is a control call
		if !backend.standing() {
			return nil
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.directory == nil {
			self.directory = newSidecarDirectory(self.ctx, backend, settings)
		}
		return self.directory
	})
	if err != nil {
		return nil
	}
	return directory
}

// Ends every listing and the open view: no page reaches a listener after it returns, and a call in flight answers
// unavailable. Safe to call again.
func (self *SidecarDirectory) close() {
	view, listings := func() (*sidecarDirectoryOpenView, []*sidecarDirectoryListing) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil, nil
		}
		self.closed = true
		listings := []*sidecarDirectoryListing{}
		for listing := range self.listings {
			listings = append(listings, listing)
		}
		view := self.view
		self.view = nil
		self.listings = map[*sidecarDirectoryListing]bool{}
		self.announcements = nil
		return view, listings
	}()
	for _, listing := range listings {
		listing.close()
	}
	if view != nil {
		view.cancel()
	}
	self.cancel()
}

// The host's link changed (§18.7's conditions): unmetered lets an open view go on past the metered cap. What
// SDK-M10-02's SetConditions calls; until an app reports its link, every view counts as metered.
func (self *SidecarDirectory) setUnmetered(unmetered bool) {
	view := func() *sidecarDirectoryOpenView {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.unmetered = unmetered
		return self.view
	}()
	if view == nil {
		return
	}
	view.backend.setMetered(!unmetered)
	if unmetered {
		view.fetchSoon()
	}
}

// Fetches view and delivers its pages each time one is asked for and every ViewRefreshInterval, until its last
// listing closes; then closes it, which drops what it fetched.
func (self *SidecarDirectory) runView(view *sidecarDirectoryOpenView) {
	defer close(view.done)
	defer view.backend.close()
	var refresh <-chan time.Time
	if 0 < self.settings.ViewRefreshInterval {
		ticker := time.NewTicker(self.settings.ViewRefreshInterval)
		defer ticker.Stop()
		refresh = ticker.C
	}
	for {
		select {
		case <-view.ctx.Done():
			return
		case <-view.kick:
		case <-refresh:
		}
		_, fetchErr := traceWithReturnError(func() (bool, error) {
			return true, view.backend.fetch(view.ctx)
		})
		if view.ctx.Err() != nil {
			return
		}
		self.publish(view, fetchErr)
	}
}

// What a failure reads as for a person. It names no announcement, repo or request (I-13).
func sidecarDirectoryMessageOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// One page for filter of what view holds now. A listing that fails still answers a page, which is not complete and
// says why.
func (self *SidecarDirectory) page(view *sidecarDirectoryOpenView, filter *sidecar.DirectoryFilter, fetchErr error) (*SidecarDirectoryPage, []*sidecar.DirectoryItem) {
	page := &SidecarDirectoryPage{
		Announcements: NewSidecarAnnouncementList(),
		Message:       sidecarDirectoryMessageOf(fetchErr),
	}
	viewPage, err := traceWithReturnError(func() (*sidecarDirectoryViewPage, error) {
		return view.backend.list(view.ctx, filter)
	})
	if err != nil || viewPage == nil {
		page.Coverage = sidecarDirectoryCoverageOf(nil)
		if page.Message == "" {
			page.Message = sidecarDirectoryMessageOf(err)
		}
		if page.Message == "" {
			page.Message = errSidecarDirectoryUnavailable.Error()
		}
		return page, nil
	}
	page.Coverage = sidecarDirectoryCoverageOf(viewPage.coverage)
	page.UntrustedWithheld = int32(viewPage.untrustedWithheld)
	for _, item := range viewPage.items {
		announcement, err := sidecarAnnouncementOf(item)
		if err != nil {
			// an item the core cannot stub is not shown, and the page is not whole without it
			page.Coverage.Complete = false
			if page.Message == "" {
				page.Message = sidecarDirectoryMessageOf(err)
			}
			continue
		}
		page.Announcements.Add(announcement)
	}
	return page, viewPage.items
}

// Delivers view's state to each of its listings, and keeps what Get answers from.
func (self *SidecarDirectory) publish(view *sidecarDirectoryOpenView, fetchErr error) {
	listings := func() []*sidecarDirectoryListing {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.view != view {
			return nil
		}
		listings := make([]*sidecarDirectoryListing, 0, len(self.listings))
		for listing := range self.listings {
			listings = append(listings, listing)
		}
		return listings
	}()
	if len(listings) == 0 {
		return
	}
	// everything the view holds, private markers included, for Get
	all, _ := self.page(view, &sidecar.DirectoryFilter{IncludePrivate: true}, nil)
	announcements := map[sidecar.Id]*SidecarAnnouncement{}
	for _, announcement := range all.Announcements.getAll() {
		announcementId := sidecar.Id(announcement.AnnouncementId.id)
		if _, used := announcements[announcementId]; used {
			// two writers use the id: Get names neither
			announcements[announcementId] = nil
			continue
		}
		announcements[announcementId] = announcement
	}
	pages := make([]*SidecarDirectoryPage, 0, len(listings))
	for _, listing := range listings {
		page, _ := self.page(view, listing.filter, fetchErr)
		pages = append(pages, page)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.view != view {
		return
	}
	self.announcements = announcements
	for i, listing := range listings {
		if self.listings[listing] {
			listing.offer(pages[i])
		}
	}
}

// Calls listener with what filter keeps of the directory, and again at each change, until the answer closes. While
// any List's answer is open the view is open, the only time a phone fetches announcements (§18.4): the first opens
// it, and closing the last drops everything it fetched. Past the metered cap a page's coverage is partial, naming the
// sidecars fetched and left, the bytes left and waiting=metered; a List whose filter sets AllowMetered then goes on
// from where the view stopped. A filter this sdk cannot read, and a directory that cannot be opened, answer one page
// that is not complete and says why.
func (self *SidecarDirectory) List(filter *SidecarDirectoryFilter, listener SidecarDirectoryListener) Sub {
	sub, err := traceWithReturn(func() Sub {
		if listener == nil {
			return newSub(func() {})
		}
		refuse := func(err error) Sub {
			page := &SidecarDirectoryPage{
				Announcements: NewSidecarAnnouncementList(),
				Coverage:      sidecarDirectoryCoverageOf(nil),
				Message:       sidecarDirectoryMessageOf(err),
			}
			go trace(func() {
				listener.DirectoryChanged(page)
			})
			return newSub(func() {})
		}
		coreFilter, err := sidecarDirectoryFilterOf(filter)
		if err != nil {
			return refuse(err)
		}
		allowMetered := filter != nil && filter.AllowMetered
		listing, view, err := func() (*sidecarDirectoryListing, *sidecarDirectoryOpenView, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed || self.ctx.Err() != nil {
				return nil, nil, errSidecarDirectoryUnavailable
			}
			view := self.view
			if view == nil {
				backend, err := self.backend.openView(&sidecar.DirectoryViewArgs{Metered: !self.unmetered, AllowMetered: allowMetered})
				if err != nil {
					return nil, nil, err
				}
				viewCtx, cancel := context.WithCancel(self.ctx)
				view = &sidecarDirectoryOpenView{
					ctx:     viewCtx,
					cancel:  cancel,
					backend: backend,
					kick:    make(chan struct{}, 1),
					done:    make(chan struct{}),
				}
				self.view = view
				go self.runView(view)
			} else if allowMetered {
				view.backend.allowMetered()
			}
			listing := newSidecarDirectoryListing(self.ctx, coreFilter, listener)
			self.listings[listing] = true
			return listing, view, nil
		}()
		if err != nil {
			return refuse(err)
		}
		view.fetchSoon()
		var once sync.Once
		return newSub(func() {
			once.Do(func() {
				self.endListing(listing)
			})
		})
	})
	if err != nil {
		return newSub(func() {})
	}
	return sub
}

// Ends listing; the view with it when it was the last, which drops what the view fetched.
func (self *SidecarDirectory) endListing(listing *sidecarDirectoryListing) {
	listing.close()
	view := func() *sidecarDirectoryOpenView {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.listings[listing] {
			return nil
		}
		delete(self.listings, listing)
		if 0 < len(self.listings) {
			return nil
		}
		view := self.view
		self.view = nil
		self.announcements = nil
		return view
	}()
	if view != nil {
		view.cancel()
	}
}

// The announcement with announcementId as the open view holds it: its attested fields, its identifier fields where
// their writer is trusted, and its prose only as untrusted text for people. Nil when the view holds none, when two
// writers use the id, and whenever no List holds the view open, since nothing is fetched for a Get.
func (self *SidecarDirectory) Get(announcementId *Id) *SidecarAnnouncement {
	announcement, err := traceWithReturn(func() *SidecarAnnouncement {
		if announcementId == nil {
			return nil
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.view == nil {
			return nil
		}
		return self.announcements[sidecar.Id(announcementId.id)]
	})
	if err != nil {
		return nil
	}
	return announcement
}

// Runs call within CallTimeout on a goroutine of its own and answers callback once: the result's Value on success,
// and the core's refusal otherwise, approval_pending:<id> while its gate holds the act for a person.
func (self *SidecarDirectory) act(callback SidecarResultCallback, call func(ctx context.Context) (string, string, error)) {
	go func() {
		result, err := traceWithReturnError(func() (*SidecarResult, error) {
			if self.ctx.Err() != nil {
				return sidecarResultOf("", errSidecarDirectoryUnavailable), nil
			}
			ctx, cancel := context.WithTimeout(self.ctx, self.settings.CallTimeout)
			defer cancel()
			value, message, err := call(ctx)
			if err != nil && self.ctx.Err() != nil {
				err = errSidecarDirectoryUnavailable
			}
			result := sidecarResultOf(value, err)
			if err == nil {
				result.Message = message
			}
			return result, nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// Sets this member's claims value for the project of projectKey, the empty key for the member's default: auto, prompt
// or off (owner decision 170). Off withdraws the project's running items at once, and a narrower value applies from
// each run's next session. The person's own act, never an agent's.
func (self *SidecarDirectory) SetRuns(projectKey string, value string, callback SidecarResultCallback) {
	self.act(callback, func(ctx context.Context) (string, string, error) {
		claims, err := sidecar.ParseDirectoryRunClaims(value)
		if err != nil {
			return "", "", err
		}
		return value, "", self.backend.setRuns(ctx, projectKey, claims)
	})
}

// Turns this sidecar's worker item on or off. A host that announces none, as phones never do, is refused by the core.
func (self *SidecarDirectory) SetWorker(enabled bool, callback SidecarResultCallback) {
	self.act(callback, func(ctx context.Context) (string, string, error) {
		return "", "", self.backend.setWorker(ctx, enabled)
	})
}

// Files request for a repo, a worker's announced services or a socket, with its role, expiry and reason, as this
// person's own. The result names the request's id, or approval_pending:<id> while the core's gate holds it.
func (self *SidecarDirectory) Request(request *SidecarAccessRequest, callback SidecarResultCallback) {
	self.act(callback, func(ctx context.Context) (string, string, error) {
		ask, err := sidecarDirectoryAskOf(request)
		if err != nil {
			return "", "", err
		}
		requestId, err := self.backend.request(ctx, ask)
		if err != nil {
			return "", "", err
		}
		return requestId.String(), "", nil
	})
}

// The requests of view: mine, which this sidecar filed, in any state, or incoming, which this device decides now.
// Nil when they cannot be read, and for any other view, so an answer that is not known never reads as none. It
// answers on the caller's thread, within ReadTimeout.
func (self *SidecarDirectory) GetRequests(view string) *SidecarAccessRequestList {
	requests, err := traceWithReturn(func() *SidecarAccessRequestList {
		filter := &sidecar.DirectoryRequestFilter{}
		switch view {
		case SidecarAccessRequestsMine:
			filter.Mine = true
		case SidecarAccessRequestsIncoming:
			filter.Decidable = true
		default:
			return nil
		}
		if self.ctx.Err() != nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
		defer cancel()
		stubs, err := self.backend.requests(ctx, filter)
		if err != nil {
			return nil
		}
		list := NewSidecarAccessRequestList()
		for _, stub := range stubs {
			if stub != nil {
				list.Add(sidecarAccessRequestOf(stub))
			}
		}
		return list
	})
	if err != nil {
		return nil
	}
	return requests
}

// Writes or replaces repoId's announcement. A person's act in the app's UI: it is signed as confirmed, by a repo
// admin, and for an owner-bound repo only on its owner's consented device, which the core checks. The result names
// the repo, with what the person should hear in its Message.
func (self *SidecarDirectory) AnnounceRepo(repoId *Id, announcement *SidecarRepoAnnouncement, callback SidecarResultCallback) {
	self.act(callback, func(ctx context.Context) (string, string, error) {
		args, err := sidecarDirectoryAnnounceArgsOf(repoId, announcement)
		if err != nil {
			return "", "", err
		}
		warnings, err := self.backend.announceRepo(ctx, args)
		if err != nil {
			return "", "", err
		}
		return repoId.String(), strings.Join(warnings, "\n"), nil
	})
}

// Revokes repoId's announcement, by the signers AnnounceRepo takes. A person's act in the app's UI. The private
// marker hides the announcement at once, and the result reads hidden: its erasure is a request the directory's
// erasure runner completes, and SidecarEvent directory_erasure names one still pending past 24 h (owner decision
// 181).
func (self *SidecarDirectory) RevokeRepo(repoId *Id, callback SidecarResultCallback) {
	self.act(callback, func(ctx context.Context) (string, string, error) {
		if repoId == nil {
			return "", "", errors.New("a revocation names its repo")
		}
		if err := self.backend.revokeRepo(ctx, sidecar.Id(repoId.id)); err != nil {
			return "", "", err
		}
		return SidecarDirectoryRevokeHidden, "", nil
	})
}
