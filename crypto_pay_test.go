package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
	CryptoPaymentTabCoin []struct {
		Name       string            `json:"name"`
		Tab        *CryptoPaymentTab `json:"tab"`
		LastChosen string            `json:"last_chosen"`
		Remembered string            `json:"remembered"`
		Expected   string            `json:"expected"`
	} `json:"cryptoPaymentTabCoin"`
	CryptoPaymentShowsMemo []struct {
		Name     string `json:"name"`
		Asset    string `json:"asset"`
		Memo     string `json:"memo"`
		Expected bool   `json:"expected"`
	} `json:"cryptoPaymentShowsMemo"`
	CryptoPaymentSteps []struct {
		Name                  string              `json:"name"`
		Asset                 string              `json:"asset"`
		Status                string              `json:"status"`
		Confirmations         int                 `json:"confirmations"`
		ConfirmationsRequired int                 `json:"confirmations_required"`
		Expected              []CryptoPaymentStep `json:"expected"`
	} `json:"cryptoPaymentSteps"`
	CryptoPaidAsset []struct {
		Name     string `json:"name"`
		Market   string `json:"market"`
		Asset    string `json:"asset"`
		Expected string `json:"expected"`
	} `json:"cryptoPaidAsset"`
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
	Asset                string `json:"asset"`
	Ticker               string `json:"ticker"`
	Label                string `json:"label"`
	LabelKey             string `json:"label_key"`
	IconKey              string `json:"icon_key"`
	Decimals             int    `json:"decimals"`
	QuoteDecimals        int    `json:"quote_decimals"`
	Market               string `json:"market"`
	UriScheme            string `json:"uri_scheme"`
	TabId                string `json:"tab_id"`
	NetworkLine          string `json:"network_line"`
	NetworkLineKey       string `json:"network_line_key"`
	NetworkWarning       string `json:"network_warning"`
	NetworkWarningKey    string `json:"network_warning_key"`
	NoPaymentLinkNote    string `json:"no_payment_link_note"`
	NoPaymentLinkNoteKey string `json:"no_payment_link_note_key"`
	EventProduct         string `json:"event_product"`
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
		"CryptoPaymentStepDone":             {CryptoPaymentStepDone, "done"},
		"CryptoPaymentStepCurrent":          {CryptoPaymentStepCurrent, "current"},
		"CryptoPaymentStepUpcoming":         {CryptoPaymentStepUpcoming, "upcoming"},
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

// Only TAO's invoice names the wallets that work. The owner, 2026-11-11: "we
// do not need to suggest btc or eth wallets. for other coins besides tao we
// don't need to suggest wallets". Every other coin the widget sells (USDC on
// Solana, Zcash, Monero, SOL, Bitcoin and ETH) names none, so its invoice
// shows no wallets line, and so does a coin added later. The shared vectors
// pin every coin, which holds the TypeScript twin to the same.
func TestCryptoWalletSuggestionsTaoOnly(t *testing.T) {
	taoWallets := []string{"btcli", "Talisman", "Nova", "SubWallet", "Polkadot.js", "Ledger", "Tangem"}
	for _, asset := range []string{CryptoAssetUsdcSolana, CryptoAssetZec, CryptoAssetXmr, CryptoAssetTao, CryptoAssetBtc, CryptoAssetEth, CryptoAssetSol} {
		if !slices.Contains(cryptoAssetOrder, asset) {
			t.Errorf("%s is not among the widget's coins", asset)
		}
	}
	pinned := map[string]bool{}
	for _, vector := range loadCryptoPayVectors(t).CryptoWalletSuggestions {
		pinned[vector.Asset] = true
	}
	for _, asset := range cryptoAssetOrder {
		wallets := CryptoWalletSuggestions(asset)
		if wallets == nil {
			t.Fatalf("CryptoWalletSuggestions(%q) is nil; the bindings need an empty list", asset)
		}
		got := stringListValues(wallets)
		if asset == CryptoAssetTao {
			if !slices.Equal(got, taoWallets) {
				t.Errorf("CryptoWalletSuggestions(%q) = %v, want %v", asset, got, taoWallets)
			}
		} else if len(got) != 0 {
			t.Errorf("%s names wallets %v; only TAO's invoice suggests wallets", asset, got)
		}
		if !pinned[asset] {
			t.Errorf("no cryptoWalletSuggestions vector pins %s", asset)
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
		lines := CryptoAssetLinesFor(asset)
		if lines == nil {
			t.Fatalf("CryptoAssetLinesFor(%q) is nil; the bindings need lines", asset)
		}
		got := cryptoAssetVector{
			Asset:                asset,
			Ticker:               CryptoAssetTicker(asset),
			Label:                CryptoAssetLabel(asset),
			LabelKey:             CryptoAssetLabelKey(asset),
			IconKey:              CryptoAssetIconKey(asset),
			Decimals:             CryptoAssetDecimals(asset),
			QuoteDecimals:        CryptoAssetQuoteDecimals(asset),
			Market:               CryptoAssetMarket(asset),
			UriScheme:            CryptoPaymentUriScheme(asset),
			TabId:                CryptoPaymentTabId(asset),
			NetworkLine:          lines.NetworkLine,
			NetworkLineKey:       lines.NetworkLineKey,
			NetworkWarning:       lines.NetworkWarning,
			NetworkWarningKey:    lines.NetworkWarningKey,
			NoPaymentLinkNote:    lines.NoPaymentLinkNote,
			NoPaymentLinkNoteKey: lines.NoPaymentLinkNoteKey,
			EventProduct:         CryptoPaymentEventProduct(asset),
		}
		if got != vector {
			t.Errorf("the facts of %q are %+v, want %+v", asset, got, vector)
		}
	}
}

// With URNETWORK_LOCALIZATIONS_KEYS_DIR naming a store's keys/ directory,
// each line's English is the store's own (its `source`, and its English
// translation), byte for byte: the Linux app looks a line up by its English
// as the gettext msgid, the Apple catalog is keyed by it, and every app shows
// the English where it has no string for the id. The lines were checked this
// way against localizations 3698bd5; the vectors' cryptoAssets hold them since.
func TestCryptoAssetLinesAreTheStoresEnglish(t *testing.T) {
	keysDir := os.Getenv("URNETWORK_LOCALIZATIONS_KEYS_DIR")
	if keysDir == "" {
		t.Skip("URNETWORK_LOCALIZATIONS_KEYS_DIR is not set; the vectors' cryptoAssets stand in for the store")
	}
	type storeKey struct {
		Source        string         `yaml:"source"`
		Localizations map[string]any `yaml:"localizations"`
	}
	source := func(key string) string {
		data, err := os.ReadFile(filepath.Join(keysDir, key+".yaml"))
		if err != nil {
			t.Fatalf("the store has no %s: %v", key, err)
		}
		var storeEntry storeKey
		if err := yaml.Unmarshal(data, &storeEntry); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if english := storeEntry.Localizations["en"]; english != storeEntry.Source {
			t.Errorf("%s: the store's English %q is not its source %q", key, english, storeEntry.Source)
		}
		return storeEntry.Source
	}
	pairs := 0
	for _, asset := range cryptoAssetOrder {
		lines := CryptoAssetLinesFor(asset)
		if lines.NetworkLineKey == "" {
			t.Errorf("%s has no network line", asset)
		}
		for key, english := range map[string]string{
			lines.NetworkLineKey:       lines.NetworkLine,
			lines.NetworkWarningKey:    lines.NetworkWarning,
			lines.NoPaymentLinkNoteKey: lines.NoPaymentLinkNote,
		} {
			if key == "" {
				connect.AssertEqual(t, english, "")
				continue
			}
			pairs += 1
			if got := source(key); got != english {
				t.Errorf("%s: %s is %q in the sdk, %q in the store", asset, key, english, got)
			}
		}
	}
	// 7 network lines, 5 warnings (none for Zcash and Monero), 1 note (TAO)
	connect.AssertEqual(t, pairs, 13)
}

// The coin a tap on a tab chooses: the coin last chosen in the tab, else the
// remembered method's, else the tab's first that can be paid with.
func TestCryptoPaymentTabCoin(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoPaymentTabCoin {
		t.Run(vector.Name, func(t *testing.T) {
			if got := CryptoPaymentTabCoin(vector.Tab, vector.LastChosen, vector.Remembered); got != vector.Expected {
				t.Fatalf("CryptoPaymentTabCoin(%q, %q) = %q, want %q", vector.LastChosen, vector.Remembered, got, vector.Expected)
			}
		})
	}
}

// Over the tabs CryptoPaymentTabs lays out, a surface that keeps the coin last
// chosen per tab gets that coin back on More coins after choosing another tab
// in between; one that kept only the last coin chosen anywhere (every app
// before the rule moved here) fell back to TAO.
func TestCryptoPaymentTabCoinKeepsTheCoinChosenInTheTab(t *testing.T) {
	assets := NewCryptoPaymentAssetList()
	for _, asset := range []string{CryptoAssetUsdcSolana, CryptoAssetTao, CryptoAssetBtc, CryptoAssetEth, CryptoAssetSol} {
		plans := NewStringList()
		plans.Add(PlanYearly)
		assets.Add(&CryptoPaymentAsset{Asset: asset, Plans: plans})
	}
	items := NewStringList()
	items.Add(PlanYearly)
	tabs := CryptoPaymentTabs(assets, nil, items)
	tab := func(id string) *CryptoPaymentTab {
		for i := 0; i < tabs.Len(); i += 1 {
			if tabs.Get(i).Id == id {
				return tabs.Get(i)
			}
		}
		t.Fatalf("no %s tab", id)
		return nil
	}
	lastChosen := map[string]string{}
	remembered := ""
	choose := func(asset string) {
		lastChosen[CryptoPaymentTabId(asset)] = asset
		remembered = asset
	}
	tap := func(id string) string {
		asset := CryptoPaymentTabCoin(tab(id), lastChosen[id], remembered)
		if asset != "" {
			choose(asset)
		}
		return asset
	}
	// first visit: the remembered method's tab is not More coins
	remembered = CryptoAssetUsdcSolana
	connect.AssertEqual(t, tap(CryptoPaymentTabMoreCoins), CryptoAssetTao)
	choose(CryptoAssetBtc)
	connect.AssertEqual(t, tap(CryptoAssetUsdcSolana), CryptoAssetUsdcSolana)
	connect.AssertEqual(t, remembered, CryptoAssetUsdcSolana)
	// back on More coins: Bitcoin, chosen there, not the first coin
	connect.AssertEqual(t, tap(CryptoPaymentTabMoreCoins), CryptoAssetBtc)
	// a fresh screen remembers the device's method
	lastChosen = map[string]string{}
	choose(CryptoAssetEth)
	lastChosen = map[string]string{}
	connect.AssertEqual(t, tap(CryptoPaymentTabMoreCoins), CryptoAssetEth)
	// the rule every app kept before: the last coin chosen anywhere
	connect.AssertEqual(t, CryptoPaymentTabCoin(tab(CryptoPaymentTabMoreCoins), CryptoAssetUsdcSolana, CryptoAssetUsdcSolana), CryptoAssetTao)
}

// The memo shows only when the server sent one, whatever the coin.
func TestCryptoPaymentShowsMemo(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoPaymentShowsMemo {
		t.Run(vector.Name, func(t *testing.T) {
			if got := CryptoPaymentShowsMemo(vector.Asset, vector.Memo); got != vector.Expected {
				t.Fatalf("CryptoPaymentShowsMemo(%q, %q) = %t, want %t", vector.Asset, vector.Memo, got, vector.Expected)
			}
		})
	}
}

// Each coin's status steps at each status, a skipped step drawn done.
func TestCryptoPaymentSteps(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoPaymentSteps {
		t.Run(vector.Name, func(t *testing.T) {
			steps := CryptoPaymentSteps(vector.Asset, vector.Status, vector.Confirmations, vector.ConfirmationsRequired)
			if steps == nil {
				t.Fatal("no step list; the bindings need an empty one")
			}
			got := []CryptoPaymentStep{}
			for i := 0; i < steps.Len(); i += 1 {
				got = append(got, *steps.Get(i))
			}
			if !slices.Equal(got, vector.Expected) {
				t.Fatalf("CryptoPaymentSteps(%q, %q, %d, %d) = %+v, want %+v", vector.Asset, vector.Status, vector.Confirmations, vector.ConfirmationsRequired, got, vector.Expected)
			}
		})
	}
}

// The coin a paid year is named by: the server's asset, never its market.
func TestCryptoPaidAsset(t *testing.T) {
	for _, vector := range loadCryptoPayVectors(t).CryptoPaidAsset {
		t.Run(vector.Name, func(t *testing.T) {
			if got := CryptoPaidAsset(vector.Market, vector.Asset); got != vector.Expected {
				t.Fatalf("CryptoPaidAsset(%q, %q) = %q, want %q", vector.Market, vector.Asset, got, vector.Expected)
			}
		})
	}
}

// The lines and the steps cross the c abi as json, in these names.
func TestCryptoAssetLinesAndStepsJson(t *testing.T) {
	data, err := json.Marshal(CryptoAssetLinesFor(CryptoAssetTao))
	if err != nil {
		t.Fatal(err)
	}
	requireJsonEqual(t, string(data), `{
		"network_line": "Bittensor network", "network_line_key": "crypto_network_tao",
		"network_warning": "Send only TAO on the Bittensor network to this address. TAO sent on any other network won't be credited.",
		"network_warning_key": "crypto_invoice_tao_network_only",
		"no_payment_link_note": "TAO has no payment link, so this QR code holds only the address. Enter the exact amount in your wallet.",
		"no_payment_link_note_key": "crypto_invoice_tao_no_payment_link"
	}`)
	data, err = json.Marshal(CryptoAssetLinesFor("ltc"))
	if err != nil {
		t.Fatal(err)
	}
	requireJsonEqual(t, string(data), `{
		"network_line": "", "network_line_key": "", "network_warning": "", "network_warning_key": "",
		"no_payment_link_note": "", "no_payment_link_note_key": ""
	}`)
	data, err = json.Marshal(CryptoPaymentSteps(CryptoAssetSol, CryptoPaymentStatusPaid, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	requireJsonEqual(t, string(data), `[
		{"status": "pending", "progress": "done", "confirmations": 0, "confirmations_required": 0},
		{"status": "seen", "progress": "done", "confirmations": 0, "confirmations_required": 0},
		{"status": "confirming", "progress": "done", "confirmations": 1, "confirmations_required": 1},
		{"status": "paid", "progress": "done", "confirmations": 0, "confirmations_required": 0}
	]`)
	data, err = json.Marshal(CryptoPaymentSteps(CryptoAssetBtc, CryptoPaymentStatusReview, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(data), "[]")
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
		"cryptoPaymentTabCoin":    len(vectors.CryptoPaymentTabCoin),
		"cryptoPaymentShowsMemo":  len(vectors.CryptoPaymentShowsMemo),
		"cryptoPaymentSteps":      len(vectors.CryptoPaymentSteps),
		"cryptoPaidAsset":         len(vectors.CryptoPaidAsset),
	} {
		if count == 0 {
			t.Errorf("no %s vectors were read", name)
		}
	}
}
