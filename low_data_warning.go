//go:build !ios_extension

package sdk

// The low-data warning: a notice, before the balance runs out, that little
// data is left. Every app hands each subscription balance it loads to
// LowDataWarningForBalance and shows the warning it returns, in the app and as
// a local notification, through the path its out-of-balance notice takes. The
// warning only informs: it never disconnects, and nothing else changes while
// it shows. When the data does run out, the out-of-balance notice takes over
// as before.
//
// There are two thresholds: a part of the period's data (10% by default) and
// an amount (1 GiB by default). Each is measured on the available data, the
// number the usage bar shows as available: what new connections can still
// reserve. Each warns once per balance period. One reading past both warns
// once, for the one with the lower amount, which is the more urgent.
//
// A balance period is the data a network's balances hold. A threshold that
// warned stays quiet until the network gets new balances that together start
// with at least the threshold's own amount: the next daily grant, a new Pro
// cycle, a purchase or a code. So the free plan's warnings come back with
// every day's grant, but a Pro network low in its month is not told again
// every day just because its small daily grant renewed. Data that comes back
// from pending connections is not new data, so a reading that rises and
// falls around a threshold warns once. The balances a threshold warned with
// are stored in the space's local state, so this holds across app restarts,
// and a sign-out clears it with the rest of the account's state.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/urnetwork/glog"
)

const (
	// Keep these untyped so gomobile exports them on every app platform.

	// a part of the period's data is left (lowDataWarningSettings.PercentLeft)
	LowDataWarningKindPercent = "percent"
	// an amount of data is left (lowDataWarningSettings.ByteCountLeft)
	LowDataWarningKindByteCount = "byte_count"
)

const lowDataWarningFileName = ".low_data_warning"

// lowDataWarningSettings holds the thresholds.
type lowDataWarningSettings struct {
	// Warns when the available data is at or below this percent of the
	// period's start balance. 10 is the part the owner chose (2026-11-28). A
	// non-positive value turns this threshold off.
	PercentLeft int64
	// Warns when the available data is at or below this amount. 1 GiB is the
	// amount the owner chose (2026-11-28). A non-positive value turns this
	// threshold off.
	ByteCountLeft ByteCount
}

func defaultLowDataWarningSettings() *lowDataWarningSettings {
	return &lowDataWarningSettings{
		PercentLeft:   10,
		ByteCountLeft: 1024 * 1024 * 1024,
	}
}

// LowDataWarning is a warning to show now. Plain fields so gomobile and the
// cgo bindings carry it as is. Spell byte counts int64 so gomobile
// generates primitive comparisons in Java.
type LowDataWarning struct {
	// LowDataWarningKindPercent or LowDataWarningKindByteCount: the threshold
	// the warning is for. The percent kind says how much of the data is left,
	// the byte count kind how many bytes.
	Kind string
	// The data still available, the amount the warning names.
	AvailableByteCount int64
	// The period's start balance, the whole of the data the percent is of.
	StartBalanceByteCount int64
	// The part of the start balance still available, in whole percent rounded
	// up, so a warning never says 0% while some data is left: 1 to 100.
	AvailablePercent int32
}

// The stored state: per threshold kind, the balances active when its warning
// showed, and the network they are of. A kind that is not stored is armed.
type lowDataWarningState struct {
	NetworkId string `json:"network_id,omitempty"`
	// threshold kind -> the balance ids the warning showed with, sorted
	ShownBalanceIds map[string][]string `json:"shown_balance_ids,omitempty"`
}

// LowDataWarningForBalance takes a subscription balance the app just loaded
// and returns the low-data warning to show now, or nil for none. A warning is
// returned once: the threshold it is for is stored as warned in the local
// state, and stays so until the network has new balances (see the file
// comment). Without a local state, or while the space is signed out, there is
// nowhere to remember a warning, so none is returned.
//
// Safe for concurrent use: the apps call it from more than one place (the
// app's balance poll and a widget's).
func LowDataWarningForBalance(localState *LocalState, result *SubscriptionBalanceResult) *LowDataWarning {
	return lowDataWarningForBalanceWithSettings(localState, result, defaultLowDataWarningSettings())
}

func lowDataWarningForBalanceWithSettings(
	localState *LocalState,
	result *SubscriptionBalanceResult,
	settings *lowDataWarningSettings,
) *LowDataWarning {
	if localState == nil || result == nil {
		return nil
	}
	var warning *LowDataWarning
	err := localState.writeSignedIn(func() error {
		state := localState.loadLowDataWarningStateWithLock()
		var changed bool
		warning, changed = nextLowDataWarning(state, result, settings)
		if !changed {
			return nil
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := localState.writePreferenceBytesWithLock(lowDataWarningFileName, data); err != nil {
			// a warning that could not be stored would show again on every
			// balance read
			warning = nil
			return err
		}
		return nil
	})
	if err != nil {
		glog.Infof("[low data]could not store the warning state: %s\n", err)
		return nil
	}
	return warning
}

// Reads the stored state. A missing, unreadable or malformed record reads as
// nothing warned yet. The caller holds authStateLock.
func (self *LocalState) loadLowDataWarningStateWithLock() *lowDataWarningState {
	state := &lowDataWarningState{}
	data, err := os.ReadFile(filepath.Join(self.localStorageDir, lowDataWarningFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			glog.Infof("[low data]could not read the warning state: %s\n", err)
		}
		return state
	}
	if err := json.Unmarshal(data, state); err != nil {
		glog.Infof("[low data]could not decode the warning state: %s\n", err)
		return &lowDataWarningState{}
	}
	return state
}

// Decides the warning for one balance reading, and updates the state in
// place. changed reports whether the state must be stored again. Pure: no
// clock, no storage.
func nextLowDataWarning(
	state *lowDataWarningState,
	result *SubscriptionBalanceResult,
	settings *lowDataWarningSettings,
) (warning *LowDataWarning, changed bool) {
	balanceIdStartByteCounts := map[string]ByteCount{}
	networkId := ""
	if result.ActiveTransferBalances != nil {
		for _, balance := range result.ActiveTransferBalances.getAll() {
			if balance == nil || balance.BalanceId == nil {
				continue
			}
			balanceIdStartByteCounts[balance.BalanceId.String()] += balance.StartBalanceByteCount
			if networkId == "" && balance.NetworkId != nil {
				networkId = balance.NetworkId.String()
			}
		}
	}
	balanceIds := make([]string, 0, len(balanceIdStartByteCounts))
	for balanceId := range balanceIdStartByteCounts {
		balanceIds = append(balanceIds, balanceId)
	}
	slices.Sort(balanceIds)

	// the warnings of another network are not this one's
	if networkId != "" && state.NetworkId != networkId {
		if state.NetworkId != "" || 0 < len(state.ShownBalanceIds) {
			state.ShownBalanceIds = nil
		}
		state.NetworkId = networkId
		changed = true
	}

	startByteCount := result.StartBalanceByteCount
	availableByteCount := result.BalanceByteCount

	type threshold struct {
		kind      string
		byteCount ByteCount
	}
	thresholds := []threshold{}
	if 0 < settings.PercentLeft && 0 < startByteCount {
		thresholds = append(thresholds, threshold{
			kind:      LowDataWarningKindPercent,
			byteCount: startByteCount * settings.PercentLeft / 100,
		})
	}
	if 0 < settings.ByteCountLeft {
		thresholds = append(thresholds, threshold{
			kind:      LowDataWarningKindByteCount,
			byteCount: settings.ByteCountLeft,
		})
	}

	// a threshold that warned comes back once new balances start with at
	// least its amount
	for _, t := range thresholds {
		shownBalanceIds, shown := state.ShownBalanceIds[t.kind]
		if !shown {
			continue
		}
		newByteCount := ByteCount(0)
		for balanceId, balanceStartByteCount := range balanceIdStartByteCounts {
			if !slices.Contains(shownBalanceIds, balanceId) {
				newByteCount += balanceStartByteCount
			}
		}
		if 0 < t.byteCount && t.byteCount <= newByteCount {
			delete(state.ShownBalanceIds, t.kind)
			changed = true
		}
	}

	// nothing to warn about with no data, and nothing left to warn before:
	// out of balance is the out-of-balance notice's
	if startByteCount <= 0 || availableByteCount <= 0 {
		return nil, changed
	}

	var warned *threshold
	for i, t := range thresholds {
		if _, shown := state.ShownBalanceIds[t.kind]; shown {
			continue
		}
		if t.byteCount < availableByteCount {
			continue
		}
		// every threshold this reading is past is warned, with one warning
		if state.ShownBalanceIds == nil {
			state.ShownBalanceIds = map[string][]string{}
		}
		state.ShownBalanceIds[t.kind] = slices.Clone(balanceIds)
		changed = true
		if warned == nil || t.byteCount < warned.byteCount {
			warned = &thresholds[i]
		}
	}
	if warned == nil {
		return nil, changed
	}

	availablePercent := (availableByteCount*100 + startByteCount - 1) / startByteCount
	availablePercent = min(max(availablePercent, 1), 100)
	return &LowDataWarning{
		Kind:                  warned.kind,
		AvailableByteCount:    availableByteCount,
		StartBalanceByteCount: startByteCount,
		AvailablePercent:      int32(availablePercent),
	}, changed
}
