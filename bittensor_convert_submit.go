//go:build !ios_extension && !js

package sdk

import (
	"context"

	"github.com/urnetwork/connect"
)

// BittensorConvertSubmit converts the estimate's amount on the key the phrase
// opens: it reads the chain again, refuses if the estimate no longer holds,
// builds and signs the one transaction here, and sends it to the chain. The
// phrase is used for this signature and dropped.
func BittensorConvertSubmit(settings *SnChainSettings, mnemonic string, estimate *BittensorConvertEstimateResult, callback BittensorConvertCallback) {
	if callback == nil {
		return
	}
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bittensorChainLandTimeout)
		defer cancel()
		signer, err := bittensorSignerFromPhrase(mnemonic)
		if err != nil {
			// the signer's errors are fixed text
			callback.Failed(BittensorLocalKeyErrorInvalidPhrase, "This recovery phrase can't be read.")
			return
		}
		engine, snErr := newBittensorChainEngine(settings)
		if snErr != nil {
			callback.Failed(snErr.Code, snErr.Message)
			return
		}
		engine.convertSubmit(ctx, signer, estimate, callback)
	})
}
