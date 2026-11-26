//go:build !ios_extension && !js

// The config head's carried records for the apps (sidecar §7.9's Writes and records rows; D-TEAM-26; owner decision
// 699; QD-1207): GetCarriedConfigRecords lists each ServiceSettings or VersioningRules record the network's newest
// verified config head carries under a signer who no longer stands as one who may sign it, for an admin's screen to
// name, with the record's signer, the signer's member and why, and who may file a fresh record. Every reader holds such a
// record under its first signer until one of those files another; no writer re-signs it.
//
//	local   the core's listing (sidecar.Sidecar.CarriedConfigRecords): the newest verified state's head, read through
//	        the config client's head reader within ReadTimeout
//	remote  ur daemon's admin.settings.show, unconfirmed, as ur admin settings reads it: its carried records, which the
//	        daemon lists from the same core call; a daemon that could not read the head's records now, or whose
//	        settings are not known, answers an error, never an empty list, which would read as every signer standing
//
// A list is the listing's answer: empty while every record's signer stands. The listing reads and changes nothing:
// filing a fresh record is ur admin settings' or ur admin versioning's act, or the app's own settings screen's. Safe for
// concurrent use.

package sdk

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/sidecar"
)

// What GetCarriedConfigRecords is held to.
type sidecarConfigAdminSettings struct {
	// The bound of the listing on the local backend, which may read the config head from evs: 30 s, as long as a
	// remote-mode Sidecar's control call may take.
	ReadTimeout time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarConfigAdminSettings() *sidecarConfigAdminSettings {
	return &sidecarConfigAdminSettings{
		ReadTimeout: 30 * time.Second,
	}
}

var (
	// The Sidecar has neither backend: it closed, or never opened.
	errSidecarCarriedRecordsUnavailable = errors.New("the config head's carried records are unavailable: the sidecar is not open")
	// The daemon could not say which records stand under a signer who no longer stands: it could not read the head's
	// records now, or its settings are not known.
	errSidecarCarriedRecordsUnknown = errors.New("the daemon could not say which config records stand under a signer who no longer stands")
)

// SidecarCarriedConfigRecord.Why's values, as the core names them (sidecar.ConfigCarried...).
const (
	SidecarCarriedRevoked   = sidecar.ConfigCarriedRevoked
	SidecarCarriedParked    = sidecar.ConfigCarriedParked
	SidecarCarriedExpired   = sidecar.ConfigCarriedExpired
	SidecarCarriedRemoved   = sidecar.ConfigCarriedRemoved
	SidecarCarriedTagged    = sidecar.ConfigCarriedTagged
	SidecarCarriedRole      = sidecar.ConfigCarriedRole
	SidecarCarriedUnlisted  = sidecar.ConfigCarriedUnlisted
	SidecarCarriedSignature = sidecar.ConfigCarriedSignature
)

// One config head record that stands under a signer who no longer stands (sidecar.ConfigCarriedRecord). json in the C
// ABI and js.
type SidecarCarriedConfigRecord struct {
	// service_settings or versioning_rules.
	Record string `json:"record"`
	// The sidecar whose identity key signed it; nil where its Signed names none.
	SignerId *Id `json:"signer_id,omitempty"`
	// The signer's member, as its row names it; nil for a tagged signer and one no row holds.
	MemberId *Id `json:"member_id,omitempty"`
	// Why the signer no longer stands as one who may sign it: revoked, parked, expired, removed, tagged, role,
	// unlisted or signature (the SidecarCarried values).
	Why string `json:"why"`
	// revoked and parked: the authorization log position it took effect at.
	Position int64 `json:"position,omitempty"`
	// expired: when the signer's admission ended, in unix milliseconds.
	NotAfterMillis int64 `json:"not_after_millis,omitempty"`
	// The role the signer's member holds now (owner, admin, network_admin, it_admin, billing_admin, auditor or
	// member), while it is a member.
	Role string `json:"role,omitempty"`
	// The signer's member is no longer the network's.
	MemberRemoved bool `json:"member_removed,omitempty"`
	// The roles whose devices may file another, in role order: owner and admin, and network_admin as well for
	// versioning_rules (sidecar.ConfigCarriedRecordFilers).
	FiledBy *StringList `json:"filed_by"`
}

// The records a config head carries under a signer who no longer stands, service_settings before versioning_rules.
type SidecarCarriedConfigRecordList struct {
	exportedList[*SidecarCarriedConfigRecord]
}

// An empty list.
func NewSidecarCarriedConfigRecordList() *SidecarCarriedConfigRecordList {
	return &SidecarCarriedConfigRecordList{
		exportedList: *newExportedList[*SidecarCarriedConfigRecord](),
	}
}

// The bound form of the core's record.
func sidecarCarriedConfigRecordOf(carried *sidecar.ConfigCarriedRecord) *SidecarCarriedConfigRecord {
	record := &SidecarCarriedConfigRecord{
		Record:         carried.Record,
		Why:            carried.Why,
		Position:       int64(carried.At),
		NotAfterMillis: carried.NotAfterMs,
		Role:           carried.Role,
		MemberRemoved:  carried.MemberRemoved,
		FiledBy:        NewStringList(),
	}
	if carried.SignerId != (sidecar.Id{}) {
		record.SignerId = newId([16]byte(carried.SignerId))
	}
	if carried.MemberId != nil {
		record.MemberId = newId([16]byte(*carried.MemberId))
	}
	record.FiledBy.addAll(sidecar.ConfigCarriedRecordFilers(carried.Record)...)
	return record
}

// admin.settings.show's reply, as far as the carried records go (sidecar/cli/admin_settings_cmd.go's).
type sidecarControlAdminSettingsCarriedReply struct {
	// The daemon could not open the head, so it lists nothing.
	Unknown bool                           `json:"unknown"`
	Carried []*sidecar.ConfigCarriedRecord `json:"carried"`
	// The daemon could not read the head's records now.
	CarriedUnknown bool `json:"carried_unknown"`
}

// Each record of this network's newest verified config head that stands under a signer who no longer stands, with why
// and who files another: see the file header. Empty while every record's signer stands; an error, never an empty
// list, before a verified state and where it cannot be said now.
func (self *Sidecar) GetCarriedConfigRecords() (*SidecarCarriedConfigRecordList, error) {
	return traceWithReturnError(func() (*SidecarCarriedConfigRecordList, error) {
		args := self.computeArgs()
		var carried []*sidecar.ConfigCarriedRecord
		switch {
		case args.local != nil:
			ctx, cancel := context.WithTimeout(args.ctx, defaultSidecarConfigAdminSettings().ReadTimeout)
			defer cancel()
			var err error
			carried, err = args.local.CarriedConfigRecords(ctx)
			if err != nil {
				return nil, err
			}
		case args.remote != nil:
			reply := &sidecarControlAdminSettingsCarriedReply{}
			params := &sidecarControlAdminSettingsParams{Network: sidecarAuthFeaturesNetwork(args.remote)}
			if err := args.remote.call(args.ctx, sidecarControlMethodAdminSettingsShow, params, false, reply); err != nil {
				return nil, err
			}
			if reply.Unknown || reply.CarriedUnknown {
				return nil, errSidecarCarriedRecordsUnknown
			}
			carried = reply.Carried
		default:
			return nil, errSidecarCarriedRecordsUnavailable
		}
		records := NewSidecarCarriedConfigRecordList()
		for _, record := range carried {
			if record != nil {
				records.Add(sidecarCarriedConfigRecordOf(record))
			}
		}
		return records, nil
	})
}
