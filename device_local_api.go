package sdk

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

// Encrypted peer verification remains enforced while its public-key reads
// use the same local authority as credential refresh and provider discovery.
func applyLocalDeviceApiKeyFetchers(settings *connect.ClientSettings, local localDeviceApi, apiUrl string) {
	if local == nil || settings.EncryptionSettings == nil {
		return
	}
	encryption := *settings.EncryptionSettings
	encryption.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
		return func(ctx context.Context) ([]byte, error) {
			result, err := connect.HttpGetWithRawFunction(ctx, local.Get, fmt.Sprintf("%s/key/%s", apiUrl, peerId), "", &connect.GetClientKeyResult{}, connect.NewNoopApiCallback[*connect.GetClientKeyResult]())
			if err != nil {
				return nil, err
			}
			return result.PublicKey, nil
		}
	}
	encryption.NewPeerClientKeyHistoryFetcher = func(peerId connect.Id) func(context.Context) ([][]byte, error) {
		return func(ctx context.Context) ([][]byte, error) {
			result, err := connect.HttpGetWithRawFunction(ctx, local.Get, fmt.Sprintf("%s/key/%s/history", apiUrl, peerId), "", &connect.GetClientKeyHistoryResult{}, connect.NewNoopApiCallback[*connect.GetClientKeyHistoryResult]())
			if err != nil {
				return nil, err
			}
			return result.History, nil
		}
	}
	settings.EncryptionSettings = &encryption
}
