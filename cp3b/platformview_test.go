package cp3b

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/message-server/api"
	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/urmessage"
)

// WHAT THE URNETWORK PLATFORM CAN READ ON THE URMESSAGE PATH, MEASURED RATHER THAN ARGUED.
//
// THE PATH. The app and the message server are both connect clients of the platform
// (sdk/message_client.go, msgrepo/cmd/message-server/transport.go). A request goes up the app's own
// TLS websocket to wss://connect.<host>; the platform reads the frame's TransferPath and writes the
// frame down the server's websocket (server/connect/resident.go, handleClientForward). TLS ends AT
// THE PLATFORM on both legs, so what the platform holds is the transfer frame itself -- exactly the
// bytes on a connect.Route in this process. Nothing else is on the path: the app sends to
// connect.DestinationId(server) with no intermediaries and no stream (sdk/message_transport.go), so
// no provider node ever carries a URmessage frame.
//
// SO THE PLATFORM HERE IS A GOROUTINE BETWEEN TWO ROUTES that forwards each frame and keeps a copy,
// and what it can READ is whatever decodes out of those copies with the public protobuf schemas
// and no key. The one thing that changes the answer is connect's per-peer encryption
// (connect/transfer_encrypt.go), and it is a setting: DefaultClientSettings leaves it OFF, which
// is what both ends of the deployed path pass, while the VPN client sets REQUIRED
// (connect/ip_remote_multi_client.go). The same exchange runs here under each mode.
//
// THE GROUND TRUTH IS THE SERVER'S OWN VIEW. A tap in front of the api handler keeps every request
// the server was handed, after any per-peer decryption. Its fields are the needles, so the absence
// under REQUIRED is searched for with needles drawn from requests the server provably received, and
// the same search, over the same kind of copy, FINDS every one of them under OFF.
func TestWhatTheUrnetworkPlatformCanReadOnTheUrmessagePath(t *testing.T) {
	shipping := shippingMessageClientMode(t)
	t.Logf("sdk.NewMessageClient builds its connect client with per-peer encryption %s", modeName(shipping))

	// ── OFF: the deployed setting ─────────────────────────────────────────────────────────
	off := newPlatformWorld(t, connect.EncryptionModeOff)
	offExchange := runExchange(t, off)
	offHanded := off.handed.all()
	offNeedles := needlesOf(offHanded)
	offRead := readAsThePlatform(off.relay.copies())
	if len(offNeedles) < 8 {
		t.Fatalf("the server was handed %d requests yielding only %d needles; the control has nothing to find",
			len(offHanded), len(offNeedles))
	}
	if missing := absentNeedles(offRead.clear, offNeedles); len(missing) != 0 {
		t.Errorf("OFF: %d of %d fields the server received are not in what the platform held: %v",
			len(missing), len(offNeedles), missing)
	}
	// the second, independent measure: the platform decodes each request WHOLE, equal field for
	// field to what the server was handed
	if matched := matchedRequests(offHanded, offRead.requests); matched != len(offHanded) {
		t.Errorf("OFF: the platform decoded %d of the %d requests the server was handed", matched, len(offHanded))
	}
	if offRead.sealed != 0 {
		t.Errorf("OFF: %d frames were sealed with per-peer encryption switched off", offRead.sealed)
	}
	assertTextsNowhere(t, "OFF", offRead.clear, offExchange.texts)
	t.Logf("OFF: the platform forwarded %d frames; %d sealed; it decoded %d requests and %d responses; "+
		"it holds %d of the %d fields the server received, and %d of the %d message texts",
		offRead.frames, offRead.sealed, len(offRead.requests), offRead.responses,
		len(offNeedles)-len(absentNeedles(offRead.clear, offNeedles)), len(offNeedles),
		textsHeld(offRead.clear, offExchange.texts), len(offExchange.texts))
	for _, line := range offRead.linkage() {
		t.Logf("OFF: the platform saw %s", line)
	}

	// ── REQUIRED: what the VPN client already sets ─────────────────────────────────────────
	required := newPlatformWorld(t, connect.EncryptionModeRequired)
	requiredExchange := runExchange(t, required)
	requiredHanded := required.handed.all()
	requiredNeedles := needlesOf(requiredHanded)
	requiredRead := readAsThePlatform(required.relay.copies())
	if len(requiredNeedles) < len(offNeedles)/2 {
		t.Fatalf("REQUIRED: the server was handed %d requests yielding %d needles, against %d under OFF; "+
			"the absence below would be searched for with too little", len(requiredHanded), len(requiredNeedles), len(offNeedles))
	}
	requiredFound := len(requiredNeedles) - len(absentNeedles(requiredRead.clear, requiredNeedles))
	if requiredFound != 0 {
		t.Errorf("REQUIRED: %d of %d fields the server received are readable in what the platform held",
			requiredFound, len(requiredNeedles))
	}
	if len(requiredRead.requests) != 0 || requiredRead.responses != 0 {
		t.Errorf("REQUIRED: the platform decoded %d requests and %d responses", len(requiredRead.requests), requiredRead.responses)
	}
	if requiredRead.sealed == 0 {
		t.Error("REQUIRED: no frame was sealed, so nothing here measured the encryption")
	}
	assertTextsNowhere(t, "REQUIRED", requiredRead.clear, requiredExchange.texts)
	for _, name := range []string{"alice", "bob"} {
		if curve, sealed := sealedWith(requiredExchange.clients[name], required.serverClient.ClientId()); !sealed {
			t.Errorf("REQUIRED: %s's session with the server is not sealed", name)
		} else if curve != tls.X25519MLKEM768 {
			t.Errorf("REQUIRED: %s's session with the server sealed under %v, not X25519MLKEM768", name, curve)
		} else {
			t.Logf("REQUIRED: %s's session with the server is sealed under %v", name, curve)
		}
	}
	t.Logf("REQUIRED: the platform forwarded %d frames; %d sealed; it decoded %d requests and %d responses; "+
		"it holds %d of the %d fields the server received, and the exchange delivered both texts",
		requiredRead.frames, requiredRead.sealed, len(requiredRead.requests), requiredRead.responses,
		requiredFound, len(requiredNeedles))

	// ── OPPORTUNISTIC: what providers set. Measured and reported, not ruled on here ───────
	opportunistic := newPlatformWorld(t, connect.EncryptionModeOpportunistic)
	opportunisticExchange := runExchange(t, opportunistic)
	opportunisticHanded := opportunistic.handed.all()
	opportunisticNeedles := needlesOf(opportunisticHanded)
	opportunisticRead := readAsThePlatform(opportunistic.relay.copies())
	assertTextsNowhere(t, "OPPORTUNISTIC", opportunisticRead.clear, opportunisticExchange.texts)
	t.Logf("OPPORTUNISTIC: the platform forwarded %d frames; %d sealed; it decoded %d hellos, %d of the %d other requests "+
		"the server's handler received, and %d responses; it holds %d of the %d fields the server received",
		opportunisticRead.frames, opportunisticRead.sealed, opportunisticRead.hellos(),
		matchedRequests(opportunisticHanded, opportunisticRead.requests), len(opportunisticHanded),
		opportunisticRead.responses,
		len(opportunisticNeedles)-len(absentNeedles(opportunisticRead.clear, opportunisticNeedles)), len(opportunisticNeedles))
	for _, line := range opportunisticRead.linkage() {
		t.Logf("OPPORTUNISTIC: the platform saw %s", line)
	}
}

// ── the world: one message server, the platform between it and every client ──────────────────

type platformWorld struct {
	ctx          context.Context
	mode         connect.EncryptionMode
	serverClient *connect.Client
	handed       *serverHanded
	relay        *platformRelay

	// the platform's /key/<client_id> directory (connect/api.go GetClientKey). A client with no
	// contract learns a peer's identity key here and nowhere else -- which is also why per-peer
	// encryption defeats a platform that READS and not one that LIES: it is the platform's answer.
	keys sync.Map
}

func newPlatformWorld(t *testing.T, mode connect.EncryptionMode) *platformWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	self := &platformWorld{ctx: ctx, mode: mode, relay: &platformRelay{}}

	serverClient := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), self.settings())
	self.publishKey(serverClient)
	connections, err := peer.NewConnections(rand.Reader, time.Now, time.Hour)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewConnections: %v", err)
	}
	checks, err := peer.NewChecks(connections, peer.DefaultMaxRequestBytes)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewChecks: %v", err)
	}
	handler, err := api.New(api.Config{
		Store:       store.NewMemoryStore(store.DefaultLimits()),
		KnownGroups: api.NewMemoryKnownGroups(),
		Front:       checks,
	})
	if err != nil {
		cancel()
		t.Fatalf("api.New: %v", err)
	}
	self.handed = &serverHanded{Handler: handler}
	served, err := peer.New(peer.Config{
		Client:      serverClient,
		Handler:     self.handed,
		Connections: connections,
		Checks:      checks,
		Capabilities: &protocol.Capabilities{
			MaxRequestBytes: peer.DefaultMaxRequestBytes,
		},
		ProtocolVersion: worldProtocolVersion,
		ServerId:        bytes.Repeat([]byte{0x5A}, 16),
	})
	if err != nil {
		cancel()
		t.Fatalf("peer.New: %v", err)
	}
	self.serverClient = serverClient
	t.Cleanup(func() {
		served.Close()
		serverClient.Close()
		cancel()
	})
	return self
}

// settings is DefaultClientSettings -- what sdk.NewMessageClient and the message server both pass --
// with only the per-peer mode changed. Off changes nothing at all.
func (self *platformWorld) settings() *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	settings.EncryptionSettings.Mode = self.mode
	if self.mode != connect.EncryptionModeOff {
		// no contracts in this process, so no companion contracts to carry handshake replies
		settings.EncryptionSettings.EncryptionControlUseCompanion = false
		settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
			return func(context.Context) ([]byte, error) {
				if key, ok := self.keys.Load(peerId); ok {
					return key.([]byte), nil
				}
				return nil, fmt.Errorf("no identity key published for %s", peerId)
			}
		}
	}
	return settings
}

func (self *platformWorld) publishKey(client *connect.Client) {
	self.keys.Store(client.ClientId(), []byte(client.ClientKeyManager().PublicKey()))
}

// device is one app: its own connect client, both of its legs running through the platform, and a
// urmessage device over the real transport and a real on-disk stream store.
func (self *platformWorld) device(t *testing.T, name string) (*urmessage.Device, *connect.Client) {
	t.Helper()
	client := connect.NewClient(self.ctx, connect.NewId(), connect.NewNoContractClientOob(), self.settings())
	t.Cleanup(client.Close)
	self.publishKey(client)

	// FOUR ROUTES AND NOT TWO: each leg ends at the platform, which is the point
	up := make(connect.Route)
	toServer := make(connect.Route)
	fromServer := make(connect.Route)
	down := make(connect.Route)
	client.RouteManager().UpdateTransport(connect.NewSendGatewayTransport(), []connect.Route{up})
	client.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{down})
	client.ContractManager().AddNoContractPeer(self.serverClient.ClientId())
	self.serverClient.RouteManager().UpdateTransport(
		connect.NewSendClientTransport(connect.DestinationId(client.ClientId())), []connect.Route{fromServer})
	self.serverClient.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransport(), []connect.Route{toServer})
	self.serverClient.ContractManager().AddNoContractPeer(client.ClientId())
	self.relay.forward(self.ctx, name+" -> server", up, toServer)
	self.relay.forward(self.ctx, "server -> "+name, fromServer, down)

	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          client,
		Server:          self.serverClient.ClientId(),
		ProtocolVersion: worldProtocolVersion,
		Timeout:         30 * time.Second,
	})
	if err != nil {
		t.Fatalf("%s: sdk.NewMessageTransport: %v", name, err)
	}
	t.Cleanup(transport.Close)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("%s: sdk.OpenStreamStore: %v", name, err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport: transport,
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
	})
	if err != nil {
		t.Fatalf("%s: urmessage.NewDevice: %v", name, err)
	}
	t.Cleanup(func() { device.Close() })
	return device, client
}

// ── the exchange: found, add, open, send, join, receive, answer ──────────────────────────────

type exchangeResult struct {
	texts   []string
	clients map[string]*connect.Client
}

func runExchange(t *testing.T, world *platformWorld) exchangeResult {
	t.Helper()
	alice, aliceClient := world.device(t, "alice")
	bob, bobClient := world.device(t, "bob")
	return exchangeResult{
		texts:   exchangeBetween(t, alice, bob),
		clients: map[string]*connect.Client{"alice": aliceClient, "bob": bobClient},
	}
}

// exchangeBetween is the whole CP3b exchange between two devices, whatever carries their frames:
// connect, alice founds and adds bob, opens, sends; bob joins, receives, answers; alice receives.
// It answers the texts typed, both of which it has checked arrived intact.
func exchangeBetween(t *testing.T, alice *urmessage.Device, bob *urmessage.Device) []string {
	t.Helper()
	ctx := context.Background()
	if err := alice.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	aliceGroup, err := alice.CreateGroup(ctx, newGroupId(t))
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	keyPackage, err := bob.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMember(keyPackage)
	if err != nil {
		t.Fatalf("alice's AddMember: %v", err)
	}
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	if _, err := aliceGroup.Send(ctx, typedByAlice); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	bobGroup, err := bob.Join(ctx, carried)
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	received, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if len(received) != 1 || received[0].Text != typedByAlice {
		t.Fatalf("bob received %+v for the one message alice sent", received)
	}
	if _, err := bobGroup.Send(ctx, typedByBob); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(back) != 1 || back[0].Text != typedByBob {
		t.Fatalf("alice received %+v for the one message bob sent", back)
	}
	return []string{typedByAlice, typedByBob}
}

// ── the platform ───────────────────────────────────────────────────────────────────────────

// relayedFrame is one transfer frame as the platform held it, with the leg it arrived on -- which the
// platform knows, because each leg is an authenticated websocket from an IP address.
type relayedFrame struct {
	leg   string
	frame []byte
}

type platformRelay struct {
	mutex  sync.Mutex
	frames []relayedFrame
}

func (self *platformRelay) forward(ctx context.Context, leg string, from connect.Route, to connect.Route) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-from:
				if !ok {
					return
				}
				self.mutex.Lock()
				self.frames = append(self.frames, relayedFrame{leg: leg, frame: bytes.Clone(frame)})
				self.mutex.Unlock()
				select {
				case to <- frame:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

func (self *platformRelay) copies() []relayedFrame {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]relayedFrame(nil), self.frames...)
}

type platformReading struct {
	frames    int
	sealed    int
	requests  map[string]*protocol.MessageServerRequest
	legs      map[string]string
	responses int
	// everything the platform holds in the clear: every frame as it arrived, and every request or
	// response it could put back together from them
	clear [][]byte
}

// readAsThePlatform decodes the copies the way the platform could: public schemas, no key.
func readAsThePlatform(relayed []relayedFrame) *platformReading {
	reading := &platformReading{
		requests: map[string]*protocol.MessageServerRequest{},
		legs:     map[string]string{},
	}
	type partsKey struct {
		leg       string
		requestId uint64
	}
	parts := map[partsKey]map[uint32][]byte{}
	whole := func(leg string, body []byte) {
		reading.clear = append(reading.clear, body)
		if strings.HasPrefix(leg, "server ->") {
			if proto.Unmarshal(body, &protocol.MessageServerResponse{}) == nil {
				reading.responses += 1
			}
			return
		}
		request := &protocol.MessageServerRequest{}
		if proto.Unmarshal(body, request) == nil && request.GetBody() != nil {
			key := fmt.Sprintf("%s/%d", leg, request.GetRequestId())
			reading.requests[key] = request
			reading.legs[key] = leg
		}
	}
	for _, relayed := range relayed {
		reading.frames += 1
		reading.clear = append(reading.clear, relayed.frame)
		transferFrame := &protocol.TransferFrame{}
		if proto.Unmarshal(relayed.frame, transferFrame) != nil {
			continue
		}
		if 0 < len(transferFrame.GetEncryptedTransferFrame()) {
			reading.sealed += 1
			continue
		}
		pack := transferFrame.GetPack()
		if pack == nil && transferFrame.GetFrame().GetMessageType() == protocol.MessageType_TransferPack {
			pack = &protocol.Pack{}
			if proto.Unmarshal(transferFrame.GetFrame().GetMessageBytes(), pack) != nil {
				pack = nil
			}
		}
		if pack == nil {
			continue
		}
		for _, frame := range pack.GetFrames() {
			switch frame.GetMessageType() {
			case protocol.MessageType_MessageMessageServerRequest, protocol.MessageType_MessageMessageServerResponse:
				whole(relayed.leg, frame.GetMessageBytes())
			case protocol.MessageType_MessageMessageServerFragment:
				fragment := &protocol.MessageServerFragment{}
				if proto.Unmarshal(frame.GetMessageBytes(), fragment) != nil {
					continue
				}
				key := partsKey{leg: relayed.leg, requestId: fragment.GetRequestId()}
				if parts[key] == nil {
					parts[key] = map[uint32][]byte{}
				}
				parts[key][fragment.GetIndex()] = fragment.GetPart()
				if uint32(len(parts[key])) == fragment.GetCount() {
					var body []byte
					for index := uint32(0); index < fragment.GetCount(); index += 1 {
						body = append(body, parts[key][index]...)
					}
					delete(parts, key)
					whole(relayed.leg, body)
				}
			}
		}
	}
	return reading
}

func (self *platformReading) hellos() int {
	count := 0
	for _, request := range self.requests {
		if request.GetHello() != nil {
			count += 1
		}
	}
	return count
}

// linkage is what the platform could write down per connection, from the requests alone.
func (self *platformReading) linkage() []string {
	type seen struct {
		kinds   map[string]int
		groups  map[string]bool
		handles map[string]bool
		keys    int
	}
	byLeg := map[string]*seen{}
	for key, request := range self.requests {
		leg := self.legs[key]
		if byLeg[leg] == nil {
			byLeg[leg] = &seen{kinds: map[string]int{}, groups: map[string]bool{}, handles: map[string]bool{}}
		}
		one := byLeg[leg]
		switch body := request.GetBody().(type) {
		case *protocol.MessageServerRequest_Hello:
			one.kinds["hello"] += 1
		case *protocol.MessageServerRequest_CreateGroup:
			one.kinds["create_group"] += 1
			one.groups[fmt.Sprintf("%x", body.CreateGroup.GetGroupId()[:6])] = true
			one.handles[fmt.Sprintf("%x", body.CreateGroup.GetInitialCommit().GetSenderHandle())] = true
			if 0 < len(body.CreateGroup.GetBootstrapWriteKey()) {
				one.keys += 1
			}
			if body.CreateGroup.GetEpochKeys() != nil {
				one.keys += 2
			}
		case *protocol.MessageServerRequest_Submit:
			one.kinds["submit"] += 1
			one.groups[fmt.Sprintf("%x", body.Submit.GetGroupId()[:6])] = true
			for _, record := range body.Submit.GetRecords() {
				one.handles[fmt.Sprintf("%x", record.GetSenderHandle())] = true
			}
			for _, keys := range body.Submit.GetEpochKeys() {
				if 0 < len(keys.GetWriteKey()) {
					one.keys += 2
				}
			}
		case *protocol.MessageServerRequest_Fetch:
			one.kinds["fetch"] += 1
			one.groups[fmt.Sprintf("%x", body.Fetch.GetGroupId()[:6])] = true
		default:
			one.kinds[fmt.Sprintf("%T", body)] += 1
		}
	}
	var lines []string
	for leg, one := range byLeg {
		lines = append(lines, fmt.Sprintf("%s: requests %v, group %v, sender_handle %v, %d raw write/read keys",
			leg, sortedCounts(one.kinds), sortedKeys(one.groups), sortedKeys(one.handles), one.keys))
	}
	sort.Strings(lines)
	return lines
}

// ── the server's own view, which is the ground truth ─────────────────────────────────────────

type serverHanded struct {
	peer.Handler
	mutex    sync.Mutex
	requests []proto.Message
}

func (self *serverHanded) keep(request proto.Message) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.requests = append(self.requests, proto.Clone(request))
}

func (self *serverHanded) all() []proto.Message {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]proto.Message(nil), self.requests...)
}

func (self *serverHanded) CreateGroup(ctx context.Context, conn *api.Connection,
	request *protocol.CreateGroupRequest) (protocol.Reason, *protocol.CreateGroupResponse, error) {
	self.keep(request)
	return self.Handler.CreateGroup(ctx, conn, request)
}

func (self *serverHanded) Submit(ctx context.Context, conn *api.Connection,
	request *protocol.SubmitRequest) (protocol.Reason, *protocol.SubmitResponse, error) {
	self.keep(request)
	return self.Handler.Submit(ctx, conn, request)
}

func (self *serverHanded) Fetch(ctx context.Context, conn *api.Connection,
	request *protocol.FetchRequest) (protocol.Reason, *protocol.FetchResponse, error) {
	self.keep(request)
	return self.Handler.Fetch(ctx, conn, request)
}

type platformNeedle struct {
	name  string
	value []byte
}

func (self platformNeedle) String() string {
	return self.name
}

// needlesOf is every field of the server's requests long enough to be found by chance never: the
// group id, every raw write and read key, every sender handle, body hash and sealed record, and every
// fetch authenticator.
func needlesOf(requests []proto.Message) []platformNeedle {
	seen := map[string]bool{}
	var needles []platformNeedle
	add := func(name string, value []byte) {
		if len(value) < 16 || seen[string(value)] {
			return
		}
		seen[string(value)] = true
		needles = append(needles, platformNeedle{name: name, value: bytes.Clone(value)})
	}
	record := func(prefix string, record *protocol.Record) {
		add(prefix+".sender_handle", record.GetSenderHandle())
		add(prefix+".body_hash", record.GetBodyHash())
		add(prefix+".record_bytes", record.GetRecordBytes())
	}
	for _, request := range requests {
		switch r := request.(type) {
		case *protocol.CreateGroupRequest:
			add("create_group.group_id", r.GetGroupId())
			add("create_group.bootstrap_write_key", r.GetBootstrapWriteKey())
			add("create_group.epoch_keys.write_key", r.GetEpochKeys().GetWriteKey())
			add("create_group.epoch_keys.read_key", r.GetEpochKeys().GetReadKey())
			record("create_group.initial_commit", r.GetInitialCommit())
		case *protocol.SubmitRequest:
			add("submit.group_id", r.GetGroupId())
			for _, one := range r.GetRecords() {
				record("submit.record", one)
			}
			for _, keys := range r.GetEpochKeys() {
				add("submit.epoch_keys.write_key", keys.GetWriteKey())
				add("submit.epoch_keys.read_key", keys.GetReadKey())
			}
		case *protocol.FetchRequest:
			add("fetch.group_id", r.GetGroupId())
			add("fetch.req_auth", r.GetReqAuth())
		}
	}
	return needles
}

func absentNeedles(clear [][]byte, needles []platformNeedle) []platformNeedle {
	var absent []platformNeedle
	for _, one := range needles {
		found := false
		for _, held := range clear {
			if bytes.Contains(held, one.value) {
				found = true
				break
			}
		}
		if !found {
			absent = append(absent, one)
		}
	}
	return absent
}

// matchedRequests counts the server's requests that the platform decoded field for field.
func matchedRequests(handed []proto.Message, decoded map[string]*protocol.MessageServerRequest) int {
	matched := 0
	for _, request := range handed {
		for _, candidate := range decoded {
			var body proto.Message
			switch one := candidate.GetBody().(type) {
			case *protocol.MessageServerRequest_CreateGroup:
				body = one.CreateGroup
			case *protocol.MessageServerRequest_Submit:
				body = one.Submit
			case *protocol.MessageServerRequest_Fetch:
				body = one.Fetch
			}
			if body != nil && proto.Equal(body, request) {
				matched += 1
				break
			}
		}
	}
	return matched
}

func textsHeld(clear [][]byte, texts []string) int {
	held := 0
	for _, text := range texts {
		for _, one := range clear {
			if bytes.Contains(one, []byte(text)) {
				held += 1
				break
			}
		}
	}
	return held
}

func assertTextsNowhere(t *testing.T, mode string, clear [][]byte, texts []string) {
	t.Helper()
	for _, text := range texts {
		for _, held := range clear {
			if bytes.Contains(held, []byte(text)) {
				t.Errorf("%s: the platform holds a message text in the clear: %q", mode, text)
				break
			}
		}
	}
}

// sealedWith is the key exchange a client's session with the server sealed under, read off the
// client's own encryption state rather than inferred from the frames.
func sealedWith(client *connect.Client, serverId connect.Id) (tls.CurveID, bool) {
	for _, state := range client.EncryptionSessionManager().PeerEncryptionStates() {
		if state.PeerId == serverId && state.Sealed {
			return state.KeyExchange, true
		}
	}
	return 0, false
}

// ── what the shipping constructor actually sets ──────────────────────────────────────────────

// shippingMessageClientMode reads the per-peer mode off a client built by sdk.NewMessageClient
// itself, the constructor the Windows app reaches through urnet_message_client_new, rather than
// restating it. The host does not resolve; nothing here needs it to.
func shippingMessageClientMode(t *testing.T) connect.EncryptionMode {
	t.Helper()
	segment := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshalling a jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(encoded)
	}
	unsigned := strings.Join([]string{
		segment(map[string]any{"alg": "HS256", "typ": "JWT"}),
		segment(map[string]any{"client_id": connect.NewId().String(), "network_name": "someone"}),
		base64.RawURLEncoding.EncodeToString([]byte("not-a-signature")),
	}, ".")
	client, err := sdk.NewMessageClient(context.Background(), &sdk.MessageClientConfig{
		ByClientJwt: unsigned,
		Host:        "example.invalid",
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageClient: %v", err)
	}
	defer client.Close()
	return client.Client().EncryptionSessionManager().Settings().Mode
}

func modeName(mode connect.EncryptionMode) string {
	switch mode {
	case connect.EncryptionModeOff:
		return "OFF"
	case connect.EncryptionModeOpportunistic:
		return "OPPORTUNISTIC"
	case connect.EncryptionModeRequired:
		return "REQUIRED"
	}
	return fmt.Sprintf("mode(%d)", int(mode))
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedCounts(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key, count := range counts {
		keys = append(keys, fmt.Sprintf("%s×%d", key, count))
	}
	sort.Strings(keys)
	return keys
}
