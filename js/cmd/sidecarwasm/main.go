//go:build js && wasm && sidecar_http

// The page Worker's main, sidecar.wasm (§18.10, D-SDK-19): the sidecar on the browser's leg, which the key origin
// https://sidecar.ur.io/v1/ runs as a Worker for ur.io's pages (§18.8).
//
// It links the sidecar root and protocol and this module's sidecarworker, which names no sdk or connect package: a
// connect-only js/wasm main is already 55.0 MB unstripped, since Go links every imported package's init, connect's IP
// stack included. The module requires both, and sdk/dependency_boundary_test.go's leg for this main fails on any
// connect, gVisor, sdk, net/rpc or template package in its closure.
//
// Build it unstripped with make build_sidecar_wasm:
//
//	CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -tags sidecar_http -trimpath -o wasm/sidecar.wasm ./cmd/sidecarwasm
//
// The main is a thin shell (SDK-M4-04): it opens one network's state, IndexedDB's store and the WebCrypto key, an
// instance on them and this leg's sidecar.Sidecar, with no components and no TokenSource, so §5.6's
// sidecarTokenManager refreshes over POST /sidecar/bind, GET /auth/refresh with the ClientKey's DPoP proof and the
// token exchange for evs and gkm (§18.10's leg row). It runs no HeadCheck and raises no unwitnessed
// (plan-SDK-small-readings #8). Everything a page asks goes to sidecarworker, whose seams this file fills: the
// Sidecar, an EvsClient of this leg built at the first read, and the Sidecar's record reader. It never listens.
//
// What this leg cannot read yet fails closed: no GkmClient exists on any leg (KEYS-M4-01 layers mls_ and gkm_ on this
// Worker), so evs reads answer unavailable, and with no config state verified here no statement judges as verified;
// the record reader has no streams until RECORD-M3-12 binds them, so record_view answers unavailable too.
//
// worker.js opens the network only while it holds the network's Web Lock (SDK-M4-05), so one Worker a network ever
// loads its keychain, token and MLS state.

package main

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// The global the loader reads once the program has started: the release version, empty in a development build, and
// the Worker's calls (bridge_js.go). Set before main first blocks, so it is there when the loader's go.run returns.
const sidecarWasmGlobalName = "urnetworkSidecar"

// The evs component's sealed directory, as every leg names it (§5.9).
const pageEvsDirectoryName = "evs"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publishPageWorker(newPageWorker(ctx))
	// the Worker lives as long as its page: nothing here returns, so later callbacks find Go running
	select {}
}

// What open takes from worker.js: the network this Worker serves, and the environment's service urls, §18.9's table's
// where absent. worker.js reads them from the frame's staged config, never from a page.
type pageOpenConfig struct {
	NetworkId string           `json:"network_id"`
	Urls      *pageServiceUrls `json:"urls,omitempty"`
}

// An environment's service urls.
type pageServiceUrls struct {
	Api      string `json:"api"`
	Auth     string `json:"auth"`
	Evs      string `json:"evs"`
	Gkm      string `json:"gkm"`
	Sockets  string `json:"sockets"`
	Dns      string `json:"dns"`
	Platform string `json:"platform"`
}

// What open answers: the opened network's state, ids as strings.
type pageStatusView struct {
	NetworkId string `json:"network_id"`
	SidecarId string `json:"sidecar_id,omitempty"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// The page Worker: at most one opened network. Safe for concurrent use.
type pageWorker struct {
	ctx context.Context
	// the key origin's database, indexedDbName; a test's own
	databaseName string

	stateLock sync.Mutex
	opened    *pageNetwork
}

// A page Worker that opens nothing until worker.js asks.
func newPageWorker(ctx context.Context) *pageWorker {
	return &pageWorker{ctx: ctx, databaseName: indexedDbName}
}

// One opened network: its instance, its Sidecar and the Worker over them.
type pageNetwork struct {
	networkId sidecar.Id
	settings  *sidecar.SidecarSettings
	instance  *sidecar.SidecarInstance
	local     *sidecar.Sidecar
	worker    *sidecarworker.Worker

	// built at the first read, once a network is enrolled
	evsLock sync.Mutex
	evs     *sidecar.EvsClient
}

// The opened network, or nil.
func (self *pageWorker) current() *pageNetwork {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.opened
}

// Opens config's network, or answers the one already open: IndexedDB, the WebCrypto key, an instance on them and this
// leg's Sidecar, unlocked by the key store's wrap as an app's is at start. A network the instance holds no record of
// opens unenrolled. Another network than the one open is refused, since a Worker serves one (§18.8's instance row).
func (self *pageWorker) open(config *pageOpenConfig) (*pageStatusView, error) {
	networkId, err := sidecar.ParseId(config.NetworkId)
	if err != nil {
		return nil, errors.New("open needs the network's id")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.opened != nil {
		if self.opened.networkId != networkId {
			return nil, errors.New("this Worker serves another network")
		}
		return self.opened.status(), nil
	}
	settings := sidecar.DefaultSidecarSettings()
	applyPageWorkerSettings(settings)
	if err := applyPageServiceUrls(settings, config.Urls); err != nil {
		return nil, err
	}
	database, err := openIndexedDb(self.databaseName)
	if err != nil {
		return nil, err
	}
	keyStore := newWebCryptoKeyStore(database)
	instance, err := newPageInstance(self.ctx, database, keyStore)
	if err != nil {
		return nil, err
	}
	var hostNetworkId *sidecar.Id
	for _, record := range instance.Networks() {
		if record.NetworkId == networkId {
			hostNetworkId = &networkId
		}
	}
	local, err := sidecar.NewSidecar(self.ctx, &pageHost{instance: instance, networkId: hostNetworkId, keyStore: keyStore}, settings)
	if err != nil {
		instance.Close()
		return nil, err
	}
	network := &pageNetwork{
		networkId: networkId,
		settings:  settings,
		instance:  instance,
		local:     local,
	}
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar: local,
		Repo:    network.repo,
		Records: network.records,
		Yield:   yieldToEventLoop,
	})
	if err != nil {
		local.Close()
		instance.Close()
		return nil, err
	}
	network.worker = worker
	// the key store's wrap of U: a store that is locked or failed creates nothing and leaves the Sidecar Locked
	local.Unlock(nil)
	self.opened = network
	return network.status(), nil
}

// Answers requestJson with its reply's JSON: the Worker's, or unavailable before open.
func (self *pageWorker) handle(ctx context.Context, requestJson []byte) []byte {
	network := self.current()
	if network == nil {
		return pageNotOpenReply(ctx, requestJson)
	}
	return network.worker.HandleJson(ctx, requestJson)
}

// Ends the opened network's Sidecar and closes its instance once the Sidecar's run loop has ended.
func (self *pageWorker) close(ctx context.Context) error {
	network := func() *pageNetwork {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		network := self.opened
		self.opened = nil
		return network
	}()
	if network == nil {
		return nil
	}
	err := network.local.CloseAndWait(ctx)
	network.instance.Close()
	return err
}

// The reply to a request before open, the same checks as an open Worker's: an operation that reaches no seam answers
// as it would, and one that would read answers unavailable.
func pageNotOpenReply(ctx context.Context, requestJson []byte) []byte {
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar: pageUnopenedSidecar{},
		Repo: func(sidecar.Id) (sidecarworker.WorkerRepo, error) {
			return nil, sidecar.ErrSidecarClosed
		},
		Records: func() (sidecarworker.WorkerRecords, error) {
			return nil, sidecar.ErrSidecarClosed
		},
	})
	if err != nil {
		return []byte(`{"id":"","ok":false,"code":"failed"}`)
	}
	return worker.HandleJson(ctx, requestJson)
}

// The sidecar of a Worker not yet open: nothing enrolled, and a sign-out that refuses, since nothing is open to sign
// out of.
type pageUnopenedSidecar struct{}

// None.
func (self pageUnopenedSidecar) Identity() *sidecar.SidecarIdentity {
	return nil
}

// Locked, as a Sidecar is before its instance opens.
func (self pageUnopenedSidecar) Status() sidecar.SidecarStatus {
	return sidecar.SidecarStatus{State: sidecar.SidecarStateLocked}
}

// Refuses.
func (self pageUnopenedSidecar) Logout(ctx context.Context) error {
	return sidecar.ErrSidecarClosed
}

// The network's state now.
func (self *pageNetwork) status() *pageStatusView {
	status := self.local.Status()
	view := &pageStatusView{
		NetworkId: self.networkId.String(),
		State:     string(status.State),
		Reason:    string(status.Reason),
	}
	if identity := self.local.Identity(); identity != nil {
		view.SidecarId = identity.SidecarId.String()
	}
	return view
}

// repoId's verified reader, on the network's EvsClient.
func (self *pageNetwork) repo(repoId sidecar.Id) (sidecarworker.WorkerRepo, error) {
	client, err := self.evsClient()
	if err != nil {
		return nil, err
	}
	return client.Repo(repoId), nil
}

// The network's EvsClient, built once the network is enrolled: this leg's fetch, the Sidecar's tokens, signer and
// identity, and the evs directory of its sealed store. Its GkmClient refuses every key and it has no config state, so
// every read fails closed until KEYS-M4-01 and this leg's config verification fill them.
func (self *pageNetwork) evsClient() (*sidecar.EvsClient, error) {
	self.evsLock.Lock()
	defer self.evsLock.Unlock()
	if self.evs != nil {
		return self.evs, nil
	}
	networkId := self.local.NetworkId()
	if networkId == nil {
		return nil, sidecar.ErrSidecarNoIdentity
	}
	store, err := self.instance.Store(networkId).Directory(pageEvsDirectoryName, pageEvsDirectoryName)
	if err != nil {
		return nil, err
	}
	client, err := sidecar.NewEvsClient(&sidecar.EvsClientArgs{
		NetworkId:    *networkId,
		EvsUrl:       self.settings.EvsUrl,
		Transport:    http.DefaultTransport,
		ServiceToken: self.local.ServiceToken,
		Sign:         self.local.Sign,
		Identity:     self.local.Identity,
		Gkm:          pageGkmUnavailable{},
		Store:        store,
		Settings:     self.settings.Evs,
	})
	if err != nil {
		return nil, err
	}
	self.evs = client
	return client, nil
}

// The Sidecar's record reader (RECORD.md section 5.5), once the network is enrolled.
func (self *pageNetwork) records() (sidecarworker.WorkerRecords, error) {
	if self.local.NetworkId() == nil {
		return nil, sidecar.ErrSidecarNoIdentity
	}
	record, err := self.local.Record()
	if err != nil {
		return nil, err
	}
	return record.Reader(), nil
}

// settings' service urls: urls where given, whole, else §18.9's table's.
func applyPageServiceUrls(settings *sidecar.SidecarSettings, urls *pageServiceUrls) error {
	if urls == nil {
		list, err := sidecar.SidecarHostListV1()
		if err != nil {
			return err
		}
		return list.ApplyServiceUrls(settings)
	}
	for _, serviceUrl := range []string{urls.Api, urls.Auth, urls.Evs, urls.Gkm, urls.Sockets, urls.Dns} {
		if serviceUrl == "" {
			return errors.New("an environment's urls name every service")
		}
	}
	settings.ApiUrl = urls.Api
	settings.AuthUrl = urls.Auth
	settings.EvsUrl = urls.Evs
	settings.GkmUrl = urls.Gkm
	settings.SocketsUrl = urls.Sockets
	settings.DnsUrl = urls.Dns
	settings.PlatformUrl = urls.Platform
	return nil
}

// The GkmClient this leg holds until KEYS-M4-01 builds the page Worker's: every call refuses with ErrGkmUnavailable
// and a new state changes nothing, so no read is ever taken as keyed, as the root's own refusing client.
type pageGkmUnavailable struct{}

// Refuses.
func (self pageGkmUnavailable) CurrentRepoKey(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Refuses.
func (self pageGkmUnavailable) RepoKey(ctx context.Context, repoId sidecar.Id, generation uint64) (*sidecar.EvsRepoKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Refuses.
func (self pageGkmUnavailable) EnsureWritable(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Refuses.
func (self pageGkmUnavailable) DropKey(ctx context.Context, repoId sidecar.Id) (*sidecar.GkmDropKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Refuses: a read_only Worker creates no group.
func (self pageGkmUnavailable) CreateGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Refuses: a read_only Worker rotates nothing.
func (self pageGkmUnavailable) RotateRepo(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, sidecar.ErrGkmUnavailable
}

// Changes nothing.
func (self pageGkmUnavailable) AuthAdvanced(state sidecar.GkmAuthView) {
}

// Refuses.
func (self pageGkmUnavailable) ForgetRepo(ctx context.Context, repoId sidecar.Id) error {
	return sidecar.ErrGkmUnavailable
}
