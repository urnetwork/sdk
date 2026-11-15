//go:build !js

package sdk

// DeviceLocal as BridgeHost on app hosts (SDK-M8-08) on the forwarding harness of device_sidecar_forward_test.go: a
// device with a forwarding role set up by a hand-off, its Forwarder following nothing (every request answers 503), and
// no sdk.Sidecar anywhere, so the app's process is stopped throughout. A link's window clients come from a fake
// generator here, and F's api from a transport that records each request; the generator wrapper's own contract checks
// on real connect clients are device/bridge_link_generator_test.go's.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A link's settings: the bridge's fixed window of one Required window client in ProvideMode_Network, direct mode as
// the bridge sets it, and for a share link FriendsAndFamily with exactly the link's aliases as its private destinations.
func TestNewBridgeLinkSettings(t *testing.T) {
	settings := sidecardevice.MobileBridgeSettings()
	provideMode, multiClientSettings, err := deviceBridgeLinkSettings(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provideMode != protocol.ProvideMode_Network || len(multiClientSettings.SharePrivateDestinations) != 0 {
		t.Errorf("a same-network link: %v, %v", provideMode, multiClientSettings.SharePrivateDestinations)
	}
	if err := sidecardevice.CheckBridgeLinkMultiClientSettings(multiClientSettings, settings.LinkAllowDirect); err != nil {
		t.Errorf("the link's settings break its shape: %v", err)
	}
	aliases := []netip.Prefix{netip.MustParsePrefix("100.64.3.0/24"), netip.MustParsePrefix("100.64.4.7/32")}
	shareMode, shareSettings, err := deviceBridgeLinkSettings(settings, aliases)
	if err != nil {
		t.Fatal(err)
	}
	if shareMode != protocol.ProvideMode_FriendsAndFamily || len(shareSettings.SharePrivateDestinations) != 2 ||
		shareSettings.SharePrivateDestinations[0] != aliases[0] || shareSettings.SharePrivateDestinations[1] != aliases[1] {
		t.Errorf("a share link: %v, %v", shareMode, shareSettings.SharePrivateDestinations)
	}
	aliases[0] = netip.MustParsePrefix("10.0.0.0/8")
	if shareSettings.SharePrivateDestinations[0] == aliases[0] {
		t.Error("the link holds the caller's aliases, not a copy")
	}
	direct := sidecardevice.DefaultBridgeSettings()
	direct.LinkAllowDirect = true
	if _, directSettings, err := deviceBridgeLinkSettings(direct, nil); err != nil || !*directSettings.OverrideAllowDirect {
		t.Errorf("direct mode as the bridge sets it: %v", err)
	}
}

// What a test's link generator was asked for.
type testingLinkGenerations struct {
	stateLock sync.Mutex
	sources   []*deviceForwardLinkSource
	targets   []connect.Id
	creds     []connect.NetworkClientCredentials
}

func (self *testingLinkGenerations) generate(ctx context.Context, source *deviceForwardLinkSource, credentials connect.NetworkClientCredentials, target connect.Id) connect.MultiClientGenerator {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sources = append(self.sources, source)
	self.targets = append(self.targets, target)
	self.creds = append(self.creds, credentials)
	return &testingDnsOwnerGenerator{}
}

func (self *testingLinkGenerations) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.sources)
}

// A forwarding device whose role for the vectors' network is set up and whose Forwarder runs, its links' window
// clients from generations.
func newTestingLinkDevice(t *testing.T, generations *testingLinkGenerations) (*testingForwardDevice, *DeviceLocal, *deviceLocalForwarding) {
	t.Helper()
	forward := newTestingForwardDevice(t)
	device := forward.device(t, func(settings *DeviceLocalSettings) {
		harness := settings.testingForwardingDeps
		settings.testingForwardingDeps = func(deps *deviceForwardingDeps) {
			harness(deps)
			deps.linkGenerator = generations.generate
		}
	})
	pending := forward.pending()
	if err := forward.setUp(t, device, pending, nil); err != nil {
		t.Fatal(err)
	}
	networkId := connect.Id(pending.GetNetworkId())
	testingForwardStarted(t, device, networkId)
	role, err := device.forwardingHost().role(networkId, false)
	if err != nil || role == nil {
		t.Fatalf("no role: %v", err)
	}
	return forward, device, role
}

// A link refusing its sign, as no test here signs a real window client.
func testingRefusingLinkSign(windowClientId connect.Id, windowClientPublicKey []byte, notAfter time.Time) ([]byte, error) {
	return nil, errors.New("this test signs nothing")
}

// A link of a role's sidecar is built on that network's forwarding client F: its window clients' generator mints under
// F's token as F's children through F's credentials, for the link's target alone; the device answers for every role's
// bridge through the host it binds, and refuses a subject no role holds, a lapsed F, and any build before it binds.
func TestNewBridgeLinkOnTheForwardingClient(t *testing.T) {
	generations := &testingLinkGenerations{}
	forward, device, role := newTestingLinkDevice(t, generations)
	subject := forward.identity.sidecarId
	target := connect.NewId()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	link, err := device.bridgeOwner.linkHost.NewBridgeLink(ctx, target, subject, nil, testingRefusingLinkSign)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	if generations.count() != 1 {
		t.Fatalf("%d generators built", generations.count())
	}
	source, credentials := generations.sources[0], generations.creds[0]
	token, err := role.keys.token()
	if err != nil {
		t.Fatal(err)
	}
	role.stateLock.Lock()
	forwardClientId := connect.Id(role.setup.GetForwardClientId())
	role.stateLock.Unlock()
	if source.sidecarId != subject || source.networkId != role.networkId || source.byJwt != token || source.forwardClientId != forwardClientId ||
		generations.targets[0] != target {
		t.Errorf("the link's source: %+v", source)
	}
	forwardCredentials, ok := credentials.(*deviceForwardLinkCredentials)
	if !ok || forwardCredentials.client == nil || forwardCredentials.forwardClientId != forwardClientId {
		t.Errorf("the link mints under other credentials than F's: %T", credentials)
	}

	// a subject no role holds
	if _, err := device.NewBridgeLink(ctx, target, connect.NewId(), nil, testingRefusingLinkSign); !errors.Is(err, sidecardevice.ErrBridgeLinkRefused) {
		t.Errorf("another subject: %v", err)
	}
	// a host nothing bound refuses every build
	if _, err := (&deviceBridgeLinkHost{}).NewBridgeLink(ctx, target, subject, nil, testingRefusingLinkSign); !errors.Is(err, sidecardevice.ErrBridgeLinkRefused) {
		t.Errorf("an unbound host: %v", err)
	}
	// F lapsed: no link, and nothing signed for one
	role.stateLock.Lock()
	role.lapsed = true
	role.stateLock.Unlock()
	if _, err := device.NewBridgeLink(ctx, target, subject, nil, testingRefusingLinkSign); !errors.Is(err, errDeviceForwardLinkLapsed) {
		t.Errorf("a lapsed F: %v", err)
	}
	if _, err := role.signWindowDelegation(ctx, &bridge.WindowDelegation{}); !errors.Is(err, errDeviceForwardLinkLapsed) {
		t.Errorf("a lapsed F's sign: %v", err)
	}
	if generations.count() != 1 {
		t.Errorf("refused builds made %d generators", generations.count()-1)
	}
}

// F's api for the credentials: each request kept, answered as answer says.
type testingLinkApi struct {
	stateLock sync.Mutex
	requests  []testingLinkApiRequest
	answer    func(path string, body []byte) (int, any)
}

type testingLinkApiRequest struct {
	path          string
	authorization string
	proof         string
	body          []byte
}

func (self *testingLinkApi) RoundTrip(request *http.Request) (*http.Response, error) {
	body := []byte{}
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		request.Body.Close()
	}
	self.stateLock.Lock()
	self.requests = append(self.requests, testingLinkApiRequest{
		path:          request.URL.Path,
		authorization: request.Header.Get("Authorization"),
		proof:         request.Header.Get("DPoP"),
		body:          body,
	})
	answer := self.answer
	self.stateLock.Unlock()
	status, reply := answer(request.URL.Path, body)
	encoded, _ := json.Marshal(reply)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(encoded)),
		Request:    request,
	}, nil
}

func (self *testingLinkApi) taken() []testingLinkApiRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]testingLinkApiRequest{}, self.requests...)
}

// A window client is minted as F's new child, on F's token with F's proof, F named as its parent and no mark of F's
// own asked for, and taken only when api confirms forward_only, strict_exp and admin_refused, which F's children
// inherit, so C13 seals S's client as its subject; one confirmed otherwise is removed. A reissue, another parent, and a
// mint of F itself are refused before anything is sent.
func TestWindowClientMintedUnderF(t *testing.T) {
	marks := map[string]any{"admin_refused": true, "strict_exp": true, "forward_only": true}
	childId := sidecar.NewId()
	api := &testingLinkApi{}
	api.answer = func(path string, body []byte) (int, any) {
		switch path {
		case "/network/auth-client":
			reply := map[string]any{"by_client_jwt": "a-window-client-token", "client_id": childId}
			for name, value := range marks {
				reply[name] = value
			}
			return http.StatusOK, reply
		case "/network/remove-client":
			return http.StatusOK, map[string]any{}
		}
		return http.StatusNotFound, map[string]any{"error": "not found"}
	}
	apiSettings, err := deviceForwardApiSettings("https://api.forward.test", "wss://platform.forward.test")
	if err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	const forwardToken = "the-forwarding-client-token"
	client, err := sidecar.NewForwardingClient(&sidecar.ForwardingClientArgs{Settings: apiSettings, Transport: api, ByJwt: forwardToken, ClientKeySeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	forwardClientId := connect.NewId()
	credentials := &deviceForwardLinkCredentials{client: client, forwardClientId: forwardClientId}
	ctx := context.Background()

	result, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &forwardClientId, Description: "link", DeviceSpec: "spec"})
	if err != nil || result.ByClientJwt != "a-window-client-token" {
		t.Fatalf("the mint: %+v %v", result, err)
	}
	requests := api.taken()
	if len(requests) != 1 || requests[0].path != "/network/auth-client" || requests[0].authorization != "Bearer "+forwardToken || requests[0].proof == "" {
		t.Fatalf("the mint's request: %+v", requests)
	}
	wire := map[string]any{}
	if err := json.Unmarshal(requests[0].body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["source_client_id"] != forwardClientId.String() || wire["forward_only"] != nil || wire["client_public_key"] != nil || wire["client_id"] != nil {
		t.Errorf("the mint's body: %v", wire)
	}

	// refused before anything is sent
	otherParent := connect.NewId()
	existing := connect.NewId()
	for _, args := range []*connect.AuthNetworkClientArgs{
		{ClientId: &existing, SourceClientId: &forwardClientId},
		{SourceClientId: &otherParent},
		nil,
	} {
		if _, err := credentials.AuthNetworkClient(ctx, args); !errors.Is(err, errDeviceForwardLinkMint) {
			t.Errorf("args %+v: %v", args, err)
		}
	}
	parentId := sidecar.Id(forwardClientId)
	if _, err := client.AuthWindowClient(ctx, &sidecar.SidecarAuthClientArgs{SourceClientId: &parentId, ForwardOnly: true, ClientPublicKey: seed}); !errors.Is(err, sidecar.ErrSidecarApiArgs) {
		t.Errorf("a mint of F itself on F: %v", err)
	}
	if count := len(api.taken()); count != 1 {
		t.Fatalf("%d requests after the refusals", count)
	}

	// an answer missing forward_only is removed and refused
	delete(marks, "forward_only")
	if _, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{Description: "link", DeviceSpec: "spec"}); !errors.Is(err, errDeviceForwardLinkMint) {
		t.Errorf("a child without forward_only: %v", err)
	}
	requests = api.taken()
	if len(requests) != 3 || requests[2].path != "/network/remove-client" || !strings.Contains(string(requests[2].body), childId.String()) {
		t.Errorf("the unmarked child was not removed: %d requests", len(requests))
	}

	// a link's retirement removes its window client on F
	if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(childId)}); err != nil {
		t.Fatal(err)
	}
	if requests := api.taken(); requests[len(requests)-1].path != "/network/remove-client" || requests[len(requests)-1].authorization != "Bearer "+forwardToken {
		t.Errorf("the removal went other than on F")
	}
}

// A clock that stays where the test puts it.
type testingFixedClock struct {
	now time.Time
}

func (self testingFixedClock) Now() time.Time {
	return self.now
}

func (self testingFixedClock) After(timeout time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

func (self testingFixedClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	return func() bool { return true }
}

// A forwarding signer for subject in networkId, its state in force linking target, at now.
func testingForwardingSigner(t *testing.T, networkId connect.Id, subject connect.Id, target connect.Id, now time.Time) (*sidecar.ForwardingSigner, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	delegationId := [32]byte{}
	if _, err := rand.Read(delegationId[:]); err != nil {
		t.Fatal(err)
	}
	signer, err := sidecar.NewForwardingSigner(&sidecar.ForwardingSignerArgs{
		NetworkId:    sidecar.Id(networkId),
		SidecarId:    sidecar.Id(subject),
		DelegationId: delegationId,
		PublicKey:    publicKey,
		Sign: func(message []byte) ([]byte, error) {
			return ed25519.Sign(privateKey, message), nil
		},
		LinkTargets: func() ([]sidecar.Id, bool) {
			return []sidecar.Id{sidecar.Id(target)}, true
		},
		Clock: testingFixedClock{now: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	return signer, publicKey
}

// A window client's delegation as the bridge asks for it.
func testingWindowDelegation(networkId connect.Id, subject connect.Id, target connect.Id, notAfter time.Time) *bridge.WindowDelegation {
	return &bridge.WindowDelegation{
		NetworkId:             networkId.Bytes(),
		SubjectSidecarId:      subject.Bytes(),
		WindowClientId:        connect.NewId().Bytes(),
		WindowClientPublicKey: bytes.Repeat([]byte{5}, ed25519.PublicKeySize),
		TargetSidecarId:       target.Bytes(),
		NotAfterMs:            notAfter.UnixMilli(),
	}
}

// The delegation's not_after is the window client's strict_exp and never later: the forwarding key signs exactly the
// time asked within its bound, under the forwarding Delegation as signer, and refuses one past the bound rather than
// sign it.
func TestNotAfterCapped(t *testing.T) {
	networkId, subject, target := connect.NewId(), connect.NewId(), connect.NewId()
	now := time.UnixMilli(1800000000000)
	signer, publicKey := testingForwardingSigner(t, networkId, subject, target, now)
	strictExp := now.Add(90 * time.Minute)
	signedBytes, err := deviceForwardSignWindowDelegation(signer, networkId, testingWindowDelegation(networkId, subject, target, strictExp))
	if err != nil {
		t.Fatal(err)
	}
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		t.Fatal(err)
	}
	delegation := &bridge.WindowDelegation{}
	if err := proto.Unmarshal(signed.GetBody(), delegation); err != nil {
		t.Fatal(err)
	}
	if delegation.GetNotAfterMs() != strictExp.UnixMilli() || !bytes.Equal(delegation.GetSubjectSidecarId(), subject.Bytes()) {
		t.Errorf("signed until %d for %x", delegation.GetNotAfterMs(), delegation.GetSubjectSidecarId())
	}
	if len(signed.GetSignerId()) != 32 || !ed25519.Verify(publicKey, bridge.SignedBytes(bridge.SignedPurposeWindowDelegation, bridge.SignedType(signed.GetType()), signed.GetBody()), signed.GetSignature()) {
		t.Error("the delegation is not the forwarding key's under the Delegation's id")
	}
	if _, err := deviceForwardSignWindowDelegation(signer, networkId, testingWindowDelegation(networkId, subject, target, now.Add(200*time.Hour))); !errors.Is(err, sidecar.ErrForwardingDelegationRefused) {
		t.Errorf("a not_after past the bound: %v", err)
	}
}

// A share link's delegation is refused on app hosts, where the forwarding signer holds no share signer yet
// (BRIDGE-M6-04), so a share link, expired cert or not, opens no contract; so is a delegation of another network,
// another subject or a target the state in force links to nowhere.
func TestExpiredShareCertRefused(t *testing.T) {
	networkId, subject, target := connect.NewId(), connect.NewId(), connect.NewId()
	now := time.UnixMilli(1800000000000)
	signer, _ := testingForwardingSigner(t, networkId, subject, target, now)
	share := testingWindowDelegation(networkId, subject, target, now.Add(time.Hour))
	share.ShareId = connect.NewId().Bytes()
	share.ShareCert = []byte("a share cert past its not_after")
	if _, err := deviceForwardSignWindowDelegation(signer, networkId, share); !errors.Is(err, sidecar.ErrForwardingDelegationRefused) {
		t.Errorf("a share link: %v", err)
	}
	if _, err := deviceForwardSignWindowDelegation(signer, connect.NewId(), testingWindowDelegation(networkId, subject, target, now.Add(time.Hour))); !errors.Is(err, errDeviceForwardLinkDelegation) {
		t.Errorf("another role's network: %v", err)
	}
	if _, err := deviceForwardSignWindowDelegation(signer, networkId, testingWindowDelegation(networkId, connect.NewId(), target, now.Add(time.Hour))); !errors.Is(err, sidecar.ErrForwardingDelegationRefused) {
		t.Errorf("another subject: %v", err)
	}
	if _, err := deviceForwardSignWindowDelegation(signer, networkId, testingWindowDelegation(networkId, subject, connect.NewId(), now.Add(time.Hour))); !errors.Is(err, sidecar.ErrForwardingDelegationRefused) {
		t.Errorf("a target linked nowhere: %v", err)
	}
}

// The role hands its bridge its own LinkSign and LinkTargetKey, so every link the bridge builds signs by the role's
// Forwarder; with no current key set, the case before a promotion, and with no Forwarder running, LinkSign errors, so
// the generator wrapper retires each window client before its first contract and the link opens none.
func TestSignErrorOpensNoContract(t *testing.T) {
	generations := &testingLinkGenerations{}
	_, device, role := newTestingLinkDevice(t, generations)
	_, bridgeSettings := device.forwardingHost().postureStore(role)
	if bridgeSettings.LinkSign == nil || bridgeSettings.LinkTargetKey == nil {
		t.Fatal("the role's bridge builds its links with no signer or binding fetch")
	}
	ctx := context.Background()
	subject, _ := role.subject()
	delegation := testingWindowDelegation(role.networkId, subject, connect.NewId(), time.Now().Add(time.Hour))
	if signed, err := bridgeSettings.LinkSign(ctx, delegation); err == nil || signed != nil {
		t.Errorf("a role with no current key set signed a delegation")
	}
	role.stop()
	if _, err := bridgeSettings.LinkSign(ctx, delegation); !errors.Is(err, errDeviceForwardLinkNoClient) {
		t.Errorf("a role with no Forwarder: %v", err)
	}
}

// A role's bridge recording each re-sign of its shares' links.
type testingResignBridge struct {
	*sidecardevice.Bridge
	stateLock sync.Mutex
	resigned  [][]connect.Id
}

func (self *testingResignBridge) ResignShares(shareIds []connect.Id) error {
	self.stateLock.Lock()
	self.resigned = append(self.resigned, append([]connect.Id{}, shareIds...))
	self.stateLock.Unlock()
	return self.Bridge.ResignShares(shareIds)
}

// A new share signer re-signs the live window clients of that share's links: the role's resign goes to its own
// bridge, with the app's process stopped, naming the shares whose signer changed.
func TestNewShareSignerResignsLiveLinks(t *testing.T) {
	forward := newTestingForwardDevice(t)
	bridges := []*testingResignBridge{}
	bridgesLock := sync.Mutex{}
	device := forward.device(t, func(settings *DeviceLocalSettings) {
		settings.testingNewDeviceBridge = func(ctx context.Context, store sidecardevice.PostureStore, bridgeSettings *sidecardevice.BridgeSettings) (deviceBridgeOwned, error) {
			bridge, err := sidecardevice.NewBridge(ctx, store, nil, bridgeSettings)
			if err != nil {
				return nil, err
			}
			recorded := &testingResignBridge{Bridge: bridge}
			bridgesLock.Lock()
			bridges = append(bridges, recorded)
			bridgesLock.Unlock()
			return recorded, nil
		}
	})
	pending := forward.pending()
	if err := forward.setUp(t, device, pending, nil); err != nil {
		t.Fatal(err)
	}
	networkId := connect.Id(pending.GetNetworkId())
	testingForwardStarted(t, device, networkId)
	role, err := device.forwardingHost().role(networkId, false)
	if err != nil || role == nil {
		t.Fatal(err)
	}
	shareId := connect.NewId()
	if err := role.resignShares([]connect.Id{shareId}); err != nil {
		t.Fatal(err)
	}
	bridgesLock.Lock()
	defer bridgesLock.Unlock()
	resignedCount := 0
	for _, recorded := range bridges {
		recorded.stateLock.Lock()
		for _, shareIds := range recorded.resigned {
			if len(shareIds) == 1 && shareIds[0] == shareId {
				resignedCount += 1
			}
		}
		recorded.stateLock.Unlock()
	}
	if resignedCount != 1 {
		t.Errorf("the share's links were re-signed %d times", resignedCount)
	}
}
