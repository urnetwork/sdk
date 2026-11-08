//go:build !ios_extension

package sdk

// manage_devices_view_controller_test.go -- the network's devices
// (manage_devices_view_controller.go): what the list holds, which credential
// every request carries, and that a removal adds this device again.
//
// The api runs against an in-process stand-in for the server's three routes, and
// the device is a stand-in that records its client limit retries. Waits are on
// the controller's own listener calls.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
)

const testingManageDevicesNetworkJwt = "synthetic-network-credential"

// testingManageDevicesServer answers GET /network/clients, GET /network/proxies
// and POST /network/remove-client, and records the credential of each request.
type testingManageDevicesServer struct {
	stateLock   sync.Mutex
	devices     []*NetworkClientInfo
	proxies     []*NetworkClientInfo
	removed     []Id
	credentials []string
	gets        int
	// a status to answer the lists with, 0 for 200
	listStatus int
	// a status to answer a removal with, 0 for 200
	removeStatus int
	// a removal answered as a client the network no longer has
	removeMissing bool
}

func (self *testingManageDevicesServer) install(t *testing.T, api *Api) {
	api.setHttpGetRaw(func(_ context.Context, requestUrl string, byJwt string) ([]byte, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.credentials = append(self.credentials, byJwt)
		self.gets += 1
		if self.listStatus != 0 {
			return nil, &connect.HttpStatusError{StatusCode: self.listStatus, Status: http.StatusText(self.listStatus)}
		}
		var clients []*NetworkClientInfo
		switch {
		case strings.HasSuffix(requestUrl, "/network/clients"):
			clients = self.devices
		case strings.HasSuffix(requestUrl, "/network/proxies"):
			clients = self.proxies
		default:
			t.Errorf("unexpected get %s", requestUrl)
		}
		list := NewNetworkClientInfoList()
		list.addAll(clients...)
		return json.Marshal(&NetworkClientsResult{Clients: list})
	})
	api.setHttpPostRaw(func(_ context.Context, requestUrl string, body []byte, byJwt string) ([]byte, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.credentials = append(self.credentials, byJwt)
		if !strings.HasSuffix(requestUrl, "/network/remove-client") {
			t.Errorf("unexpected post %s", requestUrl)
		}
		if self.removeStatus != 0 {
			return nil, &connect.HttpStatusError{StatusCode: self.removeStatus, Status: http.StatusText(self.removeStatus)}
		}
		var args RemoveNetworkClientArgs
		if err := json.Unmarshal(body, &args); err != nil {
			t.Errorf("remove-client body: %v", err)
		}
		if self.removeMissing {
			return json.Marshal(&RemoveNetworkClientResult{Error: &ApiError{Message: "Client does not exist."}})
		}
		self.removed = append(self.removed, *args.ClientId)
		drop := func(clients []*NetworkClientInfo) []*NetworkClientInfo {
			kept := []*NetworkClientInfo{}
			for _, client := range clients {
				if *client.ClientId != *args.ClientId {
					kept = append(kept, client)
				}
			}
			return kept
		}
		self.devices = drop(self.devices)
		self.proxies = drop(self.proxies)
		return json.Marshal(&RemoveNetworkClientResult{})
	})
}

func (self *testingManageDevicesServer) sentCredentials() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.credentials...)
}

func (self *testingManageDevicesServer) removedClientIds() []Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]Id{}, self.removed...)
}

func (self *testingManageDevicesServer) getCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.gets
}

// testingManageDevicesDevice stands in for the device a controller is opened
// from: its client, its api, and its client limit hold.
type testingManageDevicesDevice struct {
	api      *Api
	clientId *Id

	stateLock sync.Mutex
	status    *ClientLimitStatus
	// what each RetryClientLimit answers, in order; true once they run out
	retryAnswers []bool
	retries      chan struct{}
}

func (self *testingManageDevicesDevice) GetClientId() *Id { return self.clientId }
func (self *testingManageDevicesDevice) GetApi() *Api     { return self.api }

func (self *testingManageDevicesDevice) GetClientLimitStatus() *ClientLimitStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cloneClientLimitStatus(self.status)
}

func (self *testingManageDevicesDevice) RetryClientLimit() bool {
	answer := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.retryAnswers) == 0 {
			return true
		}
		answer := self.retryAnswers[0]
		self.retryAnswers = self.retryAnswers[1:]
		return answer
	}()
	self.retries <- struct{}{}
	return answer
}

// testingManageDevicesListener records every snapshot.
type testingManageDevicesListener struct {
	snapshots chan *ManageDevicesSnapshot
}

func newTestingManageDevicesListener() *testingManageDevicesListener {
	return &testingManageDevicesListener{snapshots: make(chan *ManageDevicesSnapshot, 64)}
}

func (self *testingManageDevicesListener) ManageDevicesChanged(snapshot *ManageDevicesSnapshot) {
	self.snapshots <- snapshot
}

// until waits for a snapshot that satisfies done, or fails.
func (self *testingManageDevicesListener) until(t *testing.T, message string, done func(*ManageDevicesSnapshot) bool) *ManageDevicesSnapshot {
	t.Helper()
	for {
		select {
		case snapshot := <-self.snapshots:
			if done(snapshot) {
				return snapshot
			}
		case <-time.After(30 * time.Second):
			t.Fatal(message)
			return nil
		}
	}
}

type testingManageDevicesSignInRetry struct {
	retries chan struct{}
}

func (self *testingManageDevicesSignInRetry) RetrySignIn() {
	self.retries <- struct{}{}
}

func newTestingManageDevicesApi(t *testing.T, byJwt string) *Api {
	t.Helper()
	api := newApi(t.Context(), nil, "https://manage-devices.example")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.CloseAndWait(ctx); err != nil {
			t.Errorf("close manage devices API: %v", err)
		}
	})
	if byJwt != "" {
		api.SetByJwt(byJwt)
	}
	return api
}

func testingManagedClient(clientId *Id, name string, spec string, authMillis int64, connected bool) *NetworkClientInfo {
	connections := NewNetworkClientConnectionList()
	if connected {
		connections.Add(&NetworkClientConnection{ClientId: clientId, ConnectionId: NewId()})
	}
	return &NetworkClientInfo{
		ClientId:    clientId,
		DeviceName:  name,
		DeviceSpec:  spec,
		AuthTime:    NewTimeUnixMilli(authMillis),
		Connections: connections,
	}
}

func testingManagedClientIds(devices *ManagedDeviceList) []Id {
	clientIds := []Id{}
	for i := 0; i < devices.Len(); i += 1 {
		clientIds = append(clientIds, *devices.Get(i).ClientId)
	}
	return clientIds
}

// The list holds the network's devices and its hosted proxies, this device
// first, then the connected ones, then the latest seen, with no proxy
// credentials. Both reads carry the network credential the api keeps.
func TestManageDevicesListsDevicesAndProxies(t *testing.T) {
	this, laptop, tablet, proxy := NewId(), NewId(), NewId(), NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{
			testingManagedClient(tablet, "", "17.0 iPad", 1_000, false),
			testingManagedClient(laptop, "Laptop", "15.5 MacBook Pro", 2_000, true),
			testingManagedClient(this, "Phone", "18.5 iPhone 16", 3_000, false),
		},
	}
	proxyClient := testingManagedClient(proxy, "", "", 500, true)
	proxyClient.DeviceDescription = "proxy"
	proxyClient.ProxyClient = &ProxyClient{ClientId: proxy, HttpProxyUrl: "https://proxy.example"}
	server.proxies = []*NetworkClientInfo{proxyClient}

	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	device := &testingManageDevicesDevice{api: api, clientId: this, retries: make(chan struct{}, 8)}
	vc := newManageDevicesViewControllerWithDevice(t.Context(), device, defaultManageDevicesSettings())
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()

	connect.AssertEqual(t, vc.GetCanManage(), true)
	vc.Start()
	snapshot := listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, snapshot.ErrorId, ManageDevicesErrorNone)
	connect.AssertEqual(t, testingManagedClientIds(snapshot.Devices), []Id{*this, *laptop, *proxy, *tablet})

	first := snapshot.Devices.Get(0)
	connect.AssertEqual(t, first.ThisDevice, true)
	connect.AssertEqual(t, first.Kind, ManagedDeviceKindDevice)
	connect.AssertEqual(t, first.Name, "Phone")
	connect.AssertEqual(t, first.Spec, "18.5 iPhone 16")
	connect.AssertEqual(t, first.LastSeenTime, int64(3_000))
	connect.AssertEqual(t, snapshot.Devices.Get(1).Connected, true)
	proxyDevice := snapshot.Devices.Get(2)
	connect.AssertEqual(t, proxyDevice.Kind, ManagedDeviceKindProxy)
	connect.AssertEqual(t, proxyDevice.Name, "proxy")
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "proxy.example") {
		t.Fatalf("the snapshot carries proxy credentials: %s", encoded)
	}

	for _, credential := range server.sentCredentials() {
		connect.AssertEqual(t, credential, testingManageDevicesNetworkJwt)
	}
}

// A device whose api holds only its client token can't manage the network's
// devices: nothing is sent, and the controller asks for a new sign-in.
func TestManageDevicesWithoutTheNetworkCredentialSendsNothing(t *testing.T) {
	clientToken, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"network_id": NewId().String(),
		"client_id":  NewId().String(),
	}).SignedString([]byte("synthetic-manage-devices-key"))
	if err != nil {
		t.Fatal(err)
	}
	server := &testingManageDevicesServer{}
	api := newTestingManageDevicesApi(t, clientToken)
	server.install(t, api)
	device := &testingManageDevicesDevice{api: api, clientId: NewId(), retries: make(chan struct{}, 8)}
	vc := newManageDevicesViewControllerWithDevice(t.Context(), device, defaultManageDevicesSettings())
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()

	connect.AssertEqual(t, vc.GetCanManage(), false)
	vc.Start()
	snapshot := listener.until(t, "no snapshot", func(s *ManageDevicesSnapshot) bool { return true })
	connect.AssertEqual(t, snapshot.ErrorId, ManageDevicesErrorSignInRequired)
	connect.AssertEqual(t, snapshot.Loading, false)
	vc.RemoveDevice(NewId())
	listener.until(t, "no snapshot", func(s *ManageDevicesSnapshot) bool { return true })
	connect.AssertEqual(t, len(server.sentCredentials()), 0)
}

// Removing a device frees its slot, and the controller ends this device's
// hold at once so it dials again, then reloads the list. The removal carries
// the network credential. Before, a user who freed a slot waited out the hold.
func TestManageDevicesRemoveRetriesTheHeldDevice(t *testing.T) {
	this, laptop := NewId(), NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{
			testingManagedClient(this, "Phone", "", 2_000, false),
			testingManagedClient(laptop, "Laptop", "", 1_000, true),
		},
	}
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	device := &testingManageDevicesDevice{
		api:      api,
		clientId: this,
		status:   &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1, Prompt: ClientLimitPromptUpgrade},
		retries:  make(chan struct{}, 8),
	}
	vc := newManageDevicesViewControllerWithDevice(t.Context(), device, defaultManageDevicesSettings())
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()
	vc.Start()
	listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	gets := server.getCount()

	vc.RemoveDevice(laptop)
	select {
	case <-device.retries:
	case <-time.After(30 * time.Second):
		t.Fatal("the removal did not retry the held device")
	}
	snapshot := listener.until(t, "the list never reloaded", func(s *ManageDevicesSnapshot) bool {
		return 0 < s.AddRetryCount && !s.Loading && s.RemovingClientId == nil && gets < server.getCount()
	})
	connect.AssertEqual(t, server.removedClientIds(), []Id{*laptop})
	connect.AssertEqual(t, testingManagedClientIds(snapshot.Devices), []Id{*this})
	connect.AssertEqual(t, snapshot.ErrorId, ManageDevicesErrorNone)
	for _, credential := range server.sentCredentials() {
		connect.AssertEqual(t, credential, testingManageDevicesNetworkJwt)
	}

	// this device is never removed here
	vc.RemoveDevice(this)
	connect.AssertEqual(t, server.removedClientIds(), []Id{*laptop})
}

// A removal inside the hold's retry interval (a second removal in a row) is
// retried once more when the interval has passed, so the user isn't left in
// the hold. A device that is not held is not retried.
func TestManageDevicesRemoveRetriesAgainAfterTheInterval(t *testing.T) {
	this, laptop, tablet := NewId(), NewId(), NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{
			testingManagedClient(laptop, "Laptop", "", 1_000, true),
			testingManagedClient(tablet, "Tablet", "", 1_000, true),
		},
	}
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	device := &testingManageDevicesDevice{
		api:          api,
		clientId:     this,
		status:       &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1, Prompt: ClientLimitPromptRemoveClient},
		retryAnswers: []bool{false},
		retries:      make(chan struct{}, 8),
	}
	interval := make(chan time.Time)
	settings := defaultManageDevicesSettings()
	settings.after = func(time.Duration) <-chan time.Time { return interval }
	vc := newManageDevicesViewControllerWithDevice(t.Context(), device, settings)
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()
	vc.Start()
	listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })

	vc.RemoveDevice(laptop)
	<-device.retries
	// the interval refused it: nothing more until the interval passes
	select {
	case <-device.retries:
		t.Fatal("the controller retried again inside the interval")
	default:
	}
	interval <- time.Now()
	select {
	case <-device.retries:
	case <-time.After(30 * time.Second):
		t.Fatal("the controller did not retry once the interval passed")
	}

	// the hold ended: a later removal retries nothing
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		device.status = noneClientLimitStatus()
	}()
	// each removal reloads both lists after it retries: two reads past the
	// load's two
	listener.until(t, "the first removal never reloaded", func(s *ManageDevicesSnapshot) bool {
		return s.RemovingClientId == nil && !s.Loading && 4 <= server.getCount()
	})
	vc.RemoveDevice(tablet)
	listener.until(t, "the second removal never reloaded", func(s *ManageDevicesSnapshot) bool {
		return s.RemovingClientId == nil && s.Devices.Len() == 0 && !s.Loading && 6 <= server.getCount()
	})
	select {
	case <-device.retries:
		t.Fatal("a removal retried a device that was not held")
	default:
	}
}

// The device's floor doubles with each retry in a row, so after a few
// removals in a row one retry interval is not enough: the controller retries
// again each interval while the hold lasts, and stops once one ends the hold,
// or the hold ends by itself.
func TestManageDevicesRemoveRetriesUntilTheFloorPasses(t *testing.T) {
	this, laptop := NewId(), NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{testingManagedClient(laptop, "Laptop", "", 1_000, true)},
	}
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)
	device := &testingManageDevicesDevice{
		api:      api,
		clientId: this,
		status:   &ClientLimitStatus{Status: ClientLimitStatusExceeded, RetryTime: 1, Prompt: ClientLimitPromptUpgrade},
		// the floor refuses the first three
		retryAnswers: []bool{false, false, false},
		retries:      make(chan struct{}, 8),
	}
	interval := make(chan time.Time)
	settings := defaultManageDevicesSettings()
	settings.after = func(time.Duration) <-chan time.Time { return interval }
	vc := newManageDevicesViewControllerWithDevice(t.Context(), device, settings)
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()
	vc.Start()
	listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })

	vc.RemoveDevice(laptop)
	<-device.retries
	for i := 0; i < 3; i += 1 {
		interval <- time.Now()
		select {
		case <-device.retries:
		case <-time.After(30 * time.Second):
			t.Fatalf("no retry after interval %d", i+1)
		}
	}
	// the third interval's retry ended the hold: no more intervals
	select {
	case interval <- time.Now():
		t.Fatal("the controller kept waiting after a retry ended the hold")
	case <-time.After(100 * time.Millisecond):
	}

	// a hold that ends by itself also stops the retries
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		device.retryAnswers = []bool{false}
	}()
	server.stateLock.Lock()
	server.devices = []*NetworkClientInfo{testingManagedClient(laptop, "Laptop", "", 1_000, true)}
	server.stateLock.Unlock()
	vc.Refresh()
	listener.until(t, "the list never reloaded", func(s *ManageDevicesSnapshot) bool {
		return !s.Loading && s.Devices.Len() == 1
	})
	vc.RemoveDevice(laptop)
	<-device.retries
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		device.status = noneClientLimitStatus()
	}()
	interval <- time.Now()
	select {
	case <-device.retries:
		t.Fatal("the controller retried a hold that had ended")
	case interval <- time.Now():
		t.Fatal("the controller kept waiting after the hold ended")
	case <-time.After(100 * time.Millisecond):
	}
}

// A sign-in the limit refused lists the devices with the sign-in's own
// network token, and a removal runs the sign-in again.
func TestManageDevicesSignInRemoveRetriesTheSignIn(t *testing.T) {
	laptop := NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{testingManagedClient(laptop, "Laptop", "", 1_000, true)},
	}
	// a sign-in in progress: the api holds no credential of its own yet
	api := newTestingManageDevicesApi(t, "")
	server.install(t, api)
	retry := &testingManageDevicesSignInRetry{retries: make(chan struct{}, 8)}
	vc := NewSignInManageDevicesViewController(api, "synthetic-sign-in-network-token", retry)
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()

	vc.Start()
	snapshot := listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, snapshot.Devices.Len(), 1)
	connect.AssertEqual(t, snapshot.Devices.Get(0).ThisDevice, false)

	vc.RemoveDevice(laptop)
	select {
	case <-retry.retries:
	case <-time.After(30 * time.Second):
		t.Fatal("the removal did not retry the sign-in")
	}
	for _, credential := range server.sentCredentials() {
		connect.AssertEqual(t, credential, "synthetic-sign-in-network-token")
	}
}

// A signed-in app with no device (a desktop app while disconnected) opens the
// list on its api alone: the api's network credential on every request, no
// device marked as this one, and a removal that only reloads the list. A
// sign-in controller with a nil retry does the same with the token it is given.
func TestManageDevicesOnTheApiAlone(t *testing.T) {
	laptop, tablet := NewId(), NewId()
	server := &testingManageDevicesServer{
		devices: []*NetworkClientInfo{
			testingManagedClient(laptop, "Laptop", "", 2_000, true),
			testingManagedClient(tablet, "Tablet", "", 1_000, false),
		},
	}
	api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
	server.install(t, api)

	vc := api.OpenManageDevicesViewController()
	defer vc.Close()
	listener := newTestingManageDevicesListener()
	defer vc.AddManageDevicesListener(listener).Close()
	connect.AssertEqual(t, vc.GetCanManage(), true)
	vc.Start()
	snapshot := listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	connect.AssertEqual(t, testingManagedClientIds(snapshot.Devices), []Id{*laptop, *tablet})
	for i := 0; i < snapshot.Devices.Len(); i += 1 {
		connect.AssertEqual(t, snapshot.Devices.Get(i).ThisDevice, false)
	}

	vc.RemoveDevice(laptop)
	snapshot = listener.until(t, "the removal never reloaded", func(s *ManageDevicesSnapshot) bool {
		return s.RemovingClientId == nil && !s.Loading && 4 <= server.getCount()
	})
	connect.AssertEqual(t, server.removedClientIds(), []Id{*laptop})
	connect.AssertEqual(t, testingManagedClientIds(snapshot.Devices), []Id{*tablet})
	// nothing to add again
	connect.AssertEqual(t, snapshot.AddRetryCount, 0)
	for _, credential := range server.sentCredentials() {
		connect.AssertEqual(t, credential, testingManageDevicesNetworkJwt)
	}

	// the sign-in form with no retry
	signIn := NewSignInManageDevicesViewController(api, "synthetic-stored-sign-in-token", nil)
	defer signIn.Close()
	signInListener := newTestingManageDevicesListener()
	defer signIn.AddManageDevicesListener(signInListener).Close()
	signIn.Start()
	signInListener.until(t, "the sign-in list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	signIn.RemoveDevice(tablet)
	snapshot = signInListener.until(t, "the sign-in removal never reloaded", func(s *ManageDevicesSnapshot) bool {
		return s.RemovingClientId == nil && !s.Loading && s.Devices.Len() == 0
	})
	connect.AssertEqual(t, snapshot.AddRetryCount, 0)
	connect.AssertEqual(t, server.removedClientIds(), []Id{*laptop, *tablet})

	// an api without a network credential sends nothing
	bare := newTestingManageDevicesApi(t, "")
	bareServer := &testingManageDevicesServer{}
	bareServer.install(t, bare)
	bareVc := bare.OpenManageDevicesViewController()
	defer bareVc.Close()
	connect.AssertEqual(t, bareVc.GetCanManage(), false)
	bareVc.Start()
	connect.AssertEqual(t, bareVc.GetSnapshot().ErrorId, ManageDevicesErrorSignInRequired)
	connect.AssertEqual(t, len(bareServer.sentCredentials()), 0)
}

// A refused credential asks for a new sign-in, any other failure is retryable,
// and a removal of a client the server no longer has reads as removed.
func TestManageDevicesErrors(t *testing.T) {
	laptop := NewId()
	newController := func(server *testingManageDevicesServer) (*ManageDevicesViewController, *testingManageDevicesListener) {
		api := newTestingManageDevicesApi(t, testingManageDevicesNetworkJwt)
		server.install(t, api)
		device := &testingManageDevicesDevice{api: api, clientId: NewId(), retries: make(chan struct{}, 8)}
		vc := newManageDevicesViewControllerWithDevice(t.Context(), device, defaultManageDevicesSettings())
		t.Cleanup(vc.Close)
		listener := newTestingManageDevicesListener()
		vc.AddManageDevicesListener(listener)
		return vc, listener
	}
	settled := func(s *ManageDevicesSnapshot) bool { return !s.Loading && s.RemovingClientId == nil && s.ErrorId != "" }

	for _, c := range []struct {
		status  int
		errorId string
	}{
		{status: http.StatusUnauthorized, errorId: ManageDevicesErrorSignInRequired},
		{status: http.StatusForbidden, errorId: ManageDevicesErrorSignInRequired},
		{status: http.StatusInternalServerError, errorId: ManageDevicesErrorLoadFailed},
	} {
		vc, listener := newController(&testingManageDevicesServer{listStatus: c.status})
		vc.Start()
		snapshot := listener.until(t, "the load never failed", settled)
		connect.AssertEqual(t, snapshot.ErrorId, c.errorId)
	}

	for _, c := range []struct {
		status  int
		errorId string
	}{
		{status: http.StatusForbidden, errorId: ManageDevicesErrorSignInRequired},
		{status: http.StatusBadGateway, errorId: ManageDevicesErrorRemoveFailed},
	} {
		server := &testingManageDevicesServer{
			devices:      []*NetworkClientInfo{testingManagedClient(laptop, "Laptop", "", 1_000, true)},
			removeStatus: c.status,
		}
		vc, listener := newController(server)
		vc.Start()
		listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
		vc.RemoveDevice(laptop)
		snapshot := listener.until(t, "the removal never failed", settled)
		connect.AssertEqual(t, snapshot.ErrorId, c.errorId)
		// still listed
		connect.AssertEqual(t, snapshot.Devices.Len(), 1)
	}

	server := &testingManageDevicesServer{
		devices:       []*NetworkClientInfo{testingManagedClient(laptop, "Laptop", "", 1_000, true)},
		removeMissing: true,
	}
	vc, listener := newController(server)
	vc.Start()
	listener.until(t, "the list never loaded", func(s *ManageDevicesSnapshot) bool { return s.Loaded })
	vc.RemoveDevice(laptop)
	snapshot := listener.until(t, "the removal never settled", func(s *ManageDevicesSnapshot) bool {
		return s.RemovingClientId == nil && s.Devices.Len() == 0
	})
	connect.AssertEqual(t, snapshot.ErrorId, ManageDevicesErrorNone)

	// the mapping itself
	connect.AssertEqual(t, manageDevicesErrorFor(ErrNetworkCredentialRequired, ManageDevicesErrorLoadFailed), ManageDevicesErrorSignInRequired)
	connect.AssertEqual(t, manageDevicesErrorFor(errors.New("synthetic transport failure"), ManageDevicesErrorRemoveFailed), ManageDevicesErrorRemoveFailed)
}

// A device opened from the view controller manager closes through it.
func TestCloseConcreteManageDevicesViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)
	device.CloseManageDevicesViewController(device.OpenManageDevicesViewController())
	requireNoOwnedViewControllers(t, device)
}
