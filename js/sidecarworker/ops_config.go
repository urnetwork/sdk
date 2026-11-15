//go:build sidecar_http || !js

// config_view, the other half of RS2's team console (§18.8's Identity bullet: a page Worker reads the config's names,
// tags, groups and policy; §23.4's M6 exit: the console shows members and policy): one verified state of §7.9's log
// and what the config head that state stands on names, each a view built field by field, never a root struct passed
// through.
//
//	members   each member by id and role, whether a recovery binding is in effect, and the emails the policy's
//	          resolution rows name it by (user:<email>)
//	sidecars  each sidecar the state holds a row for, tombstones included while it keeps them: its client name as the
//	          head's names table resolves it (ConfigNames: a duplicate takes -2, -3 in admission order), its member or
//	          its tags and sponsor, whether it is ephemeral, read_only (a page Worker) or consented (§8.5), and whether
//	          it stands: live, parked, revoked, or expired by the Worker's clock
//	names     the head's tags, groups and repo labels by id, as the policy compiled from that head names them
//	policy    whether the head holds a document, the document as its config writer saved it (§8.8), and each rule's and
//	          requests rule's detail by id (AclLocalPolicy), each list in rule_id order
//	settings  self_admit and the SCIM signer, which every verifier knows from rows; the head's other ServiceSettings
//	          with §7.9's defaults, where the head opened; and the AdminSet's k of n (§7.9's authority row)
//
// Nothing else of the head crosses: not K_sock's socket/key, not the folds' statements, not the bridge ledger. The
// view carries ids, names, roles, states and the policy's text, never a key, a wrap or a signature (I-1, I-2, I-7).
// Members' own names are the api's, not the config's: a page joins them by id.
//
// The view reads the state it is given and changes nothing in it; a config_view signs and files nothing.

package sidecarworker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The prefixes AclLocalPolicy.Names gives the head's names by kind.
const (
	configNameTagPrefix   = "tag:"
	configNameGroupPrefix = "group:"
	configNameUserPrefix  = "user:"
	configNameLabelPrefix = "evs:"
)

// IP protocol numbers, as a port range names its protocol.
const (
	configIpProtocolIcmp = 1
	configIpProtocolTcp  = 6
	configIpProtocolUdp  = 17
)

// The verified config: its members, sidecars, names and policy.
func runConfigView(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	if worker.args.Config == nil {
		return refusalReply("", CodeUnavailable)
	}
	config, err := worker.args.Config()
	if err != nil {
		return errorReply(err)
	}
	if config == nil || config.State == nil {
		return refusalReply("", CodeUnavailable)
	}
	view, err := configViewOf(ctx, worker, config, step)
	if err != nil {
		if errors.Is(err, errConfigViewUnverified) {
			return refusalReply("", CodeUnverified)
		}
		return errorReply(err)
	}
	return &Reply{Ok: true, Config: view}
}

// A head's tables or rows that do not read as a verified head's would: the view shows nothing of them.
var errConfigViewUnverified = errors.New("the config does not read as verified")

// The network's config as the console shows it.
type ConfigView struct {
	NetworkId string `json:"network_id"`
	// The state's log position, decimal, and the time api sequenced it.
	Version    string `json:"version"`
	TimeMillis int64  `json:"time_millis"`
	// The newest base's config_version, decimal: the head the names, the policy and the head's settings are of; empty
	// before base 1.
	ConfigVersion string `json:"config_version,omitempty"`
	// The head could not be opened on this device: names, tags, groups, labels and the policy are empty, and the
	// head's settings unknown.
	HeadUnread bool `json:"head_unread,omitempty"`
	// How many of the AdminSet's Owners and Admins a change to the authority takes, k of n (§7.9's authority row).
	ThresholdK int                  `json:"threshold_k"`
	ThresholdN int                  `json:"threshold_n"`
	Members    []*ConfigMemberView  `json:"members"`
	Sidecars   []*ConfigSidecarView `json:"sidecars"`
	// The head's tags and groups, each by its atom id, and its repo labels by repo, in name order.
	Tags     []*ConfigNameView   `json:"tags"`
	Groups   []*ConfigNameView   `json:"groups"`
	Labels   []*ConfigNameView   `json:"labels"`
	Policy   *ConfigPolicyView   `json:"policy,omitempty"`
	Settings *ConfigSettingsView `json:"settings"`
	// A list held more than ConfigMaxRowCount rows: the rest are not shown.
	Truncated bool `json:"truncated,omitempty"`
}

// One member.
type ConfigMemberView struct {
	MemberId string `json:"member_id"`
	// owner, admin, network_admin, it_admin, billing_admin, auditor or member (§7.5).
	Role string `json:"role"`
	// A RecoveryBind is in effect for the member (§7.9's Recovery).
	RecoveryBound bool `json:"recovery_bound,omitempty"`
	// The emails the policy's resolution rows name the member by, sorted; none for a member the policy does not name.
	Emails []string `json:"emails,omitempty"`
}

// One sidecar the state holds a row for.
type ConfigSidecarView struct {
	SidecarId string `json:"sidecar_id"`
	// Its client name as the head's names table resolves it (§7.7); empty for one the table does not name.
	Name string `json:"name,omitempty"`
	// An untagged sidecar's member; a tagged sidecar's tags by atom id, and its sponsor.
	MemberId        string   `json:"member_id,omitempty"`
	TagIds          []string `json:"tag_ids,omitempty"`
	SponsorMemberId string   `json:"sponsor_member_id,omitempty"`
	Ephemeral       bool     `json:"ephemeral,omitempty"`
	// Admitted read_only: a page Worker (D-TEAM-24).
	ReadOnly bool `json:"read_only,omitempty"`
	// An untagged device consented for its member (§8.5).
	Consented bool `json:"consented,omitempty"`
	// live, parked, revoked or expired.
	State string `json:"state"`
	// When its admission took effect, ms since the epoch; 0 for the network's first device.
	AdmittedTimeMillis int64 `json:"admitted_time_millis,omitempty"`
	// When its admission ends, ms since the epoch; 0 for none.
	NotAfterMillis int64 `json:"not_after_millis,omitempty"`
}

// One name the head gives an id.
type ConfigNameView struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

// The policy as the head holds it.
type ConfigPolicyView struct {
	// The head holds a document; without one the implicit document's rules stand (D-ACL-20).
	HasDocument bool `json:"has_document"`
	// The document as its config writer saved it: text, with its bytes as base64 too where they are not UTF-8.
	Source *NameView `json:"source,omitempty"`
	// The document is longer than ConfigPolicySourceMaxByteCount, so Source is left out.
	SourceOmitted bool                     `json:"source_omitted,omitempty"`
	Rules         []*ConfigRuleView        `json:"rules"`
	Requests      []*ConfigRequestRuleView `json:"requests"`
}

// One rule's detail, beyond what rows carry (§8.10).
type ConfigRuleView struct {
	RuleId string `json:"rule_id"`
	// The document's id and note for the rule, empty for the implicit document's and the kind defaults'.
	DocumentId string `json:"document_id,omitempty"`
	Note       string `json:"note,omitempty"`
	// The ip field's ports as a document writes them (tcp:22, udp:1000-2000, 443, *); empty for every port.
	Ports []string `json:"ports,omitempty"`
	// The ip: destinations' prefixes, the sock: destinations as written, a publish's bound socket, the mail: aliases.
	Cidrs       []string `json:"cidrs,omitempty"`
	Sockets     []string `json:"sockets,omitempty"`
	BoundSocket string   `json:"bound_socket,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// One requests rule's detail.
type ConfigRequestRuleView struct {
	RuleId     string `json:"rule_id"`
	DocumentId string `json:"document_id,omitempty"`
	Note       string `json:"note,omitempty"`
	// The rule's own evidence names harness (owner decision 142).
	NamesHarness bool     `json:"names_harness,omitempty"`
	Sockets      []string `json:"sockets,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Cidrs        []string `json:"cidrs,omitempty"`
}

// The ServiceSettings the console shows.
type ConfigSettingsView struct {
	// on or off (owner decision 112), and the indexer that signs SCIM's revocations, empty for none (§7.6): every
	// verifier knows both from rows.
	SelfAdmit           string `json:"self_admit"`
	ScimSignerSidecarId string `json:"scim_signer_sidecar_id,omitempty"`
	// The head's ServiceSettings were read: the fields below hold its values, §7.9's defaults where it sets none.
	HeadKnown bool `json:"head_known"`
	// indexers or member_scopes; empty while no choice is made (D-MAIL-15).
	TeamMail          string `json:"team_mail,omitempty"`
	StaleLeafDays     uint32 `json:"stale_leaf_days,omitempty"`
	WitnessBoundHours uint32 `json:"witness_bound_hours,omitempty"`
	// The team directory's defaults (§16.11): runs auto, prompt or off; workers on or off.
	DirectoryRuns    string `json:"directory_runs,omitempty"`
	DirectoryWorkers string `json:"directory_workers,omitempty"`
}

// The bounded lists of one config view: each takes rows up to ConfigMaxRowCount, the rest marking the view truncated,
// and steps between rows so a large state yields.
type configViewBuilder struct {
	ctx    context.Context
	worker *Worker
	step   *workerStep
	view   *ConfigView
}

// Whether a list of count rows may take another, stepping first; false marks the view truncated.
func (self *configViewBuilder) take(count int) (bool, error) {
	if err := self.step.step(self.ctx); err != nil {
		return false, err
	}
	if self.worker.settings.ConfigMaxRowCount <= count {
		self.view.Truncated = true
		return false, nil
	}
	return true, nil
}

// config's view.
func configViewOf(ctx context.Context, worker *Worker, config *WorkerConfigState, step *workerStep) (*ConfigView, error) {
	state := config.State
	thresholdK, thresholdN := state.Threshold()
	view := &ConfigView{
		NetworkId:  state.NetworkId().String(),
		Version:    strconv.FormatUint(state.Pin().GetVersion(), 10),
		TimeMillis: state.TimeMs(),
		HeadUnread: config.Head == nil,
		ThresholdK: thresholdK,
		ThresholdN: thresholdN,
		Members:    []*ConfigMemberView{},
		Sidecars:   []*ConfigSidecarView{},
		Tags:       []*ConfigNameView{},
		Groups:     []*ConfigNameView{},
		Labels:     []*ConfigNameView{},
	}
	if base := state.Base(); base != nil {
		view.ConfigVersion = strconv.FormatUint(base.GetConfigVersion(), 10)
	}
	builder := &configViewBuilder{ctx: ctx, worker: worker, step: step, view: view}

	// the policy's names by kind: tags, groups and labels by id, and each member's emails by the member's derived atom
	emailsByAtom := map[sidecar.Id][]string{}
	if config.Local != nil {
		for _, name := range config.Local.Names {
			switch {
			case strings.HasPrefix(name.Text, configNameTagPrefix):
				view.Tags = append(view.Tags, &ConfigNameView{Id: name.Id.String(), Name: strings.TrimPrefix(name.Text, configNameTagPrefix)})
			case strings.HasPrefix(name.Text, configNameGroupPrefix):
				view.Groups = append(view.Groups, &ConfigNameView{Id: name.Id.String(), Name: strings.TrimPrefix(name.Text, configNameGroupPrefix)})
			case strings.HasPrefix(name.Text, configNameLabelPrefix):
				view.Labels = append(view.Labels, &ConfigNameView{Id: name.Id.String(), Name: strings.TrimPrefix(name.Text, configNameLabelPrefix)})
			case strings.HasPrefix(name.Text, configNameUserPrefix):
				emailsByAtom[name.Id] = append(emailsByAtom[name.Id], strings.TrimPrefix(name.Text, configNameUserPrefix))
			}
		}
	}
	for _, names := range []*[]*ConfigNameView{&view.Tags, &view.Groups, &view.Labels} {
		slices.SortFunc(*names, func(a *ConfigNameView, b *ConfigNameView) int {
			return strings.Compare(a.Name, b.Name)
		})
		if worker.settings.ConfigMaxRowCount < len(*names) {
			*names = (*names)[:worker.settings.ConfigMaxRowCount]
			view.Truncated = true
		}
	}

	for _, memberId := range state.MemberIds() {
		ok, err := builder.take(len(view.Members))
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		member := state.Member(memberId)
		memberView := &ConfigMemberView{
			MemberId:      memberId.String(),
			Role:          memberRoleName(member.GetRole()),
			RecoveryBound: state.CurrentBinding(memberId) != nil,
		}
		if emails := emailsByAtom[sidecar.AclDerivedAtom(sidecar.AclDerivedAtomMember, memberId.Bytes())]; 0 < len(emails) {
			memberView.Emails = slices.Sorted(slices.Values(emails))
		}
		view.Members = append(view.Members, memberView)
	}

	sidecarIds := state.SidecarIds()
	clientNames := map[sidecar.Id]string{}
	if config.Head != nil {
		table, err := config.Head.Names()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errConfigViewUnverified, err)
		}
		rows := make([]*bridge.SidecarRow, 0, len(sidecarIds))
		for _, sidecarId := range sidecarIds {
			rows = append(rows, state.SidecarRow(sidecarId))
		}
		// §13.8's Team scope labels are not this view's to judge: a head whose names a signer refused for one would not
		// have verified, so none are passed
		if clientNames, err = sidecar.ConfigNames(table, rows, nil); err != nil {
			return nil, fmt.Errorf("%w: %w", errConfigViewUnverified, err)
		}
	}
	acl, err := state.Acl()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errConfigViewUnverified, err)
	}
	nowMillis := worker.now().UnixMilli()
	for _, sidecarId := range sidecarIds {
		ok, err := builder.take(len(view.Sidecars))
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		view.Sidecars = append(view.Sidecars, configSidecarViewOf(state, acl, sidecarId, clientNames[sidecarId], nowMillis))
	}

	if config.Local != nil {
		policy, err := configPolicyViewOf(builder, config)
		if err != nil {
			return nil, err
		}
		view.Policy = policy
	}
	view.Settings = configSettingsViewOf(state)
	return view, nil
}

// One sidecar's view at state, its name name, judged expired by nowMillis.
func configSidecarViewOf(state *sidecar.ConfigState, acl *sidecar.AclState, sidecarId sidecar.Id, name string, nowMillis int64) *ConfigSidecarView {
	row := state.SidecarRow(sidecarId)
	view := &ConfigSidecarView{
		SidecarId:          sidecarId.String(),
		Name:               name,
		MemberId:           optionalIdString(row.GetMemberId()),
		SponsorMemberId:    optionalIdString(row.GetSponsor()),
		Ephemeral:          row.GetEphemeral(),
		ReadOnly:           state.ReadOnly(sidecarId),
		AdmittedTimeMillis: row.GetAdmittedTimeMs(),
		NotAfterMillis:     row.GetNotAfter(),
	}
	for _, tagId := range row.GetTagIds() {
		if text := optionalIdString(tagId); text != "" {
			view.TagIds = append(view.TagIds, text)
		}
	}
	if len(row.GetTagIds()) == 0 && !row.GetEphemeral() && view.MemberId != "" {
		view.Consented = acl.Consented(sidecarId)
	}
	switch {
	case row.GetRevokedAt() != 0:
		view.State = "revoked"
	case row.GetParkedAt() != 0:
		view.State = "parked"
	case row.GetNotAfter() != 0 && row.GetNotAfter() <= nowMillis:
		view.State = "expired"
	default:
		view.State = "live"
	}
	return view
}

// The policy's view: the document's source, when it fits, and each rule's and requests rule's detail.
func configPolicyViewOf(builder *configViewBuilder, config *WorkerConfigState) (*ConfigPolicyView, error) {
	local := config.Local
	policy := &ConfigPolicyView{
		HasDocument: local.HasDocument,
		Rules:       []*ConfigRuleView{},
		Requests:    []*ConfigRequestRuleView{},
	}
	if config.Head != nil {
		if source := config.Head.File(sidecar.ConfigHeadPolicyPath); 0 < len(source) {
			if builder.worker.settings.ConfigPolicySourceMaxByteCount < len(source) {
				policy.SourceOmitted = true
			} else {
				sourceView := nameViewOf(source)
				policy.Source = &sourceView
			}
		}
	}
	for _, rule := range local.Rules {
		ok, err := builder.take(len(policy.Rules))
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		ruleView := &ConfigRuleView{
			RuleId:      rule.RuleId.String(),
			DocumentId:  rule.DocumentId,
			Note:        rule.Note,
			Cidrs:       configPrefixTexts(rule.Cidrs),
			Sockets:     slices.Clone(rule.Sockets),
			BoundSocket: rule.BoundSocket,
			Aliases:     slices.Clone(rule.Aliases),
		}
		for _, ports := range rule.Ports {
			ruleView.Ports = append(ruleView.Ports, configPortRangeText(ports))
		}
		policy.Rules = append(policy.Rules, ruleView)
	}
	for _, request := range local.Requests {
		ok, err := builder.take(len(policy.Requests))
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		policy.Requests = append(policy.Requests, &ConfigRequestRuleView{
			RuleId:       request.RuleId.String(),
			DocumentId:   request.DocumentId,
			Note:         request.Note,
			NamesHarness: request.NamesHarness,
			Sockets:      slices.Clone(request.Sockets),
			Aliases:      slices.Clone(request.Aliases),
			Cidrs:        configPrefixTexts(request.Cidrs),
		})
	}
	return policy, nil
}

// The settings' view at state.
func configSettingsViewOf(state *sidecar.ConfigState) *ConfigSettingsView {
	view := &ConfigSettingsView{SelfAdmit: "on"}
	if !state.SelfAdmit() {
		view.SelfAdmit = "off"
	}
	if scimSigner, ok := state.ScimSigner(); ok {
		view.ScimSignerSidecarId = scimSigner.String()
	}
	headSettings, known := state.HeadSettings()
	if !known {
		return view
	}
	view.HeadKnown = true
	switch headSettings.GetTeamMail() {
	case protocol.TeamMailMode_TEAM_MAIL_MODE_INDEXERS:
		view.TeamMail = "indexers"
	case protocol.TeamMailMode_TEAM_MAIL_MODE_MEMBER_SCOPES:
		view.TeamMail = "member_scopes"
	}
	view.StaleLeafDays, _ = state.StaleLeafDays()
	view.WitnessBoundHours, _ = state.WitnessBoundHours()
	if directory, ok := state.DirectorySettings(); ok {
		switch directory.Runs {
		case protocol.DirectoryRunsMode_DIRECTORY_RUNS_MODE_AUTO:
			view.DirectoryRuns = "auto"
		case protocol.DirectoryRunsMode_DIRECTORY_RUNS_MODE_PROMPT:
			view.DirectoryRuns = "prompt"
		case protocol.DirectoryRunsMode_DIRECTORY_RUNS_MODE_OFF:
			view.DirectoryRuns = "off"
		}
		switch directory.Workers {
		case protocol.DirectoryWorkersMode_DIRECTORY_WORKERS_MODE_ON:
			view.DirectoryWorkers = "on"
		case protocol.DirectoryWorkersMode_DIRECTORY_WORKERS_MODE_OFF:
			view.DirectoryWorkers = "off"
		}
	}
	return view
}

// A member role's name.
func memberRoleName(role bridge.MemberRole) string {
	switch role {
	case bridge.MemberRole_MEMBER_ROLE_OWNER:
		return "owner"
	case bridge.MemberRole_MEMBER_ROLE_ADMIN:
		return "admin"
	case bridge.MemberRole_MEMBER_ROLE_NETWORK_ADMIN:
		return "network_admin"
	case bridge.MemberRole_MEMBER_ROLE_IT_ADMIN:
		return "it_admin"
	case bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN:
		return "billing_admin"
	case bridge.MemberRole_MEMBER_ROLE_AUDITOR:
		return "auditor"
	case bridge.MemberRole_MEMBER_ROLE_MEMBER:
		return "member"
	}
	return "unknown"
}

// A port range as a document writes it: a protocol's prefix where it names one, then * for every port, one port, or a
// range.
func configPortRangeText(ports sidecar.AclPortRange) string {
	prefix := ""
	switch ports.Protocol {
	case 0:
	case configIpProtocolTcp:
		prefix = "tcp:"
	case configIpProtocolUdp:
		prefix = "udp:"
	case configIpProtocolIcmp:
		prefix = "icmp:"
	default:
		prefix = "ip" + strconv.FormatUint(uint64(ports.Protocol), 10) + ":"
	}
	switch {
	case ports.First == 0:
		return prefix + "*"
	case ports.First == ports.Last:
		return prefix + strconv.FormatUint(uint64(ports.First), 10)
	}
	return prefix + strconv.FormatUint(uint64(ports.First), 10) + "-" + strconv.FormatUint(uint64(ports.Last), 10)
}

// Each prefix's text, nil for none.
func configPrefixTexts(prefixes []netip.Prefix) []string {
	if len(prefixes) == 0 {
		return nil
	}
	texts := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		texts = append(texts, prefix.String())
	}
	return texts
}
