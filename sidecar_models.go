//go:build !ios_extension && !js

// The model catalog for the apps (DESIGN-COMPUTE §15.3, §6.3-§6.5, §16.4; INFER-M11-09): Models(), gomobile-safe, its
// structures JSON text, over the local core in process or, on the remote backend, over ur daemon's model methods (the
// root's infer_daemon.go, the ones ur models calls), so both answer the same JSON. It is a view controller for the Bot
// tab's model pill and model choice (DESIGN-UI.md's T1 and T4, and T10's model offered again): its state read through
// Catalog, Recommended, Settings and Pulls, one listener told when any of it changes here, and its actions Pull,
// Remove and SetSettings.
//
//	Catalog()            every model this device and policy see (the root's InferModelsListing): T4's list by mode
//	                     (local, TEE, direct), each with its label, efforts, prices, its disclosure's line, and for a
//	                     local model its file's size and state, its decode rates and why this device can't run it;
//	                     T1's model pill reads the default's entry, T10 whether the old model runs here; remote:
//	                     models.list
//	Recommended()        §6.5's default for this device, its entry, or none and why: the Bot tab's first open (§15.4,
//	                     CO-19), which then offers credit and a remote model; from the same listing
//	Pull(name, listener) a local model pulled through the chunked download (CO-49), resumed at the first chunk
//	                     missing, its status told to listener by chunks done until it ends, download_corrupt naming its
//	                     chunk; it returns at once, and one pull a model runs however many screens ask; T4's download
//	                     row and the first open's progress; remote: models.pull, polled, since the app's control
//	                     session carries no notifications
//	Pulls()              the pulls this Models() follows and those that ended since: T4's rows when a screen reopens
//	Remove(name)         a local model's files, every state of them; T4; remote: models.remove
//	Settings()           the person's models key (§16.4): T1's pill (the default) and T4's TEE-only switch and allow
//	                     list; remote: models.settings
//	SetSettings(json)    a change to it (the root's InferModelsChange), the person's own act in the app's UI as
//	                     AckDisclosure is: locally through CACL's writer as SidecarActConfirmed, remotely the Confirm
//	                     method models.set sent confirmed; no Harness tool, MCP tool or RPC reaches it, so a run never
//	                     does
//	AddListener(l)       ModelsChanged after each pull's news and end, each Remove and each SetSettings made through
//	                     this Models(): the app reads the state again. A key changed elsewhere, by ur models set or on
//	                     another device, shows at the next read.
//
// Every name is the catalog's, which the release manifest and the signed policy vouch for; an entry no signed source
// vouches for says so (signed false). Nothing here runs in the VPN extension. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The model catalog for the apps: canonical names and labels, local model pulls and the person's models key. Made once
// per Sidecar, at Models()'s first call; its pulls end at the Sidecar's Close. Safe for concurrent use.
type SidecarModels struct {
	// the Sidecar's ctx and backends, which its methods call
	args     *sidecarComputeArgs
	settings *sidecarModelsSettings
	// the local core's pull (InferPull.Pull); a test's in its place
	localPull func() (sidecar.InferPullFunc, error)

	// ends at the Sidecar's Close, and with it every pull this Models() runs or polls
	ctx    context.Context
	cancel context.CancelFunc

	listeners *connect.CallbackList[SidecarModelsListener]

	stateLock sync.Mutex
	// the local core's pulls, made at the first local Pull
	pulls *sidecar.InferPulls
	// each model's pull this Models() follows or followed, by the name asked
	namePulls map[string]*sidecarModelsPull
}

// One model's pull as this Models() follows it.
type sidecarModelsPull struct {
	// held while its listeners are told, so each hears its statuses in order, one joining late included
	tellLock sync.Mutex
	// read and written under the Models' stateLock: its newest status, its listeners, and whether a follow runs
	status    *sidecar.InferPullStatus
	listeners []SidecarModelsPullListener
	following bool
}

// A listener for one pull: its status as JSON, the daemon's models.pull reply's shape (the root's InferPullStatus):
// {"name", "state": running, done or failed, "progress": the file in hand by chunks done and bytes (CO-49), "error":
// its "code" (download_corrupt naming its "chunk", download_unavailable, download_no_space, model_refused, canceled or
// pull_failed) and "message"}. Told in order, at each chunk, file or phase, and once at its end.
type SidecarModelsPullListener interface {
	OnPull(statusJson string)
}

// A listener for the models' state: told after each pull's news and end, each Remove and each SetSettings made through
// this Models(), so the app reads Catalog, Settings or Pulls again.
type SidecarModelsListener interface {
	ModelsChanged()
}

// Models()' tunables.
type sidecarModelsSettings struct {
	// How often a remote pull's status is read again (models.pull without follow): 1 s.
	PullPollInterval time.Duration
	// The reads in a row a remote pull's poll may lose to the transport before its listeners are told it failed, the
	// daemon's pull going on: 5. A refusal the daemon answers ends the poll at once.
	PullPollFailureMaxCount int
}

// The defaults, the only place one is set.
func defaultSidecarModelsSettings() *sidecarModelsSettings {
	return &sidecarModelsSettings{
		PullPollInterval:        1 * time.Second,
		PullPollFailureMaxCount: 5,
	}
}

// The daemon's model methods, as infer_daemon.go names them; TestModelsControlWireMatchesTheDaemon holds them to that
// source on the legs that build it.
const (
	sidecarControlMethodModelsList     = "models.list"
	sidecarControlMethodModelsPull     = "models.pull"
	sidecarControlMethodModelsRemove   = "models.remove"
	sidecarControlMethodModelsSettings = "models.settings"
	sidecarControlMethodModelsSet      = "models.set"
)

// The params the daemon's model methods take, as infer_daemon.go names them; the daemon reads params strictly.
type sidecarControlModelsParams struct {
	Network string `json:"network,omitempty"`
}

// models.pull's, without follow, and models.remove's.
type sidecarControlModelsNameParams struct {
	Network string `json:"network,omitempty"`
	Name    string `json:"name"`
}

// models.set's: the change beside the network.
type sidecarControlModelsSetParams struct {
	Network string `json:"network,omitempty"`
	*sidecar.InferModelsChange
}

var (
	// What Models answers with neither a local core nor a remote backend: a closed Sidecar.
	errSidecarModelsNoCore = errors.New("models: this Sidecar has no core, local or remote")
	// A request the method cannot take; the text names no value it held.
	errSidecarModelsRequest = errors.New("models: the request is malformed")
	// Remove while this Models() pulls the model.
	errSidecarModelsPullRunning = errors.New("models: the model is being pulled; remove it once the pull ends")
)

// Why no local model is recommended where the catalog gives no reason of its own.
const sidecarModelsNoneFits = "no local model fits this device's class and memory"

// The code of a pull that failed for a reason that is no download's (InferPullErrorOf's last).
const sidecarModelsPullFailedCode = "pull_failed"

// What Recommended answers: §6.5's default for this device and its entry, or no name and why.
type sidecarModelsRecommendation struct {
	// The canonical name, empty where no local model fits.
	Name  string                    `json:"name"`
	Model *sidecar.InferModelsEntry `json:"model,omitempty"`
	// Why none is recommended, where none is.
	Reason string `json:"reason,omitempty"`
}

// The pulls Pulls answers.
type sidecarModelsPulls struct {
	Pulls []*sidecar.InferPullStatus `json:"pulls"`
}

// The wrapper over args. It runs under the Sidecar's stateLock, so it only builds.
func newSidecarModels(args *sidecarComputeArgs) *SidecarModels {
	parent := args.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &SidecarModels{
		args:     args,
		settings: defaultSidecarModelsSettings(),
		localPull: func() (sidecar.InferPullFunc, error) {
			if args.local == nil {
				return nil, errSidecarModelsNoCore
			}
			pull, err := args.local.InferPull()
			if err != nil {
				return nil, err
			}
			return pull.Pull, nil
		},
		ctx:       ctx,
		cancel:    cancel,
		listeners: connect.NewCallbackList[SidecarModelsListener](),
		namePulls: map[string]*sidecarModelsPull{},
	}
}

// Ends every pull this Models() runs or polls, at the Sidecar's Close; a local pull keeps its chunks for the next.
func (self *SidecarModels) close() {
	self.cancel()
}

// The local core, or the remote backend to call; one of them.
func (self *SidecarModels) backend() (*sidecar.Sidecar, sidecarBackend, error) {
	switch {
	case self.args.local != nil:
		return self.args.local, nil, nil
	case self.args.remote != nil:
		return nil, self.args.remote, nil
	}
	return nil, nil, errSidecarModelsNoCore
}

// A remote call's answer as JSON, as the daemon wrote it.
func (self *SidecarModels) remoteJson(remote sidecarBackend, method string, params any, confirmed bool) (string, error) {
	var result json.RawMessage
	if err := remote.call(self.ctx, method, params, confirmed, &result); err != nil {
		return "", err
	}
	return string(result), nil
}

// The catalog as the core or the daemon lists it, and its JSON: the daemon's as it wrote it, the core's as the daemon
// would.
func (self *SidecarModels) listing() (*sidecar.InferModelsListing, string, error) {
	local, remote, err := self.backend()
	if err != nil {
		return nil, "", err
	}
	if remote != nil {
		listingJson, err := self.remoteJson(remote, sidecarControlMethodModelsList, &sidecarControlModelsParams{Network: sidecarSupportNetwork(remote)}, false)
		if err != nil {
			return nil, "", err
		}
		listing := &sidecar.InferModelsListing{}
		if err := json.Unmarshal([]byte(listingJson), listing); err != nil {
			return nil, "", err
		}
		return listing, listingJson, nil
	}
	// a core with no pull, its state on no disk, lists its local models with no file state
	pull, _ := local.InferPull()
	listing, err := local.InferModels(self.ctx, nil, pull)
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(listing)
	if err != nil {
		return nil, "", err
	}
	return listing, string(encoded), nil
}

// Every model this device and policy see, JSON (§6.3, §15.3): the root's InferModelsListing, its entries by canonical
// name, the local model §6.5 recommends, and why no local or no remote model is listed where none is.
func (self *SidecarModels) Catalog() (string, error) {
	return traceWithReturnError(func() (string, error) {
		_, listingJson, err := self.listing()
		return listingJson, err
	})
}

// The local model §6.5's defaults recommend for this device, JSON: {"name", "model": its catalog entry}, or no name
// and the "reason", so the Bot tab offers credit and a remote model instead (§15.4).
func (self *SidecarModels) Recommended() (string, error) {
	return traceWithReturnError(func() (string, error) {
		listing, _, err := self.listing()
		if err != nil {
			return "", err
		}
		recommendation := &sidecarModelsRecommendation{}
		if listing.Recommended != "" {
			for _, entry := range listing.Entries {
				if entry.Kind == sidecar.InferCatalogKindLocal && slices.Contains(entry.Names, listing.Recommended) {
					recommendation.Name, recommendation.Model = listing.Recommended, entry
					break
				}
			}
		}
		if recommendation.Model == nil {
			recommendation.Reason = listing.LocalUnavailable
			if recommendation.Reason == "" {
				recommendation.Reason = sidecarModelsNoneFits
			}
		}
		encoded, err := json.Marshal(recommendation)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

// The local core's pulls, made at the first call.
func (self *SidecarModels) localPulls() (*sidecar.InferPulls, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.pulls == nil {
		pull, err := self.localPull()
		if err != nil {
			return nil, err
		}
		self.pulls = sidecar.NewInferPulls(self.ctx, pull, nil)
	}
	return self.pulls, nil
}

// Pulls name's local model, and on a desktop's daemon its engine's files, telling listener its status in order until
// it ends (see SidecarModelsPullListener); nil listens through AddListener and Pulls alone. It returns once the pull
// is started or joined: an error says it could not start, with no core, or a local core keeping no state on a disk.
// A pull that starts and fails tells listener failed, with its code; asked again, it resumes at the first chunk
// missing (CO-49).
func (self *SidecarModels) Pull(name string, listener SidecarModelsPullListener) error {
	return traceWithError(func() error {
		if name == "" {
			return errSidecarModelsRequest
		}
		_, remote, err := self.backend()
		if err != nil {
			return err
		}
		var pulls *sidecar.InferPulls
		if remote == nil {
			if pulls, err = self.localPulls(); err != nil {
				return err
			}
		}
		pull, start := self.join(name)
		func() {
			pull.tellLock.Lock()
			defer pull.tellLock.Unlock()
			status := func() *sidecar.InferPullStatus {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if listener != nil {
					pull.listeners = append(pull.listeners, listener)
				}
				return pull.status
			}()
			// one joining a running pull hears where it is now, before its next news
			if !start && listener != nil && status != nil {
				sidecarModelsTell([]SidecarModelsPullListener{listener}, status)
			}
		}()
		if start {
			go connect.HandleError(func() {
				if remote != nil {
					self.poll(pull, remote, name)
				} else {
					self.follow(pull, pulls, name)
				}
			})
		}
		return nil
	})
}

// name's pull this Models() follows, and whether no follow runs for it, which the caller then starts.
func (self *SidecarModels) join(name string) (*sidecarModelsPull, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pull, ok := self.namePulls[name]
	if ok && pull.following {
		return pull, false
	}
	pull = &sidecarModelsPull{following: true}
	self.namePulls[name] = pull
	return pull, true
}

// Tells listeners status, each call guarded.
func sidecarModelsTell(listeners []SidecarModelsPullListener, status *sidecar.InferPullStatus) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return
	}
	for _, listener := range listeners {
		connect.HandleError(func() {
			listener.OnPull(string(encoded))
		})
	}
}

// Tells pull's listeners status, its end where ended (after which a Pull starts a new follow), then this Models'
// listeners.
func (self *SidecarModels) tell(pull *sidecarModelsPull, status *sidecar.InferPullStatus, ended bool) {
	func() {
		pull.tellLock.Lock()
		defer pull.tellLock.Unlock()
		listeners := func() []SidecarModelsPullListener {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			pull.status = status
			listeners := slices.Clone(pull.listeners)
			if ended {
				pull.following = false
				pull.listeners = nil
			}
			return listeners
		}()
		sidecarModelsTell(listeners, status)
	}()
	self.changed()
}

// Tells this Models' listeners its state changed.
func (self *SidecarModels) changed() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.ModelsChanged()
		})
	}
}

// A failed status for name.
func sidecarModelsPullFailed(name string, pullErr *sidecar.InferPullError) *sidecar.InferPullStatus {
	return &sidecar.InferPullStatus{Name: name, State: sidecar.InferPullFailed, Error: pullErr}
}

// Follows name's local pull to its end, telling pull's listeners its news; the Sidecar's Close ends it as canceled.
func (self *SidecarModels) follow(pull *sidecarModelsPull, pulls *sidecar.InferPulls, name string) {
	end, err := pulls.Follow(self.ctx, name, nil, func(status *sidecar.InferPullStatus) error {
		self.tell(pull, status, false)
		return nil
	})
	if err != nil {
		end = sidecarModelsPullFailed(name, sidecar.InferPullErrorOf(err))
	}
	self.tell(pull, end, true)
}

// Polls name's pull on the daemon every PullPollInterval to its end, telling pull's listeners each status that
// changed. The first read starts the pull in the daemon, where it runs until it ends whoever polls. A refusal the
// daemon answers ends the poll at once; PullPollFailureMaxCount reads in a row lost to the transport end it too; the
// Sidecar's Close ends it as canceled.
func (self *SidecarModels) poll(pull *sidecarModelsPull, remote sidecarBackend, name string) {
	params := &sidecarControlModelsNameParams{Network: sidecarSupportNetwork(remote), Name: name}
	failureCount := 0
	told := ""
	for {
		status := &sidecar.InferPullStatus{}
		err := remote.call(self.ctx, sidecarControlMethodModelsPull, params, false, status)
		var refusal *sidecarControlError
		switch {
		case self.ctx.Err() != nil:
			self.tell(pull, sidecarModelsPullFailed(name, sidecar.InferPullErrorOf(self.ctx.Err())), true)
			return
		case errors.As(err, &refusal):
			self.tell(pull, sidecarModelsPullFailed(name, &sidecar.InferPullError{Code: sidecarModelsPullFailedCode, Message: refusal.Message}), true)
			return
		case err != nil:
			failureCount += 1
			if self.settings.PullPollFailureMaxCount <= failureCount {
				self.tell(pull, sidecarModelsPullFailed(name, &sidecar.InferPullError{
					Code:    sidecarModelsPullFailedCode,
					Message: fmt.Sprintf("the daemon did not answer %d reads of the pull, which goes on there", failureCount),
				}), true)
				return
			}
		case status.Ended():
			self.tell(pull, status, true)
			return
		default:
			failureCount = 0
			if encoded, err := json.Marshal(status); err == nil && string(encoded) != told {
				told = string(encoded)
				self.tell(pull, status, false)
			}
		}
		select {
		case <-self.ctx.Done():
		case <-time.After(self.settings.PullPollInterval):
		}
	}
}

// The pulls this Models() follows now and those that ended since, each's newest status, JSON: {"pulls": [...]}, by
// name. A pull's end stays until the model is pulled again or removed.
func (self *SidecarModels) Pulls() string {
	statuses := func() []*sidecar.InferPullStatus {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		statuses := []*sidecar.InferPullStatus{}
		for name, pull := range self.namePulls {
			status := pull.status
			if status == nil {
				status = &sidecar.InferPullStatus{Name: name, State: sidecar.InferPullRunning}
			}
			statuses = append(statuses, status)
		}
		return statuses
	}()
	slices.SortFunc(statuses, func(a *sidecar.InferPullStatus, b *sidecar.InferPullStatus) int {
		return strings.Compare(a.Name, b.Name)
	})
	encoded, err := json.Marshal(&sidecarModelsPulls{Pulls: statuses})
	if err != nil {
		return `{"pulls":[]}`
	}
	return string(encoded)
}

// Deletes every state of name's local model on this device (on a remote backend, on the daemon's): whole, part, the
// chunks done and the kept index. Refused while this Models() pulls it.
func (self *SidecarModels) Remove(name string) error {
	return traceWithError(func() error {
		if name == "" {
			return errSidecarModelsRequest
		}
		running := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			pull, ok := self.namePulls[name]
			return ok && pull.following
		}()
		if running {
			return errSidecarModelsPullRunning
		}
		local, remote, err := self.backend()
		if err != nil {
			return err
		}
		if remote != nil {
			if _, err := self.remoteJson(remote, sidecarControlMethodModelsRemove, &sidecarControlModelsNameParams{Network: sidecarSupportNetwork(remote), Name: name}, false); err != nil {
				return err
			}
		} else {
			pull, err := local.InferPull()
			if err != nil {
				return err
			}
			if err := pull.Remove(self.ctx, name); err != nil {
				return err
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if pull, ok := self.namePulls[name]; ok && !pull.following {
				delete(self.namePulls, name)
			}
		}()
		self.changed()
		return nil
	})
}

// The person's models key as this device reads it, JSON (§16.4; the root's InferModelsSettings): the default model,
// allow, tee_only, sign_in_mail, each harness's tiers and each agent label's default, and where it does not read, that
// every model is refused, and why.
func (self *SidecarModels) Settings() (string, error) {
	return traceWithReturnError(func() (string, error) {
		local, remote, err := self.backend()
		if err != nil {
			return "", err
		}
		if remote != nil {
			return self.remoteJson(remote, sidecarControlMethodModelsSettings, &sidecarControlModelsParams{Network: sidecarSupportNetwork(remote)}, false)
		}
		settings, err := local.InferModelsSettings(self.ctx)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(settings)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

// A change to the person's models key, JSON (the root's InferModelsChange: default, tee_only, sign_in_mail, allow, or
// a harness's tier's model), each field set changing and each left out staying: the person's own act in the app's UI,
// after the app showed them what changes, as AckDisclosure is. Locally it is written through CACL's writer as their
// confirmed act; remotely it is the daemon's Confirm method models.set, sent confirmed. An empty change is refused.
func (self *SidecarModels) SetSettings(changeJson string) error {
	return traceWithError(func() error {
		change := &sidecar.InferModelsChange{}
		decoder := json.NewDecoder(strings.NewReader(changeJson))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(change); err != nil {
			return fmt.Errorf("%w: %w", errSidecarModelsRequest, err)
		}
		if change.Empty() {
			return fmt.Errorf("%w: no change", errSidecarModelsRequest)
		}
		local, remote, err := self.backend()
		if err != nil {
			return err
		}
		if remote != nil {
			// the Confirm method, sent confirmed: the person changed it in the app's UI
			params := &sidecarControlModelsSetParams{Network: sidecarSupportNetwork(remote), InferModelsChange: change}
			if _, err := self.remoteJson(remote, sidecarControlMethodModelsSet, params, true); err != nil {
				return err
			}
		} else if _, err := local.InferSetModels(self.ctx, change); err != nil {
			return err
		}
		self.changed()
		return nil
	})
}

// Adds a listener for the models' state (SidecarModelsListener); Close on the answer removes it.
func (self *SidecarModels) AddListener(listener SidecarModelsListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}
