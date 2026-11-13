package sdk

// Network payout consent (CONSENT.md): the network owner signs once with the
// coldkey, and the consent covers every provider client of the network. An app
// requests the challenge, has the wallet sign its exact message
// (BittensorWalletSession.SetNetworkConsentChallenge), submits the signature
// and reads the status. The server keeps the consent chain; these calls choose
// the range and check every answer before an app shows or signs it.
//
// The errors an app shows its own words for are in the SDK's "code: message"
// form: SnErrorCodeNetworkOwnerOnly and SnErrorCodeChallengeExpired. A
// signature from another account is the submission result's
// SnErrorCodeSignatureMismatch.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/urnetwork/connect/v2026"
)

// The epochs a network consent spans after its first: through_epoch is
// from_epoch + SnNetworkConsentEpochSpan, the protocol's maximum, meaning
// "until changed".
const SnNetworkConsentEpochSpan = 65535

const (
	// the first line of every network consent message (sn protocol
	// NetworkWalletMappingConsentPrefix)
	snNetworkConsentMessagePrefix = "Approve URnetwork network wallet mapping\n"
	snNetworkConsentSchema        = "urnetwork-network-wallet-mapping-consent-v1"
	snNetworkConsentScope         = "network"
	// sn protocol MaxWalletMappingMessageBytes
	snNetworkConsentMaxMessageBytes = 8 * 1024
	// the server accepts a signed message for 300 s after issuing it
	snNetworkConsentChallengeSeconds = 300
	// sn protocol MaxWalletMappingHistory
	snNetworkConsentMaxGeneration = 4096
	// the server's refusal of a message submitted after its window
	snNetworkConsentExpiredRefusal = "network wallet mapping original challenge is expired or not yet effective"
)

// A network consent challenge: the server's exact message and the range it
// approves.
type SnNetworkConsentChallengeResult struct {
	// the exact bytes to sign
	Message      string `json:"message"`
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    int64  `json:"from_epoch"`
	ThroughEpoch int64  `json:"through_epoch"`
	// unix seconds; the server refuses the signed message after it
	ExpiresAt int64 `json:"expires_at"`
}

type SnNetworkConsentChallengeCallback connect.ApiCallback[*SnNetworkConsentChallengeResult]

// The network's newest accepted network consent.
type SnNetworkConsentStatus struct {
	// a consent exists and its through epoch is at or after the current epoch:
	// it is in force, or it starts next epoch when FromEpoch is after the
	// current epoch
	Active bool `json:"active"`
	// "" with zero epochs when the network has no consent
	ColdkeySs58  string `json:"coldkey_ss58"`
	FromEpoch    int64  `json:"from_epoch"`
	ThroughEpoch int64  `json:"through_epoch"`
	// the consent's chain generation; 0 while the server does not report it
	Generation int64 `json:"generation"`
}

type SnNetworkConsentStatusCallback connect.ApiCallback[*SnNetworkConsentStatus]

// SnNetworkConsentChallenge reads the epoch clock and the newest consent,
// chooses the range and requests the challenge for the coldkey.
func (self *Api) SnNetworkConsentChallenge(coldkeySs58 string, callback SnNetworkConsentChallengeCallback) {
	go connect.HandleError(func() {
		result, err := self.SnNetworkConsentChallengeSync(coldkeySs58)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

//gomobile:noexport
func (self *Api) SnNetworkConsentChallengeSync(coldkeySs58 string) (*SnNetworkConsentChallengeResult, error) {
	return self.SnNetworkConsentChallengeSyncWithContext(self.ctx, coldkeySs58)
}

// The range starts after both the current epoch and the newest consent's first
// epoch: the operator accepts only a prospective consent, and each generation
// starts later than the one before (CONSENT.md §8). The challenge is requested
// with the network credential; the reads use the api's current credential.
//
//gomobile:noexport
func (self *Api) SnNetworkConsentChallengeSyncWithContext(ctx context.Context, coldkeySs58 string) (*SnNetworkConsentChallengeResult, error) {
	result, err := self.snNetworkConsentChallenge(ctx, strings.TrimSpace(coldkeySs58))
	return result, snNetworkConsentError(err)
}

func (self *Api) snNetworkConsentChallenge(ctx context.Context, coldkeySs58 string) (*SnNetworkConsentChallengeResult, error) {
	networkByJwt := self.networkCredential()
	if networkByJwt == "" {
		return nil, ErrNetworkCredentialRequired
	}
	coldkey, err := snColdkeyBytes(coldkeySs58)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", SnErrorCodeInvalidAddress, err)
	}
	currentEpoch, latest, err := self.snNetworkConsentState(ctx)
	if err != nil {
		return nil, err
	}
	from := currentEpoch
	if latest != nil && from < latest.FromEpoch {
		from = latest.FromEpoch
	}
	if math.MaxInt64-SnNetworkConsentEpochSpan <= from {
		return nil, fmt.Errorf("network consent epoch %d is out of range", from)
	}
	from += 1
	through := from + SnNetworkConsentEpochSpan
	challenge, err := self.SnNetworkWalletMappingChallengeSyncWithContext(ctx, &SnNetworkWalletMappingChallengeArgs{
		ColdkeySs58:  coldkeySs58,
		FromEpoch:    from,
		ThroughEpoch: through,
	})
	if err != nil {
		return nil, err
	}
	if challenge == nil {
		return nil, errors.New("POST /sn/wallet/network-consent returned no challenge")
	}
	statement, err := decodeSnNetworkConsentStatement(challenge.Message)
	if err != nil {
		return nil, err
	}
	networkId, namesNetwork := credentialNetworkId(networkByJwt)
	if statement.Coldkey != coldkey || statement.FromEpoch != uint64(from) || statement.ThroughEpoch != uint64(through) ||
		namesNetwork && statement.NetworkId != [16]byte(networkId) {
		return nil, errors.New("the network consent challenge is for another coldkey, range or network than requested")
	}
	return &SnNetworkConsentChallengeResult{
		Message:      challenge.Message,
		ColdkeySs58:  coldkeySs58,
		FromEpoch:    from,
		ThroughEpoch: through,
		ExpiresAt:    statement.ExpiresAt,
	}, nil
}

// SnNetworkConsentSubmit submits the signed consent (POST /sn/wallet) with the
// network credential and without a client id.
func (self *Api) SnNetworkConsentSubmit(coldkeySs58 string, message string, signatureHex string, callback SnSetWalletCallback) {
	go connect.HandleError(func() {
		result, err := self.SnNetworkConsentSubmitSync(coldkeySs58, message, signatureHex)
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

//gomobile:noexport
func (self *Api) SnNetworkConsentSubmitSync(coldkeySs58 string, message string, signatureHex string) (*SnSetWalletResult, error) {
	return self.SnNetworkConsentSubmitSyncWithContext(self.ctx, coldkeySs58, message, signatureHex)
}

// A message without the network consent prefix is refused before it is sent:
// the server would take it for another kind of wallet set. A signature from
// another account is the result's SnErrorCodeSignatureMismatch, and an
// acceptance names the consent's place in the chain.
//
//gomobile:noexport
func (self *Api) SnNetworkConsentSubmitSyncWithContext(ctx context.Context, coldkeySs58 string, message string, signatureHex string) (*SnSetWalletResult, error) {
	result, err := self.snNetworkConsentSubmit(ctx, strings.TrimSpace(coldkeySs58), message, signatureHex)
	return result, snNetworkConsentError(err)
}

func (self *Api) snNetworkConsentSubmit(ctx context.Context, coldkeySs58 string, message string, signatureHex string) (*SnSetWalletResult, error) {
	networkByJwt := self.networkCredential()
	if networkByJwt == "" {
		return nil, ErrNetworkCredentialRequired
	}
	if !strings.HasPrefix(message, snNetworkConsentMessagePrefix) {
		return nil, errors.New("the message is not a network consent")
	}
	result, err := connect.HttpPostWithRawFunction(
		ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/sn/wallet", self.apiUrl),
		&SnSetWalletArgs{ColdkeySs58: coldkeySs58, Message: message, Signature: signatureHex},
		networkByJwt,
		&SnSetWalletResult{},
		connect.NewNoopApiCallback[*SnSetWalletResult](),
	)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("POST /sn/wallet returned no result")
	}
	if result.Error != nil {
		return result, nil
	}
	if hash, err := hex.DecodeString(result.MappingHash); err != nil || len(hash) != 32 ||
		result.MappingGeneration < 1 || snNetworkConsentMaxGeneration < result.MappingGeneration {
		return nil, errors.New("POST /sn/wallet accepted the network consent without its place in the chain")
	}
	return result, nil
}

// SnNetworkConsentStatus reads the epoch clock and the network's newest
// consent.
func (self *Api) SnNetworkConsentStatus(callback SnNetworkConsentStatusCallback) {
	go connect.HandleError(func() {
		result, err := self.SnNetworkConsentStatusSync()
		if callback != nil {
			callback.Result(result, err)
		}
	})
}

//gomobile:noexport
func (self *Api) SnNetworkConsentStatusSync() (*SnNetworkConsentStatus, error) {
	return self.SnNetworkConsentStatusSyncWithContext(self.ctx)
}

// Reads with the api's current credential, so any session of the network sees
// the status.
//
//gomobile:noexport
func (self *Api) SnNetworkConsentStatusSyncWithContext(ctx context.Context) (*SnNetworkConsentStatus, error) {
	currentEpoch, latest, err := self.snNetworkConsentState(ctx)
	if err != nil {
		return nil, err
	}
	status := &SnNetworkConsentStatus{}
	if latest != nil {
		status.Active = currentEpoch <= latest.ThroughEpoch
		status.ColdkeySs58 = latest.ColdkeySs58
		status.FromEpoch = latest.FromEpoch
		status.ThroughEpoch = latest.ThroughEpoch
		status.Generation = latest.MappingGeneration
	}
	return status, nil
}

// The epoch of a GET /sn/epoch answer. An answer without it is not epoch 0.
type snNetworkConsentEpoch struct {
	Epoch *int64 `json:"epoch"`
}

// The current epoch and the network's newest accepted network consent (nil
// when it has none), read with the api's current credential. The history
// route needs an independently pinned head, which an app does not have, so the
// consent is GET /sn/wallet's network entry (CONSENT.md §8).
func (self *Api) snNetworkConsentState(ctx context.Context) (int64, *SnWallet, error) {
	epoch, err := connect.HttpGetWithRawFunction(
		ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/sn/epoch", self.apiUrl),
		self.GetByJwt(),
		&snNetworkConsentEpoch{},
		connect.NewNoopApiCallback[*snNetworkConsentEpoch](),
	)
	if err != nil {
		return 0, nil, err
	}
	if epoch == nil || epoch.Epoch == nil || *epoch.Epoch < 0 {
		return 0, nil, errors.New("GET /sn/epoch returned no current epoch")
	}
	wallets, err := self.SnGetWalletSyncWithContext(ctx)
	if err != nil {
		return 0, nil, err
	}
	latest, err := snNetworkConsentLatest(wallets)
	if err != nil {
		return 0, nil, err
	}
	return *epoch.Epoch, latest, nil
}

// The one network entry of a GET /sn/wallet answer, within the protocol's
// bounds, or nil when there is none.
func snNetworkConsentLatest(result *SnGetWalletResult) (*SnWallet, error) {
	if result != nil && result.Error != nil {
		return nil, fmt.Errorf("GET /sn/wallet: %s", result.Error.Message)
	}
	if result == nil || result.Wallets == nil {
		return nil, errors.New("GET /sn/wallet returned no wallets")
	}
	var latest *SnWallet
	for _, wallet := range result.Wallets.values {
		if wallet == nil {
			return nil, errors.New("GET /sn/wallet listed an empty wallet")
		}
		if wallet.ConsentScope != SnWalletConsentScopeNetwork {
			continue
		}
		if latest != nil || wallet.ClientId != "" || !ValidateSs58(wallet.ColdkeySs58) ||
			wallet.FromEpoch < 0 || wallet.ThroughEpoch < wallet.FromEpoch || SnNetworkConsentEpochSpan < wallet.ThroughEpoch-wallet.FromEpoch ||
			wallet.MappingGeneration < 0 || snNetworkConsentMaxGeneration < wallet.MappingGeneration {
			return nil, errors.New("GET /sn/wallet listed a malformed network consent")
		}
		latest = wallet
	}
	return latest, nil
}

// The displayed network statement as the server encodes it (sn protocol
// NetworkWalletMappingStatement), field for field and in order, so a message
// decodes and encodes back to exactly its own bytes. The domain and the
// operator's approval stay raw: the server verifies them.
type snNetworkConsentStatement struct {
	Schema       string          `json:"schema"`
	Scope        string          `json:"scope"`
	Domain       json.RawMessage `json:"domain"`
	UserId       [16]byte        `json:"user_id"`
	NetworkId    [16]byte        `json:"network_id"`
	Coldkey      [32]byte        `json:"coldkey"`
	Generation   uint64          `json:"generation"`
	PreviousHash [32]byte        `json:"previous_hash"`
	Nonce        [32]byte        `json:"nonce"`
	IssuedAt     int64           `json:"issued_at"`
	ExpiresAt    int64           `json:"expires_at"`
	FromEpoch    uint64          `json:"from_epoch"`
	ThroughEpoch uint64          `json:"through_epoch"`
	Prospective  json.RawMessage `json:"prospective"`
}

// A network consent message: the prefix, then the canonical statement with
// the network schema and scope and the server's acceptance window.
func decodeSnNetworkConsentStatement(message string) (*snNetworkConsentStatement, error) {
	raw, prefixed := strings.CutPrefix(message, snNetworkConsentMessagePrefix)
	if snNetworkConsentMaxMessageBytes < len(message) || !prefixed {
		return nil, errors.New("the network consent challenge is not a network consent message")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	statement := &snNetworkConsentStatement{}
	if err := decoder.Decode(statement); err != nil {
		return nil, fmt.Errorf("the network consent challenge does not decode: %w", err)
	}
	// encoding a decoded statement cannot fail, and no bytes never match
	canonical, _ := json.Marshal(statement)
	if string(canonical) != raw || statement.Schema != snNetworkConsentSchema || statement.Scope != snNetworkConsentScope ||
		statement.IssuedAt <= 0 || statement.ExpiresAt <= statement.IssuedAt || snNetworkConsentChallengeSeconds < statement.ExpiresAt-statement.IssuedAt {
		return nil, errors.New("the network consent challenge is not a canonical network statement")
	}
	return statement, nil
}

// The consent calls' errors in the SDK's "code: message" form where an app
// shows its own words: the session is not the network owner's (the api holds
// no network credential, or the server answered 403), or the server refused
// an expired message.
func snNetworkConsentError(err error) error {
	var statusErr *connect.HttpStatusError
	switch {
	case errors.Is(err, ErrNetworkCredentialRequired) || errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s: %w", SnErrorCodeNetworkOwnerOnly, err)
	case errors.As(err, &statusErr) && strings.TrimSpace(string(statusErr.Body)) == snNetworkConsentExpiredRefusal:
		return fmt.Errorf("%s: %w", SnErrorCodeChallengeExpired, err)
	default:
		return err
	}
}
