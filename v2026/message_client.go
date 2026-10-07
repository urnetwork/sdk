//go:build !sdk_mobile_bind

package sdk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// THE OTHER HALF OF S2-7, AND IT IS THE HALF THAT WAS MISSING.
//
// message_transport_door.go injects the connect client and says, correctly, that nothing in that
// file dials, authenticates or closes one. The consequence, written down at
// cgo/exports_message.go's urnet_message_transport_new and in urmessage's package document, was
// that NO EXPORT ANYWHERE PRODUCED THE CLIENT: a C caller could reach an in-process loopback
// server and nothing else, because the only other way a connect.Client receives a frame is a
// connect.PlatformTransport dialling wss://connect.<host> with an operator-minted ByJwt, and no
// code in this workspace stood one up outside sdk/liveprobe's own main.go.
//
// THIS FILE IS THAT CONSTRUCTION, MOVED OUT OF A PROBE'S main AND INTO THE MODULE, so that the C
// abi, the live probe and anything else reach it through one declaration rather than three
// copies. It does NOT resolve S2-7's other half: the ByJwt is still an operator-admin action
// nothing here can perform, and this function does not mint, fetch or validate one. It takes the
// credential a caller already has and turns it into a client the transport can speak over.
//
// ── WHAT IT IS UNEXERCISED BY, STATED FIRST RATHER THAN IN A FOOTNOTE ────────────────────────
//
// NOTHING IN THIS MODULE HAS EVER RUN THIS AGAINST A REAL OPERATOR, and no test in it can: the
// credential is minted by an admin of a running URnetwork operator (spec B sections 9.1 and 9.2),
// there is no live deployment access here, and a ByJwt cannot be forged because the platform
// verifies it. So what the tests beside this file hold is the SHAPE -- which fields are refused,
// what the platform url derives to, that the client carries the client_id the credential names,
// and that the provide modes are set -- and NOT that a frame ever crossed. The first time this
// path carries a message will be the first time somebody runs it with a real credential.
//
// ── THE ONE LINE THAT IS INVISIBLE WHEN IT IS MISSING ────────────────────────────────────────
//
// SetProvideModesWithReturnTraffic is not decoration and it is not a performance setting. The
// message server's replies are IT sending to a client it holds no contract with, which is return
// traffic; a client that has not enabled ProvideMode_Stream is a client the platform delivers
// NOTHING to. What makes it the dangerous line rather than merely a necessary one is that its
// absence is SILENT AT EVERY HEALTH SIGNAL: the websocket connects, the client is authenticated,
// SendWithTimeout takes the frame and acks it, and the transport's own counters show requests
// sent and no error -- and every Call times out with nothing having gone wrong that anything can
// see. That observation is REPORTED off the live deployment and is not re-measured here, because
// nothing here can reach a deployment; what IS measured here is that the line ran, by
// TestAMessageClientCarriesTheCredentialsIdentityAndCanTakeReturnTraffic reading
// ContractManager().GetProvideModes(). It is performed by the CONSTRUCTOR rather than left to a
// caller to remember, which is the whole reason this file exists rather than a doc note.

var (
	// No credential. It is separate from a malformed one because the two have different
	// answers: this one is "you have not logged in", and a parse failure is "this string is
	// not the token you think it is".
	ErrMessageClientNoJwt = errors.New("sdk: a platform-attached message client needs a by_client_jwt for a network_client; an operator admin mints it (spec B 9.1)")

	// The token parsed and carries NO client_id, which is the trap this refusal exists for.
	// connect.ParseByJwtUnverified fills its ByJwt field by field and SKIPS any field whose
	// claim is absent or unparseable -- so a token with no client_id claim, or one whose
	// client_id is not a uuid, answers a ByJwt whose ClientId is sixteen zero octets and NO
	// ERROR AT ALL. A client built at the zero id dials, authenticates, and is a client_id the
	// platform routes nothing to, which is the same silent-green failure as the provide modes.
	ErrMessageClientNoClientId = errors.New("sdk: this by_client_jwt carries no client_id claim, so the client it names is the zero id and the platform would route nothing to it")

	// Nowhere to dial. Both service urls are derived from the host, so an empty host is an
	// empty authority rather than a default.
	ErrMessageClientNoHost = errors.New("sdk: a platform-attached message client needs the operator's host name; the platform and api urls are derived from it")
)

// What the app calls itself to the platform when a caller names nothing. It is a value the
// platform logs against the connection, so it has to be SOMETHING; an empty one is a connection
// nobody can attribute afterwards.
const messageClientDefaultAppVersion = "urmessage"

// MessageClientConfig is one platform-attached connect client.
type MessageClientConfig struct {
	// The operator-minted ByJwt for a network_client. It is a CREDENTIAL: it is never logged
	// by this package, it is not written to disk by this package, and a caller that puts it in
	// an error message has published it.
	ByClientJwt string

	// The operator's host name, e.g. "ur.io". The platform and api urls are derived from it
	// through [ServiceUrl], which is the same derivation NetworkSpace performs -- so a
	// non-default Env lands on the same authority the rest of this module would dial rather
	// than on a hand-built "wss://connect." + host, which is right only for the main env.
	Host string

	// The environment, "" or "main" for the deployed one. [NormalEnvName] is applied.
	Env string

	// This installation's instance id. The zero value draws a fresh one, which is what a
	// caller with nothing to persist wants; a caller that reconnects as the same instance
	// passes the id it kept.
	InstanceId connect.Id

	// What to report as the app version. "" takes [messageClientDefaultAppVersion].
	AppVersion string

	// Absolute overrides for the two derived urls, for a deployment that does not follow the
	// service-host convention. Empty takes the derivation.
	PlatformUrl string
	ApiUrl      string
}

// MessageClient is a connect client attached to the URnetwork platform, ready to be the
// [MessageTransportConfig.Client] of a §10.1 binding.
//
// IT SATISFIES [MessageTransportClient] BY FORWARDING and holds no state of its own beyond the
// client, the transport and the cancel. The forwarding is what lets one handle be both "the thing
// that owns the connection" and "the thing the transport speaks over" -- the alternative is to
// hand a caller the *connect.Client and keep the closer somewhere else, which is a lifetime a C
// caller cannot hold correctly.
type MessageClient struct {
	cancel    context.CancelFunc
	client    *connect.Client
	transport *connect.PlatformTransport

	platformUrl string
	apiUrl      string
}

// The forwarding is checked at compile time rather than at the handle registry's type assertion,
// which would fail at run time in a C caller with nothing but a log line to show for it.
var _ MessageTransportClient = (*MessageClient)(nil)

// NewMessageClient stands up a platform-attached connect client.
//
// THE CLIENT IS THIS OBJECT'S AND NOT THE CALLER'S, which is the opposite of every other seam in
// the messaging surface and is deliberate: [MessageClient.Close] is the only way the websocket,
// the platform transport's reconnect loop and the client's own goroutines are stopped, and a
// caller holding a bare *connect.Client it did not construct has no way to stop them.
//
// WHAT IT DOES NOT DO. It does not block, it does not wait for the connection, and it does not
// report whether the credential was accepted: the platform transport dials on a goroutine of its
// own and reconnects for as long as the context lives. The first thing that finds out whether any
// of this worked is [MessageTransport.Hello], and urmessage.Device.Connect is what rides out the
// ~60 second window during which a reconnecting client_id is not routed to.
func NewMessageClient(ctx context.Context, config *MessageClientConfig) (*MessageClient, error) {
	if config == nil {
		return nil, ErrMessageClientNoJwt
	}
	byJwt := strings.TrimSpace(config.ByClientJwt)
	if byJwt == "" {
		return nil, ErrMessageClientNoJwt
	}
	platformUrl, apiUrl, err := messageClientUrls(config)
	if err != nil {
		return nil, err
	}
	// PARSED FOR ITS client_id AND NOT VERIFIED, and the name of the function says so. The
	// signature is the PLATFORM's to check -- nothing here holds the key -- so this parse
	// decides one thing only: which client_id this credential names. A token this rejects
	// would have been rejected at the far end too; what it buys is that the rejection happens
	// here, with a sentence, instead of as sixty seconds of a client nobody routes to.
	parsed, err := connect.ParseByJwtUnverified(byJwt)
	if err != nil {
		// THE TOKEN IS NOT IN THIS MESSAGE. err comes from the jwt parser and carries the
		// reason, not the credential; a %w of the token would put a live credential in
		// every log that catches this.
		return nil, fmt.Errorf("sdk: this by_client_jwt could not be parsed: %w", err)
	}
	if parsed.ClientId == (connect.Id{}) {
		return nil, ErrMessageClientNoClientId
	}
	instanceId := config.InstanceId
	if instanceId == (connect.Id{}) {
		instanceId = connect.NewId()
	}
	appVersion := config.AppVersion
	if appVersion == "" {
		appVersion = messageClientDefaultAppVersion
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	strategy := connect.NewClientStrategyWithDefaults(cancelCtx)
	oob := connect.NewApiOutOfBandControl(cancelCtx, strategy, byJwt, apiUrl)
	client := connect.NewClient(cancelCtx, parsed.ClientId, oob, connect.DefaultClientSettings())
	transport := connect.NewPlatformTransport(
		client.Ctx(), strategy, client.RouteManager(), platformUrl,
		&connect.ClientAuth{ByJwt: byJwt, InstanceId: instanceId, AppVersion: appVersion},
		connect.DefaultPlatformTransportSettings(),
	)
	// The one line whose absence is invisible. See this file's header.
	client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{})

	return &MessageClient{
		cancel:      cancel,
		client:      client,
		transport:   transport,
		platformUrl: platformUrl,
		apiUrl:      apiUrl,
	}, nil
}

// messageClientUrls is the two service urls this client dials, derived rather than concatenated.
//
// IT GOES THROUGH [ServiceUrl] because that is the derivation the rest of this module already
// performs, env prefix and all: on env "main" or "" the platform is wss://connect.<host>, and on
// any other env it is wss://<env>-connect.<host>. sdk/liveprobe built "wss://connect." + host by
// hand, which silently dials the production authority from a non-production env.
func messageClientUrls(config *MessageClientConfig) (platformUrl string, apiUrl string, err error) {
	platformUrl = strings.TrimRight(config.PlatformUrl, "/")
	apiUrl = strings.TrimRight(config.ApiUrl, "/")
	if platformUrl != "" && apiUrl != "" {
		return platformUrl, apiUrl, nil
	}
	if strings.TrimSpace(config.Host) == "" {
		return "", "", ErrMessageClientNoHost
	}
	key := NetworkSpaceKey{HostName: config.Host, EnvName: NormalEnvName(config.Env)}
	values := NetworkSpaceValues{}
	if platformUrl == "" {
		platformUrl = ServiceUrl(&key, &values, "wss", "connect")
	}
	if apiUrl == "" {
		apiUrl = ServiceUrl(&key, &values, "https", "api")
	}
	return platformUrl, apiUrl, nil
}

// Client is the connect client underneath, for a caller that needs something this wrapper does
// not forward. It stays owned by this object: closing it directly leaves the platform transport
// and the context running.
func (self *MessageClient) Client() *connect.Client {
	return self.client
}

// ClientId is the client_id the credential named, which is the identity the platform routes to.
func (self *MessageClient) ClientId() connect.Id {
	return self.client.ClientId()
}

// PlatformUrl and ApiUrl are what this client actually dialled, which is the one thing a caller
// cannot otherwise check about a derivation it did not perform.
func (self *MessageClient) PlatformUrl() string {
	return self.platformUrl
}

func (self *MessageClient) ApiUrl() string {
	return self.apiUrl
}

// SendWithTimeout forwards to the client. It is one half of [MessageTransportClient].
func (self *MessageClient) SendWithTimeout(
	frame *protocol.Frame,
	destination connect.Id,
	ackCallback connect.AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	return self.client.SendWithTimeout(frame, destination, ackCallback, timeout, opts...)
}

// AddReceiveCallback forwards to the client. It is the other half.
func (self *MessageClient) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	return self.client.AddReceiveCallback(receiveCallback)
}

// Close stops the platform transport, the client and everything the context holds. It is
// idempotent, because a C caller's stop-then-release is not always in that order.
//
// IT IS connect.Client.Close AND NOT Cancel, which is a difference worth naming because
// sdk/liveprobe used to call Cancel here. Read off connect at d368fea: the two are the same call
// for all three buffers -- SendBuffer.Close and SendBuffer.Cancel have identical bodies, both
// cancelling every open sequence -- and Close additionally closes the encryption session manager
// and the WebRTC manager and drops three subscriptions. So Close is strictly more teardown for
// the same work, and nothing in it waits for a drain.
//
// WHAT WAS MEASURED AND WHAT WAS NOT. It returns promptly in the two places this is exercised --
// the sdk test beside this file and the C abi's own consumer test, both against a host that
// cannot resolve. NEITHER exercises a client with frames already queued against a route that has
// gone away, so "Close never blocks" is read off connect's source above rather than measured
// under load.
func (self *MessageClient) Close() {
	if self.transport != nil {
		self.transport.Close()
	}
	if self.client != nil {
		self.client.Close()
	}
	if self.cancel != nil {
		self.cancel()
	}
}
