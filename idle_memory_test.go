package sdk

import (
	"testing"
	"time"
)

func TestIdleMemoryTrimmerWaitsForCompleteQuietEpoch(t *testing.T) {
	stop := make(chan struct{})
	activity := make(chan struct{}, 1)
	trimmed := make(chan time.Time, 2)
	done := make(chan struct{})
	const quietDelay = 50 * time.Millisecond
	go func() {
		defer close(done)
		runIdleMemoryTrimmer(stop, activity, quietDelay, func() {
			trimmed <- time.Now()
		})
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle memory trimmer did not stop")
		}
	}()

	time.Sleep(30 * time.Millisecond)
	activityAt := time.Now()
	activity <- struct{}{}
	select {
	case at := <-trimmed:
		t.Fatalf("trim ran only %s after activity", at.Sub(activityAt))
	case <-time.After(30 * time.Millisecond):
	}

	select {
	case at := <-trimmed:
		if elapsed := at.Sub(activityAt); elapsed < quietDelay {
			t.Fatalf("trim ran only %s after activity, want at least %s", elapsed, quietDelay)
		}
	case <-time.After(4 * quietDelay):
		t.Fatal("trim did not run after a complete quiet epoch")
	}

	select {
	case <-trimmed:
		t.Fatal("trim repeated without new activity")
	case <-time.After(2 * quietDelay):
	}

	activity <- struct{}{}
	select {
	case <-trimmed:
	case <-time.After(4 * quietDelay):
		t.Fatal("new activity did not arm another quiet epoch")
	}
}

func TestPacketStatsTrafficActivityThreshold(t *testing.T) {
	stats := &PacketStats{
		RemoteEgressByteCount:  1024,
		RemoteIngressByteCount: 2048,
		LocalEgressByteCount:   512,
		LocalIngressByteCount:  256,
		BlockEgressByteCount:   128,
		BlockIngressByteCount:  64,
	}
	if got, want := packetStatsTrafficByteCount(stats), ByteCount(4032); got != want {
		t.Fatalf("packetStatsTrafficByteCount = %d, want %d", got, want)
	}
	if got := packetStatsTrafficDelta(4000, 4032); got != 32 {
		t.Fatalf("packetStatsTrafficDelta = %d, want 32", got)
	}
	if got := packetStatsTrafficDelta(4032, 4000); got != 0 {
		t.Fatalf("counter reset delta = %d, want 0", got)
	}
	if mobileIdleMemoryActivityMinByteCount != 4*1024 {
		t.Fatalf(
			"mobile activity threshold = %d, want 4096",
			mobileIdleMemoryActivityMinByteCount,
		)
	}
}
