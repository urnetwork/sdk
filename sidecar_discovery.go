//go:build !ios_extension && !js

// GetDiscovery (§18.5; §17.7.1, owner decision 778; v1, M6): a desktop app's view of service discovery, through the
// ur daemon it pairs with, beside Listeners(), the listeners list (§17.7.1's Listeners and discovery). Nil elsewhere:
// no discovery gathering, watch or advertising proxy runs in an app's sidecar role or its VPN component, whose
// LocalZone owns the DNS-SD names and answers each NODATA (§18.6's lifecycle).
//
//	List   as ur bridge devices --json: this host's LAN devices, shared or not, and the instances this device sees,
//	       by host, each with its address and why any is withheld
//	Share  §5.8's Confirm method bridge.share, sent with confirmed set, the bound call being the person's act in the
//	       app's UI after the disclosure; a share's /32 route may then wait for §16.6's approval, which the result
//	       names as approval_pending:<id>
//	Set    ur bridge discovery's switches: "own" on|off, "hide" or "unhide" an instance or a port in the hide set
//	       Listeners() shares, an unhide going as a Confirm method sent with confirmed set, "proxy" on|off
//
// Each call answers its callback once, on a goroutine of its own, within the remote backend's own bounds. The daemon
// holds every rule: a call here only asks.

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The daemon's discovery methods (the root's bridge_discovery_daemon.go), named here since a mobile build links none
// of the daemon's code. A test holds them to the root's.
const (
	sidecarControlMethodBridgeDevices         = "bridge.devices"
	sidecarControlMethodBridgeShare           = "bridge.share"
	sidecarControlMethodBridgeDiscovery       = "bridge.discovery"
	sidecarControlMethodBridgeDiscoveryUnhide = "bridge.discovery.unhide"
)

// Set's settings and values (ur bridge discovery's).
const (
	SidecarDiscoveryOwn    = "own"
	SidecarDiscoveryHide   = "hide"
	SidecarDiscoveryUnhide = "unhide"
	SidecarDiscoveryProxy  = "proxy"
	SidecarDiscoveryOn     = "on"
	SidecarDiscoveryOff    = "off"
)

// A call Set does not take.
var errSidecarDiscoverySetting = errors.New("discovery takes own on|off, hide or unhide <x>, or proxy on|off")

// Discovery's tunables in the sdk.
type sidecarDiscoverySettings struct {
	// How long one call to the daemon may take: 2 min, as an approval's.
	CallTimeout time.Duration
}

// The values above.
func defaultSidecarDiscoverySettings() *sidecarDiscoverySettings {
	return &sidecarDiscoverySettings{CallTimeout: 2 * time.Minute}
}

// A desktop app's view of discovery. Safe for concurrent use.
type SidecarDiscovery struct {
	ctx      context.Context
	remote   sidecarBackend
	settings *sidecarDiscoverySettings
}

// The DNS-SD view, beside Listeners(), the listeners list (§17.7.1's Listeners and discovery): a desktop app's,
// through ur daemon; nil elsewhere.
func (self *Sidecar) GetDiscovery() *SidecarDiscovery {
	discovery, _ := traceWithReturn(func() *SidecarDiscovery {
		_, remote := self.backends()
		if remote == nil {
			return nil
		}
		return &SidecarDiscovery{ctx: self.ctx, remote: remote, settings: defaultSidecarDiscoverySettings()}
	})
	return discovery
}

// Runs call within CallTimeout on a goroutine of its own and answers once.
func (self *SidecarDiscovery) run(call func(ctx context.Context) error, answer func(err error)) {
	go func() {
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.CallTimeout)
		defer cancel()
		answer(call(ctx))
	}()
}

// As ur bridge devices --json: the daemon's listing, as JSON.
func (self *SidecarDiscovery) List(callback SidecarBytesCallback) {
	reply := json.RawMessage{}
	self.run(func(ctx context.Context) error {
		return self.remote.call(ctx, sidecarControlMethodBridgeDevices, map[string]any{}, false, &reply)
	}, func(err error) {
		if err != nil {
			callback.Result(nil, err)
			return
		}
		callback.Result(&SidecarBytes{Ok: true, Data: reply}, nil)
	})
}

// Shares device, or with share false unshares it: the person's act in the app's UI, after the app showed the
// disclosure, which List's device carries the words of where the daemon has them.
func (self *SidecarDiscovery) Share(device string, share bool, callback SidecarResultCallback) {
	type shareReply struct {
		State             string `json:"state,omitempty"`
		ApprovalPendingId *Id    `json:"approval_pending_id,omitempty"`
	}
	reply := &shareReply{}
	params := map[string]any{"device": device, "unshare": !share}
	self.run(func(ctx context.Context) error {
		return self.remote.call(ctx, sidecarControlMethodBridgeShare, params, true, reply)
	}, func(err error) {
		if err != nil {
			// the daemon's refusal names no value (I-7)
			callback.Result(&SidecarResult{Ok: false, Message: err.Error()}, nil)
			return
		}
		result := &SidecarResult{Ok: true, Value: reply.State}
		if reply.ApprovalPendingId != nil {
			result.Value = "approval_pending:" + reply.ApprovalPendingId.String()
		}
		callback.Result(result, nil)
	})
}

// One of ur bridge discovery's switches (the file's Set row); an unhide is the person's act in the app's UI.
func (self *SidecarDiscovery) Set(setting string, value string, callback SidecarResultCallback) {
	method := sidecarControlMethodBridgeDiscovery
	confirmed := false
	params := map[string]any{"setting": setting, "value": value}
	switch setting {
	case SidecarDiscoveryOwn, SidecarDiscoveryProxy:
		if value != SidecarDiscoveryOn && value != SidecarDiscoveryOff {
			callback.Result(&SidecarResult{Ok: false, Code: SidecarCodeInvalidArgument, Message: errSidecarDiscoverySetting.Error()}, nil)
			return
		}
	case SidecarDiscoveryHide:
	case SidecarDiscoveryUnhide:
		method, confirmed = sidecarControlMethodBridgeDiscoveryUnhide, true
		params["setting"] = SidecarDiscoveryHide
	default:
		callback.Result(&SidecarResult{Ok: false, Code: SidecarCodeInvalidArgument, Message: errSidecarDiscoverySetting.Error()}, nil)
		return
	}
	self.run(func(ctx context.Context) error {
		return self.remote.call(ctx, method, params, confirmed, nil)
	}, func(err error) {
		if err != nil {
			callback.Result(&SidecarResult{Ok: false, Message: err.Error()}, nil)
			return
		}
		callback.Result(&SidecarResult{Ok: true}, nil)
	})
}
