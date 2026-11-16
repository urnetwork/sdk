package sdk

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The import of an authenticated seed bundle from pasted text or a file
// (extender_seed_import.go; EXTENDER-UPDATE2.md §2.3, §2.4): a valid bundle
// lands verified records, and every other kind gets its typed refusal with
// nothing applied, no trust anchor changed and no dial made.

// A seed test space with the fixture clock, the recorded dialer, and no
// embedded bundle.
func newTestSeedImportSpace(t *testing.T, storagePath string) (*NetworkSpace, *testSeedClock, *testSeedDialer) {
	t.Helper()
	clock := newTestSeedClock()
	dialer := newTestSeedDialer()
	testSeedInstallConnectSettings(t, dialer, nil)
	testSeedInstallEmbedded(t, "")
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
	})
	space := newTestSeedSpace(t, storagePath)
	if space.getExtenderSeed() == nil {
		t.Fatal("the seed test space runs no seed bootstrap")
	}
	return space, clock, dialer
}

func TestExtenderSeedImportAppliesAValidBundle(t *testing.T) {
	space, clock, dialer := newTestSeedImportSpace(t, testSeedStoragePath(t))
	envelope := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 3})

	result := space.ImportExtenderSeed("  " + testSeedText(t, envelope) + "\n")
	if !result.Ok || result.Error != "" || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Fatalf("the valid bundle was not accepted: %+v", result)
	}
	if result.NetworkHost != testSeedNetworkHost || result.Sequence != 3 ||
		result.RecordCount != 1 || result.RevocationCount != 0 || result.UsableCount != 1 {
		t.Errorf("the result describes another bundle: %+v", result)
	}
	if result.ExpireTime <= clock.Now().UnixMilli() {
		t.Errorf("the result's expiry %d is not ahead", result.ExpireTime)
	}
	entry := testSeedDirectoryEntry(space, 0)
	if entry == nil {
		t.Fatal("the bundle's record is not in the directory")
	}
	if entry.Source != connect.ExtenderSourceSeed || entry.State != connect.ExtenderStateActive || len(entry.PublicKey) == 0 {
		t.Errorf("the record landed as %+v", entry)
	}
	if !space.extenderDirectory.AddressUsable(testSeedExtenderIp(0)) {
		t.Error("the bundle's address is not usable")
	}
	status := space.GetExtenderSeedStatus()
	if status.Sequence != 3 || status.Source != ExtenderSeedSourceImport || status.ExpireTime != result.ExpireTime {
		t.Errorf("the status after the import is %+v", status)
	}
	// what it made usable may carry the gated release
	if count := space.extenderReleaseRequestCount(); count != 1 {
		t.Errorf("the import requested the gated release %d times", count)
	}

	// the same bundle again, in each form, changes nothing
	again := space.ImportExtenderSeedData(testSeedBytes(t, envelope))
	if !again.Ok || again.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("the raw envelope again: %+v", again)
	}
	textPath := filepath.Join(t.TempDir(), "seed.txt")
	if err := os.WriteFile(textPath, []byte(testSeedText(t, envelope)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if fromFile := space.ImportExtenderSeedFile(textPath); !fromFile.Ok || fromFile.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("the text file: %+v", fromFile)
	}
	binaryPath := filepath.Join(t.TempDir(), "seed.bin")
	if err := os.WriteFile(binaryPath, testSeedBytes(t, envelope), 0600); err != nil {
		t.Fatal(err)
	}
	if fromFile := space.ImportExtenderSeedFile(binaryPath); !fromFile.Ok || fromFile.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("the binary file: %+v", fromFile)
	}
	if missing := space.ImportExtenderSeedFile(filepath.Join(t.TempDir(), "missing")); missing.Ok ||
		missing.Error != ExtenderSeedImportErrorUnavailable {
		t.Errorf("a missing file: %+v", missing)
	}

	if count := space.extenderReleaseRequestCount(); count != 1 {
		t.Errorf("an import that made nothing new usable requested the gated release: %d", count)
	}
	// an import applies; it never dials
	if dialed := dialer.dialed(); len(dialed) != 0 {
		t.Errorf("the import dialed %v", dialed)
	}
}

// Every bundle a client must refuse, refused with its own id: nothing of it
// is applied, the trust anchor stays, and nothing is dialed (§2.4).
func TestExtenderSeedImportRefusesWithTypedResults(t *testing.T) {
	now := time.Now()
	recordSpec := func(index int) testSeedRecordSpec {
		return testSeedRecordSpec{
			index:      index,
			issueTime:  now.Add(-2 * time.Hour),
			expireTime: now.Add(48 * time.Hour),
		}
	}
	nineRecords := []*protocol.ExtenderRecord{}
	for index := range connect.ExtenderSeedMaxRecordCount + 1 {
		nineRecords = append(nineRecords, testSeedRecord(t, recordSpec(index)))
	}
	gatedRecord := recordSpec(0)
	gatedRecord.tier = connect.ExtenderDirectoryTierGated
	canaryRecord := recordSpec(0)
	canaryRecord.canary = "dns:EU"
	forgedRecord := recordSpec(0)
	forgedRecord.signer = testSeedForgerPrivateKey
	foreignRecord := recordSpec(0)
	foreignRecord.networkHost = testSeedForeignNetworkHost

	adversarialText := func(spec testSeedBundleSpec) string {
		return testSeedAdversarialText(t, testSeedAdversarialEnvelope(testSeedRootPrivateKey, testSeedBody(t, now, spec)))
	}
	// a body that tries to carry trust: an unknown field holding another root
	// key, which a reader that ignored unknown fields would never judge
	settingsBodyBytes := testSeedAdversarialEnvelope(
		testSeedRootPrivateKey,
		testSeedBody(t, now, testSeedBundleSpec{sequence: 2}),
	).Body
	settingsBodyBytes = protowire.AppendTag(settingsBodyBytes, 99, protowire.BytesType)
	settingsBodyBytes = protowire.AppendBytes(
		settingsBodyBytes,
		[]byte(testSeedForgerPrivateKey.Public().(ed25519.PublicKey)),
	)
	settingsEnvelope := testSeedAdversarialEnvelopeBytes(testSeedRootPrivateKey, settingsBodyBytes)

	oversizedRecord := testSeedRecord(t, recordSpec(0))
	oversizedRecord.RootSignature = bytes.Repeat([]byte{1}, connect.ExtenderSeedMaxEnvelopeByteCount)

	cases := []struct {
		name  string
		text  string
		error string
	}{
		{
			name:  "forged envelope",
			text:  testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{signer: testSeedForgerPrivateKey})),
			error: ExtenderSeedImportErrorForged,
		},
		{
			name: "foreign network",
			text: testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{
				networkHost: testSeedForeignNetworkHost,
				records:     []*protocol.ExtenderRecord{testSeedRecord(t, foreignRecord)},
			})),
			error: ExtenderSeedImportErrorForeignNetwork,
		},
		{
			name:  "gated record",
			text:  adversarialText(testSeedBundleSpec{records: []*protocol.ExtenderRecord{testSeedRecord(t, gatedRecord)}}),
			error: ExtenderSeedImportErrorGatedRecord,
		},
		{
			name:  "canary record",
			text:  adversarialText(testSeedBundleSpec{records: []*protocol.ExtenderRecord{testSeedRecord(t, canaryRecord)}}),
			error: ExtenderSeedImportErrorGatedRecord,
		},
		{
			name:  "valid envelope, record signed by another key",
			text:  adversarialText(testSeedBundleSpec{records: []*protocol.ExtenderRecord{testSeedRecord(t, forgedRecord)}}),
			error: ExtenderSeedImportErrorInvalidRecord,
		},
		{
			name: "expired",
			text: testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{
				issueTime:  now.Add(-20 * 24 * time.Hour),
				expireTime: now.Add(-24 * time.Hour),
				records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
					index:      0,
					issueTime:  now.Add(-21 * 24 * time.Hour),
					expireTime: now.Add(-23 * time.Hour),
				})},
			})),
			error: ExtenderSeedImportErrorExpired,
		},
		{
			name: "issued in the future",
			text: testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{
				issueTime: now.Add(time.Hour),
				records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
					index:      0,
					issueTime:  now.Add(time.Hour - time.Minute),
					expireTime: now.Add(48 * time.Hour),
				})},
			})),
			error: ExtenderSeedImportErrorNotYetValid,
		},
		{
			name:  "too many records",
			text:  adversarialText(testSeedBundleSpec{records: nineRecords}),
			error: ExtenderSeedImportErrorTooLarge,
		},
		{
			name:  "oversized envelope",
			text:  adversarialText(testSeedBundleSpec{records: []*protocol.ExtenderRecord{oversizedRecord}}),
			error: ExtenderSeedImportErrorTooLarge,
		},
		{
			name:  "oversized paste",
			text:  connect.ExtenderSeedTextPrefix + strings.Repeat("A", extenderSeedImportMaxByteCount),
			error: ExtenderSeedImportErrorTooLarge,
		},
		{
			name:  "settings in an unknown field",
			text:  testSeedText(t, settingsEnvelope),
			error: ExtenderSeedImportErrorInvalid,
		},
		{
			name:  "a K7 share",
			text:  testSeedShareText(t),
			error: ExtenderSeedImportErrorInvalid,
		},
		{
			name:  "a later seed version",
			text:  "ur-seed:2:AAAA",
			error: ExtenderSeedImportErrorInvalid,
		},
		{
			name:  "not base64",
			text:  connect.ExtenderSeedTextPrefix + "!!!",
			error: ExtenderSeedImportErrorInvalid,
		},
		{
			name:  "nothing",
			text:  "",
			error: ExtenderSeedImportErrorInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			space, _, dialer := newTestSeedImportSpace(t, testSeedStoragePath(t))
			rootKeys := space.extenderDirectory.RootKeys()
			values := space.valuesCopy()

			result := space.ImportExtenderSeed(c.text)
			if result.Ok || result.Error != c.error {
				t.Fatalf("the import answered %+v, not %s", result, c.error)
			}
			if result.Outcome != "" || result.UsableCount != 0 {
				t.Errorf("a refusal reports %+v", result)
			}
			testSeedRequireNothingApplied(t, space)
			if space.extenderDirectory.RootKeys() != rootKeys {
				t.Error("a refused import changed the root keys")
			}
			if after := space.valuesCopy(); !slices.Equal(after.ExtenderRootPublicKeys, values.ExtenderRootPublicKeys) ||
				after.ExtenderDnsName != values.ExtenderDnsName || after.GossipUrl != values.GossipUrl ||
				!slices.Equal(after.ControlDohUrlsIpv4, values.ControlDohUrlsIpv4) {
				t.Error("a refused import changed the space's settings")
			}
			if sequence, err := space.getExtenderSeed().acceptor.Sequence(testSeedNetworkHost, rootKeys); err != nil || sequence != 0 {
				t.Errorf("a refused import left the floor %d, %v", sequence, err)
			}
			if dialed := dialer.dialed(); len(dialed) != 0 {
				t.Errorf("a refused import dialed %v", dialed)
			}
		})
	}
}

// Nothing a seed carries is in the directory, and nothing in it is a
// candidate.
func testSeedRequireNothingApplied(t *testing.T, space *NetworkSpace) {
	t.Helper()
	for _, entry := range space.extenderDirectory.Snapshot().Entries {
		if entry.Source == connect.ExtenderSourceSeed {
			t.Errorf("a refused bundle applied %v", entry.Ip)
		}
	}
	for _, ipVersion := range []int{4, 6} {
		if candidates := space.extenderDirectory.Candidates(ipVersion, 16); len(candidates) != 0 {
			t.Errorf("a refused bundle left %d ipv%d candidates", len(candidates), ipVersion)
		}
	}
}

// A K7 share of the fixture network, the unsigned payload a seed is not.
func testSeedShareText(t *testing.T) string {
	t.Helper()
	text, err := connect.EncodeExtenderShare(&protocol.ExtenderShare{
		NetworkHost: testSeedNetworkHost,
		Addresses:   [][]byte{testSeedExtenderIp(0).AsSlice()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// The sequence rules of §2.2 at the import: the identical bundle again is
// idempotent, an older one is a rollback and a different one at the accepted
// sequence a conflict, and neither replaces what is in force.
func TestExtenderSeedImportSequenceRules(t *testing.T) {
	space, clock, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
	now := clock.Now()
	five := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 5})
	if result := space.ImportExtenderSeed(testSeedText(t, five)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Fatalf("sequence 5: %+v", result)
	}
	if result := space.ImportExtenderSeed(testSeedText(t, five)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("sequence 5 again: %+v", result)
	}

	otherRecord := []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
		index:      1,
		issueTime:  now.Add(-2 * time.Hour),
		expireTime: now.Add(48 * time.Hour),
	})}
	four := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 4, records: otherRecord})
	if result := space.ImportExtenderSeed(testSeedText(t, four)); result.Ok || result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("sequence 4 after 5: %+v", result)
	}
	conflicting := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 5, records: otherRecord})
	if result := space.ImportExtenderSeed(testSeedText(t, conflicting)); result.Ok || result.Error != ExtenderSeedImportErrorConflict {
		t.Errorf("another body at sequence 5: %+v", result)
	}
	if entry := testSeedDirectoryEntry(space, 1); entry != nil {
		t.Errorf("a refused bundle's record landed: %+v", entry)
	}
	six := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 6, records: otherRecord})
	if result := space.ImportExtenderSeed(testSeedText(t, six)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("sequence 6: %+v", result)
	}
	if status := space.GetExtenderSeedStatus(); status.Sequence != 6 {
		t.Errorf("the status names sequence %d", status.Sequence)
	}
}

// A revocation in the bundle dominates the record it names: the record is
// never a candidate, before or after the bundle lands (§2.3, §2.4).
func TestExtenderSeedImportRevocationDominatesItsRecord(t *testing.T) {
	space, clock, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
	now := clock.Now()
	record := testSeedRecord(t, testSeedRecordSpec{
		index:      0,
		issueTime:  now.Add(-2 * time.Hour),
		expireTime: now.Add(48 * time.Hour),
	})
	live := testSeedRecord(t, testSeedRecordSpec{
		index:      1,
		issueTime:  now.Add(-2 * time.Hour),
		expireTime: now.Add(48 * time.Hour),
	})
	envelope := testSeedEnvelope(t, now, testSeedBundleSpec{
		records:     []*protocol.ExtenderRecord{record, live},
		revocations: []*protocol.ExtenderRevocation{testSeedRevocation(t, 0, now.Add(-time.Hour-time.Minute))},
	})
	result := space.ImportExtenderSeed(testSeedText(t, envelope))
	if !result.Ok || result.RecordCount != 1 || result.RevocationCount != 1 || result.UsableCount != 1 {
		t.Fatalf("the bundle with a revocation: %+v", result)
	}
	if space.extenderDirectory.AddressUsable(testSeedExtenderIp(0)) {
		t.Error("the revoked record's address is usable")
	}
	if !space.extenderDirectory.AddressUsable(testSeedExtenderIp(1)) {
		t.Error("the live record's address is not usable")
	}
	for _, candidate := range space.extenderDirectory.Candidates(4, 16) {
		if candidate.Ip == testSeedExtenderIp(0) {
			t.Error("the revoked record is a candidate")
		}
	}
}

// An import whose commit is interrupted leaves the previous accepted bundle
// and its sequence together, and a restart finds them (§2.2, §2.4).
func TestExtenderSeedImportInterruptedPersistenceAndRestart(t *testing.T) {
	storagePath := testSeedStoragePath(t)
	space, clock, _ := newTestSeedImportSpace(t, storagePath)
	now := clock.Now()
	first := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 1})
	secondRecords := []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
		index:      1,
		issueTime:  now.Add(-2 * time.Hour),
		expireTime: now.Add(48 * time.Hour),
	})}
	second := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 2, records: secondRecords})
	if result := space.ImportExtenderSeed(testSeedText(t, first)); !result.Ok {
		t.Fatalf("the first bundle: %+v", result)
	}

	for _, step := range []string{"write", "sync", "rename"} {
		store := space.getExtenderSeed().store.(*localStateExtenderSeedStore)
		store.testingCommitStep = func(commitStep string) error {
			if commitStep == step {
				return errors.New("the process ended")
			}
			return nil
		}
		result := space.ImportExtenderSeed(testSeedText(t, second))
		store.testingCommitStep = nil
		if result.Ok || result.Error != ExtenderSeedImportErrorUnavailable {
			t.Fatalf("%s: an import whose commit failed answered %+v", step, result)
		}
		// nothing of an uncommitted bundle is applied
		if entry := testSeedDirectoryEntry(space, 1); entry != nil {
			t.Fatalf("%s: the uncommitted bundle's record landed", step)
		}
	}
	space.close()

	// the restart reads what the last commit wrote: the first bundle and its
	// sequence, never the second sequence without its bundle
	restarted, _, _ := newTestSeedImportSpace(t, storagePath)
	seed := restarted.getExtenderSeed()
	rootKeys := restarted.extenderDirectory.RootKeys()
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, rootKeys); err != nil || sequence != 1 {
		t.Fatalf("after the restart the floor is %d, %v", sequence, err)
	}
	stored, err := seed.acceptor.Stored(testSeedNetworkHost, rootKeys, seed.networkHosts(), clock.Now())
	if err != nil || stored == nil || stored.Sequence != 1 {
		t.Fatalf("after the restart the stored bundle is %v, %v", stored, err)
	}
	if result := restarted.ImportExtenderSeed(testSeedText(t, first)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("the first bundle after the restart: %+v", result)
	}
	if result := restarted.ImportExtenderSeed(testSeedText(t, second)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("the second bundle after the restart: %+v", result)
	}

	// a commit that renamed and then failed its directory sync is a commit
	third := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 3, records: secondRecords})
	store := seed.store.(*localStateExtenderSeedStore)
	store.testingCommitStep = func(commitStep string) error {
		if commitStep == "directory sync" {
			return errors.New("the directory did not sync")
		}
		return nil
	}
	result := restarted.ImportExtenderSeed(testSeedText(t, third))
	store.testingCommitStep = nil
	if !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("a commit whose directory sync failed: %+v", result)
	}
}

// The space a cloud host shares among its hosted devices runs no seed
// bootstrap and imports nothing.
func TestExtenderSeedImportRefusedByAHostedSpace(t *testing.T) {
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	space := NewPlatformNetworkSpace(ctx, "main", testSeedNetworkHost, testOfflineConnectSettings())
	defer space.Close()
	if space.getExtenderSeed() != nil {
		t.Fatal("a hosted space runs a seed bootstrap")
	}
	envelope := testSeedEnvelope(t, time.Now(), testSeedBundleSpec{})
	if result := space.ImportExtenderSeed(testSeedText(t, envelope)); result.Ok || result.Error != ExtenderSeedImportErrorUnavailable {
		t.Errorf("a hosted space imported: %+v", result)
	}
	if status := space.GetExtenderSeedStatus(); *status != (ExtenderSeedStatus{}) {
		t.Errorf("a hosted space reports %+v", status)
	}
}

// The unsigned K7 share and the seed stay two formats: neither import reads
// the other's payload, and the share's import is unchanged (§2.1).
func TestExtenderSeedImportIsSeparateFromTheShare(t *testing.T) {
	envelope := testSeedEnvelope(t, time.Now(), testSeedBundleSpec{})
	if _, err := connect.DecodeExtenderShare(testSeedText(t, envelope)); err == nil {
		t.Error("the share decoder read a seed")
	}
	space, _, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
	if result := space.ImportExtenderSeed(testSeedShareText(t)); result.Ok || result.Error != ExtenderSeedImportErrorInvalid {
		t.Errorf("the seed import read a share: %+v", result)
	}
}

// The account screen's import goes to the device's space, in every form.
func TestExtenderSeedImportThroughTheViewController(t *testing.T) {
	space, clock, dialer := newTestSeedImportSpace(t, testSeedStoragePath(t))
	deviceLocal, err := newDeviceLocalWithOverrides(
		space, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	vc := newExtenderViewController(deviceLocal.ctx, deviceLocal)
	t.Cleanup(vc.Close)

	envelope := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 9})
	if result := vc.ImportSeed(testSeedText(t, envelope)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Fatalf("ImportSeed: %+v", result)
	}
	if result := vc.ImportSeedData(testSeedBytes(t, envelope)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("ImportSeedData: %+v", result)
	}
	path := filepath.Join(t.TempDir(), "seed.bin")
	if err := os.WriteFile(path, testSeedBytes(t, envelope), 0600); err != nil {
		t.Fatal(err)
	}
	if result := vc.ImportSeedFile(path); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeIdentical {
		t.Errorf("ImportSeedFile: %+v", result)
	}
	forged := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 10, signer: testSeedForgerPrivateKey})
	if result := vc.ImportSeed(testSeedText(t, forged)); result.Ok || result.Error != ExtenderSeedImportErrorForged {
		t.Errorf("a forged bundle through the controller: %+v", result)
	}
	if status := vc.GetSeedStatus(); status.Sequence != 9 || status.Source != ExtenderSeedSourceImport {
		t.Errorf("the controller's seed status is %+v", status)
	}
	// the device dials its platform on its own; an import dials nothing
	if dialer.dialedAddress(testSeedExtenderAddress(0)) {
		t.Errorf("an import dialed the seed's extender: %v", dialer.dialed())
	}
}

// The tcp carrier address the fixture extender `index` publishes.
func testSeedExtenderAddress(index int) string {
	return netip.AddrPortFrom(testSeedExtenderIp(index), 443).String()
}

// Every seed code maps to an import id, and a code this build does not know
// is invalid input.
func TestExtenderSeedImportErrorIds(t *testing.T) {
	for code, id := range map[string]string{
		connect.ExtenderSeedCodeMalformed:         ExtenderSeedImportErrorInvalid,
		connect.ExtenderSeedCodeTooLarge:          ExtenderSeedImportErrorTooLarge,
		connect.ExtenderSeedCodeTooMany:           ExtenderSeedImportErrorTooLarge,
		connect.ExtenderSeedCodeNoRootKeys:        ExtenderSeedImportErrorNoRootKeys,
		connect.ExtenderSeedCodeSignature:         ExtenderSeedImportErrorForged,
		connect.ExtenderSeedCodeForeignNetwork:    ExtenderSeedImportErrorForeignNetwork,
		connect.ExtenderSeedCodeRecordInvalid:     ExtenderSeedImportErrorInvalidRecord,
		connect.ExtenderSeedCodeRevocationInvalid: ExtenderSeedImportErrorInvalidRecord,
		connect.ExtenderSeedCodeGatedRecord:       ExtenderSeedImportErrorGatedRecord,
		connect.ExtenderSeedCodeNotYetValid:       ExtenderSeedImportErrorNotYetValid,
		connect.ExtenderSeedCodeExpired:           ExtenderSeedImportErrorExpired,
		connect.ExtenderSeedCodeRollback:          ExtenderSeedImportErrorRollback,
		connect.ExtenderSeedCodeConflict:          ExtenderSeedImportErrorConflict,
		connect.ExtenderSeedCodeStore:             ExtenderSeedImportErrorUnavailable,
		"a code from a later build":               ExtenderSeedImportErrorInvalid,
		"":                                        ExtenderSeedImportErrorInvalid,
	} {
		if got := extenderSeedImportErrorId(code); got != id {
			t.Errorf("%q maps to %s, not %s", code, got, id)
		}
	}
}
