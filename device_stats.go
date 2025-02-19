package sdk

import (
	"sync"
	"time"
)

type DeviceStats struct {
	stateLock                 sync.Mutex
	connectEnabled            bool
	connectStartTime          time.Time
	connectCount              int
	netConnectDuration        time.Duration
	maxConnectDuration        time.Duration
	netRemoteSendByteCount    /*ByteCount*/int64
	netRemoteReceiveByteCount /*ByteCount*/int64

	successConnectDuration time.Duration
	successByteCount /*ByteCount*/int64
}

func newDeviceStats() *DeviceStats {
	return &DeviceStats{
		connectEnabled:            false,
		connectStartTime:          time.Time{},
		connectCount:              0,
		netConnectDuration:        time.Duration(0),
		maxConnectDuration:        time.Duration(0),
		netRemoteSendByteCount:    /*ByteCount*/int64(0),
		netRemoteReceiveByteCount: /*ByteCount*/int64(0),

		successConnectDuration: 120 * time.Second,
		successByteCount: /*ByteCount*/int64(64 * 1024 * 1024),
	}
}

func (self *DeviceStats) GetConnectCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.connectCount
}

func (self *DeviceStats) GetNetConnectDurationSeconds() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int(self.netConnectDuration / time.Second)
}

func (self *DeviceStats) GetMaxConnectDurationSeconds() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return int(self.maxConnectDuration / time.Second)
}

func (self *DeviceStats) GetNetRemoteSendByteCount() /*ByteCount*/int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.netRemoteSendByteCount
}

func (self *DeviceStats) GetNetRemoteReceiveByteCount() /*ByteCount*/int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.netRemoteReceiveByteCount
}

func (self *DeviceStats) GetUserSuccess() bool {
	// consider the user successful when all of
	// - max connect time is longer than 30m
	// - receive byte count more than 64mib

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	connectTimeCondition := self.successConnectDuration <= self.netConnectDuration || self.connectEnabled && self.connectStartTime.Add(self.successConnectDuration - self.netConnectDuration).Before(time.Now())
	receiveByteCountCondition := self.successByteCount <= self.netRemoteReceiveByteCount
	return connectTimeCondition && receiveByteCountCondition
}

func (self *DeviceStats) UpdateConnect(connectEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := time.Now()

	if self.connectEnabled {
		self.connectCount += 1
		connectDuration := now.Sub(self.connectStartTime)
		if self.maxConnectDuration < connectDuration {
			self.maxConnectDuration = connectDuration
		}
		self.netConnectDuration += connectDuration
	}

	if connectEnabled {
		self.connectStartTime = now
		self.connectEnabled = true
	} else {
		self.connectEnabled = false
	}
}

func (self *DeviceStats) UpdateRemoteReceive(remoteReceiveByteCount /*ByteCount*/int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.netRemoteReceiveByteCount += remoteReceiveByteCount
}

func (self *DeviceStats) UpdateRemoteSend(remoteSendByteCount /*ByteCount*/int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.netRemoteSendByteCount += remoteSendByteCount
}
