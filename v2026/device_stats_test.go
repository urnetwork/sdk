package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestDeviceStats(t *testing.T) {
	// create stats
	// open, wait, close
	// open
	// send data
	// receive data
	// wait
	// close
	// close
	// open
	// close

	// test connect count, max connect time, net data

	deviceStats := newDeviceStats()

	connect.AssertEqual(t, deviceStats.GetConnectCount(), 0)
	connect.AssertEqual(t, deviceStats.GetNetConnectDurationSeconds(), 0)
	connect.AssertEqual(t, deviceStats.GetMaxConnectDurationSeconds(), 0)
	connect.AssertEqual(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(0))
	connect.AssertEqual(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(0))
	connect.AssertEqual(t, deviceStats.GetUserSuccess(), false)

	deviceStats.UpdateConnect(true)
	connectTime1 := time.Now()
	select {
	case <-time.After(1 * time.Second):
	}

	connect.AssertEqual(t, deviceStats.GetConnectCount(), 0)
	connect.AssertEqual(t, deviceStats.GetNetConnectDurationSeconds(), 0)
	connect.AssertEqual(t, deviceStats.GetMaxConnectDurationSeconds(), 0)

	deviceStats.UpdateRemoteSend(ByteCount(1024))
	deviceStats.UpdateRemoteReceive(ByteCount(1024 * 1024))

	deviceStats.UpdateConnect(true)
	connectTime2 := time.Now()

	connect.AssertEqual(t, deviceStats.GetConnectCount(), 1)
	connect.AssertEqual(t, deviceStats.GetNetConnectDurationSeconds(), int(connectTime2.Sub(connectTime1)/time.Second))
	connect.AssertEqual(t, deviceStats.GetMaxConnectDurationSeconds(), int(connectTime2.Sub(connectTime1)/time.Second))
	connect.AssertEqual(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(1024))
	connect.AssertEqual(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(1024*1024))

	select {
	case <-time.After(2 * time.Second):
	}

	deviceStats.UpdateRemoteSend(ByteCount(2 * 1024))
	deviceStats.UpdateRemoteReceive(ByteCount(2 * 1024 * 1024))

	deviceStats.UpdateConnect(false)
	connectTime3 := time.Now()

	connect.AssertEqual(t, deviceStats.GetConnectCount(), 2)
	connect.AssertEqual(t, deviceStats.GetNetConnectDurationSeconds(), int((connectTime2.Sub(connectTime1)+connectTime3.Sub(connectTime2))/time.Second))
	connect.AssertEqual(t, deviceStats.GetMaxConnectDurationSeconds(), int(connectTime3.Sub(connectTime2)/time.Second))
	connect.AssertEqual(t, deviceStats.GetNetRemoteSendByteCount(), ByteCount(1024)+ByteCount(2*1024))
	connect.AssertEqual(t, deviceStats.GetNetRemoteReceiveByteCount(), ByteCount(1024*1024)+ByteCount(2*1024*1024))

}
