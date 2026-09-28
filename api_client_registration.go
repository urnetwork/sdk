package sdk

// Durable headless registration negotiates a dedicated server contract. The
// caller owns the immutable request before any transport may replay its bytes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urnetwork/connect"
)

const NetworkClientRegistrationSchema = "urnetwork-client-registration-v1"

// The dedicated server route admits this many encoded bytes, including JSON
// escaping and ownership fields, independently of the decoded field limits.
const networkClientRegistrationMaxRequestBytes = 16 * 1024

// The opaque request identifier is not a client identity or an authorization.
// Field order is the canonical request-hash format shared with the server.
type RegisterNetworkClientArgs struct {
	Schema            string `json:"schema"`
	RegistrationId    string `json:"registration_id"`
	ScopeSha256       string `json:"scope_sha256"`
	DeviceDescription string `json:"description"`
	DeviceSpec        string `json:"device_spec"`
}

// Only these server-issued identities may be installed by the credential owner.
type RegisterNetworkClientResult struct {
	Schema         string                      `json:"schema"`
	RegistrationId string                      `json:"registration_id"`
	RequestSha256  string                      `json:"request_sha256"`
	ClientId       *Id                         `json:"client_id,omitempty"`
	DeviceId       *Id                         `json:"device_id,omitempty"`
	ByClientJwt    string                      `json:"by_client_jwt,omitempty"`
	Error          *RegisterNetworkClientError `json:"error,omitempty"`
}

// Application rejection is a completed response, distinct from an unknown
// request outcome. Callers must not replace the opaque operation after either.
type RegisterNetworkClientError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// A failed physical request may already have committed. Its only safe retry is
// the same persisted operation at this same dedicated versioned endpoint.
type NetworkClientRegistrationUnavailableError struct{ cause error }

func (self *NetworkClientRegistrationUnavailableError) Error() string {
	return "versioned client registration reply is unavailable; original operation remains unresolved"
}

func (self *NetworkClientRegistrationUnavailableError) Unwrap() error { return self.cause }

// Unsupported servers cannot fall back to legacy client creation.
type NetworkClientRegistrationUnsupportedError struct{ Status int }

func (self *NetworkClientRegistrationUnsupportedError) Error() string {
	return fmt.Sprintf("server does not support versioned client registration (HTTP %d)", self.Status)
}

// Validate and encode before the physical request boundary. A retained owner
// can compare these bytes with its durable request without exposing its token.
//
//gomobile:noexport
func EncodeNetworkClientRegistration(args *RegisterNetworkClientArgs) ([]byte, error) {
	validHash := func(value string) bool {
		raw, err := hex.DecodeString(value)
		return err == nil && len(raw) == sha256.Size && value == strings.ToLower(value) && value != strings.Repeat("0", 64)
	}
	if args == nil || args.Schema != NetworkClientRegistrationSchema || !validHash(args.RegistrationId) || !validHash(args.ScopeSha256) || len(args.DeviceDescription) > 1024 || len(args.DeviceSpec) > 4096 {
		return nil, errors.New("versioned registration request is incomplete or unsupported")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if len(raw) > networkClientRegistrationMaxRequestBytes {
		return nil, errors.New("versioned registration encoded request exceeds the server route bound")
	}
	return raw, nil
}

// The durable scope pins the exact endpoint that will consume its request.
// No fallback or discovery can redirect creation to another API authority.
//
//gomobile:noexport
func (self *Api) NetworkClientRegistrationEndpoint() (string, error) {
	base, err := url.Parse(self.apiUrl)
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Scheme != "http" && base.Scheme != "https" {
		return "", errors.New("versioned registration API route is invalid")
	}
	return strings.TrimRight(self.apiUrl, "/") + "/network/register-client-v1", nil
}

// Transport failover may repeat only these exact bytes. Complete malformed,
// null, conflicting or mixed success/error responses stay integrity errors.
// This method never falls back to the non-idempotent legacy creation route.
//
//gomobile:noexport
func (self *Api) RegisterNetworkClientSyncWithContext(ctx context.Context, args *RegisterNetworkClientArgs) (*RegisterNetworkClientResult, error) {
	if ctx == nil {
		return nil, errors.New("versioned registration context is absent")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := EncodeNetworkClientRegistration(args)
	if err != nil {
		return nil, err
	}
	endpoint, err := self.NetworkClientRegistrationEndpoint()
	if err != nil {
		return nil, err
	}
	raw, err := self.getHttpPostRaw()(connect.WithHttpRedirectsDisabled(ctx), endpoint, bytes.Clone(body), self.GetByJwt())
	if err != nil {
		var invalid *ClientControlResponseError
		if errors.As(err, &invalid) {
			return nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, errors.Join(err, ctx.Err())
		}
		// A direct completed redirect is an endpoint contradiction. Do not
		// select one status from a joined cause tree or relabel it unavailable.
		if status, ok := err.(*connect.HttpStatusError); ok && http.StatusMultipleChoices <= status.StatusCode && status.StatusCode < http.StatusBadRequest {
			return nil, &ClientControlResponseError{detail: fmt.Sprintf("versioned registration refused HTTP %d redirect from its pinned endpoint", status.StatusCode)}
		}
		if status, complete := clientControlOnlyUnsupportedStatus(err); complete {
			return nil, &NetworkClientRegistrationUnsupportedError{Status: status}
		}
		if !transientClientControlRequestError(err) {
			return nil, err
		}
		// The complete physical cause tree is transient. No hard cause is
		// hidden by selecting one HTTP leaf from a joined error.
		return nil, &NetworkClientRegistrationUnavailableError{cause: err}
	}
	var result *RegisterNetworkClientResult
	if err := decodeClientControlJson(raw, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, &ClientControlResponseError{detail: "versioned registration returned a null response"}
	}
	digest := sha256.Sum256(body)
	if result.Schema != NetworkClientRegistrationSchema || result.RegistrationId != args.RegistrationId || result.RequestSha256 != hex.EncodeToString(digest[:]) {
		return nil, &ClientControlResponseError{detail: "versioned registration response differs from the original request"}
	}
	if result.Error != nil {
		if result.Error.Code == "" || result.ClientId != nil || result.DeviceId != nil || result.ByClientJwt != "" {
			return nil, &ClientControlResponseError{detail: "versioned registration mixed a refusal with client identity"}
		}
		return result, nil
	}
	if result.ClientId == nil || result.DeviceId == nil || result.ClientId.id == ([16]byte{}) || result.DeviceId.id == ([16]byte{}) || result.ByClientJwt == "" {
		return nil, &ClientControlResponseError{detail: "versioned registration omitted its server-issued identity"}
	}
	return result, nil
}
