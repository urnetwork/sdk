package sdk

// sn_ctf_client.go — a client's or a network's capture the flag wins, the
// "you won a slot" line beside its earnings (sn CAPTURETHEFLAG.md §10). Each
// epoch the operator auctions up to five provider and five extender slots per
// country among the miners online there for half the epoch, and pays the
// winners into its capture the flag pool, which the claim flow claims beside
// its pool. GET /sn/ctf/client (sn api/operator.yml) answers for the bearer:
// a client token gets that client's recent epochs (per role, whether it was a
// candidate and why not, its country, and whether it won) and its wins; a
// network token gets the wins of every client the network has had. Each win
// says whether its award has been claimed, and the line shows for every win
// that has not.
//
// A server before the network answer refuses a network token with 401. No
// answer of this route ever signs anyone out: its requests go over the plain
// transport, never the seam that takes a 401 as the credential's rejection,
// and a refused network token reads as no wins.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/evm"
)

// SnErrorCodeCtfUnavailable is SnCtfClientResult.Error's code when GET
// /sn/ctf/client could not be read or its answer was refused: the client's
// capture the flag status is unknown, so an app draws no line, never "no wins".
const SnErrorCodeCtfUnavailable = "ctf_unavailable"

// The roles of capture the flag: five slots of each per country.
const (
	SnCtfRoleProvider = "provider"
	SnCtfRoleExtender = "extender"
)

// Why a client was not a candidate in a role, the first rule it failed, in the
// server's order. A reason an app does not know reads as "not a candidate".
const (
	// no presence sample found it in the role during the epoch
	SnCtfReasonNotInRole = "not_in_role"
	// not an active, connected provider passing the operator's checks at the
	// epoch's end
	SnCtfReasonOfflineAtEnd = "offline_at_end"
	// its provide key is not Public
	SnCtfReasonNotPublic = "not_public"
	// no valid location at the epoch's end
	SnCtfReasonNoLocation = "no_location"
	// the extender record or all its addresses were inactive at the epoch's end
	SnCtfReasonInactive = "inactive"
	// present in its country for less than half of the epoch
	SnCtfReasonPresence = "presence"
	// its country at the end differs from where it was present
	SnCtfReasonCountryChanged = "country_changed"
	// a candidate with a lower client id has its address
	SnCtfReasonDuplicateAddress = "duplicate_address"
	// no payout coldkey
	SnCtfReasonNoWallet = "no_wallet"
	// a head client earns in one tier only
	SnCtfReasonHeadBound = "head_bound"
	// on one of the operator's own networks
	SnCtfReasonOwnNetwork = "own_network"
)

const snCtfClientTimeout = 60 * time.Second

// SnCtfClientRole is the client in one role in one epoch. Amounts are rao.
type SnCtfClientRole struct {
	// SnCtfRole*
	Role      string `json:"role"`
	Candidate bool   `json:"candidate"`
	// SnCtfReason*: why it was not a candidate; "" when it was
	Reason string `json:"reason,omitempty"`
	// its draw's country when a candidate, else its country at the epoch's
	// end: a lowercase ISO 3166-1 alpha-2 code; "" without one
	Country string `json:"country,omitempty"`
	// 4 or 6, the family of its unit's address; 0 without one
	UnitFamily int64 `json:"unit_family,omitempty"`
	// its quarter-hour presence samples in the role, 672 an epoch: present in
	// its country, required (half of those not excused), taken and excused.
	// All 0 when no sample found it in the role
	PresencePresent  int64 `json:"presence_present"`
	PresenceRequired int64 `json:"presence_required"`
	PresenceSamples  int64 `json:"presence_samples"`
	PresenceExcused  int64 `json:"presence_excused"`
	// the units in its country's draw for the role; 0 when not a candidate
	UnitsInDraw int64 `json:"units_in_draw,omitempty"`
	Won         bool  `json:"won"`
	// 1 to 5 when it won, else 0
	Rank        int64 `json:"rank,omitempty"`
	SlotRao     int64 `json:"slot_rao"`
	PotShareRao int64 `json:"pot_share_rao"`
	// its pot share is a pending award, paid when the carry returns
	Pending bool `json:"pending"`
	// the coldkey its award is paid to; "" when it did not win
	ColdkeySs58 string `json:"coldkey_ss58,omitempty"`
}

// SnCtfClientEpoch is the client in one epoch that a committed root covers.
type SnCtfClientEpoch struct {
	Epoch int64 `json:"epoch"`
	// a census was committed in time and the draw ran
	Drawn bool `json:"drawn"`
	// "pending": the root is not final, and its awards are not claimable yet;
	// "finalized": they are in the capture the flag account
	RootStatus string           `json:"root_status"`
	Provider   *SnCtfClientRole `json:"provider"`
	Extender   *SnCtfClientRole `json:"extender"`
}

type SnCtfClientEpochList struct {
	exportedList[*SnCtfClientEpoch]
}

func NewSnCtfClientEpochList() *SnCtfClientEpochList {
	return &SnCtfClientEpochList{
		exportedList: *newExportedList[*SnCtfClientEpoch](),
	}
}

// Whether a win's award has been claimed from vault2 (SnCtfWin.ClaimState).
const (
	// not claimed yet: its root is pending, its pot share waits for the carry,
	// or the coldkey's capture the flag account has not been claimed through
	// it. The "you won a slot" line shows for it
	SnCtfWinClaimStateUnclaimed = "unclaimed"
	// claimed: the account's claim paid it
	SnCtfWinClaimStateClaimed = "claimed"
	// the server could not say: show it neither as claimed nor as unclaimed
	SnCtfWinClaimStateUnknown = "unknown"
)

// SnCtfWin is one flag a client captured: a slot it won in a country and
// role. Amounts are rao.
type SnCtfWin struct {
	Epoch int64 `json:"epoch"`
	// SnCtfRole*; a role this sdk does not know is a slot of another kind
	Role string `json:"role"`
	// a lowercase ISO 3166-1 alpha-2 code
	Country     string `json:"country"`
	Rank        int64  `json:"rank"`
	SlotRao     int64  `json:"slot_rao"`
	PotShareRao int64  `json:"pot_share_rao"`
	// the slot and the pot share together
	AmountRao int64 `json:"amount_rao"`
	// the pot share is a pending award, paid when the carry returns
	Pending bool `json:"pending"`
	// "pending" or "finalized", as SnCtfClientEpoch.RootStatus
	RootStatus  string `json:"root_status"`
	ColdkeySs58 string `json:"coldkey_ss58"`
	// the client that won it: a network's answer spans its clients
	ClientId string `json:"client_id"`
	// SnCtfWinClaimState*
	ClaimState string `json:"claim_state"`
}

type SnCtfWinList struct {
	exportedList[*SnCtfWin]
}

func NewSnCtfWinList() *SnCtfWinList {
	return &SnCtfWinList{
		exportedList: *newExportedList[*SnCtfWin](),
	}
}

// SnCtfClientResult is a client's or a network's capture the flag status.
// With an Error it is unavailable: an app draws no line, never "no wins".
type SnCtfClientResult struct {
	// the client answered for; "" for a network's answer, whose wins span its
	// clients
	ClientId string `json:"client_id"`
	// when the capture the flag state was last changed, unix milliseconds
	AsOfMillis int64 `json:"as_of_millis"`
	// the client's recent epochs, newest first; empty before the first capture
	// the flag root, and for a network
	Epochs *SnCtfClientEpochList `json:"epochs"`
	// the flags captured: every win not claimed yet whatever its epoch, and
	// every win of the recent epochs; newest first (epoch, then providers
	// before extenders, then country, then rank). From a server before the
	// network answer, the wins of Epochs
	Wins *SnCtfWinList `json:"wins"`
	// the wins whose award has not been claimed yet, in Wins' order: the "you
	// won a slot" line shows for each. Never nil
	UnclaimedWins *SnCtfWinList `json:"unclaimed_wins"`
	// Wins' first, the newest win; nil without one
	LatestWin *SnCtfWin `json:"latest_win,omitempty"`
	Error     *SnError  `json:"error,omitempty"`
}

type SnCtfClientCallback connect.ApiCallback[*SnCtfClientResult]

// GET /sn/ctf/client. Amounts are {alpha, rao}; a field the role has no value
// for is null. client_id is null and epochs [] for a network; wins is absent
// from a server before the network answer.
type snCtfClientJson struct {
	AsOf     string                  `json:"as_of"`
	ClientId string                  `json:"client_id"`
	Epochs   []*snCtfClientEpochJson `json:"epochs"`
	Wins     []*snCtfWinJson         `json:"wins"`
}

// a CtfClientWin; claimed is true, false or null
type snCtfWinJson struct {
	Epoch       *int64           `json:"epoch"`
	Role        string           `json:"role"`
	Country     string           `json:"country"`
	Rank        *int64           `json:"rank"`
	ClientId    string           `json:"client_id"`
	Amount      *snCtfAmountJson `json:"amount"`
	Slot        *snCtfAmountJson `json:"slot"`
	PotShare    *snCtfAmountJson `json:"pot_share"`
	Pending     bool             `json:"pending"`
	RootStatus  string           `json:"root_status"`
	Coldkey     *string          `json:"coldkey"`
	ColdkeySs58 string           `json:"coldkey_ss58"`
	Claimed     json.RawMessage  `json:"claimed"`
}

type snCtfClientEpochJson struct {
	Epoch      *int64               `json:"epoch"`
	Drawn      bool                 `json:"drawn"`
	RootStatus string               `json:"root_status"`
	Providers  *snCtfClientRoleJson `json:"providers"`
	Extenders  *snCtfClientRoleJson `json:"extenders"`
}

type snCtfClientRoleJson struct {
	Candidate  bool    `json:"candidate"`
	Reason     *string `json:"reason"`
	Country    *string `json:"country"`
	UnitFamily *int64  `json:"unit_family"`
	Presence   *struct {
		Present  int64 `json:"present"`
		Required int64 `json:"required"`
		Samples  int64 `json:"samples"`
		Excused  int64 `json:"excused"`
	} `json:"presence"`
	UnitsInDraw *int64           `json:"units_in_draw"`
	Won         bool             `json:"won"`
	Rank        *int64           `json:"rank"`
	Slot        *snCtfAmountJson `json:"slot"`
	PotShare    *snCtfAmountJson `json:"pot_share"`
	Pending     bool             `json:"pending"`
	ColdkeySs58 *string          `json:"coldkey_ss58"`
}

// {"alpha": "0.048025660", "rao": 48025660}: the two always agree exactly.
type snCtfAmountJson struct {
	Alpha string `json:"alpha"`
	Rao   *int64 `json:"rao"`
}

// rao checks the amount's two forms against each other.
func (self *snCtfAmountJson) rao() (int64, error) {
	if self.Rao == nil || *self.Rao < 0 {
		return 0, errors.New("an amount without a rao value")
	}
	if want := fmt.Sprintf("%d.%09d", *self.Rao/SnRaoPerAlpha, *self.Rao%SnRaoPerAlpha); self.Alpha != want {
		return 0, fmt.Errorf("alpha %q is not rao %d", self.Alpha, *self.Rao)
	}
	return *self.Rao, nil
}

var snCtfCountryPattern = regexp.MustCompile(`^[a-z]{2}$`)

// role checks one role of one epoch.
func (self *snCtfClientRoleJson) role(role string) (*SnCtfClientRole, error) {
	out := &SnCtfClientRole{Role: role, Candidate: self.Candidate, Won: self.Won, Pending: self.Pending}
	if self.Reason != nil {
		out.Reason = *self.Reason
	}
	if self.Candidate == (out.Reason != "") {
		return nil, errors.New("a candidate with a reason, or a reason missing")
	}
	if self.Country != nil {
		if !snCtfCountryPattern.MatchString(*self.Country) {
			return nil, fmt.Errorf("country %q is not a lowercase ISO 3166-1 code", *self.Country)
		}
		out.Country = *self.Country
	}
	if self.UnitFamily != nil {
		if *self.UnitFamily != 4 && *self.UnitFamily != 6 {
			return nil, fmt.Errorf("unit family %d", *self.UnitFamily)
		}
		out.UnitFamily = *self.UnitFamily
	}
	if presence := self.Presence; presence != nil {
		if presence.Present < 0 || presence.Required < 0 || presence.Samples < 0 || presence.Excused < 0 {
			return nil, errors.New("a negative presence count")
		}
		out.PresencePresent, out.PresenceRequired = presence.Present, presence.Required
		out.PresenceSamples, out.PresenceExcused = presence.Samples, presence.Excused
	}
	if self.UnitsInDraw != nil {
		if *self.UnitsInDraw < 1 {
			return nil, fmt.Errorf("%d units in the draw", *self.UnitsInDraw)
		}
		out.UnitsInDraw = *self.UnitsInDraw
	}
	var err error
	if self.Slot != nil {
		if out.SlotRao, err = self.Slot.rao(); err != nil {
			return nil, fmt.Errorf("slot: %w", err)
		}
	}
	if self.PotShare != nil {
		if out.PotShareRao, err = self.PotShare.rao(); err != nil {
			return nil, fmt.Errorf("pot share: %w", err)
		}
	}
	if self.ColdkeySs58 != nil {
		out.ColdkeySs58 = strings.TrimSpace(*self.ColdkeySs58)
	}
	if !self.Won {
		return out, nil
	}
	// a win names its rank, slot, country and coldkey
	switch {
	case !self.Candidate:
		return nil, errors.New("a win that was not a candidate")
	case self.Rank == nil || *self.Rank < 1 || 5 < *self.Rank:
		return nil, errors.New("a win without a rank of 1 to 5")
	case self.Slot == nil || out.Country == "":
		return nil, errors.New("a win without its slot or country")
	case !ValidateSs58(out.ColdkeySs58):
		return nil, fmt.Errorf("a win paid to %q, not a coldkey", out.ColdkeySs58)
	}
	out.Rank = *self.Rank
	return out, nil
}

// win checks one CtfClientWin. answerClientId is the answer's client, "" for
// a network's answer.
func (self *snCtfWinJson) win(answerClientId string) (*SnCtfWin, error) {
	out := &SnCtfWin{Role: self.Role, Country: self.Country, Pending: self.Pending, RootStatus: self.RootStatus, ColdkeySs58: strings.TrimSpace(self.ColdkeySs58)}
	switch {
	case self.Epoch == nil || *self.Epoch < 0:
		return nil, errors.New("a win without its epoch")
	case !snCtfRolePattern.MatchString(self.Role):
		return nil, fmt.Errorf("role %q", self.Role)
	case !snCtfCountryPattern.MatchString(self.Country):
		return nil, fmt.Errorf("country %q is not a lowercase ISO 3166-1 code", self.Country)
	case self.Rank == nil || *self.Rank < 1 || 5 < *self.Rank:
		return nil, errors.New("a win without a rank of 1 to 5")
	case self.RootStatus != "pending" && self.RootStatus != "finalized":
		return nil, fmt.Errorf("root status %q", self.RootStatus)
	case self.Amount == nil || self.Slot == nil || self.PotShare == nil:
		return nil, errors.New("a win without its amount, slot or pot share")
	case !ValidateSs58(out.ColdkeySs58):
		return nil, fmt.Errorf("a win paid to %q, not a coldkey", out.ColdkeySs58)
	}
	out.Epoch, out.Rank = *self.Epoch, *self.Rank
	clientId, err := connect.ParseId(strings.TrimSpace(self.ClientId))
	if err != nil {
		return nil, fmt.Errorf("client %q: %w", self.ClientId, err)
	}
	out.ClientId = clientId.String()
	if answerClientId != "" {
		// a client's answer holds only its own wins
		if answerId, err := connect.ParseId(answerClientId); err != nil || answerId != clientId {
			return nil, fmt.Errorf("a win of client %s in the answer for %s", out.ClientId, answerClientId)
		}
	}
	if out.SlotRao, err = self.Slot.rao(); err != nil {
		return nil, fmt.Errorf("slot: %w", err)
	}
	if out.PotShareRao, err = self.PotShare.rao(); err != nil {
		return nil, fmt.Errorf("pot share: %w", err)
	}
	if out.AmountRao, err = self.Amount.rao(); err != nil {
		return nil, fmt.Errorf("amount: %w", err)
	}
	if out.AmountRao != out.SlotRao+out.PotShareRao {
		return nil, fmt.Errorf("an amount of %d rao that is not its slot and pot share", out.AmountRao)
	}
	if self.Coldkey != nil {
		coldkey, err := snColdkeyBytes(out.ColdkeySs58)
		named, hexErr := evm.ParseHash32(strings.TrimSpace(*self.Coldkey))
		if err != nil || hexErr != nil || coldkey != named {
			return nil, fmt.Errorf("coldkey %q is not %s", *self.Coldkey, out.ColdkeySs58)
		}
	}
	switch claimed := strings.TrimSpace(string(self.Claimed)); claimed {
	case "true":
		out.ClaimState = SnCtfWinClaimStateClaimed
	case "false":
		out.ClaimState = SnCtfWinClaimStateUnclaimed
	case "", "null":
		out.ClaimState = SnCtfWinClaimStateUnknown
	default:
		return nil, fmt.Errorf("claimed %s", claimed)
	}
	return out, nil
}

// a role: provider or extender, or one a later server adds
var snCtfRolePattern = regexp.MustCompile(`^[a-z][a-z_]*$`)

// snCtfRoleOrder puts providers before extenders, and any other role after.
func snCtfRoleOrder(role string) int {
	switch role {
	case SnCtfRoleProvider:
		return 0
	case SnCtfRoleExtender:
		return 1
	default:
		return 2
	}
}

// snCtfWinsNewestFirst orders wins as the route does: epoch descending, then
// providers before extenders, then country, then rank.
func snCtfWinsNewestFirst(wins []*SnCtfWin) {
	slices.SortStableFunc(wins, func(a *SnCtfWin, b *SnCtfWin) int {
		return cmp.Or(
			cmp.Compare(b.Epoch, a.Epoch),
			cmp.Compare(snCtfRoleOrder(a.Role), snCtfRoleOrder(b.Role)),
			strings.Compare(a.Country, b.Country),
			cmp.Compare(a.Rank, b.Rank),
		)
	})
}

// snParseCtfClient checks a GET /sn/ctf/client answer whole: any part that
// does not parse refuses it, so no line is drawn from it.
func snParseCtfClient(raw []byte) (*SnCtfClientResult, error) {
	var answer snCtfClientJson
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	asOf, err := time.Parse(time.RFC3339, answer.AsOf)
	if err != nil {
		return nil, fmt.Errorf("as_of: %w", err)
	}
	if answer.Epochs == nil {
		return nil, errors.New("the answer has no epochs list")
	}
	result := snCtfNoWins()
	result.ClientId = strings.TrimSpace(answer.ClientId)
	result.AsOfMillis = asOf.UnixMilli()
	var epochs []*SnCtfClientEpoch
	for i, epochJson := range answer.Epochs {
		if epochJson == nil || epochJson.Epoch == nil || epochJson.Providers == nil || epochJson.Extenders == nil {
			return nil, fmt.Errorf("epoch %d: an epoch without its number or roles", i)
		}
		if epochJson.RootStatus != "pending" && epochJson.RootStatus != "finalized" {
			return nil, fmt.Errorf("epoch %d: root status %q", i, epochJson.RootStatus)
		}
		epoch := &SnCtfClientEpoch{Epoch: *epochJson.Epoch, Drawn: epochJson.Drawn, RootStatus: epochJson.RootStatus}
		if epoch.Provider, err = epochJson.Providers.role(SnCtfRoleProvider); err != nil {
			return nil, fmt.Errorf("epoch %d providers: %w", epoch.Epoch, err)
		}
		if epoch.Extender, err = epochJson.Extenders.role(SnCtfRoleExtender); err != nil {
			return nil, fmt.Errorf("epoch %d extenders: %w", epoch.Epoch, err)
		}
		if !epoch.Drawn && (epoch.Provider.Won || epoch.Extender.Won) {
			return nil, fmt.Errorf("epoch %d: a win without a draw", epoch.Epoch)
		}
		epochs = append(epochs, epoch)
	}
	// newest first, whatever the order of the answer
	slices.SortStableFunc(epochs, func(a *SnCtfClientEpoch, b *SnCtfClientEpoch) int {
		return cmp.Compare(b.Epoch, a.Epoch)
	})
	var wins []*SnCtfWin
	for i, epoch := range epochs {
		if 0 < i && epochs[i-1].Epoch == epoch.Epoch {
			return nil, fmt.Errorf("epoch %d is answered twice", epoch.Epoch)
		}
		result.Epochs.Add(epoch)
		if answer.Wins != nil {
			continue
		}
		// a server before the network answer lists no wins: they are the
		// epochs'. A win whose root is pending is not claimable yet, so it is
		// unclaimed; whether one in a finalized root is claimed is unknown
		for _, role := range []*SnCtfClientRole{epoch.Provider, epoch.Extender} {
			if !role.Won {
				continue
			}
			claimState := SnCtfWinClaimStateUnknown
			if epoch.RootStatus == "pending" {
				claimState = SnCtfWinClaimStateUnclaimed
			}
			wins = append(wins, &SnCtfWin{
				Epoch:       epoch.Epoch,
				Role:        role.Role,
				Country:     role.Country,
				Rank:        role.Rank,
				SlotRao:     role.SlotRao,
				PotShareRao: role.PotShareRao,
				AmountRao:   role.SlotRao + role.PotShareRao,
				Pending:     role.Pending,
				RootStatus:  epoch.RootStatus,
				ColdkeySs58: role.ColdkeySs58,
				ClientId:    result.ClientId,
				ClaimState:  claimState,
			})
		}
	}
	type winKey struct {
		epoch    int64
		role     string
		clientId string
	}
	seen := map[winKey]bool{}
	for i, winJson := range answer.Wins {
		if winJson == nil {
			return nil, fmt.Errorf("win %d: null", i)
		}
		win, err := winJson.win(result.ClientId)
		if err != nil {
			return nil, fmt.Errorf("win %d: %w", i, err)
		}
		// a client wins one slot of a role in an epoch at most
		key := winKey{win.Epoch, win.Role, win.ClientId}
		if seen[key] {
			return nil, fmt.Errorf("win %d: client %s won epoch %d's %s draw twice", i, win.ClientId, win.Epoch, win.Role)
		}
		seen[key] = true
		wins = append(wins, win)
	}
	snCtfWinsNewestFirst(wins)
	for _, win := range wins {
		result.Wins.Add(win)
		if win.ClaimState == SnCtfWinClaimStateUnclaimed {
			result.UnclaimedWins.Add(win)
		}
	}
	if 0 < result.Wins.Len() {
		result.LatestWin = result.Wins.Get(0)
	}
	return result, nil
}

// snCtfNoWins is an answer without a win.
func snCtfNoWins() *SnCtfClientResult {
	return &SnCtfClientResult{
		Epochs:        NewSnCtfClientEpochList(),
		Wins:          NewSnCtfWinList(),
		UnclaimedWins: NewSnCtfWinList(),
	}
}

// snCtfClientResultOf is the result of a read of GET /sn/ctf/client: raw, or
// readErr when it could not be read.
func snCtfClientResultOf(raw []byte, readErr error) *SnCtfClientResult {
	unavailable := func(message string) *SnCtfClientResult {
		result := snCtfNoWins()
		result.Error = newSnError(SnErrorCodeCtfUnavailable, message)
		return result
	}
	if readErr != nil {
		return unavailable(readErr.Error())
	}
	result, err := snParseCtfClient(raw)
	if err != nil {
		return unavailable("refused the capture the flag answer: " + err.Error())
	}
	return result
}

// SnCtfClientOf reads a GET /sn/ctf/client answer that a page fetched with its
// own client or network token. Pure.
func SnCtfClientOf(answerJson string) *SnCtfClientResult {
	return snCtfClientResultOf([]byte(answerJson), nil)
}

// SnCtfClientOfResponse reads a page's own GET /sn/ctf/client with the HTTP
// status it was answered: 200 reads the body as SnCtfClientOf; 401 is no wins,
// as a server before the network answer refuses a network token, and never a
// reason to sign out; any other status, a 304 included, is unavailable (a page
// that revalidates reads its kept body with SnCtfClientOf). Pure.
func SnCtfClientOfResponse(statusCode int, answerJson string) *SnCtfClientResult {
	switch statusCode {
	case http.StatusOK:
		return SnCtfClientOf(answerJson)
	case http.StatusUnauthorized:
		return snCtfNoWins()
	default:
		return snCtfClientResultOf(nil, fmt.Errorf("the capture the flag route answered %d", statusCode))
	}
}

// snCtfClientCredentials are the credentials the device asks with, in turn:
// its network credential, which the route answers with the wins of all of the
// network's clients, as ur.io reads them, then its client token, which a
// server before the network answer takes, for that client's.
func snCtfClientCredentials(api *Api) []string {
	var credentials []string
	if networkByJwt := api.networkCredential(); networkByJwt != "" {
		credentials = append(credentials, networkByJwt)
	}
	if byJwt := api.GetByJwt(); credentialNamesClient(byJwt) {
		credentials = append(credentials, byJwt)
	}
	return credentials
}

// snCtfClient reads the device's capture the flag wins. Its requests go over
// the api's transport alone, never the seam that takes a 401 as the sent
// credential's rejection, so no answer of the route signs anyone out. A 401 to
// the network credential, a server before the network answer, asks again
// with the client token, and without one reads as no wins.
func snCtfClient(ctx context.Context, api *Api) *SnCtfClientResult {
	credentials := snCtfClientCredentials(api)
	if len(credentials) == 0 {
		return snCtfClientResultOf(nil, errors.New("the session has no credential"))
	}
	get := api.transportHttpGetRaw()
	ctx = api.clientInfoContext(ctx)
	requestUrl := fmt.Sprintf("%s/sn/ctf/client", api.apiUrl)
	for _, byJwt := range credentials {
		raw, err := get(ctx, requestUrl, byJwt)
		if err == nil {
			return snCtfClientResultOf(raw, nil)
		}
		if !ConfirmedClientRefreshRejection(err) || !isNetworkCredential(byJwt) {
			return snCtfClientResultOf(nil, err)
		}
	}
	return snCtfNoWins()
}

// SnCtfClient reads the device's capture the flag wins: its network's when it
// holds the network credential, else its client's.
func (self *snDevice) SnCtfClient(callback SnCtfClientCallback) {
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(self.ctx, snCtfClientTimeout)
		defer cancel()
		result := snCtfClient(ctx, self.api())
		if callback != nil {
			callback.Result(result, nil)
		}
	})
}
