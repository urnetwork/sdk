package sdk

import (
	"context"
	"github.com/urnetwork/connect/v2026"
)

// Embedding apps supply their own version and device type before making requests.
type ClientInfo struct {
	Version    int    `json:"v"`
	DeviceType string `json:"device_type"`
	AppVersion string `json:"app_version"`
	SdkVersion string `json:"sdk_version,omitempty"`
}

func NewClientInfo(deviceType, appVersion string) *ClientInfo {
	return &ClientInfo{Version: 1, DeviceType: deviceType, AppVersion: appVersion, SdkVersion: Version}
}
func (self *Api) SetClientInfo(info *ClientInfo) {
	value := connect.UnknownClientInfo()
	if info != nil {
		value = connect.ClientInfo{Version: 1, DeviceType: info.DeviceType, AppVersion: info.AppVersion, SdkVersion: info.SdkVersion}
		if value.SdkVersion == "" {
			value.SdkVersion = Version
		}
		value = connect.ParseClientInfo(value.Json(), "")
	}
	self.mutex.Lock()
	self.clientInfo = value
	self.mutex.Unlock()
}
func (self *Api) GetClientInfo() *ClientInfo {
	self.mutex.Lock()
	value := self.clientInfo
	self.mutex.Unlock()
	if value.Version == 0 {
		value = connect.UnknownClientInfo()
	}
	return &ClientInfo{Version: value.Version, DeviceType: value.DeviceType, AppVersion: value.AppVersion, SdkVersion: value.SdkVersion}
}
func (self *Api) clientInfoContext(ctx context.Context) context.Context {
	info := self.GetClientInfo()
	return connect.WithClientInfo(ctx, connect.ClientInfo{Version: info.Version, DeviceType: info.DeviceType, AppVersion: info.AppVersion, SdkVersion: info.SdkVersion})
}
func (self *Api) connectClientInfo() connect.ClientInfo {
	info := self.GetClientInfo()
	return connect.ClientInfo{Version: info.Version, DeviceType: info.DeviceType, AppVersion: info.AppVersion, SdkVersion: info.SdkVersion}
}
