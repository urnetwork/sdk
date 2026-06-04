package sdk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
)

type AccountWalletsListener interface {
	AccountWalletsChanged()
}

type PaymentsListener interface {
	PaymentsChanged()
}

type IsCreatingExternalWalletListener interface {
	StateChanged(bool)
}

type IsRemovingWalletListener interface {
	StateChanged(bool)
}

type UnpaidByteCountListener interface {
	StateChanged(ByteCount)
}

type PayoutWalletListener interface {
	PayoutWalletChanged(*Id)
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

type AccountPayment struct {
	PaymentId       *Id       `json:"payment_id"`
	PaymentPlanId   *Id       `json:"payment_plan_id"`
	WalletId        *Id       `json:"wallet_id"`
	NetworkId       *Id       `json:"network_id"`
	PayoutByteCount ByteCount `json:"payout_byte_count"`
	Payout          NanoCents `json:"payout_nano_cents"`
	MinSweepTime    *Time     `json:"min_sweep_time"`
	CreateTime      *Time     `json:"create_time"`

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
	payoutWalletId  *Id
	accountPayments *AccountPaymentsList
	unpaidByteCount ByteCount

	isAddingExternalWallet  bool
	isRemovingWallet        bool
	isPollingAccountWallets bool
	isPollingPayoutWallet   bool
	isFetchingTransferStats bool

	// fetch sequence numbers — newer responses replace older,
	// older responses landing after newer are discarded
	accountWalletsNextSeq    int64
	accountWalletsAppliedSeq int64
	payoutWalletNextSeq      int64
	payoutWalletAppliedSeq   int64
	paymentsNextSeq          int64
	paymentsAppliedSeq       int64
	transferStatsNextSeq     int64
	transferStatsAppliedSeq  int64

	// lifecycle for Start/Stop polling; guarded by lifecycleLock
	lifecycleLock sync.Mutex
	pollerCancel  context.CancelFunc
	pollerDone    chan struct{}

	accountWalletsListeners           *connect.CallbackList[AccountWalletsListener]
	payoutWalletListeners             *connect.CallbackList[PayoutWalletListener]
	paymentsListeners                 *connect.CallbackList[PaymentsListener]
	isCreatingExternalWalletListeners *connect.CallbackList[IsCreatingExternalWalletListener]
	isRemovingWalletListeners         *connect.CallbackList[IsRemovingWalletListener]
	unpaidByteCountListeners          *connect.CallbackList[UnpaidByteCountListener]
}

func newWalletViewController(ctx context.Context, device Device) *WalletViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &WalletViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		wallets:         NewAccountWalletsList(),
		payoutWalletId:  nil,
		accountPayments: NewAccountPaymentsList(),
		unpaidByteCount: 0,

		accountWalletsListeners:           connect.NewCallbackList[AccountWalletsListener](),
		payoutWalletListeners:             connect.NewCallbackList[PayoutWalletListener](),
		paymentsListeners:                 connect.NewCallbackList[PaymentsListener](),
		isCreatingExternalWalletListeners: connect.NewCallbackList[IsCreatingExternalWalletListener](),
		isRemovingWalletListeners:         connect.NewCallbackList[IsRemovingWalletListener](),
		unpaidByteCountListeners:          connect.NewCallbackList[UnpaidByteCountListener](),
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
	go connect.HandleError(vc.FetchPayoutWallet)
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
	glog.Info("[wvc]close")

	vc.Stop()
	vc.cancel()
}

func (vc *WalletViewController) GetNextPayoutDate() string {
	now := time.Now().UTC()
	year := now.Year()
	month := now.Month()
	day := now.Day()

	var nextPayoutDate time.Time

	switch {
	case day < 15:
		nextPayoutDate = time.Date(year, month, 15, 0, 0, 0, 0, time.UTC)
	case month == time.December:
		nextPayoutDate = time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)
	default:
		nextPayoutDate = time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
	}

	return nextPayoutDate.Format("Jan 2")
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
					glog.Infof("[wvc]error validating address %s on %s: %s", address, blockchain, err)
					callback.SendResult(false)
					return
				}

				callback.SendResult(result.Valid)
			}),
	)

}

func (vc *WalletViewController) AddIsCreatingExternalWalletListener(listener IsCreatingExternalWalletListener) Sub {
	callbackId := vc.isCreatingExternalWalletListeners.Add(listener)
	return newSub(func() {
		vc.isCreatingExternalWalletListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) isCreatingExternalWalletChanged(isProcessing bool) {
	for _, listener := range vc.isCreatingExternalWalletListeners.Get() {
		connect.HandleError(func() {
			listener.StateChanged(isProcessing)
		})
	}
}

func (vc *WalletViewController) setIsCreatingExternalWallet(state bool) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.isAddingExternalWallet = state
	}()
	vc.isCreatingExternalWalletChanged(state)
}

func (vc *WalletViewController) AddExternalWallet(address string, blockchain Blockchain) {

	blockchainUpper := strings.ToUpper(blockchain)
	if blockchainUpper != "SOL" && blockchainUpper != "MATIC" {
		glog.Infof("[wvc]invalid blockchain passed: %s", blockchainUpper)
		return
	}

	// check-and-set under the lock so concurrent callers don't both proceed
	enter := false
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		if !vc.isAddingExternalWallet {
			vc.isAddingExternalWallet = true
			enter = true
		}
	}()
	if !enter {
		return
	}
	vc.isCreatingExternalWalletChanged(true)

	args := &CreateAccountWalletArgs{
		Blockchain:       blockchainUpper,
		WalletAddress:    address,
		DefaultTokenType: "USDC",
	}

	vc.device.GetApi().CreateAccountWallet(args, CreateAccountWalletCallback(connect.NewApiCallback[*CreateAccountWalletResult](
		func(result *CreateAccountWalletResult, err error) {

			if err != nil {
				glog.Infof("[wvc]error creating an external wallet: %s", err)
				vc.setIsCreatingExternalWallet(false)
				return
			}

			vc.setIsCreatingExternalWallet(false)
			vc.FetchAccountWallets()
			vc.FetchPayoutWallet()

		})))

}

func (vc *WalletViewController) AddPayoutWalletListener(listener PayoutWalletListener) Sub {
	callbackId := vc.payoutWalletListeners.Add(listener)
	return newSub(func() {
		vc.payoutWalletListeners.Remove(callbackId)
	})
}

func (vc *WalletViewController) payoutWalletIdChanged(id *Id) {
	for _, listener := range vc.payoutWalletListeners.Get() {
		connect.HandleError(func() {
			listener.PayoutWalletChanged(id)
		})
	}
}

func (vc *WalletViewController) UpdatePayoutWallet(walletId *Id) (err error) {

	if walletId == nil {
		err = fmt.Errorf("no wallet id provided")
		return
	}

	args := &SetPayoutWalletArgs{
		WalletId: walletId,
	}

	vc.device.GetApi().SetPayoutWallet(args, SetPayoutWalletCallback(connect.NewApiCallback[*SetPayoutWalletResult](
		func(result *SetPayoutWalletResult, setWalletErr error) {

			if setWalletErr != nil {
				glog.Infof("[wvc]Error setting payout wallet: %s", setWalletErr)
				return
			}

			var id *Id
			func() {
				vc.stateLock.Lock()
				defer vc.stateLock.Unlock()
				vc.payoutWalletId = args.WalletId
				id = vc.payoutWalletId
			}()
			vc.payoutWalletIdChanged(id)

		})))

	return
}

func (vc *WalletViewController) GetPayoutWalletId() (id *Id) {
	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()
	return vc.payoutWalletId
}

func (self *WalletViewController) FetchPayoutWallet() {

	var seq int64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.payoutWalletNextSeq++
		seq = self.payoutWalletNextSeq
	}()

	self.device.GetApi().GetPayoutWallet(GetPayoutWalletCallback(connect.NewApiCallback[*GetPayoutWalletIdResult](
		func(result *GetPayoutWalletIdResult, err error) {

			if err != nil {
				glog.Infof("error fetching payout wallet: %s", err)
				return
			}

			var changed bool
			var newId *Id
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				if self.payoutWalletAppliedSeq >= seq {
					return
				}
				self.payoutWalletAppliedSeq = seq

				prior := self.payoutWalletId
				// stop polling if the result has updated the payout wallet
				if self.isPollingPayoutWallet && (
				// no existing payout wallet, but result exists or
				(prior == nil && result.WalletId != nil) ||
					// existing payout wallet does not equal incoming payout wallet
					(prior != nil && result.WalletId != nil && prior.Cmp(result.WalletId) != 0)) {
					self.isPollingPayoutWallet = false
				}

				self.payoutWalletId = result.WalletId
				newId = result.WalletId
				changed = true
			}()
			if !changed {
				return
			}
			self.payoutWalletIdChanged(newId)

		})))
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
				glog.Infof("[wvc]Error fetching account wallets: %s", err)
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
				glog.Infof("[wvc]fetch payments failed: %s", err)
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

				if err != nil || !result.Success {
					vc.setIsRemovingWallet(false)
				}

				if result.Success {
					vc.FetchAccountWallets()
				}

			}),
		),
	)
}

func (self *WalletViewController) SetIsPollingAccountWallets(isPolling bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.isPollingAccountWallets = isPolling
}

func (self *WalletViewController) SetIsPollingPayoutWallet(isPolling bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.isPollingPayoutWallet = isPolling
}

// when a user creates a circle wallet, we wait for a webhook from circle
// which creates an account_wallet entry
func (self *WalletViewController) runPoller(ctx context.Context) {
	ticker := time.NewTicker(2500 * time.Millisecond)
	defer ticker.Stop()

	defer func() {
		if r := recover(); r != nil {
			glog.Infof("[wvc]runPoller: recovered from panic: %v", r)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:

			var pollAccountWallets, pollPayoutWallet bool
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				pollAccountWallets = self.isPollingAccountWallets
				pollPayoutWallet = self.isPollingPayoutWallet
			}()

			if pollAccountWallets {
				self.FetchAccountWallets()
			}

			if pollPayoutWallet {
				self.FetchPayoutWallet()
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
				glog.Infof("[wvc]error fetching transfer stats: %s", err)
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
