package sdk

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
)

func TestDeviceClientSettingsInstallsPeerKeyFetcherWithoutMutatingInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientStrategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	settings := connect.DefaultClientSettings()
	originalEncryptionSettings := settings.EncryptionSettings

	clientSettings := newDeviceClientSettings(settings, "https://api.example", clientStrategy)

	assert.NotEqual(t, clientSettings, settings)
	assert.NotEqual(t, clientSettings.EncryptionSettings, originalEncryptionSettings)
	assert.Equal(t, settings.EncryptionSettings, originalEncryptionSettings)
	assert.Equal(t, settings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil, true)
	assert.Equal(t, clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher != nil, true)
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

	assert.Equal(t, err, nil)
	assert.Equal(t, publicKey, clientId.Bytes())
}

func TestNewDeviceLocalWithKeyMaterialRestoresClientKeySeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	assert.Equal(t, err, nil)

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
	assert.Equal(t, err, nil)
	defer deviceLocal.Close()

	seed[0] = 99

	got := deviceLocal.GetClientKeySeed()
	assert.Equal(t, got, []byte{
		0, 1, 2, 3, 4, 5, 6, 7,
		8, 9, 10, 11, 12, 13, 14, 15,
		16, 17, 18, 19, 20, 21, 22, 23,
		24, 25, 26, 27, 28, 29, 30, 31,
	})

	got[2] = 99
	assert.Equal(t, deviceLocal.GetClientKeySeed()[2], byte(2))
}

func TestDeviceLocalSetKeyMaterialRestoresClientKeySeedAndNotifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, _, err := testing_newNetworkSpace(ctx)
	assert.Equal(t, err, nil)

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
	assert.Equal(t, err, nil)
	defer deviceLocal.Close()

	listener := &testing_provideSecretKeysListener{}
	sub := deviceLocal.AddProvideSecretKeysListener(listener)
	defer sub.Close()

	deviceLocal.SetKeyMaterial(NewDeviceLocalKeyMaterial(seed, nil, nil))

	got := deviceLocal.GetClientKeySeed()
	assert.Equal(t, got, seed)
	listener.with(func() {
		assert.Equal(t, listener.event, true)
		assert.NotEqual(t, listener.provideSecretKeyList, nil)
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

	assert.Equal(t, keyMaterial.GetClientKeySeed(), []byte{1})
	assert.Equal(t, keyMaterial.GetProvideTlsCertificatePem(), []byte{2})
	assert.Equal(t, keyMaterial.GetProvideTlsPrivateKeyPem(), []byte{3})
}

func TestLocalStatePersistsDeviceLocalKeyMaterial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localState := newLocalState(ctx, t.TempDir())
	defer localState.Close()

	keyMaterial := NewDeviceLocalKeyMaterial([]byte{1}, []byte{2}, []byte{3})
	assert.Equal(t, localState.SetDeviceLocalKeyMaterial(keyMaterial), nil)

	got := localState.GetDeviceLocalKeyMaterial()
	assert.Equal(t, got.GetClientKeySeed(), []byte{1})
	assert.Equal(t, got.GetProvideTlsCertificatePem(), []byte{2})
	assert.Equal(t, got.GetProvideTlsPrivateKeyPem(), []byte{3})

	assert.Equal(t, localState.SetDeviceLocalKeyMaterial(nil), nil)
	assert.Equal(t, localState.GetDeviceLocalKeyMaterial(), (*DeviceLocalKeyMaterial)(nil))
}

func TestLocalStateLogoutClearsDeviceLocalKeyMaterial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localState := newLocalState(ctx, t.TempDir())
	defer localState.Close()

	assert.Equal(t, localState.SetDeviceLocalKeyMaterial(NewDeviceLocalKeyMaterial([]byte{1}, nil, nil)), nil)
	assert.Equal(t, localState.Logout(), nil)
	assert.Equal(t, localState.GetDeviceLocalKeyMaterial(), (*DeviceLocalKeyMaterial)(nil))
}

func testingByJwt(clientId connect.Id) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"client_id":"%s"}`, clientId)))
	return fmt.Sprintf("%s.%s.", header, payload)
}
