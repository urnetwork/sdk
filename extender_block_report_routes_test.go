//go:build !ios && !android && !js

package sdk

import (
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The sender on its real routes, in the process: the space's own client strategy
// reaches the operator's `/hello` and the report route through a real open
// extender on loopback, an attempt is the real latency probe through the recorded
// dialer, and only the pace is shortened. This pins what the seams of the other
// tests stand in for: the report arrives with the record's key and the device's
// token, and the probe tells an extender that answers from one that does not, so
// an extender that answers is never reported.
func TestExtenderBlockReportOverTheRealRoutes(t *testing.T) {
	const blockedIndex = 1
	blockedKeyHex := hex.EncodeToString(testSeedExtenderPrivateKey(blockedIndex).Public().(ed25519.PublicKey))

	for _, test := range []struct {
		name string
		// routes the suspect's address, or leaves it refused
		route    func(t *testing.T, api *testSeedApi, dialer *testSeedDialer)
		reported bool
		outcome  string
	}{
		{
			name:     "every dial of it is refused",
			route:    func(t *testing.T, api *testSeedApi, dialer *testSeedDialer) {},
			reported: true,
			outcome:  connect.ExtenderBlockAttemptFailed,
		},
		{
			name: "something else answers with another identity",
			route: func(t *testing.T, api *testSeedApi, dialer *testSeedDialer) {
				dialer.route(testSeedExtenderAddress(blockedIndex), newTestSeedExtender(t, 2, api))
			},
			reported: true,
			outcome:  connect.ExtenderBlockAttemptFailed,
		},
		{
			name: "the extender answers",
			route: func(t *testing.T, api *testSeedApi, dialer *testSeedDialer) {
				dialer.route(testSeedExtenderAddress(blockedIndex), newTestSeedExtender(t, blockedIndex, api))
			},
			reported: false,
			outcome:  connect.ExtenderBlockAttemptAnswered,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			openRecord := testSeedRecord(t, testSeedRecordSpec{
				index:      0,
				issueTime:  now.Add(-2 * time.Hour),
				expireTime: now.Add(48 * time.Hour),
			})
			gatedRecord := testSeedRecord(t, testSeedRecordSpec{
				index:      blockedIndex,
				issueTime:  now.Add(-2 * time.Hour),
				expireTime: now.Add(48 * time.Hour),
				tier:       connect.ExtenderDirectoryTierGated,
			})

			api := newTestSeedApi(t)
			dialer := newTestSeedDialer()
			dialer.route(testSeedExtenderAddress(0), newTestSeedExtender(t, 0, api, openRecord))
			test.route(t, api, dialer)
			testSeedInstallConnectSettings(t, dialer, func(connectSettings *connect.ConnectSettings) {
				connectSettings.TlsConfig = &tls.Config{RootCAs: api.roots}
			})
			previousConfigure := extenderBlockReportConfigure
			extenderBlockReportConfigure = func(settings *extenderBlockReportSettings) {
				settings.Evidence.AttemptSettleTimeout = 10 * time.Millisecond
				settings.Evidence.AttemptInterval = 100 * time.Millisecond
				settings.Evidence.ReportFreshTimeout = time.Minute
				settings.MinRequestInterval = 10 * time.Millisecond
			}
			t.Cleanup(func() {
				extenderBlockReportConfigure = previousConfigure
			})

			space := newTestSeedSpace(t, testSeedStoragePath(t))
			directory := space.extenderDirectory
			applyRecords := func(records ...*protocol.ExtenderRecord) {
				t.Helper()
				for _, record := range records {
					var err error
					if record == gatedRecord {
						_, err = directory.ApplyReleaseRecord(record, directory.ResetGeneration())
					} else {
						_, err = directory.ApplyRecord(record, connect.ExtenderSourceFeed)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			applyRecords(openRecord, gatedRecord)

			// outside the cohort, off: a failed dial arms nothing
			byJwt := testReleaseByJwt(t, connect.NewId(), connect.NewId())
			t.Cleanup(space.bindExtenderRelease(func() string {
				return byJwt
			}))
			blockedIp := testSeedExtenderIp(blockedIndex)
			directory.RecordFailure(blockedIp, connect.ExtenderConnectModeTcpTls)
			if look := directory.BlockEvidenceLook([]int{4}); look.SuspectCount != 0 {
				t.Fatal("a client outside the cohort armed a suspect")
			}

			// in the cohort, a failed dial of the released record starts it
			space.updateInPlaceValues(func(values *NetworkSpaceValues) {
				values.ExtenderGatedPreference = true
			})
			testSeedWaitForTimeout(t, 60*time.Second, func() bool {
				directory.RecordFailure(blockedIp, connect.ExtenderConnectModeTcpTls)
				return 0 < space.extenderBlockReportStats().ControlCount
			})

			if test.reported {
				testSeedWaitForTimeout(t, 60*time.Second, func() bool {
					return 0 < len(api.blockReported())
				})
				reported := api.blockReported()
				if reported[0] != blockedKeyHex {
					t.Fatalf("the report named %s, expected %s", reported[0], blockedKeyHex)
				}
				if api.servedAuthorized("POST", "/network/extender-block-report") == 0 {
					t.Error("the report carried no client token")
				}
				// the sender reads the answer a moment after the operator wrote it
				testSeedWaitForTimeout(t, 60*time.Second, func() bool {
					return space.extenderBlockReportStats().ReportCounts[extenderBlockReportRecorded] == 1
				})
				if stats := space.extenderBlockReportStats(); stats.CountedAttemptCount < 3 {
					t.Errorf("stats %+v", stats)
				}
				if api.served("GET", "/hello") < 6 {
					t.Errorf("the control route answered %d times for three attempts", api.served("GET", "/hello"))
				}
				// the suspect's own address was dialed, and only ever by an attempt
				dials := 0
				for _, address := range dialer.dialed() {
					if strings.HasPrefix(address, blockedIp.String()+":") {
						dials += 1
					}
				}
				if dials < 3 {
					t.Errorf("the suspect was dialed %d times", dials)
				}
			} else {
				testSeedWaitForTimeout(t, 60*time.Second, func() bool {
					return 0 < space.extenderBlockReportStats().AttemptCounts[test.outcome]
				})
			}

			// a little more time changes nothing: one report, or none
			reportedCount := len(api.blockReported())
			time.Sleep(500 * time.Millisecond)
			if count := len(api.blockReported()); count != reportedCount || 1 < count {
				t.Fatalf("%d reports, then %d", reportedCount, count)
			}
			stats := space.extenderBlockReportStats()
			if stats.AttemptCounts[test.outcome] == 0 {
				t.Fatalf("no attempt came to %s: %+v", test.outcome, stats)
			}
			if !test.reported && (0 < stats.CountedAttemptCount || 0 < len(stats.ReportCounts)) {
				t.Fatalf("an extender that answers went toward a report: %+v", stats)
			}
		})
	}
}
