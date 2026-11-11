package sdk

// pro_features_status.go -- the "Pro features in use" notice: a network
// without Pro whose hosted proxies hold SOCKS or WireGuard credentials, the
// two proxy features pro.yml puts in Pro (server model/pro.go).
//
// While the server's enforce_features switch is off, every plan uses SOCKS and
// WireGuard. Once it is on, the proxy refuses them to a network without Pro:
// a SOCKS sign-in fails, a WireGuard peer is dropped, and a new proxy is
// issued neither. HTTP and HTTPS keep working. Every proxy issued while the
// switch was off holds a SOCKS url, so a network without Pro that holds any
// proxy from then has the notice.
//
// The server decides everything the notice says (GET /network/proxies):
//   - the list's plan: whether the network holds Pro now, read live and not
//     from the jwt's pro claim, which is stale after an upgrade or a lapse,
//     and whether enforce_features is on;
//   - each proxy's pro_features: the Pro-only features it holds credentials
//     for.
//
// A server before the plan reports none, and the notice stays off: without a
// live Pro the client could tell a Pro network to upgrade.
//
// The prompt is the decision an app shows, the same on every platform
// (ProFeaturesStatus.Prompt):
//   - ProFeaturesPromptUpgrade, before enforcement: SOCKS and WireGuard proxies
//     are part of Pro, and an upgrade keeps them working;
//   - ProFeaturesPromptStopped, after enforcement: they stopped working on the
//     network's proxies, and an upgrade turns them back on.
//
// Both offer the app's upgrade flow, the one the client limit prompt opens.
// The app shows the prompt where it lists the network's proxies, and as a
// notice on its main screen, which ProFeaturesNoticeTracker shows and
// withdraws as ClientLimitNoticeTracker does the client limit's notice.
//
// js/src/utils/pro_features.ts is the TypeScript twin, for ur.io, and
// js/test/pro_features_vectors.json holds the vectors both replay.

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// the pro.yml feature names the server lists in a proxy's pro_features
	ProFeatureSocksProxy     = "socks_proxy"
	ProFeatureWireguardProxy = "wireguard_proxy"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// nothing to say: the network holds Pro, holds no SOCKS or WireGuard
	// proxy, or the server did not report its plan
	ProFeaturesPromptNone = ""
	// enforcement is off: SOCKS and WireGuard proxies are part of Pro, and an
	// upgrade keeps them working
	ProFeaturesPromptUpgrade = "upgrade"
	// enforcement is on: SOCKS and WireGuard stopped working on the network's
	// proxies, and an upgrade turns them back on
	ProFeaturesPromptStopped = "stopped"
)

// NetworkProxiesPlan is the plan the server reports beside the network's
// proxies (GET /network/proxies `plan`).
type NetworkProxiesPlan struct {
	// the network holds Pro now, read live by the server
	Pro bool `json:"pro"`
	// the server refuses SOCKS and WireGuard to a network without Pro
	// (pro.yml enforce_features)
	ProFeaturesEnforced bool `json:"pro_features_enforced"`
}

// ProFeaturesStatus is one readout of the network's Pro features in use.
// Plain fields so gomobile and the cgo/js bindings carry it as is.
type ProFeaturesStatus struct {
	// what the app shows: ProFeaturesPromptNone, ProFeaturesPromptUpgrade or
	// ProFeaturesPromptStopped
	Prompt string
	// the network's proxies that hold a SOCKS url, and those that hold a
	// WireGuard config, of the features the free plan does not include. A
	// proxy with both counts in both.
	SocksProxyCount     int
	WireguardProxyCount int
	// the server reported the network's plan with its proxies; false for a
	// server before it, or before the list loaded
	PlanKnown bool
	// the network holds Pro now (PlanKnown only)
	Pro bool
	// the server refuses SOCKS and WireGuard to a network without Pro
	// (PlanKnown only)
	ProFeaturesEnforced bool
}

// ProFeaturesStatusFor reads the network's proxies list (GET /network/proxies)
// into the notice's status. A nil list, or one without the plan, has no
// prompt.
func ProFeaturesStatusFor(result *NetworkClientsResult) *ProFeaturesStatus {
	status := &ProFeaturesStatus{}
	if result == nil {
		return status
	}
	if result.Clients != nil {
		for i := 0; i < result.Clients.Len(); i += 1 {
			client := result.Clients.Get(i)
			if client == nil || client.ProFeatures == nil {
				continue
			}
			if client.ProFeatures.Contains(ProFeatureSocksProxy) {
				status.SocksProxyCount += 1
			}
			if client.ProFeatures.Contains(ProFeatureWireguardProxy) {
				status.WireguardProxyCount += 1
			}
		}
	}
	if result.Plan == nil {
		return status
	}
	status.PlanKnown = true
	status.Pro = result.Plan.Pro
	status.ProFeaturesEnforced = result.Plan.ProFeaturesEnforced
	status.Prompt = proFeaturesPromptFor(status)
	return status
}

// proFeaturesPromptFor decides the prompt: none for a Pro network, for one
// with no SOCKS or WireGuard proxy, or with no plan from the server; else what
// stopped once enforcement is on, and the upgrade before.
func proFeaturesPromptFor(status *ProFeaturesStatus) string {
	switch {
	case !status.PlanKnown || status.Pro:
		return ProFeaturesPromptNone
	case status.SocksProxyCount == 0 && status.WireguardProxyCount == 0:
		return ProFeaturesPromptNone
	case status.ProFeaturesEnforced:
		return ProFeaturesPromptStopped
	default:
		return ProFeaturesPromptUpgrade
	}
}

func cloneProFeaturesStatus(status *ProFeaturesStatus) *ProFeaturesStatus {
	if status == nil {
		return nil
	}
	copied := *status
	return &copied
}

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// leave the notice as it is
	ProFeaturesNoticeNone = ""
	// show the notice and alert the user
	ProFeaturesNoticePost = "post"
	// show the notice, or replace the one showing, without alerting
	ProFeaturesNoticeUpdate = "update"
	// withdraw the notice
	ProFeaturesNoticeRemove = "remove"
)

// ProFeaturesNoticeTracker decides the notice an app shows on its main screen
// for the network's Pro features in use, the same on every platform; the app
// supplies only the words and the notice itself. Its rules are the client
// limit notice's (ClientLimitNoticeTracker), with a sign-in in place of a
// connection request:
//   - the notice shows while the status has a prompt;
//   - it alerts once per sign-in: a notice that returns after it went shows
//     again without alerting, and a prompt change (enforcement turning on)
//     replaces it in place;
//   - it goes when the prompt ends (an upgrade, or no SOCKS or WireGuard proxy
//     left) and at Reset (sign out);
//   - Dismiss, the user closing it, withdraws it until its prompt changes or
//     it ends and returns, and Reset clears that.
//
// The list of proxies shows the status's prompt whatever the tracker says.
//
// Not safe for concurrent use: an app calls it from one thread, its main thread.
type ProFeaturesNoticeTracker struct {
	// a notice shows, saying shownPrompt
	shown       bool
	shownPrompt string
	// a notice alerted during this sign-in
	alerted bool
	// the prompt the user dismissed; ProFeaturesPromptNone when none is
	dismissedPrompt string
}

func NewProFeaturesNoticeTracker() *ProFeaturesNoticeTracker {
	return &ProFeaturesNoticeTracker{}
}

// Update takes the network's current status and returns what to do with the
// notice: one of the ProFeaturesNotice actions.
func (self *ProFeaturesNoticeTracker) Update(status *ProFeaturesStatus) string {
	prompt := ProFeaturesPromptNone
	if status != nil {
		prompt = status.Prompt
	}
	if prompt == ProFeaturesPromptNone {
		// a prompt that returns later shows again, quietly
		self.dismissedPrompt = ProFeaturesPromptNone
		return self.withdraw()
	}
	if prompt == self.dismissedPrompt {
		return self.withdraw()
	}
	// a new prompt ends the dismissal of the old one
	self.dismissedPrompt = ProFeaturesPromptNone
	if self.shown && self.shownPrompt == prompt {
		return ProFeaturesNoticeNone
	}
	self.shown = true
	self.shownPrompt = prompt
	if self.alerted {
		return ProFeaturesNoticeUpdate
	}
	self.alerted = true
	return ProFeaturesNoticePost
}

// Dismiss is the user closing the notice. It returns ProFeaturesNoticeRemove
// when one shows, and keeps it withdrawn while its prompt stays the same.
func (self *ProFeaturesNoticeTracker) Dismiss() string {
	if !self.shown {
		return ProFeaturesNoticeNone
	}
	self.dismissedPrompt = self.shownPrompt
	return self.withdraw()
}

// Reset ends the sign-in, for a sign out or an account switch, and returns
// what to do with the notice: ProFeaturesNoticeRemove when one shows.
func (self *ProFeaturesNoticeTracker) Reset() string {
	self.alerted = false
	self.dismissedPrompt = ProFeaturesPromptNone
	return self.withdraw()
}

// GetShownPrompt is the prompt of the notice showing, ProFeaturesPromptNone
// when none does.
func (self *ProFeaturesNoticeTracker) GetShownPrompt() string {
	if !self.shown {
		return ProFeaturesPromptNone
	}
	return self.shownPrompt
}

func (self *ProFeaturesNoticeTracker) withdraw() string {
	if !self.shown {
		return ProFeaturesNoticeNone
	}
	self.shown = false
	self.shownPrompt = ProFeaturesPromptNone
	return ProFeaturesNoticeRemove
}
