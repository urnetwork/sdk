package sdk

import (
	"net/url"
	"slices"
	"strings"
	"time"
)

// Crypto payment rules, shared by every app and by ur.io.
//
// This is the Go half of a pair: sdk/js/src/utils/crypto_pay.ts is the
// TypeScript twin, with the same names in camelCase for the functions and in
// UPPER_SNAKE_CASE for the constants, as the JS package names its constants.
// The two must stay in step; crypto_pay_test.go and js/test/crypto_pay.test.ts
// pin the same vectors.
//
// The rule they encode is the Solana Pay one (solana_pay.go): the client never
// names its own price, address or payment uri. The server quotes the invoice
// and builds its uri, and the client only checks the uri against the address
// it shows (CheckCryptoPaymentUri) before it renders or opens it.
//
// The coins sit in the payment widget's tabs (CryptoPaymentTabs): USDC on
// Solana, Zcash and Monero each have a tab of their own, and TAO, Bitcoin, ETH
// and SOL share the "More coins" tab (mmm/BTCPAY.md §17.6). Every coin's
// facts (its ticker, label, mark, decimals, market, uri scheme and the lines
// its invoice shows) are in one table, cryptoAssetCoins, which every function
// here reads. So are the rules every app once kept a copy of: the coin a tap
// on a tab chooses (CryptoPaymentTabCoin), when the memo shows
// (CryptoPaymentShowsMemo), the status steps (CryptoPaymentSteps), the
// product the purchase events name (CryptoPaymentEventProduct) and the coin a
// paid year is named by (CryptoPaidAsset).

// The coins (the `asset` of every crypto payment route). Each is also the
// payment method of its coin; PaymentMethodCard is the other.
const (
	// USDC on Solana, an SPL token
	CryptoAssetUsdcSolana = "usdc_solana"
	CryptoAssetZec        = "zec"
	CryptoAssetXmr        = "xmr"
	// TAO, on Bittensor
	CryptoAssetTao = "tao"
	CryptoAssetBtc = "btc"
	CryptoAssetEth = "eth"
	// native SOL
	CryptoAssetSol = "sol"
)

// PaymentMethodCard is the fiat method: Stripe on every surface outside an
// app store. The coin methods are the CryptoAsset* values.
const PaymentMethodCard = "card"

// The payment widget's tab of several coins: TAO, Bitcoin, ETH and SOL. A tab
// of one coin has that coin's asset as its id.
const CryptoPaymentTabMoreCoins = "more_coins"

// The coins' marks (CryptoPaymentCoin.IconKey): each project's official mark
// in a single color, which every app bundles under this name (an Android
// drawable coin_btc, an asset catalog image coin.btc on Apple). USDC on
// Solana shows the USDC mark, and SOL the Solana mark.
const (
	CryptoIconKeyUsdc = "coin_usdc"
	CryptoIconKeyZec  = "coin_zec"
	CryptoIconKeyXmr  = "coin_xmr"
	CryptoIconKeyTao  = "coin_tao"
	CryptoIconKeyBtc  = "coin_btc"
	CryptoIconKeyEth  = "coin_eth"
	CryptoIconKeySol  = "coin_sol"
)

// An invoice's state (CryptoPaymentStatusResult.Status), first match wins.
const (
	// nothing received yet
	CryptoPaymentStatusPending = "pending"
	// something arrived, short of the amount; Remaining says how much more
	CryptoPaymentStatusSeen = "seen"
	// the amount arrived, short of the confirmations it needs
	CryptoPaymentStatusConfirming = "confirming"
	// credited
	CryptoPaymentStatusPaid = "paid"
	// past the quote's expiry with nothing received; Requote gives a new one
	CryptoPaymentStatusExpired = "expired"
	// a payment needs an operator (ReviewReason)
	CryptoPaymentStatusReview = "review"
	// no such invoice, or the status token does not match
	CryptoPaymentStatusUnknown = "unknown"
)

// How one of an invoice's status steps is drawn (CryptoPaymentStep.Progress).
const (
	// the payment is past it, whether the server reported the step or the
	// payment skipped it
	CryptoPaymentStepDone = "done"
	// where the payment is now
	CryptoPaymentStepCurrent = "current"
	// not reached yet
	CryptoPaymentStepUpcoming = "upcoming"
)

// Where a paid invoice goes (CryptoPaymentIntentResult.Delivery).
const (
	// the signed-in network
	CryptoPaymentDeliveryAccount = "account"
	// a network named on the ur.io buy-data page
	CryptoPaymentDeliveryNetwork = "network"
	// a balance code, shown once paid and emailed when an address was given
	CryptoPaymentDeliveryCode = "code"
)

// The crypto refusals, beside the other PurchaseErrorCode* values (api.go).
const (
	// the coin is switched off, or cannot be quoted right now; the UI says
	// "Temporarily unavailable"
	PurchaseErrorCodeAssetUnavailable = "asset_unavailable"
	// the quote ran out with nothing received; the UI offers "Refresh quote"
	PurchaseErrorCodeQuoteExpired = "quote_expired"
	// the invoice ended short; the UI shows the review copy
	PurchaseErrorCodeUnderpaid = "underpaid"
)

// the USDC mint on Solana mainnet, the only token a usdc_solana uri may name
const cryptoSolanaUsdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// One coin's facts, the same on every surface.
type cryptoCoin struct {
	// shown beside an amount ("0.072387 XMR")
	ticker string
	// the label in English, and its string id in the shared localizations
	// (localizations/keys), which every app shows in its own language
	label    string
	labelKey string
	// the coin's mark (CryptoIconKey*)
	iconKey string
	// the atomic unit's decimals: amount_atomic is the amount times 10^decimals
	decimals int
	// the decimals the server quotes an amount to (it rounds amounts up)
	quoteDecimals int
	// the subscription market a credit in the coin renews on, which is also
	// the store its purchase events name (EventStore*)
	market string
	// the scheme of its payment uri, "" for a coin without one: its invoice
	// shows the address and the amount, and a QR code of the address alone
	uriScheme string
	// the lines its invoice shows, each in English with its string id
	// (CryptoAssetLines): every English is the store's English byte for byte,
	// which an app that looks a line up by its English needs
	networkLine          string
	networkLineKey       string
	networkWarning       string
	networkWarningKey    string
	noPaymentLinkNote    string
	noPaymentLinkNoteKey string
}

// The coins this sdk knows, by asset (mmm/BTCPAY.md §17.7 for the four added
// on 2026-11-10). A coin missing here is never offered and never shown.
var cryptoAssetCoins = map[string]cryptoCoin{
	CryptoAssetUsdcSolana: {
		ticker:            "USDC",
		label:             "USDC on Solana",
		labelKey:          "pay_method_usdc_solana",
		iconKey:           CryptoIconKeyUsdc,
		decimals:          6,
		quoteDecimals:     6,
		market:            "solana",
		uriScheme:         "solana",
		networkLine:       "USDC on Solana",
		networkLineKey:    "crypto_network_usdc",
		networkWarning:    "Send USDC on the Solana network only. USDC sent on any other network will not arrive.",
		networkWarningKey: "usdc_solana_network_only",
	},
	CryptoAssetZec: {
		ticker:         "ZEC",
		label:          "Zcash",
		labelKey:       "pay_method_zcash",
		iconKey:        CryptoIconKeyZec,
		decimals:       8,
		quoteDecimals:  6,
		market:         "zcash",
		uriScheme:      "zcash",
		networkLine:    "Zcash, shielded",
		networkLineKey: "crypto_network_zcash",
	},
	CryptoAssetXmr: {
		ticker:         "XMR",
		label:          "Monero",
		labelKey:       "pay_method_monero",
		iconKey:        CryptoIconKeyXmr,
		decimals:       12,
		quoteDecimals:  6,
		market:         "monero",
		uriScheme:      "monero",
		networkLine:    "Monero",
		networkLineKey: "crypto_network_monero",
	},
	CryptoAssetTao: {
		ticker:        "TAO",
		label:         "TAO",
		labelKey:      "pay_method_tao",
		iconKey:       CryptoIconKeyTao,
		decimals:      9,
		quoteDecimals: 6,
		market:        "bittensor",
		// TAO has no payment uri standard
		uriScheme:            "",
		networkLine:          "Bittensor network",
		networkLineKey:       "crypto_network_tao",
		networkWarning:       "Send only TAO on the Bittensor network to this address. TAO sent on any other network won't be credited.",
		networkWarningKey:    "crypto_invoice_tao_network_only",
		noPaymentLinkNote:    "TAO has no payment link, so this QR code holds only the address. Enter the exact amount in your wallet.",
		noPaymentLinkNoteKey: "crypto_invoice_tao_no_payment_link",
	},
	CryptoAssetBtc: {
		ticker:            "BTC",
		label:             "Bitcoin",
		labelKey:          "pay_method_bitcoin",
		iconKey:           CryptoIconKeyBtc,
		decimals:          8,
		quoteDecimals:     8,
		market:            "bitcoin",
		uriScheme:         "bitcoin",
		networkLine:       "Bitcoin network",
		networkLineKey:    "crypto_network_bitcoin",
		networkWarning:    "Send only Bitcoin on the Bitcoin network to this address. Bitcoin sent on any other network won't be credited.",
		networkWarningKey: "crypto_invoice_bitcoin_network_only",
	},
	CryptoAssetEth: {
		ticker:            "ETH",
		label:             "ETH",
		labelKey:          "pay_method_eth",
		iconKey:           CryptoIconKeyEth,
		decimals:          18,
		quoteDecimals:     6,
		market:            "ethereum",
		uriScheme:         "ethereum",
		networkLine:       "Ethereum mainnet",
		networkLineKey:    "crypto_network_eth",
		networkWarning:    "Send only ETH on Ethereum mainnet to this address. Tokens and ETH on other networks, such as Base or Arbitrum, won't be credited.",
		networkWarningKey: "crypto_invoice_eth_network_only",
	},
	CryptoAssetSol: {
		ticker:            "SOL",
		label:             "SOL",
		labelKey:          "pay_method_sol",
		iconKey:           CryptoIconKeySol,
		decimals:          9,
		quoteDecimals:     6,
		market:            "solana",
		uriScheme:         "solana",
		networkLine:       "Solana (native SOL)",
		networkLineKey:    "crypto_network_sol",
		networkWarning:    "Send only SOL on the Solana network to this address. Tokens, including USDC, won't be credited.",
		networkWarningKey: "crypto_invoice_sol_network_only",
	},
}

// One tab of the payment widget's coin selector.
type cryptoPaymentTabLayout struct {
	id       string
	label    string
	labelKey string
	// its coins, in the order its sub-selector shows them
	assets []string
	// shown only while the server lists one of its coins; a tab of one coin
	// is shown whenever the selector is
	listedOnly bool
}

// The selector's tabs in order: the three main coins, then "More coins".
var cryptoPaymentTabLayouts = []cryptoPaymentTabLayout{
	{
		id:       CryptoAssetUsdcSolana,
		label:    cryptoAssetCoins[CryptoAssetUsdcSolana].label,
		labelKey: cryptoAssetCoins[CryptoAssetUsdcSolana].labelKey,
		assets:   []string{CryptoAssetUsdcSolana},
	},
	{
		id:       CryptoAssetZec,
		label:    cryptoAssetCoins[CryptoAssetZec].label,
		labelKey: cryptoAssetCoins[CryptoAssetZec].labelKey,
		assets:   []string{CryptoAssetZec},
	},
	{
		id:       CryptoAssetXmr,
		label:    cryptoAssetCoins[CryptoAssetXmr].label,
		labelKey: cryptoAssetCoins[CryptoAssetXmr].labelKey,
		assets:   []string{CryptoAssetXmr},
	},
	{
		id:         CryptoPaymentTabMoreCoins,
		label:      "More coins",
		labelKey:   "pay_method_more_coins",
		assets:     []string{CryptoAssetTao, CryptoAssetBtc, CryptoAssetEth, CryptoAssetSol},
		listedOnly: true,
	},
}

// The coins in the order every surface shows them, after Card: the tabs'
// coins in the tabs' order. The order of the server's list means nothing.
var cryptoAssetOrder = func() []string {
	assets := []string{}
	for _, layout := range cryptoPaymentTabLayouts {
		assets = append(assets, layout.assets...)
	}
	return assets
}()

func isCryptoAsset(asset string) bool {
	_, ok := cryptoAssetCoins[asset]
	return ok
}

// PaymentMethods is the method row for a purchase surface: PaymentMethodCard,
// then each coin that sells at least one of `items` (the plans or the packs
// the surface offers), always in the order USDC, Zcash, Monero, TAO, Bitcoin,
// ETH, SOL. A coin the server did not list, or one this sdk does not know, is
// never offered. The server lists only the coins it would quote now, so a coin
// whose quotes it refuses for now (its wallet scanner lagging) is not offered
// either. CryptoPaymentTabs lays the same coins out in the widget's tabs.
func PaymentMethods(assets *CryptoPaymentAssetList, items *StringList) *StringList {
	methods := NewStringList()
	methods.Add(PaymentMethodCard)
	if items == nil {
		return methods
	}
	for _, asset := range cryptoAssetOrder {
		for _, item := range items.values {
			if CryptoAssetSells(assets, asset, item) {
				methods.Add(asset)
				break
			}
		}
	}
	return methods
}

// CryptoAssetSells reports whether the server's list names `asset` as selling
// `item`, a plan (PlanYearly, PlanYearlyOnboarding) or a data pack
// (StripeItemData1Tib, StripeItemData10Tib). Monthly is card only, whatever
// the list says.
func CryptoAssetSells(assets *CryptoPaymentAssetList, asset string, item string) bool {
	if assets == nil || !isCryptoAsset(asset) || !PaymentMethodAllowsPlan(asset, item) {
		return false
	}
	for _, listed := range assets.values {
		if listed == nil || listed.Asset != asset {
			continue
		}
		if (listed.Plans != nil && listed.Plans.Contains(item)) ||
			(listed.Items != nil && listed.Items.Contains(item)) {
			return true
		}
	}
	return false
}

// PaymentMethodAllowsPlan is false only for a coin with PlanMonthly: no coin
// sells monthly, so picking a coin greys the monthly plan out ("Monthly is
// card only"). One network fee would be a large part of a regional monthly
// price.
func PaymentMethodAllowsPlan(method string, plan string) bool {
	return !(isCryptoAsset(method) && plan == PlanMonthly)
}

// A coin in the payment widget.
type CryptoPaymentCoin struct {
	// one of the CryptoAsset* values
	Asset string `json:"asset"`
	// "USDC on Solana", "Zcash", "Monero", "TAO", "Bitcoin", "ETH" or "SOL",
	// in English (CryptoAssetLabel)
	Label string `json:"label"`
	// the label's string id in the shared localizations (CryptoAssetLabelKey)
	LabelKey string `json:"label_key"`
	// the coin's mark, one of the CryptoIconKey* values. A mark is never dimmed
	// (Solana's and Bittensor's brand rules allow theirs only in black or
	// white, at full contrast), whatever Available says.
	IconKey string `json:"icon_key"`
	// the server lists the coin as payable now, for one of the surface's
	// items. An unavailable coin stays in its place and cannot be chosen: its
	// label is greyed, with "Temporarily unavailable" (pay_method_unavailable)
	// beside it, and its mark is unchanged.
	Available bool `json:"available"`
}

type CryptoPaymentCoinList struct {
	exportedList[*CryptoPaymentCoin]
}

func NewCryptoPaymentCoinList() *CryptoPaymentCoinList {
	return &CryptoPaymentCoinList{
		exportedList: *newExportedList[*CryptoPaymentCoin](),
	}
}

// A tab of the payment widget's coin selector: one coin, or the "More coins"
// tab, which opens a sub-selector of its coins.
type CryptoPaymentTab struct {
	// the asset of a tab with one coin, or CryptoPaymentTabMoreCoins
	Id string `json:"id"`
	// in English: the coin's label, or "More coins"
	Label string `json:"label"`
	// the label's string id in the shared localizations
	LabelKey string `json:"label_key"`
	// the marks the tab shows, in order, each at full contrast: its coin's, or
	// the four of "More coins" as separate marks with space between them,
	// never merged into one badge
	IconKeys *StringList `json:"icon_keys"`
	// its coins, in order: one, or TAO, Bitcoin, ETH and SOL. Each says
	// whether it can be paid with now.
	Coins *CryptoPaymentCoinList `json:"coins"`
	// at least one of its coins is available; a tab with none cannot be
	// chosen, and its label is greyed as an unavailable coin's is
	Available bool `json:"available"`
}

type CryptoPaymentTabList struct {
	exportedList[*CryptoPaymentTab]
}

func NewCryptoPaymentTabList() *CryptoPaymentTabList {
	return &CryptoPaymentTabList{
		exportedList: *newExportedList[*CryptoPaymentTab](),
	}
}

// The payment widget's coin selector for a purchase surface that sells
// `items` (as for PaymentMethods), in order: USDC on Solana, Zcash, Monero,
// then "More coins" with TAO, Bitcoin, ETH and SOL. Card stays the method
// row's own first choice (PaymentMethods).
//
// `assets` are the coins the server would quote now: the balance's
// SubscriptionBalanceResult.CryptoAssets, or CryptoPaymentAssetsResult.Assets.
// `unavailableAssets` are the coins that are on but cannot be quoted now: the
// balance's SubscriptionBalanceResult.CryptoUnavailableAssets, or
// CryptoPaymentAssetsResult.UnavailableAssets. Pass the two lists of the same
// answer; nil for a server that sends no second list. A coin is available when
// `assets` names it as selling one of `items`. The coins' per-coin `available`
// flags say the same thing, by the server's contract, and are not read.
//
//   - With no coin of either list selling one of the items, there is nothing
//     to choose: the list is empty, and the surface shows Card alone, as it did
//     before crypto.
//   - Otherwise the three main tabs are always there, and "More coins" is there
//     while either list names one of its coins (a server that predates them
//     names none).
//   - Every coin of a tab is shown in its place, never hidden. A coin that
//     cannot be paid with now, for these items, has Available false: its label
//     is greyed and says "Temporarily unavailable", and its mark, on its tab
//     and in the sub-selector, is unchanged. A coin left out of `assets` only
//     greys out while `unavailableAssets` names it; otherwise its tab, and
//     More coins with all four of its coins, would vanish instead.
func CryptoPaymentTabs(assets *CryptoPaymentAssetList, unavailableAssets *CryptoPaymentAssetList, items *StringList) *CryptoPaymentTabList {
	tabs := NewCryptoPaymentTabList()
	if items == nil {
		return tabs
	}
	sells := func(list *CryptoPaymentAssetList, asset string) bool {
		for _, item := range items.values {
			if CryptoAssetSells(list, asset, item) {
				return true
			}
		}
		return false
	}
	listed := func(asset string) bool {
		return sells(assets, asset) || sells(unavailableAssets, asset)
	}
	if !slices.ContainsFunc(cryptoAssetOrder, listed) {
		return tabs
	}
	for _, layout := range cryptoPaymentTabLayouts {
		if layout.listedOnly && !slices.ContainsFunc(layout.assets, listed) {
			continue
		}
		tab := &CryptoPaymentTab{
			Id:       layout.id,
			Label:    layout.label,
			LabelKey: layout.labelKey,
			IconKeys: NewStringList(),
			Coins:    NewCryptoPaymentCoinList(),
		}
		for _, asset := range layout.assets {
			coin := cryptoAssetCoins[asset]
			available := sells(assets, asset)
			tab.Coins.Add(&CryptoPaymentCoin{
				Asset:     asset,
				Label:     coin.label,
				LabelKey:  coin.labelKey,
				IconKey:   coin.iconKey,
				Available: available,
			})
			tab.IconKeys.Add(coin.iconKey)
			if available {
				tab.Available = true
			}
		}
		tabs.Add(tab)
	}
	return tabs
}

// The id of the tab a coin sits in: the asset itself for USDC on Solana,
// Zcash and Monero, CryptoPaymentTabMoreCoins for TAO, Bitcoin, ETH and SOL,
// and "" for anything else. A remembered method
// (CryptoInvoiceViewController.GetPaymentMethod) selects this tab.
func CryptoPaymentTabId(asset string) string {
	for _, layout := range cryptoPaymentTabLayouts {
		if slices.Contains(layout.assets, asset) {
			return layout.id
		}
	}
	return ""
}

// The coin a tap on `tab` chooses (one of the tabs of CryptoPaymentTabs; a tab
// of several coins, More coins, opens its list with this coin chosen), in this
// order:
//   - `lastChosen`, the coin last chosen in this tab while the surface was
//     up, when it can be paid with now. A surface keeps it per tab: choosing a
//     coin of another tab since does not replace it, so a tap back on More
//     coins finds the coin that was chosen there;
//   - else `remembered`, the method remembered on the device
//     (CryptoInvoiceViewController.GetPaymentMethod), when it is one of the
//     tab's coins that can be paid with now;
//   - else the tab's first coin that can be paid with now (TAO first in More
//     coins).
//
// "" for a tab none of whose coins can be paid with now, which cannot be
// chosen. A coin of another tab, Card, "" or an asset this sdk does not know
// never matches `lastChosen` or `remembered`.
func CryptoPaymentTabCoin(tab *CryptoPaymentTab, lastChosen string, remembered string) string {
	if tab == nil || tab.Coins == nil {
		return ""
	}
	choosable := []string{}
	for _, coin := range tab.Coins.values {
		if coin != nil && coin.Available && isCryptoAsset(coin.Asset) {
			choosable = append(choosable, coin.Asset)
		}
	}
	for _, asset := range []string{lastChosen, remembered} {
		if slices.Contains(choosable, asset) {
			return asset
		}
	}
	if len(choosable) == 0 {
		return ""
	}
	return choosable[0]
}

// CryptoAssetTicker is the ticker shown beside an amount ("0.072387 XMR"), ""
// for anything else.
func CryptoAssetTicker(asset string) string {
	return cryptoAssetCoins[asset].ticker
}

// The coin's label in English ("USDC on Solana", "Bitcoin"), "" for anything
// else. The label of USDC always names its chain.
func CryptoAssetLabel(asset string) string {
	return cryptoAssetCoins[asset].label
}

// The string id of the coin's label in the shared localizations
// (localizations/keys), "" for anything else.
func CryptoAssetLabelKey(asset string) string {
	return cryptoAssetCoins[asset].labelKey
}

// The coin's mark, one of the CryptoIconKey* values, "" for anything else.
func CryptoAssetIconKey(asset string) string {
	return cryptoAssetCoins[asset].iconKey
}

// A coin's own lines on its invoice and in the More coins list (mmm/BTCPAY.md
// §17.6), each in English beside its string id in the shared localizations
// (localizations/keys). An app shows its own string for the id, and the
// English when it has none: a coin is never shown with another coin's line.
// A line whose id is "" is one the coin does not have, and is not shown.
type CryptoAssetLines struct {
	// the network the coin is sent on: the invoice's first line, and the line
	// under the coin in the More coins list. "USDC on Solana"
	// (crypto_network_usdc), "Zcash, shielded", "Monero", "Bittensor
	// network", "Bitcoin network", "Ethereum mainnet", "Solana (native SOL)".
	NetworkLine    string `json:"network_line"`
	NetworkLineKey string `json:"network_line_key"`
	// shown with the address: only this coin, sent on this network, is
	// credited (usdc_solana_network_only, crypto_invoice_tao_network_only,
	// ...). None for Zcash and Monero, which have one network and no tokens.
	NetworkWarning    string `json:"network_warning"`
	NetworkWarningKey string `json:"network_warning_key"`
	// a coin without a payment link (TAO): the note under its QR code, which
	// holds the address alone, saying the amount is entered by hand
	// (crypto_invoice_tao_no_payment_link). None for every other coin.
	NoPaymentLinkNote    string `json:"no_payment_link_note"`
	NoPaymentLinkNoteKey string `json:"no_payment_link_note_key"`
}

// The coin's lines (CryptoAssetLines); every one empty for anything else.
// Never nil.
func CryptoAssetLinesFor(asset string) *CryptoAssetLines {
	coin := cryptoAssetCoins[asset]
	return &CryptoAssetLines{
		NetworkLine:          coin.networkLine,
		NetworkLineKey:       coin.networkLineKey,
		NetworkWarning:       coin.networkWarning,
		NetworkWarningKey:    coin.networkWarningKey,
		NoPaymentLinkNote:    coin.noPaymentLinkNote,
		NoPaymentLinkNoteKey: coin.noPaymentLinkNoteKey,
	}
}

// The decimals of the coin's atomic unit (an invoice's AmountAtomic is its
// Amount times 10^decimals): 6 for USDC, 8 for Zcash and Bitcoin, 12 for
// Monero, 9 for TAO and SOL, 18 for ETH. 0 for anything else.
func CryptoAssetDecimals(asset string) int {
	return cryptoAssetCoins[asset].decimals
}

// The decimals the server quotes the coin's amounts to, rounding up: 8 for
// Bitcoin, 6 for every other coin. 0 for anything else.
func CryptoAssetQuoteDecimals(asset string) int {
	return cryptoAssetCoins[asset].quoteDecimals
}

// The subscription market a credit in the coin renews on (CryptoTerm.Market,
// Subscription.Store), which is also the store its purchase events name:
// "solana" for USDC and SOL, "zcash", "monero", "bittensor" for TAO, "bitcoin"
// and "ethereum". "" for anything else.
func CryptoAssetMarket(asset string) string {
	return cryptoAssetCoins[asset].market
}

// The scheme of the coin's payment uri: "solana" (Solana Pay) for USDC and
// SOL, "zcash" (ZIP 321), "monero", "bitcoin" (BIP 21) and "ethereum" (EIP
// 681). "" for TAO, which has no payment uri, and for anything else. A uri is
// this scheme and a colon; CheckCryptoPaymentUri is the whole check an app
// runs before it draws or opens one, the scheme's included.
func CryptoPaymentUriScheme(asset string) string {
	return cryptoAssetCoins[asset].uriScheme
}

// The product a coin's Pro purchase events name (NewPurchase*Event,
// purchase.*): the year of Pro by the coin's ticker, lowercased:
// "pro_yearly_usdc", "pro_yearly_zec", "pro_yearly_xmr", "pro_yearly_tao",
// "pro_yearly_btc", "pro_yearly_eth" and "pro_yearly_sol", the welcome
// offer's year included (its plan says yearly). The store is the coin's
// market (CryptoAssetMarket), which USDC and SOL share, so the product is
// what tells their purchases apart. "" for anything else. The server takes
// any product token and none of its rollups or dashboards read it.
func CryptoPaymentEventProduct(asset string) string {
	ticker := CryptoAssetTicker(asset)
	if ticker == "" {
		return ""
	}
	return "pro_yearly_" + strings.ToLower(ticker)
}

// The coin a one-time crypto year was paid with, which Manage subscription
// names ("Paid with SOL."): `asset`, the server's word (a subscription's
// crypto_asset in GET /subscription/details, CryptoTerm.Asset), when it is a
// coin of `market`; else the market's coin as a server before native SOL had
// it: USDC on Solana for "solana", then Zcash, Monero, TAO, Bitcoin and ETH
// for theirs. Never the market alone: USDC and SOL share "solana", so a SOL
// year named by its market would read "USDC on Solana". "" for any other
// market.
func CryptoPaidAsset(market string, asset string) string {
	if isCryptoAsset(asset) && CryptoAssetMarket(asset) == market {
		return asset
	}
	for _, coin := range cryptoAssetOrder {
		if CryptoAssetMarket(coin) == market {
			return coin
		}
	}
	return ""
}

// CryptoWalletSuggestions are the wallets the invoice names as working for a
// coin ("Wallets that work: ..."), in the order to show them. Only TAO names
// any: Bittensor's own list, product names, never translated. The owner,
// 2026-11-11: "we do not need to suggest btc or eth wallets. for other coins
// besides tao we don't need to suggest wallets". So USDC on Solana, Zcash,
// Monero, SOL, Bitcoin and ETH are empty, as is anything else, and an empty
// list shows no line.
func CryptoWalletSuggestions(asset string) *StringList {
	wallets := NewStringList()
	switch asset {
	case CryptoAssetTao:
		wallets.addAll("btcli", "Talisman", "Nova", "SubWallet", "Polkadot.js", "Ledger", "Tangem")
	}
	return wallets
}

// Whether an invoice shows its memo, with Copy, for a transfer sent by hand:
// only when the server sent one. The server sends a memo for USDC on Solana
// alone (its Solana Pay reference, since every USDC invoice pays the one
// merchant address) and "" for every other coin, SOL included: a SOL invoice
// has an address of its own, and its reference rides in its payment uri. So
// a coin never shows an empty memo row, and no app decides by coin. False for
// a coin this sdk does not know. The invoice shows it only while payable, as
// it does the amount.
func CryptoPaymentShowsMemo(asset string, memo string) bool {
	return isCryptoAsset(asset) && memo != ""
}

// One of an invoice's status steps (CryptoPaymentSteps).
type CryptoPaymentStep struct {
	// the status the step stands for, with its line: CryptoPaymentStatusPending
	// (crypto_status_waiting, "Waiting for your payment"),
	// CryptoPaymentStatusSeen (crypto_status_seen, "Payment seen"),
	// CryptoPaymentStatusConfirming (crypto_status_confirming, "Confirming:
	// {count} of {required}") or CryptoPaymentStatusPaid (crypto_status_paid,
	// "Paid. Thank you!")
	Status string `json:"status"`
	// one of the CryptoPaymentStep* values
	Progress string `json:"progress"`
	// the confirming step's {count} and {required}; 0 for every other step
	Confirmations         int `json:"confirmations"`
	ConfirmationsRequired int `json:"confirmations_required"`
}

type CryptoPaymentStepList struct {
	exportedList[*CryptoPaymentStep]
}

func NewCryptoPaymentStepList() *CryptoPaymentStepList {
	return &CryptoPaymentStepList{
		exportedList: *newExportedList[*CryptoPaymentStep](),
	}
}

// The status steps an invoice in `asset` shows at `status`, in order, each
// with how it is drawn (CryptoInvoiceState.Steps reads the invoice's own).
//
// USDC on Solana has two steps, pending and paid: its server sees confirmed
// transfers only, so it waits until it is paid. Every other coin has four:
// pending, seen (part of the amount arrived), confirming (the whole amount
// arrived and collects its confirmations) and paid. The confirming step is
// listed only while the confirmations required are known (more than 0).
//
// A payment may skip steps: a whole payment never reports seen, and SOL, TAO
// and ETH, which the server sees only once final, usually go from pending
// straight to paid. A step the payment is past is drawn done
// (CryptoPaymentStepDone), whether the server reported it or not; once paid
// every step is done. A done confirming step reads its full count ({required}
// of {required}), the current one the payment's confirmations (at most
// {required}), an upcoming one 0.
//
// Pending and expired (nothing received) are at the first step. No steps for
// any other status: "" (a resumed invoice before its first poll), review (the
// review copy stands instead), unknown, or a status this sdk does not know;
// and none for an asset this sdk does not know. Whether a surface shows the
// steps of an expired quote is the surface's.
func CryptoPaymentSteps(asset string, status string, confirmations int, confirmationsRequired int) *CryptoPaymentStepList {
	steps := NewCryptoPaymentStepList()
	if !isCryptoAsset(asset) {
		return steps
	}
	statuses := []string{CryptoPaymentStatusPending, CryptoPaymentStatusPaid}
	if asset != CryptoAssetUsdcSolana {
		statuses = []string{CryptoPaymentStatusPending, CryptoPaymentStatusSeen}
		if 0 < confirmationsRequired {
			statuses = append(statuses, CryptoPaymentStatusConfirming)
		}
		statuses = append(statuses, CryptoPaymentStatusPaid)
	}
	var current int
	switch status {
	case CryptoPaymentStatusPending, CryptoPaymentStatusExpired:
		current = 0
	case CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusPaid:
		current = slices.Index(statuses, status)
		if current < 0 {
			// confirming with no confirming step listed: past what arrived,
			// short of paid
			current = len(statuses) - 2
		}
	default:
		return steps
	}
	paid := status == CryptoPaymentStatusPaid
	for i, stepStatus := range statuses {
		step := &CryptoPaymentStep{
			Status: stepStatus,
		}
		switch {
		case paid || i < current:
			step.Progress = CryptoPaymentStepDone
		case i == current:
			step.Progress = CryptoPaymentStepCurrent
		default:
			step.Progress = CryptoPaymentStepUpcoming
		}
		if stepStatus == CryptoPaymentStatusConfirming {
			step.ConfirmationsRequired = confirmationsRequired
			switch step.Progress {
			case CryptoPaymentStepDone:
				step.Confirmations = confirmationsRequired
			case CryptoPaymentStepCurrent:
				step.Confirmations = min(max(confirmations, 0), confirmationsRequired)
			}
		}
		steps.Add(step)
	}
	return steps
}

// A uri query's parameter, its name percent-decoded.
type cryptoPaymentUriParam struct {
	name string
	// the name with ASCII letters lowercased, and nothing else changed, so
	// both twins compare names the same way
	lowerName string
	rawValue  string
}

// CheckCryptoPaymentUri reports whether `uri` is a payment uri of `asset`'s
// coin that pays exactly `address`, the address the invoice shows. An app
// refuses to render or open a uri that fails it: the QR code and the "Open in
// wallet" button must never pay somewhere other than the address on screen,
// nor in another coin. It is the whole gate: an app keeps no scheme list or
// pre-check of its own.
//
// The uri is the server's `<scheme>:<address>?<query>`, with the coin's
// scheme (CryptoPaymentUriScheme). The scheme compares without case, as RFC
// 3986 says; the address between the colon and the query must be `address`
// byte for byte; and the uri must be plain printable ASCII with no fragment,
// as the server's percent-encoded uris are. TAO has no payment uri, so a TAO
// uri always fails. Monero puts a second recipient in the path, which the
// exact address match already refuses, and its query is not read. For every
// other coin the query's parameter names are percent-decoded (a malformed
// escape fails the uri) and compare with their ASCII letters lowercased:
//   - Zcash (ZIP 321) must not name a second recipient: one uri may pay several
//     addresses (`address.1=...`), and a wallet would pay them all.
//   - USDC on Solana (Solana Pay) must name the USDC mint as its one
//     `spl-token`: without it, a wallet would send the amount in SOL.
//   - SOL (Solana Pay) must name no `spl-token`, which would send a token
//     instead. Its `reference` keys, like USDC's, are not funds and are not
//     checked here.
//   - Bitcoin (BIP 21) may carry `amount` (in BTC), `label` and `message`,
//     each once, and nothing else: a `lightning` invoice, another address
//     (BIP 321's `bc`, `sp`, `lno`) or a `req-` parameter would pay something
//     the invoice does not show.
//   - ETH (EIP 681) names Ethereum mainnet after the address
//     (`<address>@1`), with no function call, and may carry `value` (in wei)
//     once and nothing else.
func CheckCryptoPaymentUri(asset string, uri string, address string) bool {
	scheme := CryptoPaymentUriScheme(asset)
	if scheme == "" || address == "" || uri == "" {
		return false
	}
	for i := 0; i < len(uri); i += 1 {
		if c := uri[i]; c <= ' ' || '~' < c || c == '#' {
			return false
		}
	}
	uriScheme, rest, ok := strings.Cut(uri, ":")
	if !ok || !strings.EqualFold(uriScheme, scheme) {
		return false
	}
	uriTarget, query, _ := strings.Cut(rest, "?")
	target := address
	if asset == CryptoAssetEth {
		// the chain id, 1 for Ethereum mainnet
		target = address + "@1"
	}
	if uriTarget != target {
		return false
	}
	if asset == CryptoAssetXmr {
		// its query names no recipient
		return true
	}
	// the query split at `&`, each parameter at its first `=`
	params := []cryptoPaymentUriParam{}
	if query != "" {
		for _, param := range strings.Split(query, "&") {
			rawName, rawValue, _ := strings.Cut(param, "=")
			name, err := url.PathUnescape(rawName)
			if err != nil {
				return false
			}
			lowerName := []byte(name)
			for i, c := range lowerName {
				if 'A' <= c && c <= 'Z' {
					lowerName[i] = c + ('a' - 'A')
				}
			}
			params = append(params, cryptoPaymentUriParam{
				name:      name,
				lowerName: string(lowerName),
				rawValue:  rawValue,
			})
		}
	}
	// every parameter's name is one of `names`, and none comes twice
	onlyOnce := func(names ...string) bool {
		seen := map[string]bool{}
		for _, param := range params {
			if !slices.Contains(names, param.lowerName) || seen[param.lowerName] {
				return false
			}
			seen[param.lowerName] = true
		}
		return true
	}
	splTokens := func() []cryptoPaymentUriParam {
		matches := []cryptoPaymentUriParam{}
		for _, param := range params {
			if param.lowerName == "spl-token" {
				matches = append(matches, param)
			}
		}
		return matches
	}
	switch asset {
	case CryptoAssetZec:
		for _, param := range params {
			if param.lowerName == "address" || strings.HasPrefix(param.lowerName, "address.") {
				return false
			}
		}
	case CryptoAssetUsdcSolana:
		matches := splTokens()
		if len(matches) != 1 || matches[0].name != "spl-token" {
			return false
		}
		mint, err := url.PathUnescape(matches[0].rawValue)
		if err != nil || mint != cryptoSolanaUsdcMint {
			return false
		}
	case CryptoAssetSol:
		if 0 < len(splTokens()) {
			return false
		}
	case CryptoAssetBtc:
		if !onlyOnce("amount", "label", "message") {
			return false
		}
	case CryptoAssetEth:
		if !onlyOnce("value") {
			return false
		}
	}
	return true
}

// The text an invoice's QR code encodes: `uri` when it passes
// CheckCryptoPaymentUri, the bare `address` for a coin without a payment uri
// (TAO, whose wallets scan an address alone), and "" for anything else, when
// the invoice shows no QR code and the address and the amount are sent by
// hand. An address that is not plain printable ASCII gets no QR code either.
func CryptoPaymentQrText(asset string, uri string, address string) string {
	if CheckCryptoPaymentUri(asset, uri, address) {
		return uri
	}
	if !isCryptoAsset(asset) || CryptoPaymentUriScheme(asset) != "" || address == "" {
		return ""
	}
	for i := 0; i < len(address); i += 1 {
		if c := address[i]; c <= ' ' || '~' < c {
			return ""
		}
	}
	return address
}

// CryptoTermNoticeDue reports whether an app shows the renewal notice for a
// one-time crypto credit now: from the end of its paid term until its grace
// ends, and never before (the credit cannot renew by itself, and a notice
// during the paid term would only nag). False for nil or for times that do
// not parse.
func CryptoTermNoticeDue(term *CryptoTerm) bool {
	return cryptoTermNoticeDueAt(term, time.Now())
}

func cryptoTermNoticeDueAt(term *CryptoTerm, now time.Time) bool {
	if term == nil {
		return false
	}
	termEnd, err := time.Parse(time.RFC3339, term.TermEndTime)
	if err != nil {
		return false
	}
	graceEnd, err := time.Parse(time.RFC3339, term.GraceEndTime)
	if err != nil {
		return false
	}
	return !now.Before(termEnd) && now.Before(graceEnd)
}
