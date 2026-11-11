package sdk

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Literal receipts keep constructor expectations independent of sizing helpers.
type privateH1RetainedSizingRow struct {
	target, root, client, provider, rxOff, rxOn, logicalOff, logicalOn ByteCount
	providerRxOn                                                       ByteCount
	h1Count, genericCount, providerH1Count                             int
	h1Bytes, genericBytes, logicalMax                                  ByteCount
}

func privateH1RetainedSizingRows() []privateH1RetainedSizingRow {
	return []privateH1RetainedSizingRow{
		{target: 32 << 20, root: 21810380, client: 15099494, provider: 6710886,
			rxOff: 7633633, rxOn: 7340032, logicalOff: 2181038, logicalOn: 2097152, providerRxOn: 1917396,
			h1Count: 256, genericCount: 21, providerH1Count: 85, h1Bytes: 174762, genericBytes: 174762, logicalMax: 2796202},
		{target: 64 << 20, root: 43620760, client: 30198988, provider: 13421772,
			rxOff: 15267266, rxOn: 14680064, logicalOff: 4362076, logicalOn: 4194304, providerRxOn: 3834792,
			h1Count: 512, genericCount: 42, providerH1Count: 170, h1Bytes: 262144, genericBytes: 262144, logicalMax: 5592405},
	}
}

// Reflection leaves the common constructor roots compilable against the old
// product. A missing optional conversion is the old identity policy.
func privateH1RetainedScale(settings *connect.ReceiveBufferSettings) (uint64, uint64) {
	value := reflect.ValueOf(settings).Elem()
	numerator := value.FieldByName("AdvertisedReceiveWindowBudgetScaleNumerator")
	denominator := value.FieldByName("AdvertisedReceiveWindowBudgetScaleDenominator")
	if !numerator.IsValid() || !denominator.IsValid() {
		return 0, 0
	}
	return numerator.Uint(), denominator.Uint()
}

// The actual serving Client keeps immutable settings privately. Read only
// scalars and owner addresses; no typed pointer or mutation crosses the package
// boundary and old products retain identity defaults.
func privateAssertH1RetainedProviderServingPolicy(t *testing.T, device *DeviceLocal, count int) {
	t.Helper()
	receive := reflect.ValueOf(device.provider.Client()).Elem().FieldByName("settings").Elem().FieldByName("ReceiveBufferSettings").Elem()
	if receive.FieldByName("H1SequenceBufferSize").Int() != int64(count) {
		t.Fatal("actual provider generation inherited the enlarged consumer H1 depth")
	}
	_, rx := device.provider.transferBudgets()
	if receive.FieldByName("ReceiveQueueBudget").Pointer() != reflect.ValueOf(rx).Pointer() ||
		receive.FieldByName("ReceiveHeadRecoveryBudget").Pointer() != reflect.ValueOf(device.transferMemory.providerHeadRecovery).Pointer() ||
		receive.FieldByName("PackQueueBudget").Pointer() != reflect.ValueOf(device.settings.ReceiveBufferSettings.PackQueueBudget).Pointer() {
		t.Fatal("actual provider generation lost its own RX/Head or shared Pack identity")
	}
	for _, name := range []string{"AdvertisedReceiveWindowBudgetScaleNumerator", "AdvertisedReceiveWindowBudgetScaleDenominator"} {
		field := receive.FieldByName(name)
		if field.IsValid() && field.Uint() != 0 {
			t.Fatal("actual provider generation inherited consumer physical-to-wire units")
		}
	}
}

// The actual platform seam owns construction; the existing local auth fixture
// prevents any external service from choosing this test's policy or lifetime.
func privateH1RetainedSizingDevice(t *testing.T, target ByteCount, provider, mobile bool) *DeviceLocal {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings := defaultDeviceLocalSettingsWithProfile(target, true)
	settings.AllowProvider = provider
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, fixture, settings, mobile)
	if device.provider != nil {
		device.provider.platformTransport.Close()
	}
	return device
}

// Check immutable capacities and exact parent identity, not transient free
// bytes from unrelated control workers sharing the same device root.
func privateAssertH1RetainedSizing(t *testing.T, device *DeviceLocal, settings *connect.ClientSettings, row privateH1RetainedSizingRow, active bool) {
	t.Helper()
	receive := settings.ReceiveBufferSettings
	memory := device.transferMemory
	rx, logical := row.rxOff, row.logicalOff
	clientTotal, providerTotal := row.root, ByteCount(deviceLocalProviderIdleTransferByteCount)
	if active {
		rx, logical = row.rxOn, row.logicalOn
		clientTotal, providerTotal = row.client, row.provider/2
	}
	if memory == nil || memory.root.TotalByteCount() != row.root || memory.client.TotalByteCount() != clientTotal ||
		memory.provider.TotalByteCount() != providerTotal || memory.nat.TotalByteCount() != row.provider/2 {
		t.Fatal("H1 backing changed an existing device parent or NAT capacity")
	}
	if receive.ReceiveQueueBudget == nil || receive.ReceiveHeadRecoveryBudget == nil || receive.PackQueueBudget == nil ||
		receive.ReceiveQueueBudget.Parent() != memory.client || receive.ReceiveHeadRecoveryBudget.Parent() != memory.client ||
		receive.PackQueueBudget.Parent() != memory.client || memory.client.Parent() != memory.root ||
		receive.ReceiveQueueBudget.TotalByteCount() != rx || receive.ReceiveHeadRecoveryBudget.TotalByteCount() != logical ||
		receive.PackQueueBudget.TotalByteCount() != logical {
		t.Fatal("actual RX/Head/Pack capacities or parent identities changed")
	}
	if receive.ReceiveQueueMaxByteCount != row.logicalMax || receive.AdvertisedReceiveWindowMaxByteCount != 0 ||
		receive.ReceiveQueueMinByteCount != 0 || !receive.ReceiveQueueRetainedByteAccounting ||
		receive.SequenceBufferSize != row.genericCount || receive.SequenceBufferByteCount != row.genericBytes ||
		receive.H1SequenceBufferSize != row.h1Count || receive.H1SequenceBufferByteCount != row.h1Bytes ||
		receive.H1SequenceBufferAdaptiveMaxSize != 0 || receive.H1SequenceBufferAdaptiveStepSize != 0 ||
		receive.H1SequenceBufferAdaptiveMaxByteCount != 0 || receive.H1SequenceBufferAdaptiveStepByteCount != 0 {
		t.Fatal("H1 sizing changed generic/byte/hold policy or lost its bounded H1 count")
	}
	if settings.SendBufferSettings.ResendQueueBudget.Parent() != memory.client ||
		!settings.SendBufferSettings.ResendQueueRetainedByteAccounting {
		t.Fatal("H1 receive backing changed sender ownership")
	}
	if numerator, denominator := privateH1RetainedScale(receive); numerator != 2 || denominator != 7 {
		t.Fatal("actual enlarged RX lost its static physical-to-wire byte conversion")
	}
}

// Default Auto prefers H1; construction must not require an explicit mode
// setter before its first real destination obtains adequate H1 capacity.
func TestPrivateH1RetainedSizingActualAutoConstructors32And64(t *testing.T) {
	for _, row := range privateH1RetainedSizingRows() {
		t.Run(fmt.Sprint(row.target>>20), func(t *testing.T) {
			device := privateH1RetainedSizingDevice(t, row.target, true, true)
			if device.GetTransportSettings().Mode != TransportModeAuto {
				t.Fatal("constructor control no longer exercises default Auto")
			}
			receive, memory := device.settings.ReceiveBufferSettings, device.transferMemory
			if memory == nil || memory.root.TotalByteCount() != row.root || memory.client.TotalByteCount() != row.root ||
				receive.ReceiveQueueBudget.Parent() != memory.client || receive.ReceiveHeadRecoveryBudget.Parent() != memory.client ||
				receive.ReceiveHeadRecoveryBudget.TotalByteCount() != row.logicalOff || receive.PackQueueBudget.TotalByteCount() != row.logicalOff ||
				receive.SequenceBufferSize != row.genericCount || receive.SequenceBufferByteCount != row.genericBytes || receive.H1SequenceBufferByteCount != row.h1Bytes {
				t.Fatal("actual default Auto constructor changed an adjacent parent/Head/Pack/generic/byte premise")
			}
			physical, count := receive.ReceiveQueueBudget.TotalByteCount(), receive.H1SequenceBufferSize
			numerator, denominator := privateH1RetainedScale(receive)
			missing := physical != row.rxOff || count != row.h1Count || numerator != 2 || denominator != 7
			if !missing {
				privateAssertH1RetainedSizing(t, device, &device.settings.ClientSettings, row, false)
			}
			_, providerRx := device.provider.transferBudgets()
			if providerRx.Parent() != device.transferMemory.provider ||
				providerRx.TotalByteCount() != 384<<10 ||
				device.transferMemory.providerHeadRecovery.Parent() != device.transferMemory.provider ||
				device.transferMemory.providerHeadRecovery.TotalByteCount() != 384<<10 ||
				providerRx == device.settings.ReceiveBufferSettings.ReceiveQueueBudget {
				t.Fatal("consumer backing leaked into the actual provider-owned RX/Head")
			}
			privateAssertH1RetainedProviderServingPolicy(t, device, row.providerH1Count)
			privateH1RetainedSizingClose(t, device)
			t.Logf("SDK_H1_RETAINED_CONSTRUCTOR target=%d default_auto=true physical_rx=%d expected_physical_rx=%d logical_head=%d h1_count=%d expected_h1_count=%d scale=%d/%d custody_drained=true",
				row.target, physical, row.rxOff, row.logicalOff, count, row.h1Count, numerator, denominator)
			if missing {
				t.Error("SDK_H1_RETAINED_BACKING_MISSING: actual default Auto constructor kept insufficient physical RX/H1 count or identity wire conversion; joined custody completed")
			}
		})
	}
}

// An odd logical byte ceiling distinguishes physical ceil(7*logical/2) from
// a floor that would silently remove one byte of the old wire permission.
func TestPrivateH1RetainedSizingOddTargetActualConstructor(t *testing.T) {
	const target ByteCount = 32<<20 + 20
	device := privateH1RetainedSizingDevice(t, target, false, false)
	receive, memory := device.settings.ReceiveBufferSettings, device.transferMemory
	if memory == nil || memory.root.TotalByteCount() != 21810393 || memory.client.TotalByteCount() != 21810393 ||
		receive.ReceiveQueueBudget.Parent() != memory.client || receive.ReceiveHeadRecoveryBudget.Parent() != memory.client ||
		receive.ReceiveHeadRecoveryBudget.TotalByteCount() != 2181039 || receive.PackQueueBudget.TotalByteCount() != 2181039 {
		t.Fatal("odd actual constructor lost its unchanged root/logical Head/Pack geometry")
	}
	physical, count := receive.ReceiveQueueBudget.TotalByteCount(), receive.H1SequenceBufferSize
	numerator, denominator := privateH1RetainedScale(receive)
	privateH1RetainedSizingClose(t, device)
	t.Logf("SDK_H1_RETAINED_ODD_CONSTRUCTOR target=%d physical_rx=%d logical_head=2181039 h1_count=%d scale=%d/%d custody_drained=true",
		target, physical, count, numerator, denominator)
	if physical != 7633637 || count != 256 || numerator != 2 || denominator != 7 {
		t.Error("SDK_H1_RETAINED_ODD_BACKING_MISSING: actual odd logical constructor did not preserve exact old permission through physical ceil; joined custody completed")
	}
}

// The production generator must copy the shared owners and scalar policy.
// A live paid RX claim spans both real destination generations.
func TestPrivateH1RetainedSizingGeneratedSettingsPreserveLiveOwnersAndProfile(t *testing.T) {
	for _, row := range privateH1RetainedSizingRows() {
		t.Run(fmt.Sprint(row.target>>20), func(t *testing.T) {
			device := privateH1RetainedSizingDevice(t, row.target, true, false)
			profile := &PerformanceProfile{WindowType: WindowTypeSpeed,
				WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2, WindowSizeReconnectScale: 1}}
			device.SetPerformanceProfile(profile)
			upgrade := connect.DefaultUpgradeMuxSettings()
			upgrade.Dns = nil
			device.SetUpgradeMuxSettings(upgrade)
			rx := device.settings.ReceiveBufferSettings.ReceiveQueueBudget
			head := device.settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget
			pack := device.settings.ReceiveBufferSettings.PackQueueBudget
			if !rx.TryReserve(row.rxOff) {
				t.Fatal("actual fresh RX owner could not admit its bounded physical capacity")
			}
			claimed := true
			t.Cleanup(func() {
				if claimed {
					rx.Release(row.rxOff)
				}
			})
			before := rx.Stats()
			var previous *connect.ApiMultiClientGenerator
			for range 2 {
				device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
				device.stateLock.Lock()
				generator := device.apiMultiClientGenerator
				device.stateLock.Unlock()
				if generator == nil || generator == previous {
					t.Fatal("real destination did not construct its next built-in settings generator")
				}
				for range 2 {
					settings := generator.NewClientSettings()
					privateAssertH1RetainedSizing(t, device, settings, row, false)
					if settings.ReceiveBufferSettings.ReceiveQueueBudget != rx || settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != head ||
						settings.ReceiveBufferSettings.PackQueueBudget != pack || !reflect.DeepEqual(rx.Stats(), before) ||
						!reflect.DeepEqual(device.GetPerformanceProfile(), profile) || settings.SendBufferSettings.LogicalDataLaneCount != 0 {
						t.Fatal("generated settings resized/replaced a live shared owner or altered Auto/profile policy")
					}
				}
				previous = generator
				device.SetConnectLocation(nil)
			}
			rx.Release(row.rxOff)
			claimed = false
			privateH1RetainedSizingClose(t, device)
		})
	}
}

// Actual public role and carrier transitions resize the same charged owners.
// Shrinking a leaf cannot discard old bytes or fund another physical claim.
func TestPrivateH1RetainedSizingRoleAndCarrierChangesPreservePaidOwners(t *testing.T) {
	for _, row := range privateH1RetainedSizingRows() {
		t.Run(fmt.Sprint(row.target>>20), func(t *testing.T) {
			device := privateH1RetainedSizingDevice(t, row.target, true, false)
			receive := device.settings.ReceiveBufferSettings
			rx, head, pack := receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget, receive.PackQueueBudget
			_, providerRx := device.provider.transferBudgets()
			providerHead := device.transferMemory.providerHeadRecovery
			if !rx.TryReserve(row.rxOff) {
				t.Fatal("actual role control could not pay for its initial RX owner")
			}
			claimed := true
			t.Cleanup(func() {
				if claimed {
					rx.Release(row.rxOff)
				}
			})
			device.SetProvideMode(ProvideModeNetwork)
			privateAssertH1RetainedSizing(t, device, &device.settings.ClientSettings, row, true)
			privateAssertH1RetainedProviderServingPolicy(t, device, row.providerH1Count)
			if providerRx.TotalByteCount() != row.providerRxOn || providerHead.TotalByteCount() != row.providerRxOn ||
				rx.UsedByteCount() != row.rxOff {
				t.Fatal("active role lost old RX custody or changed provider RX/Head policy")
			}
			privateH1RetainedSizingRefuses(t, rx)
			for _, mode := range []TransportMode{TransportModeH1, TransportModeH3, TransportModeAuto} {
				device.SetTransportSettings(&TransportSettings{Mode: mode})
				privateAssertH1RetainedSizing(t, device, &device.settings.ClientSettings, row, true)
				if receive.ReceiveQueueBudget != rx || receive.ReceiveHeadRecoveryBudget != head || receive.PackQueueBudget != pack ||
					rx.UsedByteCount() != row.rxOff {
					t.Fatal("carrier selection mutated/replaced a paid aggregate owner")
				}
				privateH1RetainedSizingRefuses(t, rx)
			}
			device.SetProvideMode(ProvideModeNone)
			privateAssertH1RetainedSizing(t, device, &device.settings.ClientSettings, row, false)
			if providerRx.TotalByteCount() != 384<<10 || providerHead.TotalByteCount() != 384<<10 ||
				rx.UsedByteCount() != row.rxOff {
				t.Fatal("returning to idle changed provider floors or duplicated RX permission")
			}
			privateH1RetainedSizingRefuses(t, rx)
			rx.Release(row.rxOff)
			claimed = false
			privateH1RetainedSizingClose(t, device)
		})
	}
}

// An unexpected successful probe still owns bytes and must return them before
// the failure unwinds the full paid owner registered above it.
func privateH1RetainedSizingRefuses(t *testing.T, budget *connect.TransferMemoryBudget) {
	t.Helper()
	if budget.TryReserve(1) {
		budget.Release(1)
		t.Fatal("a saturated/shrunken physical RX owner admitted an extra byte")
	}
}

// Provider construction replaces the RX owner after cloning consumer
// settings, so the physical-to-wire conversion must be detached there too.
func TestPrivateH1RetainedSizingProviderCloneKeepsItsOwnUnits(t *testing.T) {
	for _, row := range privateH1RetainedSizingRows() {
		t.Run(fmt.Sprint(row.target>>20), func(t *testing.T) {
			device := privateH1RetainedSizingDevice(t, row.target, true, false)
			provider := newDeviceClientSettings(&device.settings.ClientSettings, "", nil)
			pack := provider.ReceiveBufferSettings.PackQueueBudget
			if numerator, denominator := privateH1RetainedScale(provider.ReceiveBufferSettings); numerator != 2 || denominator != 7 {
				t.Fatal("real settings clone lost the consumer scalar policy before provider replacement")
			}
			// A separate empty owner tree lets this exact production helper
			// configure a new generation without resizing the serving device.
			memory := newDeviceLocalTransferMemoryForPlatform(device.settings, true)
			_, rx := configureDeviceLocalProviderMemory(provider, row.provider, memory.provider)
			memory.configureProviderHeadRecovery(provider)
			if numerator, denominator := privateH1RetainedScale(provider.ReceiveBufferSettings); numerator != 0 || denominator != 0 {
				t.Fatal("provider-owned RX inherited the consumer physical-to-wire conversion")
			}
			if rx.TotalByteCount() != row.providerRxOn || rx.Parent() != memory.provider ||
				provider.ReceiveBufferSettings.ReceiveHeadRecoveryBudget != memory.providerHeadRecovery ||
				memory.providerHeadRecovery.TotalByteCount() != row.providerRxOn ||
				provider.ReceiveBufferSettings.PackQueueBudget != pack ||
				device.transferMemory.providerHeadRecovery.TotalByteCount() != 384<<10 || memory.root.UsedByteCount() != 0 {
				t.Fatal("provider replacement changed its original RX/Head or the shared Pack owner")
			}
			privateH1RetainedSizingClose(t, device)
		})
	}
}

// Android's settings are commonly constructed at24 MiB, then given64 MiB by
// the app before DeviceLocal construction. Its provisional depth is a default,
// while an explicit caller depth of8 must survive both real factory stages.
func TestPrivateH1RetainedSizingAndroidTargetOverrideAndCallerDepth(t *testing.T) {
	for _, explicit := range []int{0, 8} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			row := privateH1RetainedSizingRows()[1]
			fixture := testingAuthClientShapeSpace(t)
			settings := defaultDeviceLocalSettingsForPlatform("android")
			if settings.MemoryTargetByteCount != 24<<20 {
				t.Fatal("Android constructor premise lost its original24 MiB profile")
			}
			settings.MemoryTargetByteCount = 64 << 20
			settings.DisableLogging = true
			if explicit != 0 {
				settings.ReceiveBufferSettings.H1SequenceBufferSize = explicit
				row.h1Count = explicit
			}
			callerSend, callerReceive := *settings.SendBufferSettings, *settings.ReceiveBufferSettings
			device := newSettingsOwnershipDevice(t, fixture, settings, true)
			device.provider.platformTransport.Close()
			baseRow := row
			baseRow.logicalMax = min(baseRow.logicalMax, callerReceive.ReceiveQueueMaxByteCount)
			privateAssertH1RetainedSizing(t, device, &device.settings.ClientSettings, baseRow, false)
			privateAssertH1RetainedProviderServingPolicy(t, device, min(170, row.h1Count))
			if !reflect.DeepEqual(*settings.SendBufferSettings, callerSend) || !reflect.DeepEqual(*settings.ReceiveBufferSettings, callerReceive) {
				t.Fatal("actual target override mutated the reusable caller settings containers")
			}
			upgrade := connect.DefaultUpgradeMuxSettings()
			upgrade.Dns = nil
			device.SetUpgradeMuxSettings(upgrade)
			device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
			device.stateLock.Lock()
			generator := device.apiMultiClientGenerator
			device.stateLock.Unlock()
			if generator == nil {
				t.Fatal("actual Android override did not create its built-in window settings factory")
			}
			privateAssertH1RetainedSizing(t, device, generator.NewClientSettings(), row, false)
			device.SetConnectLocation(nil)
			privateH1RetainedSizingClose(t, device)
		})
	}
}

// Smaller low-memory profiles and ordinary desktop/targetless construction
// retain their old units and queue depths.
func TestPrivateH1RetainedSizingLowerAndLegacyProfilesRemainUnchanged(t *testing.T) {
	for _, test := range []struct {
		name   string
		target ByteCount
		low    bool
		rx     ByteCount
		count  int
	}{
		{name: "low24", target: 24 << 20, low: true, rx: 1635778, count: 64},
		{name: "low28", target: 28 << 20, low: true, rx: 1908408, count: 74},
		{name: "desktop32", target: 32 << 20, rx: 12463074, count: 256},
		{name: "desktop64", target: 64 << 20, rx: 24926148, count: 256},
		{name: "targetless", low: true, count: 256},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := testingAuthClientShapeSpace(t)
			settings := defaultDeviceLocalSettingsWithProfile(test.target, test.low)
			settings.AllowProvider = false
			settings.DisableLogging = true
			device := newSettingsOwnershipDevice(t, fixture, settings, false)
			receive := device.settings.ReceiveBufferSettings
			wantRx := test.rx
			wantCount := test.count
			if test.target == 0 {
				wantRx = connect.MemoryScaledByteCount(8<<20, 1536<<10)
				wantCount = connect.MemoryScaledCount(256, 32)
			}
			if receive.ReceiveQueueBudget.TotalByteCount() != wantRx || receive.H1SequenceBufferSize != wantCount {
				t.Fatal("lower/legacy constructor changed its original RX capacity or H1 depth")
			}
			if numerator, denominator := privateH1RetainedScale(receive); numerator != 0 || denominator != 0 {
				t.Fatal("lower/legacy constructor acquired a retained-to-wire conversion")
			}
			if (device.transferMemory != nil) != (test.low && test.target > 0) {
				t.Fatal("new H1 sizing changed opt-in hierarchy selection")
			}
			if device.transferMemory != nil && receive.ReceiveHeadRecoveryBudget.TotalByteCount() != wantRx {
				t.Fatal("lower profile Head no longer follows its original logical RX")
			}
			privateH1RetainedSizingClose(t, device)
		})
	}
}

// Constructor controls qualify graph/policy ownership, not a physical-device
// memory result. All asynchronous device owners must nevertheless join.
func privateH1RetainedSizingClose(t *testing.T, device *DeviceLocal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := device.CloseAndWait(ctx); err != nil {
		t.Fatal("actual H1 sizing device failed joined close", err)
	}
	budgets := []*connect.TransferMemoryBudget{device.settings.SendBufferSettings.ResendQueueBudget,
		device.settings.ReceiveBufferSettings.ReceiveQueueBudget, device.settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget,
		device.settings.ReceiveBufferSettings.PackQueueBudget}
	if memory := device.transferMemory; memory != nil {
		budgets = append(budgets, memory.providerHeadRecovery, memory.client, memory.provider, memory.root)
	}
	for _, budget := range budgets {
		if budget == nil {
			continue
		}
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatal("actual H1 sizing constructor retained unjoined custody", stats)
		}
	}
}
