//go:build !ios && !android && !js

package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
)

// The forward dial failures of the provider role's extender (A7,
// connect#230). The role set no ErrorHandler on its extender server, so a
// forward whose dial failed -- the forward of an activation probe included --
// left the client a stream that closed, and the extender's own log said
// nothing at all. The role now logs them, at most once a minute for each
// kind, with the count of the failures not logged since.

// Records the formatted `[extender]provide ... dial err` lines a role logs,
// and hands each to a waiting test.
type testingForwardDialLogger struct {
	connect.Logger
	stateLock sync.Mutex
	lines     []string
	logged    chan string
}

func newTestingForwardDialLogger() *testingForwardDialLogger {
	return &testingForwardDialLogger{
		Logger: connect.NewNoopLogger(),
		logged: make(chan string, 64),
	}
}

func (self *testingForwardDialLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(line, "[extender]provide ") || !strings.Contains(line, " dial err = ") {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.lines = append(self.lines, line)
	}()
	select {
	case self.logged <- line:
	default:
	}
}

func (self *testingForwardDialLogger) forwardDialLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

// The failures of each kind log their first line at once and then one line
// an interval, which carries the count of the failures not logged since. The
// stream and the datagram forward are counted apart, and every other stage
// the server reports is not logged.
func TestExtenderForwardDialErrorLogLogsEachKindOnceAnInterval(t *testing.T) {
	log := newTestingForwardDialLogger()
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	errorLog := newExtenderForwardDialErrorLog(log, func() time.Time { return now }, time.Minute)
	dialErr := errors.New("dial tcp6 [2001:db8::443]:443: connect: network is unreachable")

	errorLog.report(extender.ExtenderErrorStageForwardDial, dialErr)
	for range 3 {
		now = now.Add(10 * time.Second)
		errorLog.report(extender.ExtenderErrorStageForwardDial, dialErr)
	}
	errorLog.report(extender.ExtenderErrorStageDatagramForwardDial, dialErr)
	// a client's own failures are not forward dial failures
	errorLog.report("header authorization", errors.New("secret signature is not allowed"))
	errorLog.report("relay", errors.New("broken pipe"))
	// a minute after the first line
	now = now.Add(30 * time.Second)
	errorLog.report(extender.ExtenderErrorStageForwardDial, dialErr)
	now = now.Add(time.Second)
	errorLog.report(extender.ExtenderErrorStageForwardDial, dialErr)

	connect.AssertEqual(t, log.forwardDialLines(), []string{
		"[extender]provide forward dial err = " + dialErr.Error() + "\n",
		"[extender]provide datagram forward dial err = " + dialErr.Error() + "\n",
		"[extender]provide forward dial err = " + dialErr.Error() + " (3 more since the last)\n",
	})
}

// A non-positive interval logs every failure.
func TestExtenderForwardDialErrorLogWithoutAnIntervalLogsEveryFailure(t *testing.T) {
	log := newTestingForwardDialLogger()
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	errorLog := newExtenderForwardDialErrorLog(log, func() time.Time { return now }, -1)
	dialErr := errors.New("context deadline exceeded")
	for range 3 {
		errorLog.report(extender.ExtenderErrorStageForwardDial, dialErr)
	}
	connect.AssertEqual(t, len(log.forwardDialLines()), 3)
}

// The composed path: forwards through the running role's tcp carrier whose
// dials fail are logged by the role, the first at once, the next within the
// minute only counted, and the count carried by the first line after the
// minute. Before, the role set no ErrorHandler and nothing was logged.
func TestDeviceLocalProviderExtenderLogsForwardDialFailures(t *testing.T) {
	log := newTestingForwardDialLogger()
	dialErr := errors.New("the forward egress is down")
	dialed := make(chan string, 8)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.Log = log
		settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
			select {
			case dialed <- network + " " + address:
			default:
			}
			return nil, dialErr
		}
	})
	fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})

	forward := func() {
		t.Helper()
		connectSettings := connect.DefaultConnectSettings()
		connectSettings.ConnectTimeout = 10 * time.Second
		connectSettings.TlsTimeout = 10 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(
			ctx,
			connectSettings,
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					ServerName:  "front.example",
					Port:        fixture.tcpPort,
				},
				Ip:        netip.MustParseAddr("127.0.0.1"),
				PublicKey: fixture.extender().publicKey,
			},
			&connect.ExtenderDial{
				DestinationHost: testProvideExtenderHost,
				DestinationPort: 443,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		select {
		case dial := <-dialed:
			if dial != "tcp4 "+testProvideExtenderHost+":443" {
				t.Fatalf("forward dial = %q, expected the family narrowed destination", dial)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the extender did not dial forward")
		}
		// the extender closes the stream of a forward it could not dial, and
		// has reported the failure by then
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(conn); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("the extender kept the stream of a forward it could not dial")
			}
		}
	}

	forward()
	select {
	case line := <-log.logged:
		connect.AssertEqual(t, line, "[extender]provide forward dial err = "+dialErr.Error()+"\n")
	case <-time.After(10 * time.Second):
		t.Fatal("the role did not log the forward dial failure")
	}

	forward()
	fixture.clock.advance(extenderForwardDialErrorLogInterval)
	forward()
	select {
	case line := <-log.logged:
		connect.AssertEqual(t, line, "[extender]provide forward dial err = "+dialErr.Error()+" (1 more since the last)\n")
	case <-time.After(10 * time.Second):
		t.Fatal("the role did not log the forward dial failure after the interval")
	}
	connect.AssertEqual(t, len(log.forwardDialLines()), 2)
}
