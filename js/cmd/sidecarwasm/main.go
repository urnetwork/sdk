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
// The main is a thin shell (SDK-M4-04): it opens one network's state, from that network's own IndexedDB database,
// urnetwork-sidecar-v1-<network_id> (store_indexeddb.go), its store and its WebCrypto root key, an instance on them
// and this leg's sidecar.Sidecar, with no components and no TokenSource, so §5.6's
// sidecarTokenManager refreshes over POST /sidecar/bind, GET /auth/refresh with the ClientKey's DPoP proof and the
// token exchange for evs and gkm (§18.10's leg row). It runs no HeadCheck and raises no unwitnessed
// (plan-SDK-small-readings #8). Everything a page asks goes to sidecarworker, whose seams this file fills: the
// Sidecar, an EvsClient of this leg built at the first read, and the Sidecar's record reader. It never listens.
//
// The same member replica as the native SDK checks config bases, group keys and EVS pushes. Views
// become available once this browser's own read-only admission and authorization are published.
//
// worker.js opens the network only while it holds the network's Web Lock, urnetwork-sidecar-<network_id> (SDK-M4-05),
// so one Worker a network ever loads its keychain, token and MLS state, and, each network being its own database and
// instance, that lock covers everything the Worker writes (owner decision 217). The lock is named by the id worker.js
// was given, so open takes a network id only in its canonical text: a Worker under a lock that names the id another
// way could open the same database beside its holder.

package main

import (
	"context"
	"errors"
	"sync"

	"github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// The global the loader reads once the program has started: the release version, empty in a development build, and
// the Worker's calls (bridge_js.go). Set before main first blocks, so it is there when the loader's go.run returns.
const sidecarWasmGlobalName = "urnetworkSidecar"

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
	// A quick burner (§18.8, SDK-M8-14): memory only, on newBurnerInstance, with no Web Lock and no IndexedDB, its
	// close capability handed to the frame's document alone (keystore_memory.go).
	Burner bool `json:"burner,omitempty"`
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
	// what a network's database is named under, before its id: indexedDbNamePrefix, or a test's own
	databaseNamePrefix string

	stateLock sync.Mutex
	opened    *pageNetwork
}

// A page Worker that opens nothing until worker.js asks.
func newPageWorker(ctx context.Context) *pageWorker {
	return &pageWorker{ctx: ctx, databaseNamePrefix: indexedDbNamePrefix}
}

// One opened network: its instance, its Sidecar and the Worker over them.
type pageNetwork struct {
	ctx       context.Context
	ephemeral bool
	networkId sidecar.Id
	settings  *sidecar.SidecarSettings
	instance  *sidecar.SidecarInstance
	local     *sidecar.Sidecar
	worker    *sidecarworker.Worker
	// a quick burner's, which answers in place of worker; nil for a page Worker
	burner *sidecarworker.QuickBurner

	replicaLock sync.Mutex
	replica     *sidecar.MemberReplica
	closed      bool
	cardsConfig *sidecar.ConfigClient
	cardsEvs    *sidecar.EvsClient
}

// The opened network, or nil.
func (self *pageWorker) current() *pageNetwork {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.opened
}

// Opens config's network, or answers the one already open: the network's own IndexedDB database, its WebCrypto root
// key, an instance on them and this leg's Sidecar, unlocked by the key store's wrap as an app's is at start. A network
// the instance holds no record of opens unenrolled. Another network than the one open is refused, since a Worker
// serves one (§18.8's instance row). The id must be a network's, in its canonical text, the spelling worker.js names
// the network's lock by, and the database's name is built from the parsed id, never from the string given.
func (self *pageWorker) open(config *pageOpenConfig) (*pageStatusView, error) {
	networkId, err := sidecar.ParseId(config.NetworkId)
	if err != nil || networkId == (sidecar.Id{}) || config.NetworkId != networkId.String() {
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
	if err := applyPageServiceUrls(settings, config.Urls); err != nil {
		return nil, err
	}
	// §18.4's browsers row: the page Worker keeps 128 MiB of search cache in its own store, the burner its buckets in
	// memory (SDK-M10-04)
	if config.Burner {
		sidecarworker.DefaultQuickBurnerSearchSettings().Apply(settings)
	} else {
		sidecarworker.DefaultPageSearchSettings().Apply(settings)
	}
	if config.Burner {
		network, err := self.openBurner(networkId, settings)
		if err != nil {
			return nil, err
		}
		self.opened = network
		return network.status(), nil
	}
	database, err := openIndexedDb(indexedDbNetworkName(self.databaseNamePrefix, networkId))
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
		ctx:       self.ctx,
		networkId: networkId,
		settings:  settings,
		instance:  instance,
		local:     local,
	}
	workerSettings := sidecarworker.DefaultWorkerSettings()
	workerSettings.DirectoryViewCoalesce = settings.Directory.ViewCoalesce
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar:        local,
		Enroll:         network.enroll,
		Admission:      network.admission,
		Repo:           network.repo,
		Records:        network.records,
		RecordReadable: network.recordReadable,
		Directory:      network.directory,
		Config:         network.config,
		Search:         network.search,
		Drops:          network.drops,
		Yield:          yieldToEventLoop,
		Settings:       workerSettings,
	})
	if err != nil {
		local.Close()
		instance.Close()
		return nil, err
	}
	network.worker = worker
	// the key store's wrap of U: a store that is locked or failed creates nothing and leaves the Sidecar Locked
	network.unlock()
	self.opened = network
	return network.status(), nil
}

// Answers requestJson with its reply's JSON: the Worker's, or unavailable before open.
func (self *pageWorker) handle(ctx context.Context, requestJson []byte) []byte {
	network := self.current()
	if network == nil {
		return pageNotOpenReply(ctx, requestJson)
	}
	if network.burner != nil {
		return network.burner.HandleJson(ctx, requestJson)
	}
	return network.worker.HandleJson(ctx, requestJson)
}

// Reports the frame's conditions (loader.js's browserConditions) to the opened network's search; nothing before open.
func (self *pageWorker) conditions(conditions *sidecarworker.WorkerConditions) {
	network := self.current()
	if network == nil {
		return
	}
	if network.burner != nil {
		network.burner.SetConditions(conditions)
		return
	}
	network.worker.SetConditions(conditions)
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
	network.closeReplica()
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

// repoId's drops, for the console's drop_states (SDK-M10-05): its reads on the network's EvsClient, which fail closed
// as every read here does until this leg's config verification fills the client.
func (self *pageNetwork) drops(repoId sidecar.Id) (sidecarworker.WorkerDrops, error) {
	client, err := self.evsClient()
	if err != nil {
		return nil, err
	}
	return client.Repo(repoId), nil
}

// The Sidecar's search (§5.4), whose calls answer unavailable until a host installs its engine: on this leg nothing does
// yet, since its sync, scope and key sources are the core's composition (SEARCH-M10-26's deferral).
func (self *pageNetwork) search() (sidecarworker.WorkerSearch, error) {
	return self.local, nil
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
