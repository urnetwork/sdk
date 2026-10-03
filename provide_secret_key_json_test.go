// Deterministic binary secrets exercise the actual codec, provider, RPC and
// cold LocalState boundaries without logging any generated or retained key.
package sdk

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// Fixed invalid UTF-8 removes random key generation from the regression oracle.
func testingBinaryProvideSecret() string {
	return string([]byte{
		0xff, 0x00, 0x80, 0xc0, 0xaf, 0xed, 0xa0, 0x80,
		0xf4, 0x90, 0x80, 0x80, 0x7f, 0x01, 0x02, 0x03,
		0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
		0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13,
	})
}

func TestProvideSecretKeyJsonBinaryPreservesEveryByte(t *testing.T) {
	allBytes := make([]byte, 256)
	for index := range allBytes {
		allBytes[index] = byte(index)
	}
	for _, secret := range []string{testingBinaryProvideSecret(), string(allBytes)} {
		original := ProvideSecretKey{ProvideMode: ProvideModeNetwork, ProvideSecretKey: secret}
		data, err := json.Marshal(original)
		if err != nil || !utf8.Valid(data) {
			t.Fatal("binary provider secret did not produce valid JSON")
		}
		var restored ProvideSecretKey
		if err := json.Unmarshal(data, &restored); err != nil || restored != original {
			t.Fatal("JSON changed the actual provider secret bytes")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields["provide_secret_key_base64"] == nil || fields["provide_secret_key"] != nil {
			t.Fatal("binary provider secret used an ambiguous or lossy representation")
		}
	}
}

func TestProvideSecretKeyJsonLegacyLiteralsRemainLiteral(t *testing.T) {
	for _, secret := range []string{"", "synthetic-plain", "base64:/w==", "provide_secret_key_base64:/w==", "/w==", "literal-\ufffd-\u00e9", "with\x00zero"} {
		original := ProvideSecretKey{ProvideMode: ProvideModePublic, ProvideSecretKey: secret}
		legacy, err := json.Marshal(provideSecretKeyJSON(original))
		if err != nil {
			t.Fatal("could not encode the legacy literal fixture")
		}
		var restored ProvideSecretKey
		if json.Unmarshal(legacy, &restored) != nil || restored != original {
			t.Fatal("legacy provider secret was reinterpreted")
		}
		updated, err := json.Marshal(restored)
		if err != nil || !bytes.Equal(updated, legacy) {
			t.Fatal("valid UTF-8 provider secret changed its legacy JSON form")
		}
	}
	for _, data := range []string{"null", "{}", `{"provide_mode":null,"provide_secret_key":null}`} {
		original := ProvideSecretKey{ProvideMode: ProvideModeNetwork, ProvideSecretKey: "retained-literal"}
		restored := original
		if json.Unmarshal([]byte(data), &restored) != nil || restored != original {
			t.Fatal("legacy absent or null field changed its existing meaning")
		}
	}
}

func TestProvideSecretKeyJsonRejectsMalformedBinaryWithoutMutation(t *testing.T) {
	for _, data := range []string{
		`{"provide_secret_key_base64":null}`,
		`{"provide_secret_key_base64":17}`,
		`{"provide_secret_key_base64":[]}`,
		`{"provide_secret_key_base64":"_w=="}`,
		`{"provide_secret_key_base64":"/w="}`,
		`{"provide_secret_key_base64":"/x=="}`,
		`{"provide_secret_key_base64":"/w==\n"}`,
		`{"provide_secret_key_base64":" /w=="}`,
		`{"provide_secret_key_base64":"/w==","provide_secret_key":"retained-literal"}`,
		`{"provide_secret_key_base64":"/w==","provide_secret_key":17}`,
		`{"provide_secret_key_base64":"/w==","provide_mode":1.0}`,
		`{"provide_secret_key_base64":"/w==","provide_mode":true}`,
		`[]`,
		`"unexpected-string"`,
	} {
		original := ProvideSecretKey{ProvideMode: ProvideModeNetwork, ProvideSecretKey: "retained-literal"}
		restored := original
		err := json.Unmarshal([]byte(data), &restored)
		if err == nil || err.Error() != "decode provide secret key" || restored != original {
			t.Fatal("malformed binary secret was accepted, exposed, or partially applied")
		}
	}
	for _, c := range []struct {
		data   string
		secret string
	}{
		{data: `{"provide_secret_key_base64":"/w=="}`, secret: string([]byte{0xff})},
		{data: `{"provide_secret_key_base64":"/w==","provide_secret_key":""}`, secret: string([]byte{0xff})},
		{data: `{"provide_secret_key_base64":"/w==","provide_secret_key":null}`, secret: string([]byte{0xff})},
		{data: `{"provide_secret_key_base64":""}`, secret: ""},
	} {
		key := ProvideSecretKey{ProvideMode: ProvideModeNetwork, ProvideSecretKey: "prior-value"}
		if json.Unmarshal([]byte(c.data), &key) != nil || key.ProvideSecretKey != c.secret || key.ProvideMode != ProvideModeNetwork {
			t.Fatal("unambiguous binary representation did not restore the actual bytes")
		}
	}
}

func TestDeviceLocalBinaryProvideSecretKeysSurviveSaveAndColdRestore(t *testing.T) {
	home := t.TempDir()
	fixture := testingPairedAuthSpaceAt(t, home)
	fixture.seedDistinctLogin(t)
	device := testingDeviceKeyLoadConstructor(t, fixture, nil)
	device.InitProvideSecretKeys()
	secrets := device.GetProvideSecretKeys()
	if secrets.Len() == 0 {
		t.Fatal("actual provider initialization produced no secret keys")
	}
	for _, secret := range secrets.values {
		secret.ProvideSecretKey = testingBinaryProvideSecret()
	}
	device.LoadProvideSecretKeys(secrets)
	material := device.GetKeyMaterial()
	if device.SaveProvideSecretKeys() != nil || device.SaveKeyMaterial() != nil {
		t.Fatal("actual provider could not save its deterministic binary state")
	}
	testingRequireProviderKeyState(t, fixture.localState, material, secrets)
	if device.GetAutoSave() {
		t.Fatal("binary provider persistence enabled preference autosave")
	}
	testingDeviceKeyLoadJoin(t, device)
	fixture.networkSpace.close()

	reopened := testingPairedAuthSpaceAt(t, home)
	auth := testingPairedAuthSnapshot(t, reopened)
	reopened.initialJwt, reopened.adminJwt, reopened.instanceId = auth.GetByClientJwt(), auth.GetByJwt(), auth.GetInstanceId()
	if reopened.initialJwt != fixture.initialJwt || reopened.adminJwt != fixture.adminJwt || reopened.instanceId == nil || reopened.instanceId.String() != fixture.instanceId.String() {
		t.Fatal("cold provider restore changed the stored auth owner")
	}
	loaded, err := reopened.localState.LoadProvideSecretKeys()
	loadedMaterial, materialErr := reopened.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || materialErr != nil || loaded == nil || loadedMaterial == nil {
		t.Fatal("cold provider state was unavailable after a successful save")
	}
	testingRequireProviderKeyState(t, reopened.localState, material, secrets)
	restored := testingDeviceKeyLoadConstructor(t, reopened, loadedMaterial)
	restored.LoadProvideSecretKeys(loaded)
	// The comparison reads real provider bytes and fresh durable bytes, not just
	// two copies of the encoded JSON or a mode-presence display flag.
	testingRequireProviderKeyState(t, reopened.localState, restored.GetKeyMaterial(), restored.GetProvideSecretKeys())
	if restored.SaveProvideSecretKeys() != nil || restored.GetAutoSave() {
		t.Fatal("cold provider state did not support independent explicit persistence")
	}
	testingRequireProviderKeyState(t, reopened.localState, material, secrets)
}

func TestCheckedBinaryProvideSecretFailureStopsActualStartupAndPreservesRecord(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	path := filepath.Join(fixture.localState.localStorageDir, ".provide_secret_keys")
	original := []byte(`[{"provide_mode":1,"provide_secret_key_base64":"invalid!binary"}]`)
	if os.WriteFile(path, original, LocalStorageFilePermissions) != nil {
		t.Fatal("could not seed the failed binary secret observation")
	}
	secrets, readErr := fixture.localState.LoadProvideSecretKeys()
	if readErr == nil {
		device := testingDeviceKeyLoadConstructor(t, fixture, nil)
		if secrets == nil {
			device.InitProvideSecretKeys()
		} else {
			device.LoadProvideSecretKeys(secrets)
		}
		if device.SaveProvideSecretKeys() != nil {
			t.Fatal("counterfactual startup failed before its durable assertion")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("failed binary secret observation admitted startup and overwrote the record")
	}
	if readErr == nil || readErr.Error() != "decode provide secret keys" || secrets != nil {
		t.Fatal("failed binary secret observation did not stop startup")
	}
}

func TestProvideSecretBinaryUsesUnchangedActualRpcFields(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingDeviceKeyLoadConstructor(t, fixture, nil)
	_, client := testingPreferenceRpc(t, device)
	secret := &ProvideSecretKey{ProvideMode: ProvideModeNetwork, ProvideSecretKey: testingBinaryProvideSecret()}
	var ignored RpcVoid
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.LoadProvideSecretKeys", []*ProvideSecretKey{secret}, &ignored); err != nil {
		t.Fatal("existing provider-secret RPC did not accept raw bytes")
	}
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	response := &DeviceRemoteSyncResponse{}
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, response); err != nil || response.Error != "" {
		t.Fatal("existing provider-secret sync did not complete")
	}
	found := false
	for _, observed := range response.State.LoadProvideSecretKeys.Value {
		if observed.ProvideMode == secret.ProvideMode {
			found = true
			if observed.ProvideSecretKey != secret.ProvideSecretKey {
				t.Fatal("existing RPC or sync changed the raw provider secret")
			}
		}
	}
	if !found || !response.State.LoadProvideSecretKeys.IsSet {
		t.Fatal("existing RPC sync omitted the loaded provider secret")
	}
}
