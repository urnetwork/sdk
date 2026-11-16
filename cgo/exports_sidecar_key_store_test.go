// sdk.Sidecar's app interfaces through the real c abi (SDK-M8-15; sidecar §22.5's §18 row, §18.5's SidecarKeyStore
// and SidecarPresenceSigner, D-SDK-15, FS-124's app-host rows): a C host hands urnet_new_sidecar_local its key store
// as three C functions and a user_data, and its presence key as two, and the library calls them through the adapters
// the export builds. Every answer a C store can give reaches the sdk as one status, with nothing thrown and nothing
// created on an answer the sdk cannot vouch for; a Sidecar opened through the abi keeps §5.9's store rules; and a
// presence key's evidence crosses as the bytes the key answered, both §16.6 profiles' vectors among them.
//
// The C side is abitest's store, signer and result callback, written as a C host writes them. Nothing here enrolls, so
// the core's scan of this host's app stores, which only an enrolled sidecar or an enrollment runs, reads nothing.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/cgo/abitest"
)

// How long a test waits for the library: far past any answer here, so reaching it is a failure.
const testingSidecarAbiTimeout = 30 * time.Second

// The string a char* the library returned holds, freed as the library's strings are; false for NULL.
func testingAbiString(value reflect.Value) (string, bool) {
	if value.IsNil() {
		return "", false
	}
	text := reflect.ValueOf(goString).Call([]reflect.Value{value})[0].String()
	reflect.ValueOf(urnet_free_string).Call([]reflect.Value{value})
	return text, true
}

// An adapter of the export's type over a C host's functions and user_data, field for field, as the export builds it.
func testingAbiAdapter[T any](t *testing.T, adapter *T, userData unsafe.Pointer, functions map[string]unsafe.Pointer) *T {
	t.Helper()
	fields := reflect.ValueOf(adapter).Elem()
	*(*unsafe.Pointer)(unsafe.Pointer(fields.FieldByName("userData").UnsafeAddr())) = userData
	for name, function := range functions {
		field := fields.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("the adapter has no field %s", name)
		}
		*(*unsafe.Pointer)(unsafe.Pointer(field.UnsafeAddr())) = function
	}
	return adapter
}

// Each answer a C store gives crosses as the status the sdk reads, once a call: a write's status as the C function
// returned it, a read's json decoded, and NULL or text that is no json as no secret, which the sdk's adapter takes as
// Failed (sidecar_keystore_test.go's table), never as Absent.
func TestSidecarKeyStoreAnswersCrossTheAbi(t *testing.T) {
	store := abitest.NewSidecarKeyStore()
	defer store.Free()
	var keyStore sdk.SidecarKeyStore = testingAbiAdapter(t, &cAdapterSidecarKeyStore{}, store.UserData(), map[string]unsafe.Pointer{
		"cbPutSecret":    store.PutSecretFunction(),
		"cbGetSecret":    store.GetSecretFunction(),
		"cbDeleteSecret": store.DeleteSecretFunction(),
	})

	if status := keyStore.PutSecret("sidecar.u", "dSBvZiBhIHRlc3Q="); status != sdk.SidecarKeyStoreOk {
		t.Fatalf("a working put answered %d", status)
	}
	if secret := keyStore.GetSecret("sidecar.u"); secret == nil || secret.Status != sdk.SidecarSecretPresent || secret.ValueBase64 != "dSBvZiBhIHRlc3Q=" {
		t.Fatalf("a working get answered %+v", secret)
	}
	if secret := keyStore.GetSecret("sidecar.other"); secret == nil || secret.Status != sdk.SidecarSecretAbsent || secret.ValueBase64 != "" {
		t.Errorf("a get of a name never put answered %+v", secret)
	}
	for _, mode := range []struct {
		name   string
		mode   int
		write  int32
		secret *sdk.SidecarSecret
	}{
		{"locked", abitest.SidecarKeyStoreLocked, sdk.SidecarKeyStoreLocked, &sdk.SidecarSecret{Status: sdk.SidecarSecretLocked}},
		{"failed", abitest.SidecarKeyStoreFailed, sdk.SidecarKeyStoreFailed, &sdk.SidecarSecret{Status: sdk.SidecarSecretFailed}},
		{"a NULL read", abitest.SidecarKeyStoreNull, sdk.SidecarKeyStoreFailed, nil},
		{"a read that is no json", abitest.SidecarKeyStoreGarbage, sdk.SidecarKeyStoreFailed, nil},
		{"a status no header names", abitest.SidecarKeyStoreUnknown, 99, &sdk.SidecarSecret{Status: 99}},
		{"a value that is no base64", abitest.SidecarKeyStoreCorrupt, sdk.SidecarKeyStoreFailed, &sdk.SidecarSecret{Status: sdk.SidecarSecretPresent, ValueBase64: "not base64!"}},
	} {
		store.SetMode(mode.mode)
		before := store.Counts()
		if status := keyStore.PutSecret("sidecar.u", "b3RoZXI="); status != mode.write {
			t.Errorf("%s: put answered %d", mode.name, status)
		}
		secret := keyStore.GetSecret("sidecar.u")
		if (secret == nil) != (mode.secret == nil) || secret != nil && *secret != *mode.secret {
			t.Errorf("%s: get answered %+v", mode.name, secret)
		}
		if status := keyStore.DeleteSecret("sidecar.u"); status != mode.write {
			t.Errorf("%s: delete answered %d", mode.name, status)
		}
		after := store.Counts()
		if after.PutCount-before.PutCount != 1 || after.GetCount-before.GetCount != 1 || after.DeleteCount-before.DeleteCount != 1 ||
			after.WrittenCount != before.WrittenCount || after.DeletedCount != before.DeletedCount {
			t.Errorf("%s: the host took %+v after %+v", mode.name, after, before)
		}
	}
	store.SetMode(abitest.SidecarKeyStoreWorking)
	if secret := keyStore.GetSecret("sidecar.u"); secret == nil || secret.Status != sdk.SidecarSecretPresent || secret.ValueBase64 != "dSBvZiBhIHRlc3Q=" {
		t.Errorf("a refused write changed the item: %+v", secret)
	}
}

// A Sidecar opened through the abi on a C store keeps §5.9's rules (D-CORE-15, D-SDK-15): on a locked store it opens
// locked and creates nothing until urnet_sidecar_unlock(""), whose callback answers once, makes U; reopened on the
// same store it opens U again and writes nothing; and on every answer the sdk cannot vouch for it opens locked and
// creates nothing, on the store or in its state directory.
func TestNewSidecarLocalThroughTheAbiKeepsTheStoreRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://api.sdktest.example", "wss://connect.sdktest.example", testingOfflineClientStrategySettings())
	defer networkSpace.Close()
	// no Sidecar here reaches an api: the test is the store's
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	defer handleRelease(spaceHandle)

	// a Sidecar opened through the abi on store over stateDir, its handle released and the Sidecar closed and waited
	// for at the test's end, before the store is freed
	open := func(store *abitest.SidecarKeyStore, stateDir string) (uint64, *sdk.Sidecar) {
		t.Helper()
		settingsBytes, err := json.Marshal(map[string]any{"state_dir": stateDir})
		if err != nil {
			t.Fatal(err)
		}
		settings := cString(string(settingsBytes))
		defer cStringFree(settings)
		outError := socketABIOut(urnet_new_sidecar_local, 9)
		sidecarHandle := socketABICall(
			urnet_new_sidecar_local,
			spaceHandle,
			testingAbiFunction(urnet_new_sidecar_local, 1, store.DeleteSecretFunction()),
			testingAbiFunction(urnet_new_sidecar_local, 2, store.GetSecretFunction()),
			testingAbiFunction(urnet_new_sidecar_local, 3, store.PutSecretFunction()),
			store.UserData(),
			nil,
			nil,
			nil,
			settings,
			outError,
		)[0].Uint()
		if sidecarHandle == 0 {
			t.Fatalf("no Sidecar opened: %q", socketABIError(outError))
		}
		local, ok := resolveHandle[*sdk.Sidecar](sidecarHandle, "the test")
		if !ok {
			t.Fatal("the handle is no Sidecar")
		}
		t.Cleanup(func() {
			socketABICall(urnet_sidecar_close, sidecarHandle)
			closeCtx, closeCancel := context.WithTimeout(context.Background(), testingSidecarAbiTimeout)
			defer closeCancel()
			if err := local.CloseAndWait(closeCtx); err != nil {
				t.Errorf("close: %v", err)
			}
			handleRelease(sidecarHandle)
		})
		return sidecarHandle, local
	}
	// waits until the state the abi reads is want, at each state event, subscribed before each read
	awaitState := func(sidecarHandle uint64, local *sdk.Sidecar, want string) {
		t.Helper()
		changed := make(chan struct{}, 1)
		sub := local.AddEventListener(&testingSidecarStateListener{changed: changed})
		defer sub.Close()
		deadline := time.After(testingSidecarAbiTimeout)
		for {
			state, _ := testingAbiString(socketABICall(urnet_sidecar_get_state, sidecarHandle)[0])
			if state == want {
				return
			}
			select {
			case <-changed:
			case <-deadline:
				t.Fatalf("the state is %q, never %q", state, want)
			}
		}
	}
	// the state directory's files
	stateFiles := func(stateDir string) []string {
		t.Helper()
		files := []string{}
		err := filepath.WalkDir(stateDir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr == nil && !entry.IsDir() {
				files = append(files, path)
			}
			return walkErr
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return files
	}

	// locked: nothing created until the unlock, which makes U once and answers once
	store := abitest.NewSidecarKeyStore()
	t.Cleanup(store.Free)
	stateDir := t.TempDir()
	store.SetMode(abitest.SidecarKeyStoreLocked)
	lockedHandle, locked := open(store, stateDir)
	awaitState(lockedHandle, locked, "locked")
	if counts := store.Counts(); counts.WrittenCount != 0 || counts.DeletedCount != 0 || counts.ItemCount != 0 {
		t.Errorf("a locked store took writes: %+v", counts)
	}
	if files := stateFiles(stateDir); len(files) != 0 {
		t.Errorf("a locked store left files: %v", files)
	}
	store.SetMode(abitest.SidecarKeyStoreWorking)
	unlocked := abitest.NewResultCallback()
	t.Cleanup(unlocked.Free)
	passphrase := cString("")
	socketABICall(urnet_sidecar_unlock, lockedHandle, passphrase, testingAbiFunction(urnet_sidecar_unlock, 2, unlocked.ResultFunction()), unlocked.UserData())
	cStringFree(passphrase)
	if count, resultJson, errorText := unlocked.Wait(testingSidecarAbiTimeout); count != 1 || errorText != "" {
		t.Fatalf("the unlock answered %d times: %s %q", count, resultJson, errorText)
	}
	awaitState(lockedHandle, locked, "unenrolled")
	if counts := store.Counts(); counts.WrittenCount != 1 || counts.ItemCount != 1 {
		t.Errorf("the unlock made U in %+v", counts)
	}

	// reopened on the same store and state: U opens, and nothing is written
	socketABICall(urnet_sidecar_close, lockedHandle)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), testingSidecarAbiTimeout)
	if err := locked.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
	closeCancel()
	before := store.Counts()
	reopenedHandle, reopened := open(store, stateDir)
	awaitState(reopenedHandle, reopened, "unenrolled")
	if after := store.Counts(); after.WrittenCount != before.WrittenCount || after.DeletedCount != before.DeletedCount || after.ItemCount != 1 {
		t.Errorf("the reopen changed the store: %+v after %+v", after, before)
	}

	// each answer the sdk cannot vouch for: locked, nothing created
	for _, mode := range []struct {
		name string
		mode int
	}{
		{"failed", abitest.SidecarKeyStoreFailed},
		{"a NULL read", abitest.SidecarKeyStoreNull},
		{"a read that is no json", abitest.SidecarKeyStoreGarbage},
		{"a status no header names", abitest.SidecarKeyStoreUnknown},
		{"a value that is no base64", abitest.SidecarKeyStoreCorrupt},
	} {
		failing := abitest.NewSidecarKeyStore()
		t.Cleanup(failing.Free)
		failing.SetMode(mode.mode)
		failingDir := t.TempDir()
		failingHandle, failingSidecar := open(failing, failingDir)
		awaitState(failingHandle, failingSidecar, "locked")
		if counts := failing.Counts(); counts.GetCount == 0 || counts.WrittenCount != 0 || counts.DeletedCount != 0 {
			t.Errorf("%s: the store took %+v", mode.name, counts)
		}
		if files := stateFiles(failingDir); len(files) != 0 {
			t.Errorf("%s: files were created: %v", mode.name, files)
		}
	}
	if count := unlocked.Count(); count != 1 {
		t.Errorf("the unlock answered %d times", count)
	}
}

// Hears state events.
type testingSidecarStateListener struct {
	changed chan struct{}
}

func (self *testingSidecarStateListener) SidecarEventReceived(event *sdk.SidecarEvent) {
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// A presence key's evidence crosses the abi as the bytes the key answered (FS-124, §16.6's two profiles): each
// countersignature vector's evidence, answered by a C host's key, reaches the sdk as the same base64, which the sdk's
// own test (sidecar_presence_vectors_test.go) holds byte-equal through to the statement; the message the key signs
// reaches it as the sdk passed it; and a NULL answer reads as refused.
func TestSidecarPresenceSignerAnswersCrossTheAbi(t *testing.T) {
	vectorsBytes, err := os.ReadFile(filepath.Join("..", "testdata", "presence", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	vectors := struct {
		Countersignatures []struct {
			Name   string `json:"name"`
			Key    string `json:"key"`
			Signed string `json:"signed"`
		} `json:"countersignatures"`
	}{}
	if err := json.Unmarshal(vectorsBytes, &vectors); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]int{}
	for _, vector := range vectors.Countersignatures {
		signedBytes, err := hex.DecodeString(vector.Signed)
		if err != nil {
			t.Fatal(err)
		}
		// the statement's countersignature, Signed's field 5, as its bytes are on the wire (§6.9)
		var evidenceBytes []byte
		for encoded := signedBytes; 0 < len(encoded) && evidenceBytes == nil; {
			number, fieldType, size := protowire.ConsumeTag(encoded)
			if size < 0 {
				t.Fatal(protowire.ParseError(size))
			}
			encoded = encoded[size:]
			if number == 5 && fieldType == protowire.BytesType {
				evidenceBytes, size = protowire.ConsumeBytes(encoded)
			} else {
				size = protowire.ConsumeFieldValue(number, fieldType, encoded)
			}
			if size < 0 {
				t.Fatal(protowire.ParseError(size))
			}
			encoded = encoded[size:]
		}
		if len(evidenceBytes) == 0 {
			t.Fatalf("%s: no countersignature", vector.Name)
		}
		answer := base64.StdEncoding.EncodeToString(evidenceBytes)
		signer := abitest.NewPresenceSigner(answer)
		var presenceSigner sdk.SidecarPresenceSigner = testingAbiAdapter(t, &cAdapterSidecarPresenceSigner{}, signer.UserData(), map[string]unsafe.Pointer{
			"cbGetAttestation":   signer.GetAttestationFunction(),
			"cbSignWithPresence": signer.SignWithPresenceFunction(),
		})
		message := base64.StdEncoding.EncodeToString([]byte("synthetic act bytes of " + vector.Name))
		if crossed := presenceSigner.SignWithPresence(message); crossed != answer {
			t.Errorf("%s: the evidence crossed as %q", vector.Name, crossed)
		}
		if count, asked := signer.Asked(); count != 1 || asked != message {
			t.Errorf("%s: the key was asked %d times, last %q", vector.Name, count, asked)
		}
		signer.SetAnswersNull(true)
		if refused := presenceSigner.SignWithPresence(message); refused != "" {
			t.Errorf("%s: a NULL answer read as %q", vector.Name, refused)
		}
		if attestation := presenceSigner.GetAttestation(); attestation != "" {
			t.Errorf("%s: the attestation read as %q", vector.Name, attestation)
		}
		signer.Free()
		profiles[vector.Key] += 1
	}
	if profiles["webauthn-es256"] == 0 || profiles["ecdsa-p256-sha256"] == 0 {
		t.Errorf("the vectors cover %v", profiles)
	}
}

// A host that gives the store with a function missing gets no answer from it, never a call through NULL: the export
// builds the adapter once delete_secret is given, and each invoke shim answers its zero value for a NULL function, a
// read nil and a write 0, which the sdk reads as Failed. Before the shims checked, the core's first read of such a
// store ended the process in C, past any recover.
func TestAMissingHostFunctionAnswersNothing(t *testing.T) {
	store := abitest.NewSidecarKeyStore()
	defer store.Free()
	var keyStore sdk.SidecarKeyStore = testingAbiAdapter(t, &cAdapterSidecarKeyStore{}, store.UserData(), map[string]unsafe.Pointer{
		"cbDeleteSecret": store.DeleteSecretFunction(),
	})
	if secret := keyStore.GetSecret("sidecar.u"); secret != nil {
		t.Errorf("a NULL get_secret answered %+v", secret)
	}
	if status := keyStore.PutSecret("sidecar.u", "dQ=="); status != sdk.SidecarKeyStoreFailed {
		t.Errorf("a NULL put_secret answered %d", status)
	}
	if status := keyStore.DeleteSecret("sidecar.u"); status != sdk.SidecarKeyStoreOk {
		t.Errorf("the given delete_secret answered %d", status)
	}
	if counts := store.Counts(); counts.PutCount != 0 || counts.GetCount != 0 || counts.DeleteCount != 1 {
		t.Errorf("the host took %+v", counts)
	}
	var presenceSigner sdk.SidecarPresenceSigner = testingAbiAdapter(t, &cAdapterSidecarPresenceSigner{}, nil, map[string]unsafe.Pointer{})
	if answer := presenceSigner.SignWithPresence("bWVzc2FnZQ=="); answer != "" {
		t.Errorf("a NULL sign_with_presence answered %q", answer)
	}
}
