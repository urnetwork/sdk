package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Use the real device boundary with the existing local auth fixture. The
// explicit platform seam avoids depending on the test runner's host OS.
func newSettingsOwnershipDevice(t *testing.T, fixture *testingAuthClientShape, settings *DeviceLocalSettings, mobile bool) *DeviceLocal {
	t.Helper()
	device, err := newDeviceLocalWithOverridesForPlatform(
		fixture.networkSpace, fixture.initialJwt, "settings-owner", "test", "0",
		fixture.instanceId, settings, connect.NewId(), mobile,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	return device
}

// This common control uses only pre-existing APIs. Before the fix, the real
// constructor overwrites the caller's nested budget pointers and accounting.
func TestDeviceLocalReusableSettingsPreservesCallerBudgetContainers(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 32 * 1024 * 1024
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	send := *settings.SendBufferSettings
	receive := *settings.ReceiveBufferSettings
	webRtc := *settings.WebRtcSettings
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	if settings.SendBufferSettings.ResendQueueBudget != send.ResendQueueBudget ||
		settings.ReceiveBufferSettings.ReceiveQueueBudget != receive.ReceiveQueueBudget ||
		settings.ReceiveBufferSettings.PackQueueBudget != receive.PackQueueBudget ||
		settings.WebRtcSettings.MemoryBudget != webRtc.MemoryBudget ||
		settings.WebRtcSettings.NetworkPeerMemoryBudget != webRtc.NetworkPeerMemoryBudget ||
		settings.SendBufferSettings.ResendQueueRetainedByteAccounting != send.ResendQueueRetainedByteAccounting ||
		settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting != receive.ReceiveQueueRetainedByteAccounting {
		t.Fatal("device construction mutated reusable caller budget containers")
	}
	if device.settings.SendBufferSettings == settings.SendBufferSettings ||
		device.settings.ReceiveBufferSettings == settings.ReceiveBufferSettings ||
		device.settings.WebRtcSettings == settings.WebRtcSettings {
		t.Fatal("device retained mutable caller settings containers")
	}
}

// Constructing a second device from the exact same settings must not replace
// the first device's admission path or charge the second owner's root.
func TestDeviceLocalReusableSettingsKeepsLiveOwnersIndependent(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = 32 * 1024 * 1024
	assertDeviceLocalSettingsOwnerIndependence(t, settings, true)
}

// The same real reservation/close proof covers old mobile construction and
// the new host-selected policy without adding any exported test seam.
func assertDeviceLocalSettingsOwnerIndependence(t *testing.T, settings *DeviceLocalSettings, mobile bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings.HostedIncompatible = true
	settings.AllowProvider = false
	settings.DisableLogging = true
	first := newSettingsOwnershipDevice(t, fixture, settings, mobile)
	firstPack := first.settings.ReceiveBufferSettings.PackQueueBudget
	firstSend := first.settings.SendBufferSettings.ResendQueueBudget
	second := newSettingsOwnershipDevice(t, fixture, settings, mobile)
	if first.settings.ReceiveBufferSettings.PackQueueBudget != firstPack ||
		first.settings.SendBufferSettings.ResendQueueBudget != firstSend {
		t.Fatal("second constructor replaced the first live owner's budget pointers")
	}
	secondPack := second.settings.ReceiveBufferSettings.PackQueueBudget
	if firstPack == nil || secondPack == nil || firstPack == secondPack ||
		firstPack.Parent() != first.transferMemory.client ||
		secondPack.Parent() != second.transferMemory.client ||
		first.transferMemory.root == second.transferMemory.root {
		t.Fatal("independent devices share an admission owner")
	}
	firstUsed := first.transferMemory.root.UsedByteCount()
	secondUsed := second.transferMemory.root.UsedByteCount()
	claim := firstPack.TotalByteCount()
	if !firstPack.TryReserve(claim) {
		t.Fatal("fresh first Pack owner could not admit its fixed capacity")
	}
	if firstPack.TryReserve(1) {
		firstPack.Release(1)
		t.Fatal("saturated first Pack owner admitted beyond its capacity")
	}
	if first.transferMemory.root.UsedByteCount() != firstUsed+claim ||
		second.transferMemory.root.UsedByteCount() != secondUsed ||
		!secondPack.TryReserve(claim) {
		firstPack.Release(claim)
		t.Fatal("one owner's saturation consumed another owner's admission")
	}
	secondPack.Release(claim)
	firstPack.Release(claim)
	if err := first.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.ctx.Err() != nil || !secondPack.TryReserve(claim) {
		t.Fatal("closing the first owner disabled the second")
	}
	secondPack.Release(claim)
	if second.transferMemory.root.UsedByteCount() != secondUsed {
		t.Fatal("second owner did not release its exact reservation")
	}
	if err := second.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
}
