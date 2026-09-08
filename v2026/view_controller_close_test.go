package sdk

import (
	"context"
	"testing"
)

func newViewControllerCloseTestDevice(t *testing.T) *DeviceRemote {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, device := testing_newSyncedDeviceLocalRemote(t, ctx)
	return device
}

func requireNoOwnedViewControllers(t *testing.T, device *DeviceRemote) {
	t.Helper()
	if count := openedViewControllerCount(device); count != 0 {
		t.Fatalf("opened view controllers=%d, want 0", count)
	}
}

func TestCloseConcreteConnectViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseConnectViewController(device.OpenConnectViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcreteContractViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseContractViewController(device.OpenContractViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcreteContractDetailsViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseContractDetailsViewController(device.OpenClientContractDetailsViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcreteBlockActionViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseBlockActionViewController(device.OpenBlockActionViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcreteLocationsViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseLocationsViewController(device.OpenLocationsViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcretePeerViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.ClosePeerViewController(device.OpenPeerViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcreteDevicesViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseDevicesViewController(device.OpenDevicesViewController())

	requireNoOwnedViewControllers(t, device)
}

func TestCloseConcretePostQuantumIdentityViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.ClosePostQuantumIdentityViewController(device.OpenPostQuantumIdentityViewController())

	requireNoOwnedViewControllers(t, device)
}
