package sdk

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/connect"
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

	wallets                 *AccountWalletsList
	isAddingExternalWallet  bool
	isRemovingWallet        bool
	payoutWalletId          *Id
	accountPayments         *AccountPaymentsList
	isPollingAccountWallets bool
	isPollingPayoutWallet   bool
	isFetchingTransferStats bool
	unpaidByteCount         ByteCount

	stateLock sync.Mutex

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

		wallets:                 NewAccountWalletsList(),
		isAddingExternalWallet:  false,
		isRemovingWallet:        false,
		payoutWalletId:          nil,
		accountPayments:         NewAccountPaymentsList(),
		isPollingAccountWallets: false,
		isPollingPayoutWallet:   false,
		isFetchingTransferStats: false,
		unpaidByteCount:         0,

		accountWalletsListeners:           connect.NewCallbackList[AccountWalletsListener](),
		payoutWalletListeners:             connect.NewCallbackList[PayoutWalletListener](),
		paymentsListeners:                 connect.NewCallbackList[PaymentsListener](),
		isCreatingExternalWalletListeners: connect.NewCallbackList[IsCreatingExternalWalletListener](),
		isRemovingWalletListeners:         connect.NewCallbackList[IsRemovingWalletListener](),
		unpaidByteCountListeners:          connect.NewCallbackList[UnpaidByteCountListener](),
	}
	return vc
}

func (self *WalletViewController) Start() {
	go self.FetchAccountWallets()
	go self.FetchPayoutWallet()
	go self.FetchPayments()
	go self.FetchTransferStats()
	self.pollNewWallets()
}

func (vc *WalletViewController) Stop() {
	// FIXME
}

func (vc *WalletViewController) Close() {
	glog.Info("[wvc]close")

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
				}

				callback.SendResult(result.Valid)
			}),
	)

}

func (vc *WalletViewController) AddIsCreatingExternalWalletListener(listener IsCreatingExternalWalletListener) Sub {
	callbackId := vc.isCreatingExternalWalletListeners.Add(listener)
	return newSub(func() {
		vc.accountWalletsListeners.Remove(callbackId)
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

	vc.stateLock.Lock()
	defer vc.stateLock.Unlock()

	vc.isAddingExternalWallet = state

	vc.isCreatingExternalWalletChanged(vc.isAddingExternalWallet)

}

func (vc *WalletViewController) AddExternalWallet(address string, blockchain Blockchain) {

	if !vc.isAddingExternalWallet {

		blockchainUpper := strings.ToUpper(blockchain)
		if blockchainUpper != "SOL" && blockchainUpper != "MATIC" {
			glog.Infof("[wvc]invalid blockchain passed: %s", blockchainUpper)
			return
		}

		vc.setIsCreatingExternalWallet(true)

		args := &CreateAccountWalletArgs{
			Blockchain:       blockchainUpper,
			WalletAddress:    address,
			DefaultTokenType: "USDC",
		}

		vc.device.GetApi().CreateAccountWallet(args, CreateAccountWalletCallback(connect.NewApiCallback[*CreateAccountWalletResult](
			func(result *CreateAccountWalletResult, err error) {

				if err != nil {
					glog.Infof("[wvc]error creating an external wallet: %s", err)
					// err = createErr
					return
				}

				vc.setIsCreatingExternalWallet(false)
				vc.FetchAccountWallets()
				vc.FetchPayoutWallet()

			})))

	}

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
				glog.Infof("[wvc]Error setting payout wallet: %s", err)
				return
			}

			vc.stateLock.Lock()
			vc.payoutWalletId = args.WalletId
			vc.stateLock.Unlock()
			vc.payoutWalletIdChanged(vc.payoutWalletId)

		})))

	return
}

func (vc *WalletViewController) GetPayoutWalletId() (id *Id) {
	return vc.payoutWalletId
}

func (self *WalletViewController) setPayoutWalletId(id *Id) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.payoutWalletId = id
	}()

	self.payoutWalletIdChanged(id)
}

func (self *WalletViewController) FetchPayoutWallet() {

	self.device.GetApi().GetPayoutWallet(GetPayoutWalletCallback(connect.NewApiCallback[*GetPayoutWalletIdResult](
		func(result *GetPayoutWalletIdResult, err error) {

			if err != nil {
				glog.Infof("error fetching payout wallet: %s", err)
				return
			}

			// if is polling
			// check if existing value matches result
			if self.isPollingPayoutWallet && (
			// no existing payout wallet, but result exists or
			(self.payoutWalletId == nil && result.WalletId != nil) ||
				// existing payout wallet does not equal incoming payout wallet
				(self.payoutWalletId != nil && result.WalletId != nil && self.payoutWalletId.Cmp(result.WalletId) != 0)) {
				self.SetIsPollingPayoutWallet(false)
			}

			self.setPayoutWalletId(result.WalletId)

		})))
}

func (vc *WalletViewController) GetWallets() *AccountWalletsList {
	return vc.wallets
}

func (vc *WalletViewController) FilterWalletsById(idStr string) *AccountWallet {

	id, err := ParseId(idStr)
	if err != nil {
		return nil
	}

	for i := 0; i < vc.wallets.Len(); i++ {

		wallet := vc.wallets.Get(i)

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

func (vc *WalletViewController) setAccountWallets(wallets *AccountWalletsList) {
	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		vc.wallets = wallets
	}()

	vc.accountWalletsChanged()
}

func (self *WalletViewController) FetchAccountWallets() {

	self.device.GetApi().GetAccountWallets(connect.NewApiCallback[*GetAccountWalletsResult](
		func(results *GetAccountWalletsResult, err error) {

			if err != nil {
				glog.Infof("[wvc]Error fetching account wallets: %s", err)
				return
			}

			newWalletsList := NewAccountWalletsList()
			var wallets []*AccountWallet

			// if we are polling for new wallets
			// and the new results length does not match the existing wallets length
			// stop polling
			if self.isPollingAccountWallets && results.Wallets.Len() != self.wallets.Len() {
				self.SetIsPollingAccountWallets(false)
			}

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

			self.setAccountWallets(newWalletsList)

			// we fetch wallets after removing a wallet
			if self.isRemovingWallet {
				self.setIsRemovingWallet(false)
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
	return vc.accountPayments
}

func (vc *WalletViewController) setAccountPayments(payments []*AccountPayment) {

	func() {
		vc.stateLock.Lock()
		defer vc.stateLock.Unlock()
		if len(payments) > 0 {
			vc.accountPayments = NewAccountPaymentsList()
			vc.accountPayments.addAll(payments...)
		}
	}()

	vc.paymentsChanged()

}

func (vc *WalletViewController) FetchPayments() {

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

			vc.setAccountPayments(payouts)

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

	if !vc.isRemovingWallet {
		vc.setIsRemovingWallet(true)

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
}

func (self *WalletViewController) SetIsPollingAccountWallets(isPolling bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isPollingAccountWallets = isPolling
	}()
}

func (self *WalletViewController) SetIsPollingPayoutWallet(isPolling bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isPollingPayoutWallet = isPolling
	}()
}

// when a user creates a circle wallet, we wait for a webhook from circle
// which creates an account_wallet entry
func (self *WalletViewController) pollNewWallets() {

	go func() {
		ticker := time.NewTicker(2500 * time.Millisecond)
		defer ticker.Stop()

		defer func() {
			if r := recover(); r != nil {
				glog.Infof("[wvc]pollNewWallets: recovered from panic: %v", r)
			}
		}()

		for {
			select {
			case <-self.ctx.Done():
				return
			case <-ticker.C:

				if self.isPollingAccountWallets {
					self.FetchAccountWallets()
				}

				if self.isPollingPayoutWallet {
					self.FetchPayoutWallet()
				}
			}
		}
	}()

}

func (self *WalletViewController) setIsFetchingTransferStats(isFetching bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.isFetchingTransferStats = isFetching
	}()
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

func (self *WalletViewController) setUnpaidByteCount(count ByteCount) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.unpaidByteCount = count
	}()

	self.unpaidByteCountChanged(count)
}

func (self *WalletViewController) GetUnpaidByteCount() ByteCount {
	return self.unpaidByteCount
}

/**
 * Sum of paid and unpaid bytes provided
 **/
func (self *WalletViewController) FetchTransferStats() {

	if !self.isFetchingTransferStats {

		self.setIsFetchingTransferStats(true)

		self.device.GetApi().GetTransferStats(connect.NewApiCallback[*TransferStatsResult](
			func(results *TransferStatsResult, err error) {

				if err != nil {
					glog.Infof("[wvc]error fetching transfer stats: %s", err)
					return
				}

				if results != nil {
					self.setUnpaidByteCount(results.UnpaidBytesProvided)
				}

				self.setIsFetchingTransferStats(false)

			}))

	}

}
