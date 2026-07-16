package sdk

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
)

func TestDeviceClientSettingsInstallsPeerKeyFetcherWithoutMutatingInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	settings := connect.DefaultClientSettings()
	originalEncryptionSettings := settings.EncryptionSettings

	clientSettings := newDeviceClientSettings(settings, "https://api.example", clientStrategy)

	connect.AssertNotEqual(t, clientSettings, settings)
	connect.AssertNotEqual(t, clientSettings.EncryptionSettings, originalEncryptionSettings)
	connect.AssertEqual(t, settings.EncryptionSettings, originalEncryptionSettings)
	connect.AssertEqual(t, settings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil, true)
	connect.AssertEqual(t, clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil, true)
}

func TestDeviceClientSettingsPreservesConfiguredPeerKeyFetcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	settings := connect.DefaultClientSettings()
	settings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			return peerId.Bytes(), nil
		}
	}

	clientSettings := newDeviceClientSettings(settings, "https://api.example", clientStrategy)
	clientId := connect.NewId()
	publicKey, err := clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher(clientId)(ctx)

	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, publicKey, clientId.Bytes())
}

func TestNewDeviceLocalWithKeyMaterialRestoresClientKeySeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	keyMaterial := NewDeviceLocalKeyMaterial(seed, nil, nil)

	clientId := connect.NewId()
	deviceLocal, err := NewDeviceLocalWithKeyMaterial(
		networkSpace,
		testingByJwt(clientId),
		"",
		"",
		"",
		NewId(),
		false,
		keyMaterial,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	seed[0] = 99

	got := deviceLocal.GetClientKeySeed()
	connect.AssertEqual(t, got, []byte{
		0, 1, 2, 3, 4, 5, 6, 7,
		8, 9, 10, 11, 12, 13, 14, 15,
		16, 17, 18, 19, 20, 21, 22, 23,
		24, 25, 26, 27, 28, 29, 30, 31,
	})

	got[2] = 99
	connect.AssertEqual(t, deviceLocal.GetClientKeySeed()[2], byte(2))
}

func TestDeviceLocalSetKeyMaterialRestoresClientKeySeedAndNotifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(31 - i)
	}

	clientId := connect.NewId()
	deviceLocal, err := NewDeviceLocalWithDefaults(
		networkSpace,
		testingByJwt(clientId),
		"",
		"",
		"",
		NewId(),
		false,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	listener := &testing_provideSecretKeysListener{}
	sub := deviceLocal.AddProvideSecretKeysListener(listener)
	defer sub.Close()

	deviceLocal.SetKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil))

	got := deviceLocal.GetClientKeySeed()
	connect.AssertEqual(t, got, seed)
	listener.with(func() {
		connect.AssertEqual(t, listener.event, true)
		connect.AssertNotEqual(t, listener.provideSecretKeyList, nil)
	})
}

func TestDeviceLocalKeyMaterialClonesBytes(t *testing.T) {
	keyMaterial := NewDeviceLocalKeyMaterial([]byte{1}, []byte{2}, []byte{3})

	clientKeySeed := keyMaterial.GetClientKeySeed()
	provideTlsCertificate := keyMaterial.GetProvideTlsCertificatePem()
	provideTlsPrivateKey := keyMaterial.GetProvideTlsPrivateKeyPem()
	clientKeySeed[0] = 9
	provideTlsCertificate[0] = 9
	provideTlsPrivateKey[0] = 9

	connect.AssertEqual(t, keyMaterial.GetClientKeySeed(), []byte{1})
	connect.AssertEqual(t, keyMaterial.GetProvideTlsCertificatePem(), []byte{2})
	connect.AssertEqual(t, keyMaterial.GetProvideTlsPrivateKeyPem(), []byte{3})
}

func TestLocalStatePersistsDeviceLocalKeyMaterial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localState := newLocalState(ctx, t.TempDir())
	defer localState.Close()

	keyMaterial := NewDeviceLocalKeyMaterial([]byte{1}, []byte{2}, []byte{3})
	connect.AssertEqual(t, localState.SetDeviceLocalKeyMaterial(keyMaterial), nil)

	got := localState.GetDeviceLocalKeyMaterial()
	connect.AssertEqual(t, got.GetClientKeySeed(), []byte{1})
	connect.AssertEqual(t, got.GetProvideTlsCertificatePem(), []byte{2})
	connect.AssertEqual(t, got.GetProvideTlsPrivateKeyPem(), []byte{3})

	connect.AssertEqual(t, localState.SetDeviceLocalKeyMaterial(nil), nil)
	connect.AssertEqual(t, localState.GetDeviceLocalKeyMaterial(), (*DeviceLocalKeyMaterial)(nil))
}

func TestLocalStateLogoutClearsDeviceLocalKeyMaterial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localState := newLocalState(ctx, t.TempDir())
	defer localState.Close()

	connect.AssertEqual(t, localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial([]byte{1}, nil, nil)), nil)
	connect.AssertEqual(t, localState.Logout(), nil)
	connect.AssertEqual(t, localState.GetDeviceLocalKeyMaterial(), (*DeviceLocalKeyMaterial)(nil))
}

func testingByJwt(clientId connect.Id) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"client_id":"%s"}`, clientId)))
	return fmt.Sprintf("%s.%s.", header, payload)
}
