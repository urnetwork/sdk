package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// a minimal local device for view controller tests.
// no destination or network is needed since the tests only exercise
// device state and listeners.
func testing_newViewControllerDevice(ctx context.Context) (*DeviceLocal, error) {
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		return nil, err
	}
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	return newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
}

type testing_localOverrideAppIdsListener struct {
	stateLock sync.Mutex
	count     int
}

func (self *testing_localOverrideAppIdsListener) LocalOverrideAppIdsChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
}

func (self *testing_localOverrideAppIdsListener) getCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

func TestBlockActionViewControllerLocalOverrideAppIds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	vc := device.OpenBlockActionViewController()
	defer device.CloseViewController(vc)

	listener := &testing_localOverrideAppIdsListener{}
	sub := vc.AddLocalOverrideAppIdsListener(listener)
	defer sub.Close()

	localAppIds := NewStringList()
	localAppIds.Add("com.example.local")
	remoteAppIds := NewStringList()
	remoteAppIds.Add("com.example.remote")
	overrides := NewBlockActionOverrideList()
	overrides.Add(&BlockActionOverride{
		OverrideId:    NewId(),
		AppIds:        localAppIds,
		RouteOverride: &RouteOverride{Local: true},
	})
	overrides.Add(&BlockActionOverride{
		OverrideId:    NewId(),
		AppIds:        remoteAppIds,
		RouteOverride: &RouteOverride{Local: false},
	})
	// the device dispatches the overrides change synchronously
	device.SetBlockActionOverrides(overrides)

	localOverrideAppIds := vc.GetLocalOverrideAppIds()
	connect.AssertEqual(t, 1, localOverrideAppIds.Included.Len())
	connect.AssertEqual(t, 1, localOverrideAppIds.Excluded.Len())
	connect.AssertEqual(t, true, localOverrideAppIds.Included.Contains("com.example.local"))
	connect.AssertEqual(t, false, localOverrideAppIds.Included.Contains("com.example.remote"))
	connect.AssertEqual(t, true, localOverrideAppIds.Excluded.Contains("com.example.remote"))
	connect.AssertEqual(t, false, localOverrideAppIds.Excluded.Contains("com.example.local"))
	connect.AssertEqual(t, 1, listener.getCount())

	// setting the same overrides again must not fire the listener,
	// since the derived sets did not change
	device.SetBlockActionOverrides(overrides)
	connect.AssertEqual(t, 1, listener.getCount())
}

func TestBlockActionViewControllerWindowTrim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, err := testing_newViewControllerDevice(ctx)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	vc := device.OpenBlockActionViewController()
	defer device.CloseViewController(vc)

	windowDuration := time.Duration(vc.GetWindowDurationSeconds()) * time.Second
	now := time.Now()
	expiredBlockAction := &BlockAction{
		BlockActionId: NewId(),
		Time:          now.Add(-2 * windowDuration).UnixMilli(),
		Block:         true,
	}
	freshBlockAction := &BlockAction{
		BlockActionId: NewId(),
		Time:          now.UnixMilli(),
		Block:         false,
		Local:         true,
	}
	blockActions := NewBlockActionList()
	blockActions.Add(expiredBlockAction)
	blockActions.Add(freshBlockAction)
	vc.BlockActionWindowChanged(&BlockActionWindow{
		BlockActions: blockActions,
	})

	windowBlockActions := vc.GetBlockActions()
	connect.AssertEqual(t, 1, windowBlockActions.Len())
	connect.AssertEqual(t, freshBlockAction.BlockActionId, windowBlockActions.Get(0).BlockActionId)
}
