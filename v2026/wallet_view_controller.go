//go:build !ios_extension

// The account's wallets, its past payments and its unpaid byte count.
//
// USDC provider payouts ended on 2026-11-12, and provider earnings are paid in
// SN25α to a Bittensor coldkey (see sn_wallet and the network consent). So this
// controller never reads or sets a USDC payout wallet (`/account/payout-wallet`)
// and never adds one: the payout wallet listener, the next payout date and the
// external wallet create were removed. The payments are the network's past
// USDC payouts, read-only history.
package sdk

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

type AccountWalletsListener interface {
	AccountWalletsChanged()
}

type PaymentsListener interface {
	PaymentsChanged()
}

type IsRemovingWalletListener interface {
	StateChanged(bool)
}

type UnpaidByteCountListener interface {
	StateChanged(int64)
}

type AccountWallet struct {
	WalletId         *Id        `json:"wallet_id"`
	CircleWalletId   string     `json:"circle_wallet_id,omitempty"`
	NetworkId        *Id        `json:"network_id"`
	WalletType       WalletType `json:"wallet_type"`
	Blockchain       string     `json:"blockchain"`
	WalletAddress    string     `json:"wallet_address"`
	Active           bool       `json:"active"`
	DefaultTokenType string     `json:"default_token_type"`
	CreateTime       *Time      `json:"create_time"`
	HasSeekerToken   bool       `json:"has_seeker_token"`
}

// One past USDC payout of the network (`GET /account/payments`), history only:
// a completed payment was paid, and one that is not completed was not paid and
// will not be, since USDC payouts ended on 2026-11-12. Never show it as pending.
type AccountPayment struct {
	PaymentId       *Id   `json:"payment_id"`
	PaymentPlanId   *Id   `json:"payment_plan_id"`
	WalletId        *Id   `json:"wallet_id"`
	NetworkId       *Id   `json:"network_id"`
	PayoutByteCount int64 `json:"payout_byte_count"`
	Payout          int64 `json:"payout_nano_cents"`
	// the subsidized parts of Payout
	SubsidyPayout      int64 `json:"subsidy_payout_nano_cents"`
	ReliabilitySubsidy int64 `json:"reliability_subsidy_nano_cents"`
	MinSweepTime       *Time `json:"min_sweep_time"`
	CreateTime         *Time `json:"create_time"`

	PaymentRecord  string  `json:"payment_record,omitempty"`
	TokenType      string  `json:"token_type"`
	TokenAmount    float64 `json:"token_amount,omitempty"`
	PaymentTime    *Time   `json:"payment_time,omitempty"`
	PaymentReceipt string  `json:"payment_receipt,omitempty"`
	WalletAddress  string  `json:"wallet_address"`
	Blockchain     string  `json:"blockchain,omitempty"`
	TxHash         string  `json:"tx_hash,omitempty"`

	Completed    bool  `json:"completed,omitempty"`
	CompleteTime *Time `json:"complete_time,omitempty"`

	Canceled   bool  `json:"canceled"`
	CancelTime *Time `json:"cancel_time,omitempty"`
}

type WalletViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	// all fields below guarded by stateLock
	wallets         *AccountWalletsList
	accountPayments *AccountPaymentsList
	unpaidByteCount ByteCount

	isRemovingWallet        bool
	isPollingAccountWallets bool
	isFetchingTransferStats bool

	// fetch sequence numbers — newer responses replace older,
	// older responses landing after newer are discarded
	accountWalletsNextSeq    int64
	accountWalletsAppliedSeq int64
	paymentsNextSeq          int64
	paymentsAppliedSeq       int64
	transferStatsNextSeq     int64
	transferStatsAppliedSeq  int64

	// lifecycle for Start/Stop polling; guarded by lifecycleLock
	lifecycleLock sync.Mutex
	pollerCancel  context.CancelFunc
	pollerDone    chan struct{}

	accountWalletsListeners   *connect.CallbackList[AccountWalletsListener]
	paymentsListeners         *connect.CallbackList[PaymentsListener]
	isRemovingWalletListeners *connect.CallbackList[IsRemovingWalletListener]
	unpaidByteCountListeners  *connect.CallbackList[UnpaidByteCountListener]
}

func newWalletViewController(ctx context.Context, device Device) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		wallets:         NewAccountWalletsList(),
		accountPayments: NewAccountPaymentsList(),
		unpaidByteCount: 0,

		accountWalletsListeners:   connect.NewCallbackList[AccountWalletsListener](),
		paymentsListeners:         connect.NewCallbackList[PaymentsListener](),
		isRemovingWalletListeners: connect.NewCallbackList[IsRemovingWalletListener](),
		unpaidByteCountListeners:  connect.NewCallbackList[UnpaidByteCountListener](),
	}
	return vc
}

func (vc *WalletViewController) Start() {
	vc.lifecycleLock.Lock()
	if vc.pollerCancel != nil {
		vc.lifecycleLock.Unlock()
		return
	}
	pollerCtx, pollerCancel := context.WithCancel(vc.ctx)
	pollerDone := make(chan struct{})
	vc.pollerCancel = pollerCancel
	vc.pollerDone = pollerDone
	vc.lifecycleLock.Unlock()

	go connect.HandleError(vc.FetchAccountWallets)
	go connect.HandleError(vc.FetchPayments)
	go connect.HandleError(vc.FetchTransferStats)

	go connect.HandleError(func() {
		defer close(pollerDone)
		vc.runPoller(pollerCtx)
	})
}

func (vc *WalletViewController) Stop() {
	vc.lifecycleLock.Lock()
	cancel := vc.pollerCancel
	done := vc.pollerDone
	vc.pollerCancel = nil
	vc.pollerDone = nil
	vc.lifecycleLock.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (vc *WalletViewController) Close() {
	deviceLog(vc.device).Info("[wvc]close")

	vc.Stop()
	vc.cancel()
}

type ValidateAddressCallback interface {
	SendResult(valid bool)
}

type validateAddressCallbackImpl struct {
	sendResult func(bool)
}

func (v *validateAddressCallbackImpl) SendResult(valid bool) {
	v.sendResult(valid)
}

// Return the concrete type instead of the interface pointer
func NewValidateAddressCallback(sendResult func(bool)) *validateAddressCallbackImpl {
	return &validateAddressCallbackImpl{sendResult: sendResult}
}

func (vc *WalletViewController) ValidateAddress(
	address string,
	blockchain Blockchain,
	callback ValidateAddressCallback,
) {

	vc.device.GetApi().WalletValidateAddress(
		&WalletValidateAddressArgs{
			Address: address,
			Chain:   blockchain,
		},
		connect.NewApiCallback[*WalletValidateAddressResult](
			func(result *WalletValidateAddressResult, err error) {

				if err != nil {
					deviceLog(vc.device).Infof("[wvc]error validating address %s on %s: %s", address, blockchain, err)
					callback.SendResult(false)
					return
				}

				callback.SendResult(result.Valid)
			}),
	)

}

func (vc *WalletViewController) GetWallets() *AccountWalletsList {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.wallets
}

func (vc *WalletViewController) FilterWalletsById(idStr string) *AccountWallet {

	id, err := ParseId(idStr)
	if err != nil {
		return nil
	}

	wallets := vc.GetWallets()
	for i := 0; i < wallets.Len(); i++ {

		wallet := wallets.Get(i)

		if wallet.WalletId.Cmp(id) == 0 {
			return wallet
		}

	}

	return nil

}

func (vc *WalletViewController) AddAccountWalletsListener(listener AccountWalletsListener) Sub {
	callbackId := vc.accountWalletsListeners.Add(listener)
	return newSub(func() {
		vc.accountWalletsListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) accountWalletsChanged() {
	for _, listener := range vc.accountWalletsListeners.Get() {
		connect.HandleError(func() {
			listener.AccountWalletsChanged()
		})
	}
}

func (self *WalletViewController) FetchAccountWallets() {

	var seq int64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.accountWalletsNextSeq++
		seq = self.accountWalletsNextSeq
	}()

	self.device.GetApi().GetAccountWallets(connect.NewApiCallback[*GetAccountWalletsResult](
		func(results *GetAccountWalletsResult, err error) {

			if err != nil {
				deviceLog(self.device).Infof("[wvc]Error fetching account wallets: %s", err)
				return
			}

			newWalletsList := NewAccountWalletsList()
			var wallets []*AccountWallet

			for i := 0; i < results.Wallets.Len(); i++ {

				walletResult := results.Wallets.Get(i)

				wallet := &AccountWallet{
					WalletId:         walletResult.WalletId,
					CircleWalletId:   walletResult.CircleWalletId,
					NetworkId:        walletResult.NetworkId,
					WalletType:       walletResult.WalletType,
					Blockchain:       walletResult.Blockchain,
					WalletAddress:    walletResult.WalletAddress,
					Active:           walletResult.Active,
					DefaultTokenType: walletResult.DefaultTokenType,
					CreateTime:       walletResult.CreateTime,
					HasSeekerToken:   walletResult.HasSeekerToken,
				}

				wallets = append(wallets, wallet)

			}

			newWalletsList.addAll(wallets...)

			applied := false
			triggerRemovingDone := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.accountWalletsAppliedSeq >= seq {
					return
				}
				self.accountWalletsAppliedSeq = seq

				// if we are polling for new wallets and the result length differs
				// from the prior, stop polling
				if self.isPollingAccountWallets && newWalletsList.Len() != self.wallets.Len() {
					self.isPollingAccountWallets = false
				}

				self.wallets = newWalletsList
				applied = true

				// we fetch wallets after removing a wallet
				if self.isRemovingWallet {
					self.isRemovingWallet = false
					triggerRemovingDone = true
				}
			}()
			if !applied {
				return
			}
			self.accountWalletsChanged()
			if triggerRemovingDone {
				self.isRemovingWalletChanged(false)
			}

		}))

}

func (vc *WalletViewController) AddPaymentsListener(listener PaymentsListener) Sub {
	callbackId := vc.paymentsListeners.Add(listener)
	return newSub(func() {
		vc.paymentsListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) paymentsChanged() {
	for _, listener := range vc.paymentsListeners.Get() {
		connect.HandleError(func() {
			listener.PaymentsChanged()
		})
	}
}

func (vc *WalletViewController) GetAccountPayments() *AccountPaymentsList {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.accountPayments
}

func (vc *WalletViewController) FetchPayments() {

	var seq int64
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.paymentsNextSeq++
		seq = vc.paymentsNextSeq
	}()

	vc.device.GetApi().GetAccountPayments(connect.NewApiCallback[*GetNetworkAccountPaymentsResult](
		func(results *GetNetworkAccountPaymentsResult, err error) {

			if err != nil {
				deviceLog(vc.device).Infof("[wvc]fetch payments failed: %s", err)
				return
			}

			payouts := []*AccountPayment{}

			// todo - why is results.AccountPayments nil and not an empty array?
			if results != nil && results.AccountPayments != nil {

				for i := 0; i < results.AccountPayments.Len(); i += 1 {
					accountPayment := results.AccountPayments.Get(i)
					payouts = append(payouts, accountPayment)
				}
			}

			applied := false
			func() {
				vc.stateLock.Lock()
				defer vc.stateLock.Unlock()
				if vc.paymentsAppliedSeq >= seq {
					return
				}
				vc.paymentsAppliedSeq = seq
				if len(payouts) > 0 {
					vc.accountPayments = NewAccountPaymentsList()
					vc.accountPayments.addAll(payouts...)
				}
				applied = true
			}()
			if applied {
				vc.paymentsChanged()
			}

		}))

}

func (vc *WalletViewController) AddIsRemovingWalletListener(listener IsRemovingWalletListener) Sub {
	callbackId := vc.isRemovingWalletListeners.Add(listener)
	return newSub(func() {
		vc.isRemovingWalletListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) isRemovingWalletChanged(isRemoving bool) {
	for _, listener := range vc.isRemovingWalletListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isRemoving)
		})
	}
}

func (vc *WalletViewController) setIsRemovingWallet(isRemoving bool) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.isRemovingWallet = isRemoving
	}()
	vc.isRemovingWalletChanged(isRemoving)
}

func (vc *WalletViewController) RemoveWallet(walletId *Id) {

	// check-and-set under the lock so concurrent callers don't both proceed
	enter := false
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		if !vc.isRemovingWallet {
			vc.isRemovingWallet = true
			enter = true
		}
	}()
	if !enter {
		return
	}
	vc.isRemovingWalletChanged(true)

	vc.device.GetApi().RemoveWallet(
		&RemoveWalletArgs{
			WalletId: walletId.IdStr,
		},
		RemoveWalletCallback(connect.NewApiCallback[*RemoveWalletResult](
			func(result *RemoveWalletResult, err error) {

				if err != nil || result == nil || !result.Success {
					vc.setIsRemovingWallet(false)
					return
				}

				// the wallets refetch ends the removing state
				vc.FetchAccountWallets()

			}),
		),
	)
}

func (self *WalletViewController) SetIsPollingAccountWallets(isPolling bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.isPollingAccountWallets = isPolling
}

// when a user creates a circle wallet, we wait for a webhook from circle
// which creates an account_wallet entry
func (self *WalletViewController) runPoller(ctx context.Context) {
	ticker := time.NewTicker(2500 * time.Millisecond)
	defer ticker.Stop()

	defer func() {
		if r := recover(); r != nil {
			deviceLog(self.device).Infof("[wvc]runPoller: recovered from panic: %v", r)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:

			var pollAccountWallets bool
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				pollAccountWallets = self.isPollingAccountWallets
			}()

			if pollAccountWallets {
				self.FetchAccountWallets()
			}
		}
	}
}

func (self *WalletViewController) AddUnpaidByteCountListener(listener UnpaidByteCountListener) Sub {
	callbackId := self.unpaidByteCountListeners.Add(listener)
	return newSub(func() {
		self.unpaidByteCountListeners.Remove(callbackId)
	})
}

func (self *WalletViewController) unpaidByteCountChanged(count ByteCount) {
	for _, listener := range self.unpaidByteCountListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(count)
		})
	}
}

func (self *WalletViewController) GetUnpaidByteCount() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.unpaidByteCount
}

/**
 * Sum of paid and unpaid bytes provided
 **/
func (self *WalletViewController) FetchTransferStats() {

	// check-and-set under the lock so concurrent callers don't both proceed
	var seq int64
	enter := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.isFetchingTransferStats {
			self.isFetchingTransferStats = true
			enter = true
		}
		self.transferStatsNextSeq++
		seq = self.transferStatsNextSeq
	}()
	if !enter {
		return
	}

	self.device.GetApi().GetTransferStats(connect.NewApiCallback[*TransferStatsResult](
		func(results *TransferStatsResult, err error) {

			defer func() {
				self.stateLock.Lock()
				self.isFetchingTransferStats = false
				self.stateLock.Unlock()
			}()

			if err != nil {
				deviceLog(self.device).Infof("[wvc]error fetching transfer stats: %s", err)
				return
			}

			if results == nil {
				return
			}

			applied := false
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.transferStatsAppliedSeq >= seq {
					return
				}
				self.transferStatsAppliedSeq = seq
				self.unpaidByteCount = results.UnpaidBytesProvided
				applied = true
			}()
			if applied {
				self.unpaidByteCountChanged(results.UnpaidBytesProvided)
			}

		}))

}
