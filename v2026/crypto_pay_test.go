package sdk

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

// The crypto payment rules replay js/test/crypto_pay_vectors.json, the vectors
// the TypeScript twin (js/test/crypto_pay.test.ts) replays too, so the two
// cannot drift apart.

type cryptoPayVectors struct {
	PaymentMethods []struct {
		Name     string                  `json:"name"`
		Assets   *CryptoPaymentAssetList `json:"assets"`
		Items    *StringList             `json:"items"`
		Expected []string                `json:"expected"`
	} `json:"paymentMethods"`
	CryptoAssetSells []struct {
		Assets   *CryptoPaymentAssetList `json:"assets"`
		Asset    string                  `json:"asset"`
		Item     string                  `json:"item"`
		Expected bool                    `json:"expected"`
	} `json:"cryptoAssetSells"`
	PaymentMethodAllowsPlan []struct {
		Method   string `json:"method"`
		Plan     string `json:"plan"`
		Expected bool   `json:"expected"`
	} `json:"paymentMethodAllowsPlan"`
	CryptoAssetTicker []struct {
		Asset    string `json:"asset"`
		Expected string `json:"expected"`
	} `json:"cryptoAssetTicker"`
	CryptoWalletSuggestions []struct {
		Asset    string   `json:"asset"`
		Expected []string `json:"expected"`
	} `json:"cryptoWalletSuggestions"`
	CheckCryptoPaymentUri []struct {
		Name     string `json:"name"`
		Asset    string `json:"asset"`
		Uri      string `json:"uri"`
		Address  string `json:"address"`
		Expected bool   `json:"expected"`
	} `json:"checkCryptoPaymentUri"`
	CryptoTermNoticeDue []struct {
		Name     string      `json:"name"`
		Term     *CryptoTerm `json:"term"`
		Now      string      `json:"now"`
		Expected bool        `json:"expected"`
	} `json:"cryptoTermNoticeDue"`
}

func loadCryptoPayVectors(t *testing.T) *cryptoPayVectors {
	t.Helper()
	data, err := os.ReadFile("js/test/crypto_pay_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := &cryptoPayVectors{}
	if err := json.Unmarshal(data, vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// The wire strings are the server's and the other platforms'; a drift here
// sends a value the server refuses.
func TestCryptoPayConstants(t *testing.T) {
	pins := map[string][2]string{
		"CryptoAssetUsdcSolana":             {CryptoAssetUsdcSolana, "usdc_solana"},
		"CryptoAssetZec":                    {CryptoAssetZec, "zec"},
		"CryptoAssetXmr":                    {CryptoAssetXmr, "xmr"},
		"PaymentMethodCard":                 {PaymentMethodCard, "card"},
		"CryptoPaymentStatusPending":        {CryptoPaymentStatusPending, "pending"},
		"CryptoPaymentStatusSeen":           {CryptoPaymentStatusSeen, "seen"},
		"CryptoPaymentStatusConfirming":     {CryptoPaymentStatusConfirming, "confirming"},
		"CryptoPaymentStatusPaid":           {CryptoPaymentStatusPaid, "paid"},
		"CryptoPaymentStatusExpired":        {CryptoPaymentStatusExpired, "expired"},
		"CryptoPaymentStatusReview":         {CryptoPaymentStatusReview, "review"},
		"CryptoPaymentStatusUnknown":        {CryptoPaymentStatusUnknown, "unknown"},
		"CryptoPaymentDeliveryAccount":      {CryptoPaymentDeliveryAccount, "account"},
		"CryptoPaymentDeliveryNetwork":      {CryptoPaymentDeliveryNetwork, "network"},
		"CryptoPaymentDeliveryCode":         {CryptoPaymentDeliveryCode, "code"},
		"PurchaseErrorCodeAssetUnavailable": {PurchaseErrorCodeAssetUnavailable, "asset_unavailable"},
		"PurchaseErrorCodeQuoteExpired":     {PurchaseErrorCodeQuoteExpired, "quote_expired"},
		"PurchaseErrorCodeUnderpaid":        {PurchaseErrorCodeUnderpaid, "underpaid"},
		"EventStoreZcash":                   {EventStoreZcash, "zcash"},
		"EventStoreMonero":                  {EventStoreMonero, "monero"},
		"SubscriptionStoreCrypto":           {SubscriptionStoreCrypto, "crypto"},
		"CryptoInvoicePhaseNone":            {CryptoInvoicePhaseNone, "none"},
		"CryptoInvoicePhaseStarting":        {CryptoInvoicePhaseStarting, "starting"},
		"CryptoInvoicePhaseOpen":            {CryptoInvoicePhaseOpen, "open"},
		"CryptoInvoicePhasePaid":            {CryptoInvoicePhasePaid, "paid"},
		"CryptoInvoicePhaseFailed":          {CryptoInvoicePhaseFailed, "failed"},
	}
	for name, pin := range pins {
		if pin[0] != pin[1] {
			t.Errorf("%s = %q, want %q", name, pin[0], pin[1])
		}
	}
}

func TestPaymentMethods(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).PaymentMethods {
		t.Run(vector.Name, func(t *testing.T) {
			got := stringListValues(PaymentMethods(vector.Assets, vector.Items))
			if !slices.Equal(got, vector.Expected) {
				t.Fatalf("PaymentMethods = %v, want %v", got, vector.Expected)
			}
		})
	}
}

func TestCryptoAssetSells(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoAssetSells {
		if got := CryptoAssetSells(vector.Assets, vector.Asset, vector.Item); got != vector.Expected {
			t.Errorf("CryptoAssetSells(%s, %q) = %t, want %t", vector.Asset, vector.Item, got, vector.Expected)
		}
	}
}

func TestPaymentMethodAllowsPlan(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).PaymentMethodAllowsPlan {
		if got := PaymentMethodAllowsPlan(vector.Method, vector.Plan); got != vector.Expected {
			t.Errorf("PaymentMethodAllowsPlan(%q, %q) = %t, want %t", vector.Method, vector.Plan, got, vector.Expected)
		}
	}
}

func TestCryptoAssetTicker(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoAssetTicker {
		if got := CryptoAssetTicker(vector.Asset); got != vector.Expected {
			t.Errorf("CryptoAssetTicker(%q) = %q, want %q", vector.Asset, got, vector.Expected)
		}
	}
}

func TestCryptoWalletSuggestions(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoWalletSuggestions {
		wallets := CryptoWalletSuggestions(vector.Asset)
		if wallets == nil {
			t.Fatalf("CryptoWalletSuggestions(%q) is nil; the bindings need an empty list", vector.Asset)
		}
		if got := stringListValues(wallets); !slices.Equal(got, vector.Expected) {
			t.Errorf("CryptoWalletSuggestions(%q) = %v, want %v", vector.Asset, got, vector.Expected)
		}
	}
}

func TestCheckCryptoPaymentUri(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CheckCryptoPaymentUri {
		t.Run(vector.Name, func(t *testing.T) {
			if got := CheckCryptoPaymentUri(vector.Asset, vector.Uri, vector.Address); got != vector.Expected {
				t.Fatalf("CheckCryptoPaymentUri(%q, %q, %q) = %t, want %t", vector.Asset, vector.Uri, vector.Address, got, vector.Expected)
			}
		})
	}
}

func TestCryptoTermNoticeDue(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoTermNoticeDue {
		t.Run(vector.Name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, vector.Now)
			if err != nil {
				t.Fatal(err)
			}
			if got := cryptoTermNoticeDueAt(vector.Term, now); got != vector.Expected {
				t.Fatalf("notice due = %t, want %t", got, vector.Expected)
			}
		})
	}
	// the exported form reads the clock
	if CryptoTermNoticeDue(&CryptoTerm{
		Market:       "monero",
		TermEndTime:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		GraceEndTime: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}) != true {
		t.Fatal("a term that ended an hour ago, in its grace, has no notice")
	}
}

// Every vector set is replayed: an empty one means the file and this test no
// longer agree on its shape.
func TestCryptoPayVectorsAreAllRead(t *testing.T) {
	vectors := loadCryptoPayVectors(t)
	for name, count := range map[string]int{
		"paymentMethods":          len(vectors.PaymentMethods),
		"cryptoAssetSells":        len(vectors.CryptoAssetSells),
		"paymentMethodAllowsPlan": len(vectors.PaymentMethodAllowsPlan),
		"cryptoAssetTicker":       len(vectors.CryptoAssetTicker),
		"cryptoWalletSuggestions": len(vectors.CryptoWalletSuggestions),
		"checkCryptoPaymentUri":   len(vectors.CheckCryptoPaymentUri),
		"cryptoTermNoticeDue":     len(vectors.CryptoTermNoticeDue),
	} {
		if count == 0 {
			t.Errorf("no %s vectors were read", name)
		}
	}
}
