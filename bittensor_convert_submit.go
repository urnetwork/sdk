//go:build !ios_extension && !js

package sdk

import (
	"context"

	"github.com/urnetwork/connect"
)

// BittensorConvertSubmit converts the estimate's amount on the key the phrase
// opens: it reads the chain again, refuses if the estimate no longer holds,
// builds and signs the one transaction here, and sends it to the chain. The
// phrase is used for this signature and dropped. The result says it was sent;
// BittensorConvertWait says what it did. Without a callback it does nothing, since
// nobody would know what became of the sale.
func BittensorConvertSubmit(settings *SnChainSettings, mnemonic string, estimate *BittensorConvertEstimateResult, callback BittensorConvertSubmitCallback) {
	if callback == nil {
		return
	}
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bittensorConvertSubmitTimeout)
		defer cancel()
		callback.Result(bittensorConvertSubmitFor(ctx, settings, mnemonic, estimate), nil)
	})
}

// bittensorConvertSubmitFor is the exported submit, and every result it gives
// carries the disclaimer.
func bittensorConvertSubmitFor(ctx context.Context, settings *SnChainSettings, mnemonic string, estimate *BittensorConvertEstimateResult) *BittensorConvertSubmission {
	submission := func() *BittensorConvertSubmission {
		signer, err := bittensorSignerFromPhrase(mnemonic)
		if err != nil {
			// the signer's errors are fixed text, and this one is fixed here too
			return &BittensorConvertSubmission{Error: newSnError(BittensorLocalKeyErrorInvalidPhrase, "This recovery phrase can't be read.")}
		}
		engine, snErr := newBittensorChainEngineForExport(settings)
		if snErr != nil {
			return &BittensorConvertSubmission{Error: snErr}
		}
		return engine.convertSubmit(ctx, signer, estimate)
	}()
	submission.FundsDisclaimer = bittensorConvertFundsDisclaimer
	return submission
}

// BittensorConvertWait says what a sent conversion did, once a finalized block
// shows it or its era has ended, about 13 minutes at the most. It holds no key,
// and an app can call it again with the same submission after it was away.
func BittensorConvertWait(settings *SnChainSettings, submission *BittensorConvertSubmission, callback BittensorConvertReceiptCallback) {
	if callback == nil {
		return
	}
	go connect.HandleError(func() {
		ctx, cancel := context.WithTimeout(context.Background(), bittensorChainLandTimeout)
		defer cancel()
		callback.Result(bittensorConvertWaitFor(ctx, settings, submission), nil)
	})
}

// bittensorConvertWaitFor is the exported wait, and every receipt it gives
// carries the disclaimer.
func bittensorConvertWaitFor(ctx context.Context, settings *SnChainSettings, submission *BittensorConvertSubmission) *BittensorConvertReceipt {
	receipt := func() *BittensorConvertReceipt {
		engine, snErr := newBittensorChainEngineForExport(settings)
		if snErr != nil {
			return &BittensorConvertReceipt{Error: snErr}
		}
		return engine.convertWait(ctx, submission)
	}()
	receipt.FundsDisclaimer = bittensorConvertFundsDisclaimer
	return receipt
}
