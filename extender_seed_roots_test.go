package sdk

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Root rollover and removal at the seed store (EXTENDER-UPDATE2.md §2.2,
// §2.4): a bundle signed only by a new key waits for the authenticated root
// update that brings the key, the sequence continues across the rotation, a
// removed key's bundles are refused after the update, and a floor whose bundle
// no root in force verifies is void -- dropped from the store as soon as the
// space installs the keys, so neither a removed nor a hostile root holds the
// space's floor.

// The operator's next root key, which a rotation brings.
var testSeedRotatedRootPrivateKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x53}, ed25519.SeedSize))

// A root a hostile share's settings carry.
var testSeedHostileRootPrivateKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x54}, ed25519.SeedSize))

func testSeedRootHex(rootPrivateKey ed25519.PrivateKey) string {
	return hex.EncodeToString(rootPrivateKey.Public().(ed25519.PublicKey))
}

// Installs `rootPrivateKeys`' public keys as the space's configured root keys,
// the way a settings change does.
func testSeedSetRootKeys(space *NetworkSpace, rootPrivateKeys ...ed25519.PrivateKey) {
	rootPublicKeyHexes := []string{}
	for _, rootPrivateKey := range rootPrivateKeys {
		rootPublicKeyHexes = append(rootPublicKeyHexes, testSeedRootHex(rootPrivateKey))
	}
	space.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ExtenderRootPublicKeys = rootPublicKeyHexes
	})
}

// A bundle `signer` signs whose one record, of the fixture extender `index`,
// `signer` signs too.
func testSeedSignedBundleSpec(t *testing.T, now time.Time, signer ed25519.PrivateKey, sequence uint64, index int) testSeedBundleSpec {
	t.Helper()
	return testSeedBundleSpec{
		sequence: sequence,
		signer:   signer,
		records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
			index:      index,
			issueTime:  now.Add(-2 * time.Hour),
			expireTime: now.Add(48 * time.Hour),
			signer:     signer,
		})},
	}
}

// The floor the space's seed store file holds for the fixture network as a
// reader that trusts `rootKeys` reads it: another process holding the space,
// or this one after the keys change back.
func testSeedStoredSequence(t *testing.T, space *NetworkSpace, rootKeys *connect.ExtenderRootKeySet) uint64 {
	t.Helper()
	store := space.getExtenderSeed().store.(*localStateExtenderSeedStore)
	reader := connect.NewExtenderSeedAcceptor(&localStateExtenderSeedStore{localStorageDir: store.localStorageDir}, nil)
	sequence, err := reader.Sequence(testSeedNetworkHost, rootKeys)
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func testSeedRootKeySetOf(t *testing.T, rootPrivateKey ed25519.PrivateKey) *connect.ExtenderRootKeySet {
	t.Helper()
	rootKeys, err := connect.NewExtenderRootKeySetFromHex(testSeedRootHex(rootPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return rootKeys
}

func TestExtenderSeedRootRolloverAndRemoval(t *testing.T) {
	space, clock, dialer := newTestSeedImportSpace(t, testSeedStoragePath(t))
	now := clock.Now()
	importSpec := func(spec testSeedBundleSpec) *ExtenderSeedImportResult {
		return space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, spec)))
	}
	rotated := testSeedRotatedRootPrivateKey

	if result := importSpec(testSeedBundleSpec{sequence: 5}); !result.Ok {
		t.Fatalf("the operator's bundle: %+v", result)
	}
	// a bundle signed only by the next key waits for the root update that
	// brings the key
	if result := importSpec(testSeedSignedBundleSpec(t, now, rotated, 6, 1)); result.Ok || result.Error != ExtenderSeedImportErrorForged {
		t.Fatalf("the next key's bundle before the root update: %+v", result)
	}

	// the rotation ships both keys
	testSeedSetRootKeys(space, testSeedRootPrivateKey, rotated)
	if result := importSpec(testSeedSignedBundleSpec(t, now, rotated, 6, 1)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Fatalf("the next key's bundle after the root update: %+v", result)
	}
	// the sequence continues across the rotation
	if result := importSpec(testSeedSignedBundleSpec(t, now, rotated, 4, 2)); result.Ok || result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("a lower sequence under the next key: %+v", result)
	}

	// the old key is dropped: its bundles are refused from now on, and the
	// floor, which a bundle the next key signed set, still binds
	testSeedSetRootKeys(space, rotated)
	if result := importSpec(testSeedSignedBundleSpec(t, now, testSeedRootPrivateKey, 7, 3)); result.Ok || result.Error != ExtenderSeedImportErrorForged {
		t.Errorf("the removed key's bundle: %+v", result)
	}
	if result := importSpec(testSeedSignedBundleSpec(t, now, rotated, 5, 2)); result.Ok || result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("a lower sequence after the removal: %+v", result)
	}
	if sequence := testSeedStoredSequence(t, space, testSeedRootKeySetOf(t, rotated)); sequence != 6 {
		t.Errorf("the floor after the removal is %d", sequence)
	}
	// the removed key's records left the candidates with it
	if space.extenderDirectory.AddressUsable(testSeedExtenderIp(0)) {
		t.Error("a record the removed key signed is still usable")
	}
	if !space.extenderDirectory.AddressUsable(testSeedExtenderIp(1)) {
		t.Error("the next key's record is not usable")
	}
	if dialed := dialer.dialed(); len(dialed) != 0 {
		t.Errorf("the rollover dialed %v", dialed)
	}
}

// A floor whose bundle no key in force verifies is void, and the space drops it
// from the store when it installs the keys, before the change returns: a
// reader that still trusts the removed key -- another process holding the
// space -- no longer finds it there, and the space is where a fresh install
// starts, replay bounded by the validity window.
func TestExtenderSeedRootRemovalDropsTheVoidFloorAtOnce(t *testing.T) {
	space, clock, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
	now := clock.Now()
	if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 9}))); !result.Ok {
		t.Fatalf("the operator's bundle: %+v", result)
	}
	if sequence := testSeedStoredSequence(t, space, testSeedRootKeySet(t)); sequence != 9 {
		t.Fatalf("the floor before the removal is %d", sequence)
	}

	testSeedSetRootKeys(space, testSeedRotatedRootPrivateKey)

	if sequence := testSeedStoredSequence(t, space, testSeedRootKeySet(t)); sequence != 0 {
		t.Errorf("the store still holds the removed key's floor %d", sequence)
	}
	if status := space.GetExtenderSeedStatus(); status.Sequence != 0 || status.Source != "" {
		t.Errorf("the status after the removal is %+v", status)
	}
	result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedSignedBundleSpec(t, now, testSeedRotatedRootPrivateKey, 3, 1))))
	if !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("the next key's bundle at a lower sequence: %+v", result)
	}
}

// A share's settings are the one way a user replaces the trust anchor (K7),
// and a hostile root taken that way can sign a bundle at any sequence. Once the
// user takes the operator's root back, that floor is void: it never holds the
// genuine feed's sequence below it, and nothing the hostile root signed stays
// (§2.2, K7).
func TestExtenderSeedHostileRootFromAShareLosesItsFloor(t *testing.T) {
	space, clock, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
	now := clock.Now()
	deviceLocal, err := newDeviceLocalWithOverrides(
		space, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	vc := newExtenderViewController(deviceLocal.ctx, deviceLocal)
	t.Cleanup(vc.Close)

	if result := vc.ImportSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 2}))); !result.Ok {
		t.Fatalf("the operator's bundle: %+v", result)
	}

	shareText, err := connect.EncodeExtenderShare(&protocol.ExtenderShare{
		NetworkHost: testSeedNetworkHost,
		Addresses:   [][]byte{testSeedExtenderIp(7).AsSlice()},
		Settings: &protocol.ExtenderShareSettings{
			RootPublicKeys: [][]byte{testSeedHostileRootPrivateKey.Public().(ed25519.PublicKey)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := vc.ImportShare(shareText, true); !result.Ok {
		t.Fatalf("the share with settings: %+v", result)
	}
	hostileSequence := uint64(1) << 40
	hostile := testSeedEnvelope(t, now, testSeedSignedBundleSpec(t, now, testSeedHostileRootPrivateKey, hostileSequence, 4))
	if result := vc.ImportSeed(testSeedText(t, hostile)); !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Fatalf("the hostile root's bundle under the hostile root: %+v", result)
	}

	// the user takes the operator's root back
	testSeedSetRootKeys(space, testSeedRootPrivateKey)

	if sequence := testSeedStoredSequence(t, space, testSeedRootKeySetOf(t, testSeedHostileRootPrivateKey)); sequence != 0 {
		t.Errorf("the store still holds the hostile floor %d", sequence)
	}
	result := vc.ImportSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 3})))
	if !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("the operator's next bundle after the hostile root: %+v", result)
	}
	if space.extenderDirectory.AddressUsable(testSeedExtenderIp(4)) {
		t.Error("a record the hostile root signed is still usable")
	}
	if status := vc.GetSeedStatus(); status.Sequence != 3 {
		t.Errorf("the status after the operator's root came back is %+v", status)
	}
}

// Keys a hello installs reach the directory without the space hearing of it;
// the seed store is revalidated at the next look: the next wake of the loop,
// or the next pass.
func TestExtenderSeedRevalidatesAfterKeysTheSpaceDidNotInstall(t *testing.T) {
	for _, look := range []string{"loop", "pass"} {
		t.Run(look, func(t *testing.T) {
			if look == "loop" {
				previousLoopDisabled := extenderSeedLoopDisabled.Swap(false)
				t.Cleanup(func() {
					extenderSeedLoopDisabled.Store(previousLoopDisabled)
				})
			}
			space, clock, _ := newTestSeedImportSpace(t, testSeedStoragePath(t))
			if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 9}))); !result.Ok {
				t.Fatalf("the operator's bundle: %+v", result)
			}

			// as the network client's hello installs keys
			space.extenderDirectory.SetRootKeys(testSeedRootKeySetOf(t, testSeedRotatedRootPrivateKey))

			switch look {
			case "loop":
				connect.NetworkChanged()
				// the loop drops the floor, then reads the status's own again
				testSeedWaitFor(t, func() bool {
					return testSeedStoredSequence(t, space, testSeedRootKeySet(t)) == 0 &&
						space.GetExtenderSeedStatus().Sequence == 0
				})
			case "pass":
				if sequence := testSeedStoredSequence(t, space, testSeedRootKeySet(t)); sequence != 9 {
					t.Fatalf("the floor before the pass is %d", sequence)
				}
				space.getExtenderSeed().awaitPass(context.Background())
				if sequence := testSeedStoredSequence(t, space, testSeedRootKeySet(t)); sequence != 0 {
					t.Errorf("after the pass the store still holds the void floor %d", sequence)
				}
			}
			if status := space.GetExtenderSeedStatus(); status.Sequence != 0 {
				t.Errorf("the status after the revalidation is %+v", status)
			}
		})
	}
}
