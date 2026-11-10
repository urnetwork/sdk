package sdk

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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
	CryptoAssets        []cryptoAssetVector `json:"cryptoAssets"`
	CryptoPaymentQrText []struct {
		Name     string `json:"name"`
		Asset    string `json:"asset"`
		Uri      string `json:"uri"`
		Address  string `json:"address"`
		Expected string `json:"expected"`
	} `json:"cryptoPaymentQrText"`
	CryptoPaymentTabLayout []cryptoPaymentTabLayoutVector `json:"cryptoPaymentTabLayout"`
	CryptoPaymentTabs      []struct {
		Name              string                  `json:"name"`
		Assets            *CryptoPaymentAssetList `json:"assets"`
		UnavailableAssets *CryptoPaymentAssetList `json:"unavailable_assets"`
		Items             *StringList             `json:"items"`
		Expected          []struct {
			Id             string   `json:"id"`
			Available      bool     `json:"available"`
			AvailableCoins []string `json:"available_coins"`
		} `json:"expected"`
	} `json:"cryptoPaymentTabs"`
}

// A tab of the payment widget as it is laid out, whatever is available.
type cryptoPaymentTabLayoutVector struct {
	Id       string   `json:"id"`
	Label    string   `json:"label"`
	LabelKey string   `json:"label_key"`
	IconKeys []string `json:"icon_keys"`
	Coins    []struct {
		Asset    string `json:"asset"`
		Label    string `json:"label"`
		LabelKey string `json:"label_key"`
		IconKey  string `json:"icon_key"`
	} `json:"coins"`
}

// One coin's facts, as every surface reads them.
type cryptoAssetVector struct {
	Asset         string `json:"asset"`
	Ticker        string `json:"ticker"`
	Label         string `json:"label"`
	LabelKey      string `json:"label_key"`
	IconKey       string `json:"icon_key"`
	Decimals      int    `json:"decimals"`
	QuoteDecimals int    `json:"quote_decimals"`
	Market        string `json:"market"`
	UriScheme     string `json:"uri_scheme"`
	TabId         string `json:"tab_id"`
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
		"CryptoAssetTao":                    {CryptoAssetTao, "tao"},
		"CryptoAssetBtc":                    {CryptoAssetBtc, "btc"},
		"CryptoAssetEth":                    {CryptoAssetEth, "eth"},
		"CryptoAssetSol":                    {CryptoAssetSol, "sol"},
		"CryptoPaymentTabMoreCoins":         {CryptoPaymentTabMoreCoins, "more_coins"},
		"CryptoIconKeyUsdc":                 {CryptoIconKeyUsdc, "coin_usdc"},
		"CryptoIconKeyZec":                  {CryptoIconKeyZec, "coin_zec"},
		"CryptoIconKeyXmr":                  {CryptoIconKeyXmr, "coin_xmr"},
		"CryptoIconKeyTao":                  {CryptoIconKeyTao, "coin_tao"},
		"CryptoIconKeyBtc":                  {CryptoIconKeyBtc, "coin_btc"},
		"CryptoIconKeyEth":                  {CryptoIconKeyEth, "coin_eth"},
		"CryptoIconKeySol":                  {CryptoIconKeySol, "coin_sol"},
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
		"EventStoreSolana":                  {EventStoreSolana, "solana"},
		"EventStoreZcash":                   {EventStoreZcash, "zcash"},
		"EventStoreMonero":                  {EventStoreMonero, "monero"},
		"EventStoreBittensor":               {EventStoreBittensor, "bittensor"},
		"EventStoreBitcoin":                 {EventStoreBitcoin, "bitcoin"},
		"EventStoreEthereum":                {EventStoreEthereum, "ethereum"},
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

// Every coin's facts, read through each function the apps call.
func TestCryptoAssets(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoAssets {
		asset := vector.Asset
		got := cryptoAssetVector{
			Asset:         asset,
			Ticker:        CryptoAssetTicker(asset),
			Label:         CryptoAssetLabel(asset),
			LabelKey:      CryptoAssetLabelKey(asset),
			IconKey:       CryptoAssetIconKey(asset),
			Decimals:      CryptoAssetDecimals(asset),
			QuoteDecimals: CryptoAssetQuoteDecimals(asset),
			Market:        CryptoAssetMarket(asset),
			UriScheme:     CryptoPaymentUriScheme(asset),
			TabId:         CryptoPaymentTabId(asset),
		}
		if got != vector {
			t.Errorf("the facts of %q are %+v, want %+v", asset, got, vector)
		}
	}
}

// The vectors and the sdk's table name the same coins, so a coin added to one
// alone fails here.
func TestCryptoAssetsVectorsNameEveryCoin(t *testing.T) {
	vectorAssets := []string{}
	for _, vector := range loadCryptoPayVectors(t).CryptoAssets {
		if vector.Ticker != "" {
			vectorAssets = append(vectorAssets, vector.Asset)
		}
	}
	if !slices.Equal(vectorAssets, cryptoAssetOrder) {
		t.Fatalf("the vectors' coins are %v, the sdk's %v", vectorAssets, cryptoAssetOrder)
	}
	for _, asset := range cryptoAssetOrder {
		if !isCryptoAsset(asset) {
			t.Errorf("%q is in the order but not in the table", asset)
		}
	}
	connect.AssertEqual(t, len(cryptoAssetCoins), len(cryptoAssetOrder))
}

func TestCryptoPaymentQrText(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoPaymentQrText {
		if got := CryptoPaymentQrText(vector.Asset, vector.Uri, vector.Address); got != vector.Expected {
			t.Errorf("%s: CryptoPaymentQrText(%q, %q, %q) = %q, want %q", vector.Name, vector.Asset, vector.Uri, vector.Address, got, vector.Expected)
		}
	}
}

// Each case's tabs in order, each tab laid out as cryptoPaymentTabLayout
// says, its marks the same whatever is available, with the case's coins
// available and every other coin not.
func TestCryptoPaymentTabs(t *testing.T) {
	vectors := loadCryptoPayVectors(t)
	layoutAssets := []string{}
	for _, layout := range vectors.CryptoPaymentTabLayout {
		for _, coin := range layout.Coins {
			layoutAssets = append(layoutAssets, coin.Asset)
		}
	}
	if !slices.Equal(layoutAssets, cryptoAssetOrder) {
		t.Fatalf("the layout's coins are %v, the method order %v", layoutAssets, cryptoAssetOrder)
	}
	for _, vector := range vectors.CryptoPaymentTabs {
		tabs := CryptoPaymentTabs(vector.Assets, vector.UnavailableAssets, vector.Items)
		if tabs == nil {
			t.Errorf("%s: no tab list; the bindings need an empty one", vector.Name)
			continue
		}
		gotIds := []string{}
		for i := 0; i < tabs.Len(); i += 1 {
			gotIds = append(gotIds, tabs.Get(i).Id)
		}
		expectedIds := []string{}
		for _, expected := range vector.Expected {
			expectedIds = append(expectedIds, expected.Id)
		}
		if !slices.Equal(gotIds, expectedIds) {
			t.Errorf("%s: tabs %v, want %v", vector.Name, gotIds, expectedIds)
			continue
		}
		for i, expected := range vector.Expected {
			tab := tabs.Get(i)
			layoutIndex := slices.IndexFunc(vectors.CryptoPaymentTabLayout, func(layout cryptoPaymentTabLayoutVector) bool {
				return layout.Id == tab.Id
			})
			if layoutIndex < 0 {
				t.Errorf("%s: tab %q is not in the layout", vector.Name, tab.Id)
				continue
			}
			layout := vectors.CryptoPaymentTabLayout[layoutIndex]
			if tab.Label != layout.Label || tab.LabelKey != layout.LabelKey {
				t.Errorf("%s: tab %q is labeled %q (%q), want %q (%q)", vector.Name, tab.Id, tab.Label, tab.LabelKey, layout.Label, layout.LabelKey)
			}
			if got := stringListValues(tab.IconKeys); !slices.Equal(got, layout.IconKeys) {
				t.Errorf("%s: tab %q shows the marks %v, want %v", vector.Name, tab.Id, got, layout.IconKeys)
			}
			if tab.Available != expected.Available {
				t.Errorf("%s: tab %q available = %t, want %t", vector.Name, tab.Id, tab.Available, expected.Available)
			}
			if tab.Coins == nil || tab.Coins.Len() != len(layout.Coins) {
				t.Errorf("%s: tab %q has coins %+v, want %d", vector.Name, tab.Id, tab.Coins, len(layout.Coins))
				continue
			}
			for j, layoutCoin := range layout.Coins {
				coin := tab.Coins.Get(j)
				if coin.Asset != layoutCoin.Asset || coin.Label != layoutCoin.Label || coin.LabelKey != layoutCoin.LabelKey || coin.IconKey != layoutCoin.IconKey {
					t.Errorf("%s: tab %q coin %d is %+v, want %+v", vector.Name, tab.Id, j, *coin, layoutCoin)
				}
				if available := slices.Contains(expected.AvailableCoins, coin.Asset); coin.Available != available {
					t.Errorf("%s: %q available = %t, want %t", vector.Name, coin.Asset, coin.Available, available)
				}
			}
		}
	}
}

// The tabs cross the c abi as json, in these names, with every list a list
// (never null) even when empty. Nothing in them says to dim a mark: an
// unavailable coin greys its label only.
func TestCryptoPaymentTabsJson(t *testing.T) {
	assets := NewCryptoPaymentAssetList()
	for _, asset := range []string{CryptoAssetXmr, CryptoAssetSol} {
		plans := NewStringList()
		plans.Add(PlanYearly)
		assets.Add(&CryptoPaymentAsset{Asset: asset, Plans: plans})
	}
	items := NewStringList()
	items.Add(PlanYearly)
	data, err := json.Marshal(CryptoPaymentTabs(assets, nil, items))
	if err != nil {
		t.Fatal(err)
	}
	requireJsonEqual(t, string(data), `[
		{"id": "usdc_solana", "label": "USDC on Solana", "label_key": "pay_method_usdc_solana",
		 "icon_keys": ["coin_usdc"],
		 "coins": [{"asset": "usdc_solana", "label": "USDC on Solana", "label_key": "pay_method_usdc_solana", "icon_key": "coin_usdc", "available": false}],
		 "available": false},
		{"id": "zec", "label": "Zcash", "label_key": "pay_method_zcash",
		 "icon_keys": ["coin_zec"],
		 "coins": [{"asset": "zec", "label": "Zcash", "label_key": "pay_method_zcash", "icon_key": "coin_zec", "available": false}],
		 "available": false},
		{"id": "xmr", "label": "Monero", "label_key": "pay_method_monero",
		 "icon_keys": ["coin_xmr"],
		 "coins": [{"asset": "xmr", "label": "Monero", "label_key": "pay_method_monero", "icon_key": "coin_xmr", "available": true}],
		 "available": true},
		{"id": "more_coins", "label": "More coins", "label_key": "pay_method_more_coins",
		 "icon_keys": ["coin_tao", "coin_btc", "coin_eth", "coin_sol"],
		 "coins": [
			{"asset": "tao", "label": "TAO", "label_key": "pay_method_tao", "icon_key": "coin_tao", "available": false},
			{"asset": "btc", "label": "Bitcoin", "label_key": "pay_method_bitcoin", "icon_key": "coin_btc", "available": false},
			{"asset": "eth", "label": "ETH", "label_key": "pay_method_eth", "icon_key": "coin_eth", "available": false},
			{"asset": "sol", "label": "SOL", "label_key": "pay_method_sol", "icon_key": "coin_sol", "available": true}
		 ],
		 "available": true}
	]`)
	data, err = json.Marshal(CryptoPaymentTabs(nil, nil, items))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(data), "[]")
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
		"cryptoAssets":            len(vectors.CryptoAssets),
		"cryptoPaymentQrText":     len(vectors.CryptoPaymentQrText),
		"cryptoPaymentTabLayout":  len(vectors.CryptoPaymentTabLayout),
		"cryptoPaymentTabs":       len(vectors.CryptoPaymentTabs),
	} {
		if count == 0 {
			t.Errorf("no %s vectors were read", name)
		}
	}
}
