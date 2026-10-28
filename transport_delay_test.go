package sdk

// transport_delay_test.go -- the transport quality (transport_delay.go,
// device_local_transport_delay.go, connect/DELAY.md §10 as calibrated by the
// thresholds report): the quality scores and the level boundaries, the
// per-second reading of the link, the second-worst window statistic, the
// release hysteresis, the DNS fallback by byte share, expiry, the generation
// fence, and the readout on a local device and across the device rpc.
//
// The window tests place samples in whole wall-clock seconds of their choosing.
// The device tests feed a multi client generation through the entry its
// subscription calls, since a multi client publishes only what its window
// transports measure.

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// a direction of a sample with no standing queue reading
const testTransportDelayNoQueueDelay = time.Duration(-1)

// The device settings the window reads, at their defaults.
func testTransportDelaySettings() *DeviceLocalSettings {
	return &DeviceLocalSettings{
		EnableTransportDelay:                true,
		TransportDelayWindowCount:           defaultTransportDelayWindowCount,
		TransportDelayWindowTimeout:         defaultTransportDelayWindowTimeout,
		TransportDelayGoodUpBitsPerSecond:   defaultTransportDelayGoodUpBitsPerSecond,
		TransportDelayBadUpBitsPerSecond:    defaultTransportDelayBadUpBitsPerSecond,
		TransportDelayGoodDownBitsPerSecond: defaultTransportDelayGoodDownBitsPerSecond,
		TransportDelayBadDownBitsPerSecond:  defaultTransportDelayBadDownBitsPerSecond,
		TransportDelayGoodUpQueueDelay:      defaultTransportDelayGoodUpQueueDelay,
		TransportDelayGoodDownQueueDelay:    defaultTransportDelayGoodDownQueueDelay,
		TransportDelayBadUpQueueDelay:       defaultTransportDelayBadUpQueueDelay,
		TransportDelayBadDownQueueDelay:     defaultTransportDelayBadDownQueueDelay,
		TransportDelayQualityStep:           defaultTransportDelayQualityStep,
		TransportDelayQualityHysteresis:     defaultTransportDelayQualityHysteresis,
	}
}

// A window taking the samples of generation 1 of a stand-in multi client,
// which only serves as the generation's identity.
func testTransportDelayWindowWithSettings(settings *DeviceLocalSettings) (*transportDelayWindow, *connect.RemoteUserNatMultiClient) {
	window := newTransportDelayWindow(settings)
	multi := new(connect.RemoteUserNatMultiClient)
	window.reset(1, multi)
	return window, multi
}

func testTransportDelayWindow() (*transportDelayWindow, *connect.RemoteUserNatMultiClient) {
	return testTransportDelayWindowWithSettings(testTransportDelaySettings())
}

// A whole second on the wall clock, which the window buckets by.
func testTransportDelayBase() time.Time {
	return time.Unix(time.Now().Unix(), 0)
}

// The middle of the second-th second after base.
func testTransportDelayAt(base time.Time, second int) time.Time {
	return base.Add(time.Duration(second)*time.Second + 500*time.Millisecond)
}

// An interval of one second of one connection that ends at endTime, with no
// reading yet.
func testTransportDelayStats(connectionId connect.Id, transportType connect.TransportType, endTime time.Time) *connect.TransportDelayStats {
	return &connect.TransportDelayStats{
		ConnectionId:  connectionId,
		TransportType: transportType,
		StartTime:     endTime.Add(-time.Second),
		EndTime:       endTime,
	}
}

// An interval whose writers the link held to these rates (0 for not held),
// with the bytes of a second at each rate.
func testTransportDelayLimited(
	connectionId connect.Id,
	transportType connect.TransportType,
	endTime time.Time,
	upBitsPerSecond int64,
	downBitsPerSecond int64,
) *connect.TransportDelayStats {
	stats := testTransportDelayStats(connectionId, transportType, endTime)
	stats.SendBitsPerSecond = upBitsPerSecond
	stats.SendByteCount = upBitsPerSecond / 8
	stats.ReceiveBitsPerSecond = downBitsPerSecond
	stats.ReceiveByteCount = downBitsPerSecond / 8
	return stats
}

// An interval with these standing queues (testTransportDelayNoQueueDelay for
// none) and a few bytes each way.
func testTransportDelayQueue(
	connectionId connect.Id,
	transportType connect.TransportType,
	endTime time.Time,
	upQueueDelay time.Duration,
	downQueueDelay time.Duration,
) *connect.TransportDelayStats {
	stats := testTransportDelayStats(connectionId, transportType, endTime)
	stats.SendByteCount = 1000
	stats.SendQueueDelay = max(upQueueDelay, 0)
	stats.SendQueueDelayValid = 0 <= upQueueDelay
	stats.ReceiveByteCount = 1000
	stats.ReceiveQueueDelay = max(downQueueDelay, 0)
	stats.ReceiveQueueDelayValid = 0 <= downQueueDelay
	return stats
}

func testTransportDelayAdd(t *testing.T, window *transportDelayWindow, multi *connect.RemoteUserNatMultiClient, stats *connect.TransportDelayStats) {
	t.Helper()
	if !window.add(1, multi, stats, stats.EndTime) {
		t.Fatalf("the window dropped a live sample of its generation: %+v", stats)
	}
}

func testTransportDelayNear(t *testing.T, name string, got float64, expected float64) {
	t.Helper()
	if math.Abs(got-expected) > 1e-9 {
		t.Fatalf("%s = %v, expected %v", name, got, expected)
	}
}

// q_throughput on its ratio scale: 0 without a reading and from the good rate
// up, 1 from the bad rate down, and half way at their geometric mean.
func TestTransportDelayThroughputQuality(t *testing.T) {
	const good = 1_500_000
	const bad = 350_000
	testTransportDelayNear(t, "no reading", transportDelayThroughputQuality(0, good, bad), 0)
	testTransportDelayNear(t, "the good rate", transportDelayThroughputQuality(good, good, bad), 0)
	testTransportDelayNear(t, "above the good rate", transportDelayThroughputQuality(10*good, good, bad), 0)
	testTransportDelayNear(t, "the bad rate", transportDelayThroughputQuality(bad, good, bad), 1)
	testTransportDelayNear(t, "below the bad rate", transportDelayThroughputQuality(1, good, bad), 1)
	geometricMean := int64(math.Round(math.Sqrt(float64(good) * float64(bad))))
	if quality := transportDelayThroughputQuality(geometricMean, good, bad); math.Abs(quality-0.5) > 1e-4 {
		t.Fatalf("the geometric mean of the thresholds = %v, expected 0.5", quality)
	}
	// a bad threshold of 0 still bounds the scale, at the lowest rate a
	// sample reports
	testTransportDelayNear(t, "a zero bad threshold", transportDelayThroughputQuality(1, good, 0), 1)
	if quality := transportDelayThroughputQuality(1000, good, 0); !(0 < quality && quality < 1) {
		t.Fatalf("a zero bad threshold = %v, expected a quality inside (0, 1)", quality)
	}
}

// q_delay is linear between the thresholds.
func TestTransportDelayQueueQuality(t *testing.T) {
	const good = 500 * time.Millisecond
	const bad = 2500 * time.Millisecond
	testTransportDelayNear(t, "no queue", transportDelayQueueQuality(0, good, bad), 0)
	testTransportDelayNear(t, "the good queue", transportDelayQueueQuality(good, good, bad), 0)
	testTransportDelayNear(t, "half way", transportDelayQueueQuality(1500*time.Millisecond, good, bad), 0.5)
	testTransportDelayNear(t, "the bad queue", transportDelayQueueQuality(bad, good, bad), 1)
	testTransportDelayNear(t, "past the bad queue", transportDelayQueueQuality(10*time.Second, good, bad), 1)
}

// The level is 0 exactly at a quality of 0 and any other quality rounds up to
// a step. A worse level is taken at once at every level; a better one only
// once the quality is the hysteresis below the level's band.
func TestTransportDelayLevel(t *testing.T) {
	const step = 0.25
	const hysteresis = 0.1
	for _, c := range []struct {
		name     string
		quality  float64
		level    float64
		expected float64
	}{
		{"zero is zero", 0, 0, 0},
		{"any quality leaves 0", 1e-12, 0, 0.25},
		{"a quality on a step", 0.25, 0, 0.25},
		{"a quality just past a step", 0.2500001, 0, 0.5},
		{"the top", 1, 0, 1},
		// attack: a worse level at once, from every level
		{"attack one band", 0.51, 0.5, 0.75},
		{"attack past a band", 0.9, 0.25, 1},
		{"attack from the band's top", 0.2500001, 0.25, 0.5},
		// release from 0.5, whose band is (0.25, 0.5]: at 0.15 or below
		{"inside the band", 0.3, 0.5, 0.5},
		{"inside the hysteresis below", 0.16, 0.5, 0.5},
		{"the hysteresis below the band", 0.15, 0.5, 0.25},
		{"a fall of more than a band", 0.01, 1, 0.25},
		// returning to 0 needs a quality of 0, whatever the hysteresis
		{"the lowest level holds above 0", 1e-12, 0.25, 0.25},
		{"the lowest level returns to 0", 0, 0.25, 0},
		{"a level returns to 0 at once", 0, 1, 0},
		{"the top holds inside the hysteresis", 0.66, 1, 1},
		{"the top releases past it", 0.64, 1, 0.75},
	} {
		testTransportDelayNear(t, c.name, transportDelayLevel(c.quality, c.level, step, hysteresis), c.expected)
	}

	// a step that does not divide 1 clamps the top level to 1, whose band is
	// (0.9, 1]
	testTransportDelayNear(t, "a clamped top", transportDelayLevel(0.95, 0, 0.3, hysteresis), 1)
	testTransportDelayNear(t, "a clamped top's hysteresis", transportDelayLevel(0.81, 1, 0.3, hysteresis), 1)
	testTransportDelayNear(t, "below a clamped top's hysteresis", transportDelayLevel(0.79, 1, 0.3, hysteresis), 0.9)
	// float noise on a step boundary does not round up a whole step
	testTransportDelayNear(t, "noise on a step", transportDelayLevel(0.1+0.2, 0, 0.1, 0), 0.3)
	// without hysteresis a level releases as soon as the quality leaves its
	// band
	testTransportDelayNear(t, "no hysteresis", transportDelayLevel(0.24, 0.5, step, 0), 0.25)
}

// The level boundaries the report sets with the default thresholds: 1043,
// 725 and 504 kbit/s down, 521, 362 and 252 kbit/s up, and 1.0, 1.5 and 2.0 s
// of standing queue in either direction, between the levels 0.25, 0.5, 0.75
// and 1, read through the window from two sampled seconds.
func TestTransportDelayLevelBoundaries(t *testing.T) {
	level := func(stats func(endTime time.Time) *connect.TransportDelayStats) *TransportDelay {
		t.Helper()
		window, multi := testTransportDelayWindow()
		base := testTransportDelayBase()
		for second := range 2 {
			testTransportDelayAdd(t, window, multi, stats(testTransportDelayAt(base, second)))
		}
		delay, _ := window.evaluate(testTransportDelayAt(base, 1))
		return delay
	}
	connectionId := connect.NewId()
	for _, c := range []struct {
		bitsPerSecond int64
		expected      float64
	}{
		{1_500_000, 0},
		{1_050_000, 0.25},
		{1_035_000, 0.5},
		{730_000, 0.5},
		{720_000, 0.75},
		{510_000, 0.75},
		{500_000, 1},
	} {
		down := level(func(endTime time.Time) *connect.TransportDelayStats {
			return testTransportDelayLimited(connectionId, connect.TransportTypeH1, endTime, 0, c.bitsPerSecond)
		})
		if down.DownQuality != c.expected || down.DownBitsPerSecond != c.bitsPerSecond {
			t.Fatalf("down %d bit/s reads %+v, expected level %v", c.bitsPerSecond, down, c.expected)
		}
		// up is half the rate at every boundary
		up := level(func(endTime time.Time) *connect.TransportDelayStats {
			return testTransportDelayLimited(connectionId, connect.TransportTypeH1, endTime, c.bitsPerSecond/2, 0)
		})
		if up.UpQuality != c.expected || up.UpBitsPerSecond != c.bitsPerSecond/2 {
			t.Fatalf("up %d bit/s reads %+v, expected level %v", c.bitsPerSecond/2, up, c.expected)
		}
	}
	for _, c := range []struct {
		queueDelay time.Duration
		expected   float64
	}{
		{500 * time.Millisecond, 0},
		{501 * time.Millisecond, 0.25},
		{1000 * time.Millisecond, 0.25},
		{1001 * time.Millisecond, 0.5},
		{1500 * time.Millisecond, 0.5},
		{1501 * time.Millisecond, 0.75},
		{2000 * time.Millisecond, 0.75},
		{2001 * time.Millisecond, 1},
	} {
		delay := level(func(endTime time.Time) *connect.TransportDelayStats {
			return testTransportDelayQueue(connectionId, connect.TransportTypeH1, endTime, c.queueDelay, c.queueDelay)
		})
		if delay.UpQuality != c.expected || delay.DownQuality != c.expected {
			t.Fatalf("a %s queue reads %+v, expected level %v both ways", c.queueDelay, delay, c.expected)
		}
		connect.AssertEqual(t, delay.UpQueueDelayMillis, c.queueDelay.Milliseconds())
		connect.AssertEqual(t, delay.DownQueueDelayMillis, c.queueDelay.Milliseconds())
	}
}

// The window transports share one access link, so a second's throughput is
// the sum over the connections: the limited rates plus the byte rates of the
// other active connections. Six connections that each read their share of a
// healthy 8 Mbit/s link read the link, not the share.
func TestTransportDelayWindowSumsTheLinkAcrossConnections(t *testing.T) {
	const share = 1_333_333
	if transportDelayThroughputQuality(share, defaultTransportDelayGoodDownBitsPerSecond, defaultTransportDelayBadDownBitsPerSecond) == 0 {
		t.Fatal("one connection's share scores 0 on its own, so the sum is not under test")
	}
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	connectionIds := []connect.Id{}
	for range 6 {
		connectionIds = append(connectionIds, connect.NewId())
	}
	for second := range 3 {
		for _, connectionId := range connectionIds {
			testTransportDelayAdd(t, window, multi, testTransportDelayLimited(
				connectionId, connect.TransportTypeH3, testTransportDelayAt(base, second), 0, share,
			))
		}
	}
	delay, _ := window.evaluate(testTransportDelayAt(base, 2))
	connect.AssertEqual(t, delay, &TransportDelay{
		DownBitsPerSecond: 6 * share,
	})

	// a held connection plus the byte rate of another active one: 1.2
	// Mbit/s alone would read 0.25, and with 75 kB a second beside it the
	// link carries 1.8 Mbit/s
	window, multi = testTransportDelayWindow()
	held := connect.NewId()
	active := connect.NewId()
	for second := range 2 {
		endTime := testTransportDelayAt(base, second)
		testTransportDelayAdd(t, window, multi, testTransportDelayLimited(held, connect.TransportTypeH1, endTime, 0, 1_200_000))
		stats := testTransportDelayStats(active, connect.TransportTypeH3, endTime)
		stats.ReceiveByteCount = 75_000
		testTransportDelayAdd(t, window, multi, stats)
	}
	delay, _ = window.evaluate(testTransportDelayAt(base, 1))
	connect.AssertEqual(t, delay, &TransportDelay{
		DownBitsPerSecond: 1_800_000,
	})

	// a second in which the link held no writer has no throughput reading,
	// however few bytes flowed
	window, multi = testTransportDelayWindow()
	for second := range 3 {
		stats := testTransportDelayStats(active, connect.TransportTypeH3, testTransportDelayAt(base, second))
		stats.SendByteCount = 100
		stats.ReceiveByteCount = 100
		testTransportDelayAdd(t, window, multi, stats)
	}
	delay, _ = window.evaluate(testTransportDelayAt(base, 2))
	connect.AssertEqual(t, delay, noneTransportDelay())
}

// A standing queue on the user's link is common to every connection crossing
// it, so a second reads the second-highest queue of two or more connections:
// one connection's spike, such as a stalled server edge, does not raise the
// level, and two connections queueing do. A second with one reading reads
// that one.
func TestTransportDelayWindowQueueSecondHighest(t *testing.T) {
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	spiking := connect.NewId()
	others := []connect.Id{connect.NewId(), connect.NewId()}
	for second := range 3 {
		endTime := testTransportDelayAt(base, second)
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(spiking, connect.TransportTypeH1, endTime, testTransportDelayNoQueueDelay, 2500*time.Millisecond))
		for _, connectionId := range others {
			testTransportDelayAdd(t, window, multi, testTransportDelayQueue(connectionId, connect.TransportTypeH3, endTime, testTransportDelayNoQueueDelay, 100*time.Millisecond))
		}
	}
	delay, _ := window.evaluate(testTransportDelayAt(base, 2))
	connect.AssertEqual(t, delay, &TransportDelay{
		DownQueueDelayMillis: 100,
	})

	// a second connection queueing as deep is the link
	for second := 3; second < 5; second += 1 {
		endTime := testTransportDelayAt(base, second)
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(spiking, connect.TransportTypeH1, endTime, testTransportDelayNoQueueDelay, 2500*time.Millisecond))
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(others[0], connect.TransportTypeH3, endTime, testTransportDelayNoQueueDelay, 2500*time.Millisecond))
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(others[1], connect.TransportTypeH3, endTime, testTransportDelayNoQueueDelay, 100*time.Millisecond))
	}
	delay, _ = window.evaluate(testTransportDelayAt(base, 4))
	connect.AssertEqual(t, delay, &TransportDelay{
		DownQuality:          1,
		DownQueueDelayMillis: 2500,
	})

	// the only connection that reports a queue reads it
	window, multi = testTransportDelayWindow()
	for second := range 2 {
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(spiking, connect.TransportTypeH1, testTransportDelayAt(base, second), testTransportDelayNoQueueDelay, 2500*time.Millisecond))
	}
	delay, _ = window.evaluate(testTransportDelayAt(base, 1))
	connect.AssertEqual(t, delay.DownQuality, 1.0)
}

// Each signal scores the second-worst of the newest N sampled seconds: a
// single bad second does not raise the level and two do, fewer than two
// sampled seconds score nothing, and seconds older than the newest N do not
// count.
func TestTransportDelayWindowSecondWorstSecond(t *testing.T) {
	base := testTransportDelayBase()
	connectionId := connect.NewId()
	read := func(secondCount int, badSeconds ...int) *TransportDelay {
		t.Helper()
		window, multi := testTransportDelayWindow()
		for second := range secondCount {
			bitsPerSecond := int64(5_000_000)
			queueDelay := 100 * time.Millisecond
			if slices.Contains(badSeconds, second) {
				bitsPerSecond = 100_000
				queueDelay = 2500 * time.Millisecond
			}
			// the link held the writer up; the queue read down
			stats := testTransportDelayLimited(connectionId, connect.TransportTypeH1, testTransportDelayAt(base, second), bitsPerSecond, 0)
			stats.ReceiveByteCount = 1000
			stats.ReceiveQueueDelay = queueDelay
			stats.ReceiveQueueDelayValid = true
			testTransportDelayAdd(t, window, multi, stats)
		}
		delay, _ := window.evaluate(testTransportDelayAt(base, secondCount-1))
		return delay
	}

	connect.AssertEqual(t, read(10, 4), &TransportDelay{
		UpBitsPerSecond:      5_000_000,
		DownQueueDelayMillis: 100,
	})
	connect.AssertEqual(t, read(10, 4, 7), &TransportDelay{
		UpQuality:            1,
		DownQuality:          1,
		UpBitsPerSecond:      100_000,
		DownQueueDelayMillis: 2500,
	})
	// one sampled second, however bad, scores nothing
	connect.AssertEqual(t, read(1, 0), noneTransportDelay())
	connect.AssertEqual(t, read(2, 0, 1).DownQuality, 1.0)
	// with twelve sampled seconds the two oldest are past the newest ten
	connect.AssertEqual(t, read(12, 0, 1).DownQuality, 0.0)
	connect.AssertEqual(t, read(12, 10, 11).DownQuality, 1.0)
}

// The hysteresis applies only to a release: a level falls once the quality is
// 0.1 below the level's band, at the report's release edges of 1.8, 1.3 and
// 0.8 s of queue, and rises at once. A window of two sampled seconds lets each
// step set the score.
func TestTransportDelayWindowReleaseHysteresis(t *testing.T) {
	settings := testTransportDelaySettings()
	settings.TransportDelayWindowCount = 2
	window, multi := testTransportDelayWindowWithSettings(settings)
	base := testTransportDelayBase()
	connectionId := connect.NewId()
	second := 0
	read := func(queueDelay time.Duration) *TransportDelay {
		t.Helper()
		for range 2 {
			testTransportDelayAdd(t, window, multi, testTransportDelayQueue(connectionId, connect.TransportTypeH1, testTransportDelayAt(base, second), testTransportDelayNoQueueDelay, queueDelay))
			second += 1
		}
		delay, _ := window.evaluate(testTransportDelayAt(base, second-1))
		return delay
	}
	var previous *TransportDelay
	for _, c := range []struct {
		queueDelay time.Duration
		expected   float64
	}{
		// q 0.95: attack from 0
		{2400 * time.Millisecond, 1},
		// q 0.66, 0.645: the top releases at 0.65 (1.8 s)
		{1820 * time.Millisecond, 1},
		{1790 * time.Millisecond, 0.75},
		// q 0.425, 0.39: 0.75 releases at 0.4 (1.3 s)
		{1350 * time.Millisecond, 0.75},
		{1280 * time.Millisecond, 0.5},
		// q 0.175, 0.14: 0.5 releases at 0.15 (0.8 s)
		{850 * time.Millisecond, 0.5},
		{780 * time.Millisecond, 0.25},
		// q 0.05: the lowest level holds while the quality is above 0
		{600 * time.Millisecond, 0.25},
		// q 0.55, 0.85: rises at once, past a band if need be
		{1600 * time.Millisecond, 0.75},
		{2200 * time.Millisecond, 1},
		// q 0: back to 0 at once
		{500 * time.Millisecond, 0},
	} {
		delay := read(c.queueDelay)
		if delay.DownQuality != c.expected {
			t.Fatalf("a %s queue after %+v reads level %v, expected %v", c.queueDelay, previous, delay.DownQuality, c.expected)
		}
		connect.AssertEqual(t, delay.DownQueueDelayMillis, c.queueDelay.Milliseconds())
		if previous != nil && transportDelayLevelChanged(previous, delay) != (previous.DownQuality != delay.DownQuality) {
			t.Fatalf("the listeners would hear %+v after %+v", delay, previous)
		}
		previous = delay
	}
}

// A second is the fallback's when DNS-carried transports carried most of its
// bytes, and Fallback follows the second that set the reported level: the
// worse direction's.
func TestTransportDelayWindowFallbackByByteShare(t *testing.T) {
	base := testTransportDelayBase()
	dns := connect.NewId()
	usual := connect.NewId()
	read := func(dnsTransportType connect.TransportType, dnsByteCount ByteCount, usualByteCount ByteCount) *TransportDelay {
		t.Helper()
		window, multi := testTransportDelayWindow()
		for second := range 2 {
			endTime := testTransportDelayAt(base, second)
			// each held to 100 kbit/s: the link reads 200 kbit/s, level 1
			dnsStats := testTransportDelayLimited(dns, dnsTransportType, endTime, 0, 100_000)
			dnsStats.ReceiveByteCount = dnsByteCount
			testTransportDelayAdd(t, window, multi, dnsStats)
			usualStats := testTransportDelayLimited(usual, connect.TransportTypeH1, endTime, 0, 100_000)
			usualStats.ReceiveByteCount = usualByteCount
			testTransportDelayAdd(t, window, multi, usualStats)
		}
		delay, _ := window.evaluate(testTransportDelayAt(base, 1))
		connect.AssertEqual(t, delay.DownQuality, 1.0)
		return delay
	}
	connect.AssertEqual(t, read(connect.TransportTypeH3Dns, 60_000, 40_000).Fallback, true)
	connect.AssertEqual(t, read(connect.TransportTypeH3DnsPump, 60_000, 40_000).Fallback, true)
	connect.AssertEqual(t, read(connect.TransportTypeH3Dns, 40_000, 60_000).Fallback, false)
	// half is not most
	connect.AssertEqual(t, read(connect.TransportTypeH3Dns, 50_000, 50_000).Fallback, false)
	// a transport that is not DNS-carried is never the fallback
	connect.AssertEqual(t, read(connect.TransportTypeP2p, 60_000, 40_000).Fallback, false)

	// the worse direction sets it: down on the fallback at level 0.5, up on
	// the usual transports at level 1, and the other way around
	directions := func(downTransportType connect.TransportType, downBitsPerSecond int64, upTransportType connect.TransportType, upBitsPerSecond int64) *TransportDelay {
		t.Helper()
		window, multi := testTransportDelayWindow()
		for second := range 2 {
			endTime := testTransportDelayAt(base, second)
			testTransportDelayAdd(t, window, multi, testTransportDelayLimited(dns, downTransportType, endTime, 0, downBitsPerSecond))
			testTransportDelayAdd(t, window, multi, testTransportDelayLimited(usual, upTransportType, endTime, upBitsPerSecond, 0))
		}
		delay, _ := window.evaluate(testTransportDelayAt(base, 1))
		return delay
	}
	delay := directions(connect.TransportTypeH3Dns, 900_000, connect.TransportTypeH1, 100_000)
	if delay.DownQuality != 0.5 || delay.UpQuality != 1 || delay.Fallback {
		t.Fatalf("a worse direction on the usual transports read %+v, expected no fallback", delay)
	}
	delay = directions(connect.TransportTypeH3Dns, 300_000, connect.TransportTypeH1, 500_000)
	if delay.DownQuality != 1 || delay.UpQuality != 0.5 || !delay.Fallback {
		t.Fatalf("a worse direction on the fallback read %+v, expected the fallback", delay)
	}
	// normal operation is never the fallback's
	window, multi := testTransportDelayWindow()
	for second := range 2 {
		testTransportDelayAdd(t, window, multi, testTransportDelayLimited(dns, connect.TransportTypeH3Dns, testTransportDelayAt(base, second), 5_000_000, 5_000_000))
	}
	if delay, _ := window.evaluate(testTransportDelayAt(base, 1)); delay.Fallback {
		t.Fatalf("a healthy fallback read %+v", delay)
	}
}

// A connection counts once in a second: an interval that closes early can
// put two of its samples in one second, and the later one stands for it
// rather than their sum.
func TestTransportDelayWindowCountsAConnectionOncePerSecond(t *testing.T) {
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	connectionId := connect.NewId()
	for second := range 2 {
		secondStart := base.Add(time.Duration(second) * time.Second)
		testTransportDelayAdd(t, window, multi, testTransportDelayLimited(connectionId, connect.TransportTypeH1, secondStart.Add(200*time.Millisecond), 0, 100_000))
		testTransportDelayAdd(t, window, multi, testTransportDelayLimited(connectionId, connect.TransportTypeH1, secondStart.Add(800*time.Millisecond), 0, 200_000))
	}
	delay, _ := window.evaluate(testTransportDelayAt(base, 1))
	connect.AssertEqual(t, delay.DownBitsPerSecond, int64(200_000))
}

// A sample counts for M after its interval ends, so a direction returns to 0
// without new traffic (D4), and with the second-worst statistic a level ends
// once all but one of its bad seconds aged out.
func TestTransportDelayWindowExpiry(t *testing.T) {
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	connectionId := connect.NewId()
	for second := range 2 {
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(connectionId, connect.TransportTypeH1, testTransportDelayAt(base, second), testTransportDelayNoQueueDelay, 2500*time.Millisecond))
	}
	oldest := testTransportDelayAt(base, 0)
	if delay, live := window.evaluate(oldest.Add(defaultTransportDelayWindowTimeout - time.Millisecond)); !live || delay.DownQuality != 1 {
		t.Fatalf("the bad seconds aged out early: %+v live %t", delay, live)
	}
	delay, live := window.evaluate(oldest.Add(defaultTransportDelayWindowTimeout))
	if !live {
		t.Fatal("the newer bad second aged out with the older")
	}
	connect.AssertEqual(t, delay, noneTransportDelay())
	delay, live = window.evaluate(testTransportDelayAt(base, 1).Add(defaultTransportDelayWindowTimeout))
	if live {
		t.Fatal("samples M old are still live")
	}
	connect.AssertEqual(t, delay, noneTransportDelay())
	connect.AssertEqual(t, len(window.samples), 0)

	// a sample already M old when it arrives is not taken
	stats := testTransportDelayQueue(connectionId, connect.TransportTypeH1, oldest, testTransportDelayNoQueueDelay, 2500*time.Millisecond)
	if window.add(1, multi, stats, oldest.Add(defaultTransportDelayWindowTimeout)) {
		t.Fatal("the window took a sample older than its timeout")
	}
}

// The window takes the samples of one multi client generation: a sample of a
// retired generation or multi client is dropped, a reset drops the samples
// and the levels, and with no multi client nothing is taken.
func TestTransportDelayWindowGenerationFence(t *testing.T) {
	window := newTransportDelayWindow(testTransportDelaySettings())
	base := testTransportDelayBase()
	now := testTransportDelayAt(base, 1)
	bad := func(second int) *connect.TransportDelayStats {
		return testTransportDelayQueue(connect.NewId(), connect.TransportTypeH1, testTransportDelayAt(base, second), testTransportDelayNoQueueDelay, 2500*time.Millisecond)
	}

	retired := new(connect.RemoteUserNatMultiClient)
	current := new(connect.RemoteUserNatMultiClient)
	if window.add(0, nil, bad(0), now) {
		t.Fatal("a window with no multi client took a sample")
	}
	window.reset(1, retired)
	for second := range 2 {
		if !window.add(1, retired, bad(second), now) {
			t.Fatal("the window dropped a sample of its generation")
		}
	}
	if delay, _ := window.evaluate(now); delay.DownQuality != 1 {
		t.Fatalf("down quality = %v, expected 1", delay.DownQuality)
	}

	window.reset(2, current)
	connect.AssertEqual(t, window.get(), noneTransportDelay())
	if delay, live := window.evaluate(now); live || *delay != (TransportDelay{}) {
		t.Fatalf("the reset kept the retired generation's samples: %+v live %t", delay, live)
	}
	for _, c := range []struct {
		name       string
		generation int64
		multi      *connect.RemoteUserNatMultiClient
	}{
		{"the retired generation", 1, retired},
		{"the retired multi client", 2, retired},
		{"the retired generation number", 1, current},
		{"no multi client", 2, nil},
	} {
		if window.add(c.generation, c.multi, bad(0), now) || window.add(c.generation, c.multi, bad(1), now) {
			t.Fatalf("the window took a late sample of %s", c.name)
		}
	}
	if delay, _ := window.evaluate(now); *delay != (TransportDelay{}) {
		t.Fatalf("a dropped sample reached the readout: %+v", delay)
	}
	if !window.add(2, current, bad(0), now) {
		t.Fatal("the window dropped a sample of the current generation")
	}

	// the destination removed: no multi client, and nothing is taken
	window.reset(3, nil)
	if window.add(3, nil, bad(0), now) || window.add(2, current, bad(0), now) {
		t.Fatal("a window with no multi client took a sample")
	}
	connect.AssertEqual(t, window.get(), noneTransportDelay())
}

// A transport that redials over and over adds a connection per dial; past its
// bound the window drops its oldest samples.
func TestTransportDelayWindowSampleBound(t *testing.T) {
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	connectionIds := []connect.Id{}
	for i := range transportDelayWindowMaxSampleCount + 1 {
		connectionId := connect.NewId()
		connectionIds = append(connectionIds, connectionId)
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(
			connectionId, connect.TransportTypeH1, base.Add(time.Duration(i)*time.Millisecond), testTransportDelayNoQueueDelay, 0,
		))
	}
	connect.AssertEqual(t, len(window.samples), transportDelayWindowMaxSampleCount)
	connect.AssertEqual(t, window.samples[0].connectionId, connectionIds[1])
	connect.AssertEqual(t, window.samples[len(window.samples)-1].connectionId, connectionIds[len(connectionIds)-1])
}

// The device settings carry the report's defaults, the durations bind as
// milliseconds, and a window built from zeroed settings keeps working
// structure.
func TestTransportDelaySettingsDefaults(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	connect.AssertEqual(t, settings.EnableTransportDelay, true)
	connect.AssertEqual(t, settings.TransportDelayWindowCount, 10)
	connect.AssertEqual(t, settings.TransportDelayWindowTimeout, 20*time.Second)
	connect.AssertEqual(t, settings.TransportDelayGoodUpBitsPerSecond, int64(750_000))
	connect.AssertEqual(t, settings.TransportDelayBadUpBitsPerSecond, int64(175_000))
	connect.AssertEqual(t, settings.TransportDelayGoodDownBitsPerSecond, int64(1_500_000))
	connect.AssertEqual(t, settings.TransportDelayBadDownBitsPerSecond, int64(350_000))
	connect.AssertEqual(t, settings.TransportDelayGoodUpQueueDelay, 500*time.Millisecond)
	connect.AssertEqual(t, settings.TransportDelayGoodDownQueueDelay, 500*time.Millisecond)
	connect.AssertEqual(t, settings.TransportDelayBadUpQueueDelay, 2500*time.Millisecond)
	connect.AssertEqual(t, settings.TransportDelayBadDownQueueDelay, 2500*time.Millisecond)
	connect.AssertEqual(t, settings.TransportDelayQualityStep, 0.25)
	connect.AssertEqual(t, settings.TransportDelayQualityHysteresis, 0.10)
	connect.AssertEqual(t, DefaultLowMemoryDeviceLocalSettings().EnableTransportDelay, true)

	connect.AssertEqual(t, settings.GetTransportDelayWindowTimeoutMillis(), int64(20_000))
	connect.AssertEqual(t, settings.GetTransportDelayGoodUpQueueDelayMillis(), int64(500))
	connect.AssertEqual(t, settings.GetTransportDelayBadDownQueueDelayMillis(), int64(2500))
	settings.SetTransportDelayWindowTimeoutMillis(5000)
	connect.AssertEqual(t, settings.TransportDelayWindowTimeout, 5*time.Second)
	settings.SetTransportDelayGoodUpQueueDelayMillis(400)
	connect.AssertEqual(t, settings.TransportDelayGoodUpQueueDelay, 400*time.Millisecond)
	settings.SetTransportDelayGoodDownQueueDelayMillis(600)
	connect.AssertEqual(t, settings.TransportDelayGoodDownQueueDelay, 600*time.Millisecond)
	settings.SetTransportDelayBadUpQueueDelayMillis(3000)
	connect.AssertEqual(t, settings.GetTransportDelayBadUpQueueDelayMillis(), int64(3000))
	settings.SetTransportDelayBadDownQueueDelayMillis(2000)
	connect.AssertEqual(t, settings.TransportDelayBadDownQueueDelay, 2*time.Second)

	// the directions keep their own queue thresholds
	window := newTransportDelayWindow(settings)
	connect.AssertEqual(t, window.up.goodQueueDelay, 400*time.Millisecond)
	connect.AssertEqual(t, window.up.badQueueDelay, 3*time.Second)
	connect.AssertEqual(t, window.down.goodQueueDelay, 600*time.Millisecond)
	connect.AssertEqual(t, window.down.badQueueDelay, 2*time.Second)

	window = newTransportDelayWindow(&DeviceLocalSettings{
		TransportDelayQualityHysteresis: -1,
	})
	connect.AssertEqual(t, window.windowCount, defaultTransportDelayWindowCount)
	connect.AssertEqual(t, window.windowTimeout, defaultTransportDelayWindowTimeout)
	connect.AssertEqual(t, window.qualityStep, defaultTransportDelayQualityStep)
	connect.AssertEqual(t, window.qualityHysteresis, 0.0)
	connect.AssertEqual(t, window.interval, time.Second)
	window = newTransportDelayWindow(&DeviceLocalSettings{
		TransportDelayQualityStep: 2,
	})
	connect.AssertEqual(t, window.qualityStep, 1.0)
}

// testingTransportDelayListener queues every readout it hears.
type testingTransportDelayListener struct {
	delays chan *TransportDelay
}

func newTestingTransportDelayListener() *testingTransportDelayListener {
	return &testingTransportDelayListener{
		delays: make(chan *TransportDelay, 64),
	}
}

func (self *testingTransportDelayListener) TransportDelayChanged(delay *TransportDelay) {
	self.delays <- delay
}

func (self *testingTransportDelayListener) next(t *testing.T, message string) *TransportDelay {
	t.Helper()
	select {
	case delay := <-self.delays:
		if delay == nil {
			t.Fatalf("%s: the listener heard nil", message)
		}
		return delay
	case <-time.After(30 * time.Second):
		t.Fatal(message)
		return nil
	}
}

// Fails if the listener hears anything within wait.
func (self *testingTransportDelayListener) quiet(t *testing.T, wait time.Duration, message string) {
	t.Helper()
	select {
	case delay := <-self.delays:
		t.Fatalf("%s: %+v", message, delay)
	case <-time.After(wait):
	}
}

// The device's multi client and the connection generation that built it.
func testTransportDelayMultiClient(t *testing.T, device *DeviceLocal) (*connect.RemoteUserNatMultiClient, int64) {
	t.Helper()
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	multi, ok := device.remoteUserNatClient.(*connect.RemoteUserNatMultiClient)
	if !ok {
		t.Fatalf("the device has no multi client: %T", device.remoteUserNatClient)
	}
	return multi, device.connectionGeneration
}

// Reports whether the device holds a subscription to a multi client's delay
// stats.
func testTransportDelaySubscribed(device *DeviceLocal) bool {
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	return device.transportDelayStatsSub != nil
}

// A device whose destinations build multi clients with no transports, so the
// tests feed the samples.
func testTransportDelayDeviceSettings(settings *DeviceLocalSettings) {
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return &rpcLeakTestGenerator{}
	}
}

func testTransportDelayConnect(device *DeviceLocal) *ConnectLocation {
	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)
	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	device.SetConnectLocation(location)
	return location
}

// Waits until the device's readout satisfies done.
func testWaitTransportDelay(t *testing.T, device Device, done func(delay *TransportDelay) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !done(device.GetTransportDelay()) {
		if !time.Now().Before(deadline) {
			t.Fatalf("the device never reported the transport delay, last = %+v", device.GetTransportDelay())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Feeds the bad reading of one connection to the device, in the two seconds
// up to now: up, a link held to 300 kbit/s, which scores 0.63 for level 0.75;
// down, a 1.5 s standing queue, which scores 0.5.
func testTransportDelayFeedBad(
	device *DeviceLocal,
	connectionGeneration int64,
	multi *connect.RemoteUserNatMultiClient,
	connectionId connect.Id,
	now time.Time,
) {
	for _, endTime := range []time.Time{now.Add(-time.Second), now} {
		stats := testTransportDelayStats(connectionId, connect.TransportTypeH1, endTime)
		stats.SendBitsPerSecond = 300_000
		stats.SendByteCount = 37_500
		stats.ReceiveByteCount = 1000
		stats.ReceiveQueueDelay = 1500 * time.Millisecond
		stats.ReceiveQueueDelayValid = true
		device.updateTransportDelayStats(connectionGeneration, multi, stats)
	}
}

var testTransportDelayBad = &TransportDelay{
	UpQuality:            0.75,
	DownQuality:          0.5,
	UpBitsPerSecond:      300_000,
	DownQueueDelayMillis: 1500,
}

// A local device reports the quality of its current multi client generation:
// the listener hears each level change and not a change inside a level, a
// rebuild resets the readout and the listener hears the reset, a sample of
// the retired generation is dropped, and with the destination removed the
// device reads normal operation.
func TestDeviceLocalTransportDelayFollowsTheMultiClientGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	testTransportDelayDeviceSettings(settings)
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	listener := newTestingTransportDelayListener()
	sub := device.AddTransportDelayChangeListener(listener)
	defer sub.Close()
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())

	location := testTransportDelayConnect(device)
	first, firstGeneration := testTransportDelayMultiClient(t, device)
	if !testTransportDelaySubscribed(device) {
		t.Fatal("the device did not subscribe to its multi client's delay stats")
	}
	now := time.Now()
	testTransportDelayFeedBad(device, firstGeneration, first, connect.NewId(), now)
	connect.AssertEqual(t, listener.next(t, "the bad seconds reached no listener"), testTransportDelayBad)
	connect.AssertEqual(t, device.GetTransportDelay(), testTransportDelayBad)

	// a second connection reading 1.3 s in the newer second sets that
	// second's queue, which scores 0.4: inside the 0.5 level, no event
	stats := testTransportDelayStats(connect.NewId(), connect.TransportTypeH3, now)
	stats.ReceiveByteCount = 1000
	stats.ReceiveQueueDelay = 1300 * time.Millisecond
	stats.ReceiveQueueDelayValid = true
	device.updateTransportDelayStats(firstGeneration, first, stats)
	testWaitTransportDelay(t, device, func(delay *TransportDelay) bool {
		return delay.DownQueueDelayMillis == 1300
	})
	listener.quiet(t, 200*time.Millisecond, "a change inside a level fired the listener")

	// a rebuild drops the retired generation's samples and levels
	device.Reconnect(location)
	connect.AssertEqual(t, listener.next(t, "the rebuild reached no listener"), noneTransportDelay())
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())
	second, secondGeneration := testTransportDelayMultiClient(t, device)
	if second == first || secondGeneration <= firstGeneration {
		t.Fatal("the reconnect did not build a new multi client generation")
	}

	// the retired multi client's transports can still publish
	testTransportDelayFeedBad(device, firstGeneration, first, connect.NewId(), time.Now())
	testTransportDelayFeedBad(device, secondGeneration, first, connect.NewId(), time.Now())
	listener.quiet(t, 200*time.Millisecond, "a sample of the retired generation fired the listener")
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())

	testTransportDelayFeedBad(device, secondGeneration, second, connect.NewId(), time.Now())
	connect.AssertEqual(t, listener.next(t, "the new generation's seconds reached no listener"), testTransportDelayBad)

	// no multi client: normal operation
	device.RemoveDestination()
	connect.AssertEqual(t, listener.next(t, "the removed destination reached no listener"), noneTransportDelay())
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())
	if testTransportDelaySubscribed(device) {
		t.Fatal("the device kept the retired multi client's subscription")
	}
	testTransportDelayFeedBad(device, secondGeneration, second, connect.NewId(), time.Now())
	listener.quiet(t, 200*time.Millisecond, "a sample with no multi client fired the listener")
}

// With EnableTransportDelay off, as on a hosted device, the device subscribes
// to nothing and reads normal operation.
func TestDeviceLocalTransportDelayDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	testTransportDelayDeviceSettings(settings)
	settings.EnableTransportDelay = false
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	testTransportDelayConnect(device)
	multi, generation := testTransportDelayMultiClient(t, device)
	if testTransportDelaySubscribed(device) {
		t.Fatal("a device with the transport delay off subscribed to its multi client")
	}
	testTransportDelayFeedBad(device, generation, multi, connect.NewId(), time.Now())
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())
}

// The quality crosses the device rpc: the remote's listener and getter report
// what the device process reports, a reconnect replays the level, the getter
// keeps the last value while the device process is gone, and a remote that
// reaches a new device process hears its level.
func TestDeviceRemoteTransportDelay(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, deviceRemote := testExtenderStatusSyncedDeviceLocalRemoteWithSettings(
		t,
		networkSpace,
		testTransportDelayDeviceSettings,
	)
	listener := newTestingTransportDelayListener()
	sub := deviceRemote.AddTransportDelayChangeListener(listener)
	defer sub.Close()
	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), noneTransportDelay())

	testTransportDelayConnect(deviceLocal)
	multi, generation := testTransportDelayMultiClient(t, deviceLocal)
	testTransportDelayFeedBad(deviceLocal, generation, multi, connect.NewId(), time.Now())
	// a sync replays normal operation when the listener attaches; skip it
	delay := listener.next(t, "the bad seconds reached no remote listener")
	for *delay == (TransportDelay{}) {
		delay = listener.next(t, "the bad seconds reached no remote listener")
	}
	connect.AssertEqual(t, delay, testTransportDelayBad)
	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), testTransportDelayBad)

	// a resync rebuilds the rpc session with no change on the device, and
	// the sync reverse replays the level
	deviceRemote.Sync()
	connect.AssertEqual(t, listener.next(t, "the reconnect replayed no level"), testTransportDelayBad)
	if !deviceRemote.waitForSync(30 * time.Second) {
		t.Fatal("the device remote did not sync again")
	}
	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), testTransportDelayBad)

	// the cache survives the device process going away
	deviceLocal.Close()
	testWaitRemoteDisconnected(t, deviceRemote)
	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), testTransportDelayBad)

	// a new device process on the remote's address has no destination yet:
	// the sync replays its normal operation over the cached level
	localSettings := testExtenderStatusDeviceSettings()
	localSettings.EnableRpc = true
	testTransportDelayDeviceSettings(localSettings)
	replacement, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", deviceRemote.GetInstanceId(), localSettings, deviceRemote.clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(30 * time.Second) {
		t.Fatal("the device remote did not sync with the new device process")
	}
	connect.AssertEqual(t, listener.next(t, "the new device process replayed no level"), noneTransportDelay())
	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), noneTransportDelay())
}

// An app updated ahead of the device process it is attached to reads normal
// operation and keeps its rpc session: the device process answers neither the
// getter nor the listener.
func TestDeviceRemoteTransportDelayWithoutTheMethods(t *testing.T) {
	deviceRemote := newTestDeviceRemoteWithNoService(t)
	service := testExtenderProvideDeviceProcess(t, deviceRemote, &testingOptionalMethodRpc{})

	connect.AssertEqual(t, deviceRemote.GetTransportDelay(), noneTransportDelay())
	sub := deviceRemote.AddTransportDelayChangeListener(newTestingTransportDelayListener())
	defer sub.Close()

	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("the missing transport delay methods tore the rpc session down")
	}
	var reply bool
	if err := service.Call("DeviceLocalRpc.Ping", true, &reply); err != nil || !reply {
		t.Fatalf("the rpc session did not survive: reply = %t err = %v", reply, err)
	}
}
