//go:build !ios_extension && !js

// §18.5's bound search types (SDK-M10-01; R-SDK-13, D-SDK-22): the query an app asks, the hits and progress a search
// streams, a repo's status and the indexer a remote search names. Each crosses gobind, and as json the C ABI and js
// (gen_types.go), so every field is a string, a bool, an int32 or int64 count or milliseconds, an *Id or an
// exportedList, never a sidecar-package value (R-SDK-5). Apps only render (D-SDK-22): assembly, masking, confirmation,
// ranking, indexer choice and merging are the core's, and a hit here carries what the core decided, mapped field by
// field.
//
// A query's text is the person's own and goes only between the app and its core on this device, or its daemon in
// remote mode: nothing here logs one, stores one or puts one in an error's text (I-19, I-13).

package sdk

// §18.5's query. json in the C ABI and js.
type SidecarSearchQuery struct {
	// §11.14's query: terms AND; "quoted" phrases; -term drops only confirmed containment.
	Text string `json:"text"`
	// Empty: every repo this sidecar reads with its index on.
	RepoIds *IdList `json:"repo_ids,omitempty"`
	// cached (the default: buckets and any cached all), latest, or all ("search more"). Empty is cached.
	Scope string `json:"scope,omitempty"`
	// Unix milliseconds; 0 is unbounded. A range inside the buckets never needs all.
	SinceMillis int64 `json:"since_millis,omitempty"`
	UntilMillis int64 `json:"until_millis,omitempty"`
	// Near hits even when 3 or more exact hits exist.
	Fuzzy bool `json:"fuzzy,omitempty"`
	// "all" may download past §18.4's 25 MiB on a metered link.
	AllowMetered bool `json:"allow_metered,omitempty"`
	// "": the network's remote mode (§18.4); "off": this search stays on the device.
	Remote string `json:"remote,omitempty"`
	// Hits a page; 0 is §18.5's 50.
	PageSize int32 `json:"page_size,omitempty"`
}

// The query scopes, as SidecarSearchQuery.Scope names them.
const (
	SidecarSearchScopeCached = "cached"
	SidecarSearchScopeLatest = "latest"
	SidecarSearchScopeAll    = "all"
)

// A query's remote setting that keeps the search on the device.
const SidecarSearchRemoteOff = "off"

// One hit, as the core decided it (§18.5). json in the C ABI and js.
type SidecarSearchHit struct {
	RepoId *Id `json:"repo_id"`
	// The document's path, or a derived document's registered name (§11.14).
	Path string `json:"path"`
	// The path's version the hit rests on.
	Version int64 `json:"version"`
	// Unix milliseconds.
	TimeMillis int64 `json:"time_millis"`
	// The subject or the title.
	Title string `json:"title,omitempty"`
	// The sender, or the writer's label.
	From string `json:"from,omitempty"`
	// Verified hits: around the match; mail: i/'s snippet; else empty.
	Snippet string `json:"snippet,omitempty"`
	// verified, confirmed, possible, near or unmatched: a local possible or near hit an indexer's checked span
	// disproved, which apps fold into one "not matched by" row (§11.14).
	State string `json:"state"`
	// latest or all.
	Partition string `json:"partition"`
	// near: the corrected term.
	Correction string `json:"correction,omitempty"`
	// Empty: this device; else the answering indexer's config device name ("verified by").
	By string `json:"by,omitempty"`
}

// The hit states, as SidecarSearchHit.State names them.
const (
	SidecarSearchHitVerified  = "verified"
	SidecarSearchHitConfirmed = "confirmed"
	SidecarSearchHitPossible  = "possible"
	SidecarSearchHitNear      = "near"
	SidecarSearchHitUnmatched = "unmatched"
)

// The partitions, as SidecarSearchHit.Partition names them.
const (
	SidecarSearchPartitionLatest = "latest"
	SidecarSearchPartitionAll    = "all"
)

// One page's hits, in the core's rank order.
type SidecarSearchHitList struct {
	exportedList[*SidecarSearchHit]
}

// An empty list.
func NewSidecarSearchHitList() *SidecarSearchHitList {
	return &SidecarSearchHitList{
		exportedList: *newExportedList[*SidecarSearchHit](),
	}
}

// Where a search stands, beside each page (§18.5). json in the C ABI and js.
type SidecarSearchProgress struct {
	// Partitions searched, of all the search reads.
	Searched int32 `json:"searched"`
	Total    int32 `json:"total"`
	// Scope all: bytes still to fetch, and fetched so far.
	FetchByteCount   int64 `json:"fetch_byte_count,omitempty"`
	FetchedByteCount int64 `json:"fetched_byte_count,omitempty"`
	// Recent changes not yet searchable.
	Uncovered int32 `json:"uncovered,omitempty"`
	// "", metered, offline or update. metered waits until the app asks again with AllowMetered.
	Waiting string `json:"waiting,omitempty"`
	// The oldest time this search covered, in unix milliseconds; 0 for none yet.
	CoveredFromMillis int64 `json:"covered_from_millis,omitempty"`
	// "", notice, asking, answered, partial, fallback or off (§18.4's remote row).
	Remote string `json:"remote,omitempty"`
	// The indexer asked, or the one a notice names.
	RemoteBy *SidecarIndexer `json:"remote_by,omitempty"`
	// "", none, offline, busy, refused, unreachable, off or notice.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// Why the search stopped short, on a page with done set: one of the SidecarSearchCode values, empty when it did
	// not. A reading beside §18.5's fields, which give a search no error: the page still holds every hit found before.
	Code string `json:"code,omitempty"`
	// The code's fixed text, for a person. Never a query, a path or an error's own text (I-13, I-19).
	Message string `json:"message,omitempty"`
}

// Why a search, a status or a cache call did not do what it asked. A format value: apps read them.
const (
	// The Sidecar closed, the call was cancelled, no search engine runs on this sidecar yet, or no daemon answers.
	SidecarSearchCodeUnavailable = "SEARCH_UNAVAILABLE"
	// The query or the call's arguments are not ones the core takes.
	SidecarSearchCodeInvalid = "SEARCH_INVALID"
	// Too many searches at once: asking again later may run.
	SidecarSearchCodeBusy = "SEARCH_BUSY"
	// The index changed under the search (an erasure in scope, a lost read): search again.
	SidecarSearchCodeExpired = "SEARCH_EXPIRED"
	// This host may not do what was asked: a remote-mode Sidecar's background window, or a mode or notice answer on a
	// host that asks no indexer (§18.7's control row).
	SidecarSearchCodeRefused = "SEARCH_REFUSED"
	// Anything else.
	SidecarSearchCodeFailed = "SEARCH_FAILED"
)

// Each code's message, the only text a refusal carries.
var sidecarSearchCodeMessages = map[string]string{
	SidecarSearchCodeUnavailable: "search is not available now",
	SidecarSearchCodeInvalid:     "the search is not one this device takes",
	SidecarSearchCodeBusy:        "too many searches at once: try again",
	SidecarSearchCodeExpired:     "what was searched changed: search again",
	SidecarSearchCodeRefused:     "this device does not do that",
	SidecarSearchCodeFailed:      "the search failed",
}

// A repo's search status (§18.5). json in the C ABI and js.
type SidecarSearchStatus struct {
	IndexOn         bool `json:"index_on"`
	FormatSupported bool `json:"format_supported"`
	// The policy in force: background, wifi_charging, on_demand or never.
	Policy string `json:"policy"`
	// This network's remote mode: on, own or off.
	Remote string `json:"remote"`
	// Unix milliseconds: the start of the buckets held, and how far all is cached; 0 when all is not cached.
	LatestFromMillis       int64 `json:"latest_from_millis,omitempty"`
	AllCachedThroughMillis int64 `json:"all_cached_through_millis,omitempty"`
	AllCachedByteCount     int64 `json:"all_cached_byte_count,omitempty"`
	AllMissingByteCount    int64 `json:"all_missing_byte_count,omitempty"`
	Uncovered              int32 `json:"uncovered,omitempty"`
}

// An indexer that may answer this device, or the one a remote search named (§18.5). json in the C ABI and js.
// SDK-M10-03's GetIndexers lists them; a search's progress names one by RemoteBy.
type SidecarIndexer struct {
	SidecarId *Id `json:"sidecar_id"`
	// Its config device name.
	Name string `json:"name,omitempty"`
	// Its member, or a tagged device's sponsor.
	MemberId *Id    `json:"member_id,omitempty"`
	Member   string `json:"member,omitempty"`
	// A device of this member.
	Own bool `json:"own,omitempty"`
	// online, unreachable, busy or unknown.
	State string `json:"state,omitempty"`
	// Another member's: the person answered its notice.
	Acknowledged bool `json:"acknowledged,omitempty"`
	// The repos it answered in the last 24 h.
	ServedRepoIds *IdList `json:"served_repo_ids,omitempty"`
}

// App-implemented: called on a Go thread with each state of the search's current page, in order (§18.5).
type SidecarSearchListener interface {
	// hits replaces the last page given; done says no later page will come.
	SearchPage(hits *SidecarSearchHitList, progress *SidecarSearchProgress, done bool)
}
