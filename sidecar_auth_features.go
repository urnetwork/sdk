//go:build !ios_extension && !js

// The authorization features' prompt for the apps (DESIGN-COMPUTE §16.6's turn on and prompt rows, §15.3; CACL-M11-03):
// GetAuthFeatures answers each of §16.6's features as the core sees it, and TurnOnAuthFeature is the prompt's act.
//
//	state   each feature: on or off, whether this release and the services serving the network implement it, whether
//	        this person may turn it on, the network's Owners and Admins, who can, and the sidecars that would pause until
//	        they update, which api lists to an Owner's or Admin's device alone
//	act     TurnOnAuthFeature is the person's act in the app's prompt, confirmed in its UI (D-CORE-13); an Owner's own
//	        first use, the bot tab or a first enclave, asks in that step. The core refuses, signing nothing, a feature
//	        this person may not turn on, naming who can, and nothing ever turns a feature off
//	remote  a remote-mode Sidecar reads ur daemon's admin.settings.show, its features, and turns one on by the Confirm
//	        method admin.settings.feature, sent confirmed
//
// An act that needs a feature that is off answers the core's refusal (sidecar.AuthFeatureRequiredError), whose text
// names the feature and who can turn it on; an app raises its prompt from GetAuthFeatures. Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/sidecar"
)

// What the features' calls are held to.
type sidecarAuthFeaturesSettings struct {
	// The bound of TurnOnAuthFeature on the local backend, which files the config head and waits for the base that
	// names it: 60 s, the config writer's own bound on one publish.
	TurnOnTimeout time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarAuthFeaturesSettings() *sidecarAuthFeaturesSettings {
	return &sidecarAuthFeaturesSettings{
		TurnOnTimeout: 60 * time.Second,
	}
}

// The control methods a remote-mode Sidecar calls, as ur's admin settings command names them
// (sidecar/cli/admin_settings_cmd.go), which the sdk cannot import.
const (
	sidecarControlMethodAdminSettingsShow    = "admin.settings.show"
	sidecarControlMethodAdminSettingsFeature = "admin.settings.feature"
)

var (
	// The Sidecar has neither backend: it closed, or never opened.
	errSidecarAuthFeaturesUnavailable = errors.New("the authorization features are unavailable: the sidecar is not open")
	// The daemon answered its settings without the features: it could not say, so nothing is offered.
	errSidecarAuthFeaturesUnknown = errors.New("the daemon did not say which authorization features are on")
)

// One authorization feature as this device sees it, for the turn-on prompt. json in the C ABI and js.
type SidecarAuthFeature struct {
	// agent_repos, compute_policy, listeners or enclaves.
	Name string `json:"name"`
	// On for this network.
	On bool `json:"on"`
	// This release implements it: one it does not is never offered.
	Implemented bool `json:"implemented"`
	// Every service serving the network implements it, as api last said; false while unknown (SupportKnown).
	Supported    bool `json:"supported"`
	SupportKnown bool `json:"support_known"`
	// This person may turn it on from this device now.
	MayTurnOn bool `json:"may_turn_on"`
	// The network's Owners and Admins, who can, by member id.
	AdminMemberIds *IdList `json:"admin_member_ids"`
	// The sidecars that would pause until they update, as api lists them to an Owner's or Admin's device; empty
	// elsewhere and while unknown (PausingKnown).
	PausingSidecarIds *IdList `json:"pausing_sidecar_ids"`
	PausingKnown      bool    `json:"pausing_known"`
	// More sidecars may pause than are listed.
	PausingMore bool `json:"pausing_more"`
}

// Every feature, in bit order.
type SidecarAuthFeatures struct {
	exportedList[*SidecarAuthFeature]
}

// An empty list.
func NewSidecarAuthFeatures() *SidecarAuthFeatures {
	return &SidecarAuthFeatures{
		exportedList: *newExportedList[*SidecarAuthFeature](),
	}
}

// The feature named name, nil when the list holds none.
func (self *SidecarAuthFeatures) Feature(name string) *SidecarAuthFeature {
	for _, feature := range self.values {
		if feature != nil && feature.Name == name {
			return feature
		}
	}
	return nil
}

// The bound form of the core's status.
func sidecarAuthFeatureOf(status *sidecar.AuthFeatureStatus) *SidecarAuthFeature {
	feature := &SidecarAuthFeature{
		Name:              status.Name,
		On:                status.On,
		Implemented:       status.Implemented,
		Supported:         status.Supported,
		SupportKnown:      status.SupportKnown,
		MayTurnOn:         status.MayTurnOn,
		AdminMemberIds:    NewIdList(),
		PausingSidecarIds: NewIdList(),
		PausingKnown:      status.PausingKnown,
		PausingMore:       status.PausingMore,
	}
	for _, memberId := range status.AdminMemberIds {
		feature.AdminMemberIds.Add(newId([16]byte(memberId)))
	}
	for _, sidecarId := range status.PausingSidecarIds {
		feature.PausingSidecarIds.Add(newId([16]byte(sidecarId)))
	}
	return feature
}

// admin.settings.show's and admin.settings.feature's params.
type sidecarControlAdminSettingsParams struct {
	Network string `json:"network,omitempty"`
	Feature string `json:"feature,omitempty"`
}

// admin.settings.show's reply, as far as the features go.
type sidecarControlAdminSettingsReply struct {
	Features []*sidecar.AuthFeatureStatus `json:"features"`
}

// The network the remote backend's pairing bound, as a method's network argument.
func sidecarAuthFeaturesNetwork(remote sidecarBackend) string {
	if named, ok := remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// Each authorization feature as this device sees it, in bit order (§16.6): the core's on the local backend, the daemon's
// on the remote one. An error, never an empty list, before a verified state and where the daemon could not say.
func (self *Sidecar) GetAuthFeatures() (*SidecarAuthFeatures, error) {
	return traceWithReturnError(func() (*SidecarAuthFeatures, error) {
		args := self.computeArgs()
		var statuses []*sidecar.AuthFeatureStatus
		switch {
		case args.local != nil:
			var err error
			statuses, err = args.local.AuthFeatureStatuses()
			if err != nil {
				return nil, err
			}
		case args.remote != nil:
			reply := &sidecarControlAdminSettingsReply{}
			params := &sidecarControlAdminSettingsParams{Network: sidecarAuthFeaturesNetwork(args.remote)}
			if err := args.remote.call(args.ctx, sidecarControlMethodAdminSettingsShow, params, false, reply); err != nil {
				return nil, err
			}
			if len(reply.Features) == 0 {
				return nil, errSidecarAuthFeaturesUnknown
			}
			statuses = reply.Features
		default:
			return nil, errSidecarAuthFeaturesUnavailable
		}
		features := NewSidecarAuthFeatures()
		for _, status := range statuses {
			if status != nil {
				features.Add(sidecarAuthFeatureOf(status))
			}
		}
		return features, nil
	})
}

// Turns the feature named name on for this network, the person's act in the app's prompt, confirmed in its UI
// (D-CORE-13): see the file header. Answers the feature as it stands after, on once its base lands. A name that is no
// feature's is refused before anything is sent; the core refuses, signing nothing, a feature this person may not turn
// on, its error naming who can.
func (self *Sidecar) TurnOnAuthFeature(name string) (*SidecarAuthFeature, error) {
	return traceWithReturnError(func() (*SidecarAuthFeature, error) {
		feature, err := sidecar.ParseAuthFeature(name)
		if err != nil {
			return nil, err
		}
		args := self.computeArgs()
		var status *sidecar.AuthFeatureStatus
		switch {
		case args.local != nil:
			ctx, cancel := context.WithTimeout(args.ctx, defaultSidecarAuthFeaturesSettings().TurnOnTimeout)
			defer cancel()
			// the person confirmed it in the app's prompt; an app has no harness ancestor
			status, err = args.local.TurnOnAuthFeature(ctx, &sidecar.AuthFeatureTurnOnArgs{Feature: feature, Confirmed: true})
		case args.remote != nil:
			status = &sidecar.AuthFeatureStatus{}
			params := &sidecarControlAdminSettingsParams{Network: sidecarAuthFeaturesNetwork(args.remote), Feature: name}
			err = args.remote.call(args.ctx, sidecarControlMethodAdminSettingsFeature, params, true, status)
		default:
			return nil, errSidecarAuthFeaturesUnavailable
		}
		if err != nil {
			return nil, err
		}
		return sidecarAuthFeatureOf(status), nil
	})
}
