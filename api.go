package sdk

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/urnetwork/connect"
)

// the api is asychronous, which is the most natural for the target platforms

type Api struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientStrategy *connect.ClientStrategy

	apiUrl string

	mutex sync.Mutex
	byJwt string

	httpPostRaw connect.HttpPostRawFunction
	httpGetRaw  connect.HttpGetRawFunction
}

func newApi(
	ctx context.Context,
	clientStrategy *connect.ClientStrategy,
	apiUrl string,
) *Api {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &Api{
		ctx:            cancelCtx,
		cancel:         cancel,
		clientStrategy: clientStrategy,
		apiUrl:         apiUrl,
		httpPostRaw:    nil,
		httpGetRaw:     nil,
	}
}

// this gets attached to api calls that need it
func (self *Api) SetByJwt(byJwt string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.byJwt = byJwt
}

func (self *Api) GetByJwt() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.byJwt
}

func (self *Api) setHttpPostRaw(httpPostRaw connect.HttpPostRawFunction) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.httpPostRaw = httpPostRaw
}

func (self *Api) getHttpPostRaw() connect.HttpPostRawFunction {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpPostRaw != nil {
		return self.httpPostRaw
	}

	return func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		return connect.HttpPostWithStrategyRaw(ctx, self.clientStrategy, requestUrl, requestBodyBytes, byJwt)
	}
}

func (self *Api) setHttpGetRaw(httpGetRaw connect.HttpGetRawFunction) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.httpGetRaw = httpGetRaw
}

func (self *Api) getHttpGetRaw() connect.HttpGetRawFunction {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.httpGetRaw != nil {
		return self.httpGetRaw
	}

	return func(ctx context.Context, requestUrl string, byJwt string) ([]byte, error) {
		return connect.HttpGetWithStrategyRaw(ctx, self.clientStrategy, requestUrl, byJwt)
	}
}

func (self *Api) getHttpPostStreamRaw() connect.HttpPostStreamRawFunction {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return func(ctx context.Context, requestUrl string, body io.Reader, byJwt string) ([]byte, error) {
		return connect.HttpPostStreamWithStrategyRaw(ctx, requestUrl, body, byJwt)
	}
}

func (self *Api) Close() {
	self.cancel()
}

type AuthLoginCallback connect.ApiCallback[*AuthLoginResult]

// `model.AuthLoginArgs`
type AuthLoginArgs struct {
	UserAuth    string          `json:"user_auth,omitempty"`
	AuthJwtType string          `json:"auth_jwt_type,omitempty"`
	AuthJwt     string          `json:"auth_jwt,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type WalletAuthArgs struct {
	PublicKey  string `json:"wallet_address,omitempty"`
	Signature  string `json:"wallet_signature,omitempty"`
	Message    string `json:"wallet_message,omitempty"`
	Blockchain string `json:"blockchain,omitempty"`
}

// `model.AuthLoginResult`
type AuthLoginResult struct {
	UserName    string                  `json:"user_name,omitempty"`
	UserAuth    string                  `json:"user_auth,omitempty"`
	AuthAllowed *StringList             `json:"auth_allowed,omitempty"`
	Error       *AuthLoginResultError   `json:"error,omitempty"`
	Network     *AuthLoginResultNetwork `json:"network,omitempty"`
	WalletAuth  *WalletAuthArgs         `json:"wallet_login,omitempty"`
}

// `model.AuthLoginResultError`
type AuthLoginResultError struct {
	SuggestedUserAuth string `json:"suggested_user_auth,omitempty"`
	Message           string `json:"message"`
}

// `model.AuthLoginResultNetwork`
type AuthLoginResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

func (self *Api) AuthLogin(authLogin *AuthLoginArgs, callback AuthLoginCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/login", self.apiUrl),
			authLogin,
			self.GetByJwt(),
			&AuthLoginResult{},
			callback,
		)
	})
}

type AuthLoginWithPasswordCallback connect.ApiCallback[*AuthLoginWithPasswordResult]

type AuthLoginWithPasswordArgs struct {
	UserAuth         string `json:"user_auth"`
	Password         string `json:"password"`
	VerifyOtpNumeric bool   `json:"verify_otp_numeric,omitempty"`
}

type AuthLoginWithPasswordResult struct {
	VerificationRequired *AuthLoginWithPasswordResultVerification `json:"verification_required,omitempty"`
	Network              *AuthLoginWithPasswordResultNetwork      `json:"network,omitempty"`
	Error                *AuthLoginWithPasswordResultError        `json:"error,omitempty"`
}

type AuthLoginWithPasswordResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type AuthLoginWithPasswordResultNetwork struct {
	ByJwt       string `json:"by_jwt,omitempty"`
	NetworkName string `json:"name,omitempty"`
}

type AuthLoginWithPasswordResultError struct {
	Message string `json:"message"`
}

func (self *Api) AuthLoginWithPassword(authLoginWithPassword *AuthLoginWithPasswordArgs, callback AuthLoginWithPasswordCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/login-with-password", self.apiUrl),
			authLoginWithPassword,
			self.GetByJwt(),
			&AuthLoginWithPasswordResult{},
			callback,
		)
	})
}

type AuthVerifyCallback connect.ApiCallback[*AuthVerifyResult]

type AuthVerifyArgs struct {
	UserAuth   string `json:"user_auth"`
	VerifyCode string `json:"verify_code"`
}

type AuthVerifyResult struct {
	Network *AuthVerifyResultNetwork `json:"network,omitempty"`
	Error   *AuthVerifyResultError   `json:"error,omitempty"`
}

type AuthVerifyResultNetwork struct {
	ByJwt string `json:"by_jwt"`
}

type AuthVerifyResultError struct {
	Message string `json:"message"`
}

func (self *Api) AuthVerify(authVerify *AuthVerifyArgs, callback AuthVerifyCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/verify", self.apiUrl),
			authVerify,
			self.GetByJwt(),
			&AuthVerifyResult{},
			callback,
		)
	})
}

type AuthPasswordResetCallback connect.ApiCallback[*AuthPasswordResetResult]

type AuthPasswordResetArgs struct {
	UserAuth string `json:"user_auth"`
}

type AuthPasswordResetResult struct {
	UserAuth string `json:"user_auth"`
}

func (self *Api) AuthPasswordReset(authPasswordReset *AuthPasswordResetArgs, callback AuthPasswordResetCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/password-reset", self.apiUrl),
			authPasswordReset,
			self.GetByJwt(),
			&AuthPasswordResetResult{},
			callback,
		)
	})
}

type AuthVerifySendCallback connect.ApiCallback[*AuthVerifySendResult]

type AuthVerifySendArgs struct {
	UserAuth   string `json:"user_auth"`
	UseNumeric bool   `json:"use_numeric,omitempty"`
}

type AuthVerifySendResult struct {
	UserAuth string `json:"user_auth"`
}

func (self *Api) AuthVerifySend(authVerifySend *AuthVerifySendArgs, callback AuthVerifySendCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/verify-send", self.apiUrl),
			authVerifySend,
			self.GetByJwt(),
			&AuthVerifySendResult{},
			callback,
		)
	})
}

type NetworkCheckCallback connect.ApiCallback[*NetworkCheckResult]

type NetworkCheckArgs struct {
	NetworkName string `json:"network_name"`
}

type NetworkCheckResult struct {
	Available bool `json:"available"`
}

func (self *Api) NetworkCheck(networkCheck *NetworkCheckArgs, callback NetworkCheckCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/network-check", self.apiUrl),
			networkCheck,
			self.GetByJwt(),
			&NetworkCheckResult{},
			callback,
		)
	})
}

/**
 * Network Create
 */

type NetworkCreateCallback connect.ApiCallback[*NetworkCreateResult]

type NetworkCreateArgs struct {
	UserName         string          `json:"user_name,omitempty"`
	UserAuth         string          `json:"user_auth,omitempty"`
	AuthJwt          string          `json:"auth_jwt,omitempty"`
	AuthJwtType      string          `json:"auth_jwt_type,omitempty"`
	Password         string          `json:"password,omitempty"`
	NetworkName      string          `json:"network_name,omitempty"`
	Terms            bool            `json:"terms"`
	GuestMode        bool            `json:"guest_mode"`
	VerifyOtpNumeric bool            `json:"verify_use_numeric,omitempty"`
	ReferralCode     string          `json:"referral_code,omitempty"`
	WalletAuth       *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type NetworkCreateResult struct {
	Network              *NetworkCreateResultNetwork      `json:"network,omitempty"`
	VerificationRequired *NetworkCreateResultVerification `json:"verification_required,omitempty"`
	Error                *NetworkCreateResultError        `json:"error,omitempty"`
}

type NetworkCreateResultNetwork struct {
	ByJwt       string `json:"by_jwt,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
}

type NetworkCreateResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type NetworkCreateResultError struct {
	Message string `json:"message"`
}

func (self *Api) NetworkCreate(networkCreate *NetworkCreateArgs, callback NetworkCreateCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/network-create", self.apiUrl),
			networkCreate,
			self.GetByJwt(),
			&NetworkCreateResult{},
			callback,
		)
	})
}

/**
 * Delete network
 */

type NetworkDeleteResult struct{}

type NetworkDeleteCallback connect.ApiCallback[*NetworkDeleteResult]

func (self *Api) NetworkDelete(callback NetworkDeleteCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/network-delete", self.apiUrl),
			nil,
			self.GetByJwt(),
			&NetworkDeleteResult{},
			callback,
		)
	})
}

type AuthNetworkClientCallback connect.ApiCallback[*AuthNetworkClientResult]

type AuthNetworkClientArgs struct {
	// FIXME how to bring this back as optional with gomobile. Use a new type *OptionalId?
	// if omitted, a new client_id is created
	// ClientId string `json:"client_id,omitempty"`
	Description string `json:"description"`
	DeviceSpec  string `json:"device_spec"`
}

type AuthNetworkClientResult struct {
	ByClientJwt string                  `json:"by_client_jwt,omitempty"`
	Error       *AuthNetworkClientError `json:"error,omitempty"`
}

type AuthNetworkClientError struct {
	// can be a hard limit or a rate limit
	ClientLimitExceeded bool   `json:"client_limit_exceeded"`
	Message             string `json:"message"`
}

func (self *Api) AuthNetworkClient(authNetworkClient *AuthNetworkClientArgs, callback AuthNetworkClientCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/auth-client", self.apiUrl),
			authNetworkClient,
			self.GetByJwt(),
			&AuthNetworkClientResult{},
			callback,
		)
	})
}

type GetNetworkClientsCallback connect.ApiCallback[*NetworkClientsResult]

type NetworkClientResident struct {
	ClientId              *Id      `json:"client_id"`
	InstanceId            *Id      `json:"instance_id"`
	ResidentId            *Id      `json:"resident_id"`
	ResidentHost          string   `json:"resident_host"`
	ResidentService       string   `json:"resident_service"`
	ResidentBlock         string   `json:"resident_block"`
	ResidentInternalPorts *IntList `json:"resident_internal_ports"`
}

type NetworkClientsResult struct {
	Clients *NetworkClientInfoList `json:"clients"`
}

type NetworkClientInfo struct {
	ClientId    *Id    `json:"client_id"`
	NetworkId   *Id    `json:"network_id"`
	Description string `json:"description"`
	DeviceSpec  string `json:"device_spec"`

	CreateTime *Time `json:"create_time"`
	AuthTime   *Time `json:"auth_time"`

	Resident    *NetworkClientResident       `json:"resident,omitempty"`
	ProvideMode ProvideMode                  `json:"provide_mode"`
	Connections *NetworkClientConnectionList `json:"connections"`
}

type NetworkClientConnection struct {
	ClientId          *Id    `json:"client_id"`
	ConnectionId      *Id    `json:"connection_id"`
	ConnectTime       *Time  `json:"connect_time"`
	DisconnectTime    *Time  `json:"disconnect_time,omitempty"`
	ConnectionHost    string `json:"connection_host"`
	ConnectionService string `json:"connection_service"`
	ConnectionBlock   string `json:"connection_block"`
}

func (self *Api) GetNetworkClients(callback GetNetworkClientsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/clients", self.apiUrl),
			self.GetByJwt(),
			&NetworkClientsResult{},
			callback,
		)
	})
}

type FindLocationsCallback connect.ApiCallback[*FindLocationsResult]

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
}

type FindLocationsResult struct {
	Specs *ProviderSpecList `json:"specs"`
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups *LocationGroupResultList `json:"groups"`
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations *LocationResultList       `json:"locations"`
	Devices   *LocationDeviceResultList `json:"devices"`
}

type LocationResult struct {
	LocationId   *Id          `json:"location_id"`
	LocationType LocationType `json:"location_type"`
	Name         string       `json:"name"`
	// FIXME
	City string `json:"city,omitempty"`
	// FIXME
	Region string `json:"region,omitempty"`
	// FIXME
	Country           string `json:"country,omitempty"`
	CountryCode       string `json:"country_code,omitempty"`
	CityLocationId    *Id    `json:"city_location_id,omitempty"`
	RegionLocationId  *Id    `json:"region_location_id,omitempty"`
	CountryLocationId *Id    `json:"country_location_id,omitempty"`
	ProviderCount     int    `json:"provider_count,omitempty"`
	MatchDistance     int    `json:"match_distance,omitempty"`
	Stable            bool   `json:"stable"`
	StrongPrivacy     bool   `json:"strong_privacy"`
}

type LocationGroupResult struct {
	LocationGroupId *Id    `json:"location_group_id"`
	Name            string `json:"name"`
	ProviderCount   int    `json:"provider_count,omitempty"`
	Promoted        bool   `json:"promoted,omitempty"`
	MatchDistance   int    `json:"match_distance,omitempty"`
}

type LocationDeviceResult struct {
	ClientId   *Id    `json:"client_id"`
	DeviceName string `json:"device_name"`
}

func (self *Api) GetProviderLocations(callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/provider-locations", self.apiUrl),
			self.GetByJwt(),
			&FindLocationsResult{},
			callback,
		)
	})
}

func (self *Api) FindProviderLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/find-provider-locations", self.apiUrl),
			findLocations,
			self.GetByJwt(),
			&FindLocationsResult{},
			callback,
		)
	})
}

func (self *Api) FindLocations(findLocations *FindLocationsArgs, callback FindLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/find-locations", self.apiUrl),
			findLocations,
			self.GetByJwt(),
			&FindLocationsResult{},
			callback,
		)
	})
}

type FindProvidersCallback connect.ApiCallback[*FindProvidersResult]

type FindProvidersArgs struct {
	LocationId       *Id     `json:"location_id,omitempty"`
	LocationGroupId  *Id     `json:"location_group_id,omitempty"`
	Count            int     `json:"count"`
	ExcludeClientIds *IdList `json:"exclude_location_ids,omitempty"`
}

type FindProvidersResult struct {
	ClientIds *IdList `json:"client_ids,omitempty"`
}

func (self *Api) FindProviders(findProviders *FindProvidersArgs, callback FindProvidersCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/find-providers", self.apiUrl),
			findProviders,
			self.GetByJwt(),
			&FindProvidersResult{},
			callback,
		)
	})
}

type ProviderSpec struct {
	LocationId      *Id  `json:"location_id,omitempty"`
	LocationGroupId *Id  `json:"location_group_id,omitempty"`
	ClientId        *Id  `json:"client_id,omitempty"`
	BestAvailable   bool `json:"best_available,omitempty"`
}

func (self *ProviderSpec) toConnectProviderSpec() *connect.ProviderSpec {
	connectProviderSpec := &connect.ProviderSpec{}
	if self.LocationId != nil {
		connectLocationId := self.LocationId.toConnectId()
		connectProviderSpec.LocationId = &connectLocationId
	}
	if self.LocationGroupId != nil {
		connectLocationGroupId := self.LocationGroupId.toConnectId()
		connectProviderSpec.LocationGroupId = &connectLocationGroupId
	}
	if self.ClientId != nil {
		connectClientId := self.ClientId.toConnectId()
		connectProviderSpec.ClientId = &connectClientId
	}
	if self.BestAvailable {
		connectProviderSpec.BestAvailable = true
	}

	return connectProviderSpec
}

type FindProviders2Callback connect.ApiCallback[*FindProviders2Result]

type FindProviders2Args struct {
	Specs            *ProviderSpecList `json:"specs"`
	Count            int               `json:"count"`
	ExcludeClientIds *IdList           `json:"exclude_client_ids"`
}

type FindProviders2Result struct {
	ProviderStats *FindProvidersProviderList `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                *Id `json:"client_id"`
	EstimatedBytesPerSecond int `json:"estimated_bytes_per_second"`
}

func (self *Api) FindProviders2(findProviders2 *FindProviders2Args, callback FindProviders2Callback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/find-providers2", self.apiUrl),
			findProviders2,
			self.GetByJwt(),
			&FindProviders2Result{},
			callback,
		)
	})
}

type WalletCircleInitCallback connect.ApiCallback[*WalletCircleInitResult]

type WalletCircleInitResult struct {
	UserToken   *CircleUserToken       `json:"user_token,omitempty"`
	ChallengeId string                 `json:"challenge_id,omitempty"`
	Error       *WalletCircleInitError `json:"error,omitempty"`
}

type WalletCircleInitError struct {
	Message string `json:"message"`
}

func (self *Api) WalletCircleInit(callback WalletCircleInitCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/wallet/circle-init", self.apiUrl),
			nil,
			self.GetByJwt(),
			&WalletCircleInitResult{},
			callback,
		)
	})
}

type WalletValidateAddressCallback connect.ApiCallback[*WalletValidateAddressResult]

type WalletValidateAddressArgs struct {
	Address string `json:"address,omitempty"`
	Chain   string `json:"chain,omitempty"`
}

type WalletValidateAddressResult struct {
	Valid bool `json:"valid,omitempty"`
}

func (self *Api) WalletValidateAddress(walletValidateAddress *WalletValidateAddressArgs, callback WalletValidateAddressCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/wallet/validate-address", self.apiUrl),
			walletValidateAddress,
			self.GetByJwt(),
			&WalletValidateAddressResult{},
			callback,
		)
	})
}

type WalletType = string

const (
	WalletTypeCircleUserControlled WalletType = "circle_uc"
	WalletTypeXch                  WalletType = "xch"
	WalletTypeSol                  WalletType = "sol"
)

type Blockchain = string

const (
	SOL   Blockchain = "SOL"
	MATIC Blockchain = "MATIC"
)

type CreateAccountWalletArgs struct {
	Blockchain       Blockchain `json:"blockchain"`
	WalletAddress    string     `json:"wallet_address"`
	DefaultTokenType string     `json:"default_token_type"`
}

type CreateAccountWalletResult struct {
	WalletId *Id `json:"wallet_id"`
}

type CreateAccountWalletCallback connect.ApiCallback[*CreateAccountWalletResult]

func (self *Api) CreateAccountWallet(createAccountWallet *CreateAccountWalletArgs, callback CreateAccountWalletCallback) {

	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/account/wallet", self.apiUrl),
			createAccountWallet,
			self.GetByJwt(),
			&CreateAccountWalletResult{},
			callback,
		)
	})
}

/**
 * Set payout wallet
 */

type SetPayoutWalletArgs struct {
	WalletId *Id `json:"wallet_id"`
}

type SetPayoutWalletResult struct{}

type SetPayoutWalletCallback connect.ApiCallback[*SetPayoutWalletResult]

func (self *Api) SetPayoutWallet(payoutWallet *SetPayoutWalletArgs, callback SetPayoutWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/account/payout-wallet", self.apiUrl),
			payoutWallet,
			self.GetByJwt(),
			&SetPayoutWalletResult{},
			callback,
		)
	})
}

type GetAccountWalletsResult struct {
	Wallets *AccountWalletsList `json:"wallets"`
}

type GetAccountWalletsCallback connect.ApiCallback[*GetAccountWalletsResult]

func (self *Api) GetAccountWallets(callback GetAccountWalletsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/wallets", self.apiUrl),
			self.GetByJwt(),
			&GetAccountWalletsResult{},
			callback,
		)
	})
}

type GetPayoutWalletIdResult struct {
	WalletId *Id `json:"wallet_id"`
}

type GetPayoutWalletCallback connect.ApiCallback[*GetPayoutWalletIdResult]

func (self *Api) GetPayoutWallet(callback GetPayoutWalletCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/payout-wallet", self.apiUrl),
			self.GetByJwt(),
			&GetPayoutWalletIdResult{},
			callback,
		)
	})
}

type CircleUserToken struct {
	UserToken     string `json:"user_token"`
	EncryptionKey string `json:"encryption_key"`
}

type CircleWalletInfo struct {
	WalletId             string    `json:"wallet_id"`
	TokenId              string    `json:"token_id"`
	Blockchain           string    `json:"blockchain"`
	BlockchainSymbol     string    `json:"blockchain_symbol"`
	CreateDate           string    `json:"create_date"`
	BalanceUsdcNanoCents NanoCents `json:"balance_usdc_nano_cents"`
}

type WalletBalanceCallback connect.ApiCallback[*WalletBalanceResult]

type WalletBalanceResult struct {
	WalletInfo *CircleWalletInfo `json:"wallet_info,omitempty"`
}

func (self *Api) WalletBalance(callback WalletBalanceCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/wallet/balance", self.apiUrl),
			self.GetByJwt(),
			&WalletBalanceResult{},
			callback,
		)
	})
}

type GetAccountPaymentsCallback connect.ApiCallback[*GetNetworkAccountPaymentsResult]

type GetNetworkAccountPaymentsError struct {
	Message string `json:"message"`
}

type GetNetworkAccountPaymentsResult struct {
	AccountPayments *AccountPaymentsList            `json:"account_payments,omitempty"`
	Error           *GetNetworkAccountPaymentsError `json:"error,omitempty"`
}

func (self *Api) GetAccountPayments(callback GetAccountPaymentsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/payments", self.apiUrl),
			self.byJwt,
			&GetNetworkAccountPaymentsResult{},
			callback,
		)
	})
}

type WalletCircleTransferOutCallback connect.ApiCallback[*WalletCircleTransferOutResult]

type WalletCircleTransferOutArgs struct {
	ToAddress           string    `json:"to_address"`
	AmountUsdcNanoCents NanoCents `json:"amount_usdc_nano_cents"`
	Terms               bool      `json:"terms"`
}

type WalletCircleTransferOutResult struct {
	UserToken   *CircleUserToken              `json:"user_token,omitempty"`
	ChallengeId string                        `json:"challenge_id,omitempty"`
	Error       *WalletCircleTransferOutError `json:"error,omitempty"`
}

type WalletCircleTransferOutError struct {
	Message string `json:"message"`
}

func (self *Api) WalletCircleTransferOut(walletCircleTransferOut *WalletCircleTransferOutArgs, callback WalletCircleTransferOutCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/wallet/circle-transfer-out", self.apiUrl),
			walletCircleTransferOut,
			self.GetByJwt(),
			&WalletCircleTransferOutResult{},
			callback,
		)
	})
}

type Subscription struct {
	SubscriptionId *Id    `json:"subscription_id"`
	Store          string `json:"store"`
	Plan           string `json:"plan"`
}

type TransferBalance struct {
	BalanceId             *Id       `json:"balance_id"`
	NetworkId             *Id       `json:"network_id"`
	StartTime             string    `json:"start_time"`
	EndTime               string    `json:"end_time"`
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	// how much money the platform made after subtracting fees
	NetRevenue       NanoCents `json:"net_revenue"`
	BalanceByteCount ByteCount `json:"balance_byte_count"`
}

type SubscriptionBalanceCallback connect.ApiCallback[*SubscriptionBalanceResult]

type SubscriptionBalanceResult struct {
	/*
	 * StartBalanceByteCount - The available balance the user starts the day with
	 */
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	/**
	 * BalanceByteCount - The remaining balance the user has available
	 */
	BalanceByteCount ByteCount `json:"balance_byte_count"`
	/**
	 * OpenTransferByteCount - The total number of bytes tied up in open transfers
	 */
	OpenTransferByteCount     ByteCount            `json:"open_transfer_byte_count"`
	CurrentSubscription       *Subscription        `json:"current_subscription,omitempty"`
	ActiveTransferBalances    *TransferBalanceList `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents NanoCents            `json:"pending_payout_usd_nano_cents"`
	UpdateTime                string               `json:"update_time"`
}

func (self *Api) SubscriptionBalance(callback SubscriptionBalanceCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/subscription/balance", self.apiUrl),
			self.GetByJwt(),
			&SubscriptionBalanceResult{},
			callback,
		)
	})
}

/**
 * Create subscription payment id
 */

type SubscriptionCreatePaymentIdCallback connect.ApiCallback[*SubscriptionCreatePaymentIdResult]

type SubscriptionCreatePaymentIdArgs struct {
}

type SubscriptionCreatePaymentIdResult struct {
	SubscriptionPaymentId *Id                               `json:"subscription_payment_id,omitempty"`
	Error                 *SubscriptionCreatePaymentIdError `json:"error,omitempty"`
}

type SubscriptionCreatePaymentIdError struct {
	Message string `json:"message"`
}

func (self *Api) SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, callback SubscriptionCreatePaymentIdCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/subscription/create-payment-id", self.apiUrl),
			createPaymentId,
			self.GetByJwt(),
			&SubscriptionCreatePaymentIdResult{},
			callback,
		)
	})
}

/**
 * Get network user
 */

type GetNetworkUserError struct {
	Message string `json:"message"`
}

type GetNetworkUserResult struct {
	NetworkUser *NetworkUser         `json:"network_user,omitempty"`
	Error       *GetNetworkUserError `json:"error,omitempty"`
}

type GetNetworkUserCallback connect.ApiCallback[*GetNetworkUserResult]

func (self *Api) GetNetworkUser(callback GetNetworkUserCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/user", self.apiUrl),
			self.GetByJwt(),
			&GetNetworkUserResult{},
			callback,
		)
	})
}

/**
 * Update Network User
 **/

type NetworkUserUpdateArgs struct {
	NetworkName string `json:"network_name"`
}

type NetworkUserUpdateError struct {
	Message string `json:"message"`
}

type NetworkUserUpdateResult struct {
	Error *NetworkUserUpdateError `json:"error,omitempty"`
}

type NetworkUserUpdateCallback connect.ApiCallback[*NetworkUserUpdateResult]

func (self *Api) NetworkUserUpdate(updateNetworkUser *NetworkUserUpdateArgs, callback NetworkUserUpdateCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/user/update", self.apiUrl),
			updateNetworkUser,
			self.GetByJwt(),
			&NetworkUserUpdateResult{},
			callback,
		)
	})
}

/**
 * Get network referral code
 */

type GetNetworkReferralCodeResult struct {
	ReferralCode   string                       `json:"referral_code,omitempty"`
	TotalReferrals int                          `json:"total_referrals"`
	Error          *GetNetworkReferralCodeError `json:"error,omitempty"`
}

type GetNetworkReferralCodeError struct {
	Message string `json:"message"`
}

type GetNetworkReferralCodeCallback connect.ApiCallback[*GetNetworkReferralCodeResult]

func (self *Api) GetNetworkReferralCode(
	callback GetNetworkReferralCodeCallback,
) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/referral-code", self.apiUrl),
			self.GetByJwt(),
			&GetNetworkReferralCodeResult{},
			callback,
		)
	})
}

/**
 * Validate referral code
 */

type ValidateReferralCodeArgs struct {
	ReferralCode string `json:"referral_code"`
}

type ValidateReferralCodeResult struct {
	IsValid  bool `json:"is_valid"`
	IsCapped bool `json:"is_capped"`
}

type ValidateReferralCodeCallback connect.ApiCallback[*ValidateReferralCodeResult]

func (self *Api) ValidateReferralCode(
	validateReferralCode *ValidateReferralCodeArgs,
	callback ValidateReferralCodeCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/referral-code/validate", self.apiUrl),
			validateReferralCode,
			self.GetByJwt(),
			&ValidateReferralCodeResult{},
			callback,
		)
	})
}

/**
 * Update network referral
 * Users can edit their referral code to associate with a different parent network.
 */

type SetNetworkReferralArgs struct {
	ReferralCode string `json:"referral_code"`
}

type SetNetworkReferralError struct {
	Message string `json:"message"`
}

type SetNetworkReferralResult struct {
	Error *SetNetworkReferralError `json:"error,omitempty"`
}

type SetNetworkReferralCallback connect.ApiCallback[*SetNetworkReferralResult]

func (self *Api) SetNetworkReferral(
	args *SetNetworkReferralArgs,
	callback SetNetworkReferralCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/account/set-referral", self.apiUrl),
			args,
			self.GetByJwt(),
			&SetNetworkReferralResult{},
			callback,
		)
	})
}

/**
 * Get referral network
 * This returns the parent referral network of the current network.
 */

type ReferralNetwork struct {
	Id   *Id    `json:"id"`
	Name string `json:"name"`
}

type GetReferralNetworkError struct {
	Message string `json:"message"`
}

type GetReferralNetworkResult struct {
	Network *ReferralNetwork         `json:"network,omitempty"`
	Error   *GetReferralNetworkError `json:"error,omitempty"`
}

type GetReferralNetworkCallback connect.ApiCallback[*GetReferralNetworkResult]

func (self *Api) GetReferralNetwork(callback GetReferralNetworkCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/referral-network", self.apiUrl),
			self.GetByJwt(),
			&GetReferralNetworkResult{},
			callback,
		)
	})
}

/**
 * Unlink parent referral network from account
 */
type UnlinkReferralNetworkResult struct {
}

type UnlinkReferralNetworkCallback connect.ApiCallback[*UnlinkReferralNetworkResult]

func (self *Api) UnlinkReferralNetwork(
	callback UnlinkReferralNetworkCallback,
) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/unlink-referral-network", self.apiUrl),
			self.GetByJwt(),
			&UnlinkReferralNetworkResult{},
			callback,
		)
	})
}

/**
 * Remove wallet
 */

type RemoveWalletError struct {
	Message string `json:"message"`
}

type RemoveWalletResult struct {
	Success bool               `json:"success"`
	Error   *RemoveWalletError `json:"error,omitempty"`
}

type RemoveWalletArgs struct {
	WalletId string `json:"wallet_id"`
}

type RemoveWalletCallback connect.ApiCallback[*RemoveWalletResult]

func (self *Api) RemoveWallet(
	removeWallet *RemoveWalletArgs,
	callback RemoveWalletCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/account/wallets/remove", self.apiUrl),
			removeWallet,
			self.GetByJwt(),
			&RemoveWalletResult{},
			callback,
		)
	})
}

/**
 * Send feedback
 */

type FeedbackSendArgs struct {
	Needs     *FeedbackSendNeeds `json:"needs"`
	StarCount int                `json:"star_count"`
}

type FeedbackSendNeeds struct {
	Other string `json:"other"`
}

type FeedbackSendResult struct {
	FeedbackId *Id `json:"feedback_id"`
}

type SendFeedbackCallback connect.ApiCallback[*FeedbackSendResult]

func (self *Api) SendFeedback(
	sendFeedback *FeedbackSendArgs,
	callback SendFeedbackCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/feedback/send-feedback", self.apiUrl),
			sendFeedback,
			self.GetByJwt(),
			&FeedbackSendResult{},
			callback,
		)
	})
}

/**
 * Update Account Preferences
 */

type AccountPreferencesSetArgs struct {
	ProductUpdates bool `json:"product_updates"`
}

type AccountPreferencesSetResult struct{}

type AccountPreferencesSetCallback connect.ApiCallback[*AccountPreferencesSetResult]

func (self *Api) AccountPreferencesUpdate(
	accountPreferences *AccountPreferencesSetArgs,
	callback AccountPreferencesSetCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/preferences/set-preferences", self.apiUrl),
			accountPreferences,
			self.GetByJwt(),
			&AccountPreferencesSetResult{},
			callback,
		)
	})
}

/**
 * Fetch Account Preferences
 **/

type AccountPreferencesGetResult struct {
	ProductUpdates bool `json:"product_updates,omitempty"`
}

type AccountPreferencesGetCallback connect.ApiCallback[*AccountPreferencesGetResult]

func (self *Api) AccountPreferencesGet(callback AccountPreferencesGetCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/preferences", self.apiUrl),
			self.GetByJwt(),
			&AccountPreferencesGetResult{},
			callback,
		)
	})
}

/**
 * Fetch Provider Transfer Stats
 **/

type TransferStatsResult struct {
	PaidBytesProvided   ByteCount `json:"paid_bytes_provided"`
	UnpaidBytesProvided ByteCount `json:"unpaid_bytes_provided"`
}

type GetTransferStatsCallback connect.ApiCallback[*TransferStatsResult]

func (self *Api) GetTransferStats(callback GetTransferStatsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/transfer/stats", self.apiUrl),
			self.GetByJwt(),
			&TransferStatsResult{},
			callback,
		)
	})
}

/**
 * Auth Code Login
 * https://docs.ur.io/api/api-reference/auth/code-login
 */

type AuthCodeLoginArgs struct {
	AuthCode string `json:"auth_code"`
}

type AuthCodeLoginError struct {
	Message string `json:"message"`
}

type AuthCodeLoginResult struct {
	Jwt   string              `json:"by_jwt"`
	Error *AuthCodeLoginError `json:"error,omitempty"`
}

type AuthCodeLoginCallback connect.ApiCallback[*AuthCodeLoginResult]

func (self *Api) AuthCodeLogin(
	codeLoginArgs *AuthCodeLoginArgs,
	callback AuthCodeLoginCallback,
) (*AuthCodeLoginResult, error) {
	return connect.HttpPostWithRawFunction(
		self.ctx,
		self.getHttpPostRaw(),
		fmt.Sprintf("%s/auth/code-login", self.apiUrl),
		codeLoginArgs,
		self.GetByJwt(),
		&AuthCodeLoginResult{},
		callback,
	)
}

/**
 * Auth code create
 */

type AuthCodeCreateArgs struct {
	DurationMinutes float64 `json:"duration_minutes,omitempty"`
	Uses            int     `json:"uses,omitempty"`
}

type AuthCodeCreateResult struct {
	AuthCode        string               `json:"auth_code,omitempty"`
	DurationMinutes float64              `json:"duration_minutes,omitempty"`
	Uses            int                  `json:"uses,omitempty"`
	Error           *AuthCodeCreateError `json:"error,omitempty"`
}

type AuthCodeCreateError struct {
	AuthCodeLimitExceeded bool   `json:"auth_code_limit_exceeded,omitempty"`
	Message               string `json:"message,omitempty"`
}

type AuthCodeCreateCallback connect.ApiCallback[*AuthCodeCreateResult]

func (self *Api) AuthCodeCreate(
	codeCreateArgs *AuthCodeCreateArgs,
	callback AuthCodeCreateCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/code-create", self.apiUrl),
			codeCreateArgs,
			self.GetByJwt(),
			&AuthCodeCreateResult{},
			callback,
		)
	})
}

/**
 * Guest upgrade to brand new network
 */

type UpgradeGuestCallback connect.ApiCallback[*UpgradeGuestResult]

type UpgradeGuestArgs struct {
	UserAuth    string          `json:"user_auth,omitempty"`
	AuthJwt     string          `json:"auth_jwt,omitempty"`
	AuthJwtType string          `json:"auth_jwt_type,omitempty"`
	Password    string          `json:"password,omitempty"`
	NetworkName string          `json:"network_name,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type UpgradeGuestNetwork struct {
	ByJwt string `json:"by_jwt,omitempty"`
}

type UpgradeGuestResult struct {
	Network              *UpgradeGuestNetwork            `json:"network,omitempty"`
	VerificationRequired *UpgradeGuestResultVerification `json:"verification_required,omitempty"`
	Error                *UpgradeGuesteResultError       `json:"error,omitempty"`
}

type UpgradeGuestResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuesteResultError struct {
	Message string `json:"message"`
}

func (self *Api) UpgradeGuest(upgradeGuest *UpgradeGuestArgs, callback UpgradeGuestCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/upgrade-guest", self.apiUrl),
			upgradeGuest,
			self.GetByJwt(),
			&UpgradeGuestResult{},
			callback,
		)
	})
}

/**
 * Merge guest account with existing account
 */

type UpgradeGuestExistingCallback connect.ApiCallback[*UpgradeGuestExistingResult]

type UpgradeGuestExistingArgs struct {
	UserAuth    string          `json:"user_auth,omitempty"`
	Password    string          `json:"password,omitempty"`
	AuthJwt     string          `json:"auth_jwt,omitempty"`
	AuthJwtType string          `json:"auth_jwt_type,omitempty"`
	WalletAuth  *WalletAuthArgs `json:"wallet_auth,omitempty"`
}

type UpgradeGuestExistingNetwork struct {
	ByJwt string `json:"by_jwt,omitempty"`
}

type UpgradeGuestExistingResultVerification struct {
	UserAuth string `json:"user_auth"`
}

type UpgradeGuesteExistingResultError struct {
	Message string `json:"message"`
}

type UpgradeGuestExistingResult struct {
	Network              *UpgradeGuestExistingNetwork            `json:"network,omitempty"`
	VerificationRequired *UpgradeGuestExistingResultVerification `json:"verification_required,omitempty"`
	Error                *UpgradeGuesteExistingResultError       `json:"error,omitempty"`
}

func (self *Api) UpgradeGuestExisting(upgradeGuest *UpgradeGuestExistingArgs, callback UpgradeGuestExistingCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/auth/upgrade-guest-existing", self.apiUrl),
			upgradeGuest,
			self.GetByJwt(),
			&UpgradeGuestExistingResult{},
			callback,
		)
	})
}

/**
 * Verify Seeker Token Holder
 */

type VerifySeekerNftHolderArgs struct {
	PublicKey string `json:"wallet_address,omitempty"`
	Signature string `json:"wallet_signature,omitempty"`
	Message   string `json:"wallet_message,omitempty"`
}

type VerifySeekerNftHolderError struct {
	Message string `json:"message"`
}

type VerifySeekerNftHolderResult struct {
	Success bool                        `json:"success"`
	Error   *VerifySeekerNftHolderError `json:"error,omitempty"`
}

type VerifySeekerNftHolderCallback connect.ApiCallback[*VerifySeekerNftHolderResult]

func (self *Api) VerifySeekerHolder(verify *VerifySeekerNftHolderArgs, callback VerifySeekerNftHolderCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/account/wallets/verify-seeker", self.apiUrl),
			verify,
			self.GetByJwt(),
			&VerifySeekerNftHolderResult{},
			callback,
		)
	})
}

/**
 * Leaderboard
 */

type GetLeaderboardArgs struct{}

type LeaderboardEarner struct {
	NetworkId         string  `json:"network_id"`
	NetworkName       string  `json:"network_name"`
	NetMiBCount       float32 `json:"net_mib_count"`
	IsPublic          bool    `json:"is_public"`
	ContainsProfanity bool    `json:"contains_profanity"`
}

type LeaderboardResult struct {
	Earners *LeaderboardEarnersList `json:"earners"`
	Error   *LeaderboardError       `json:"error,omitempty"`
}

type LeaderboardError struct {
	Message string `json:"message"`
}

type GetLeaderboardCallback connect.ApiCallback[*LeaderboardResult]

func (self *Api) GetLeaderboard(args *GetLeaderboardArgs, callback GetLeaderboardCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/stats/leaderboard", self.apiUrl),
			args,
			self.GetByJwt(),
			&LeaderboardResult{},
			callback,
		)
	})
}

/**
 * Get Network Leaderboard ranking
 */

type NetworkRanking struct {
	NetMiBCount       float32 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

type GetNetworkRankingResult struct {
	NetworkRanking *NetworkRanking         `json:"network_ranking"`
	Error          *GetNetworkRankingError `json:"error,omitempty"`
}
type GetNetworkRankingError struct {
	Message string `json:"message"`
}

type GetNetworkLeaderboardRankingCallback connect.ApiCallback[*GetNetworkRankingResult]

func (self *Api) GetNetworkLeaderboardRanking(callback GetNetworkLeaderboardRankingCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/ranking", self.apiUrl),
			self.GetByJwt(),
			&GetNetworkRankingResult{},
			callback,
		)
	})
}

/**
 * Set Network Leaderboard public
 */

type SetNetworkRankingPublicArgs struct {
	IsPublic bool `json:"is_public"`
}

type SetNetworkRankingPublicResult struct {
	Error *SetNetworkRankingPublicError `json:"error,omitempty"`
}

type SetNetworkRankingPublicError struct {
	Message string `json:"message"`
}

type SetNetworkLeaderboardPublicCallback connect.ApiCallback[*SetNetworkRankingPublicResult]

func (self *Api) SetNetworkLeaderboardPublic(args *SetNetworkRankingPublicArgs, callback SetNetworkLeaderboardPublicCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/ranking-visibility", self.apiUrl),
			args,
			self.GetByJwt(),
			&SetNetworkRankingPublicResult{},
			callback,
		)
	})
}

/**
 * Network points
 * Eventually these will be transferrable to token
 */

type AccountPoint struct {
	NetworkId        *Id        `json:"network_id"`
	Event            string     `json:"event"`
	PointValue       NanoPoints `json:"point_value"`
	AccountPaymentId *Id        `json:"account_payment_id"`
	PaymentPlanId    *Id        `json:"payment_plan_id,omitempty"`
	LinkedNetworkId  *Id        `json:"linked_network_id,omitempty"`
	CreateTime       *Time      `json:"create_time"`
}

type AccountPointsResult struct {
	AccountPoints *AccountPointsList `json:"account_points"`
}

type GetAccountPointsCallback connect.ApiCallback[*AccountPointsResult]

func (self *Api) GetAccountPoints(callback GetAccountPointsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/account/points", self.apiUrl),
			self.GetByJwt(),
			&AccountPointsResult{},
			callback,
		)
	})
}

/**
 * Network block location
 */

type NetworkBlockLocationArgs struct {
	LocationId *Id `json:"location_id"`
}

type NetworkBlockLocationResult struct {
	Error *NetworkBlockLocationError `json:"error,omitempty"`
}

type NetworkBlockLocationError struct {
	Message string `json:"message"`
}

type NetworkBlockLocationCallback connect.ApiCallback[*NetworkBlockLocationResult]

func (self *Api) NetworkBlockLocation(args *NetworkBlockLocationArgs, callback NetworkBlockLocationCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/block-location", self.apiUrl),
			args,
			self.GetByJwt(),
			&NetworkBlockLocationResult{},
			callback,
		)
	})
}

/**
 * Network unblock location
 */

type NetworkUnblockLocationArgs struct {
	LocationId *Id `json:"location_id"`
}

type NetworkUnblockLocationResult struct {
	Error *NetworkUnblockLocationError `json:"error,omitempty"`
}

type NetworkUnblockLocationError struct {
	Message string `json:"message"`
}

type NetworkUnblockLocationCallback connect.ApiCallback[*NetworkUnblockLocationResult]

func (self *Api) NetworkUnblockLocation(args *NetworkUnblockLocationArgs, callback NetworkUnblockLocationCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/network/unblock-location", self.apiUrl),
			args,
			self.GetByJwt(),
			&NetworkUnblockLocationResult{},
			callback,
		)
	})
}

/**
 * Fetch network blocked locations
 */

type GetNetworkBlockedLocationsResult struct {
	BlockedLocations *BlockedLocationsList `json:"blocked_locations"`
}

type GetNetworkBlockedLocationsCallback connect.ApiCallback[*GetNetworkBlockedLocationsResult]

func (self *Api) GetNetworkBlockedLocations(callback GetNetworkBlockedLocationsCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/blocked-locations", self.apiUrl),
			self.GetByJwt(),
			&GetNetworkBlockedLocationsResult{},
			callback,
		)
	})
}

/**
 * Network reliability
 */

type ReliabilityWindow struct {
	MeanReliabilityWeight float64 `json:"mean_reliability_weight"`
	MinTimeUnixMilli      int64   `json:"min_time_unix_milli"`
	MinBucketNumber       int64   `json:"min_bucket_number"`
	MaxTimeUnixMilli      int64   `json:"max_time_unix_milli"`
	// exclusive
	MaxBucketNumber       int64 `json:"max_bucket_number"`
	BucketDurationSeconds int   `json:"bucket_duration_seconds"`

	MaxClientCount int `json:"max_client_count"`
	// valid+invalid
	MaxTotalClientCount int `json:"max_total_client_count"`

	ReliabilityWeights *Float64List           `json:"reliability_weights"`
	ClientCounts       *IntList               `json:"client_counts"`
	TotalClientCounts  *IntList               `json:"total_client_counts"`
	CountryMultipliers *CountryMultiplierList `json:"country_multipliers"`
}

type GetNetworkReliabilityError struct {
	Message string `json:"message"`
}

type GetNetworkReliabilityResult struct {
	ReliabilityWindow *ReliabilityWindow          `json:"reliability_window,omitempty"`
	Error             *GetNetworkReliabilityError `json:"error,omitempty"`
}

type GetNetworkReliabilityCallback connect.ApiCallback[*GetNetworkReliabilityResult]

func (self *Api) GetNetworkReliability(callback GetNetworkReliabilityCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/network/reliability", self.apiUrl),
			self.GetByJwt(),
			&GetNetworkReliabilityResult{},
			callback,
		)
	})
}

/**
 * Solana Payment Intents
 */

type SolanaPaymentIntentArgs struct {
	Reference string `json:"reference"`
}

type SolanaPaymentIntentResult struct {
	Error *SolanaPaymentIntentError `json:"error,omitempty"`
}

type SolanaPaymentIntentError struct {
	Message string `json:"message"`
}

type SolanaPaymentIntentCallback connect.ApiCallback[*SolanaPaymentIntentResult]

func (self *Api) CreateSolanaPaymentIntent(args *SolanaPaymentIntentArgs, callback SolanaPaymentIntentCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/solana/payment-intent", self.apiUrl),
			args,
			self.GetByJwt(),
			&SolanaPaymentIntentResult{},
			callback,
		)
	})
}

/**
 * Stripe Payment Intent
 */

type StripeCreatePaymentIntentArgs struct{}

type StripeCreatePaymentIntentErr struct {
	Message string `json:"message"`
}

type StripeCreatePaymentIntentResult struct {
	PaymentIntents *StripePaymentIntentList      `json:"payment_intents,omitempty"`
	EphemeralKey   string                        `json:"ephemeral_key,omitempty"`
	CustomerId     string                        `json:"customer_id,omitempty"`
	PublishableKey string                        `json:"publishable_key,omitempty"`
	Error          *StripeCreatePaymentIntentErr `json:"error,omitempty"`
}

type StripePaymentIntentCallback connect.ApiCallback[*StripeCreatePaymentIntentResult]

func (self *Api) CreateStripePaymentIntent(args *StripeCreatePaymentIntentArgs, callback StripePaymentIntentCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/stripe/payment-intent", self.apiUrl),
			args,
			self.GetByJwt(),
			&StripeCreatePaymentIntentResult{},
			callback,
		)
	})
}

/**
 * Stripe Customer Portal
 */

type StripeCreateCustomerPortalArgs struct{}

type StripeCreateCustomerPortalError struct {
	Message string `json:"message"`
}

type StripeCreateCustomerPortalResult struct {
	Url   string                           `json:"url,omitempty"`
	Error *StripeCreateCustomerPortalError `json:"error,omitempty"`
}

type StripeCreateCustomerPortalCallback connect.ApiCallback[*StripeCreateCustomerPortalResult]

func (self *Api) StripeCreateCustomerPortal(args *StripeCreateCustomerPortalArgs, callback StripeCreateCustomerPortalCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/stripe/customer-portal", self.apiUrl),
			args,
			self.GetByJwt(),
			&StripeCreateCustomerPortalResult{},
			callback,
		)
	})
}

/**
 * Upload logs
 */

type UploadLogsError struct {
	Message string `json:"message"`
}

type UploadLogsResult struct {
	Error *UploadLogsError `json:"error,omitempty"`
}

type UploadLogsCallback connect.ApiCallback[*UploadLogsResult]

func (self *Api) uploadLogs(
	feedbackId string,
	body io.Reader,
	callback UploadLogsCallback,
) {
	go connect.HandleError(func() {
		connect.HttpPostWithStreamFunction(
			self.ctx,
			self.getHttpPostStreamRaw(),
			fmt.Sprintf("%s/log/%s/upload", self.apiUrl, feedbackId),
			body,
			self.GetByJwt(),
			&UploadLogsResult{},
			callback,
		)
	})
}

/**
 * Refresh JWT token
 */

type RefreshJwtResultError struct {
	Message string `json:"message"`
}

type RefreshJwtResult struct {
	ByJwt string                 `json:"by_jwt,omitempty"`
	Error *RefreshJwtResultError `json:"error,omitempty"`
}

type RefreshJwtCallback connect.ApiCallback[*RefreshJwtResult]

func (self *Api) RefreshJwt(callback RefreshJwtCallback) {
	go connect.HandleError(func() {
		connect.HttpGetWithRawFunction(
			self.ctx,
			self.getHttpGetRaw(),
			fmt.Sprintf("%s/auth/refresh", self.apiUrl),
			self.GetByJwt(),
			&RefreshJwtResult{},
			callback,
		)
	})
}

func (self *Api) RefreshJwtSync() (*RefreshJwtResult, error) {
	return connect.HttpGetWithRawFunction(
		self.ctx,
		self.getHttpGetRaw(),
		fmt.Sprintf("%s/auth/refresh", self.apiUrl),
		self.GetByJwt(),
		&RefreshJwtResult{},
		connect.NewNoopApiCallback[*RefreshJwtResult](),
	)
}

/**
 * redeem balance code
 */

type RedeemBalanceCodeArgs struct {
	Secret string `json:"secret"`
}

type RedeemBalanceCodeResult struct {
	TransferBalance *RedeemBalanceCodeTransferBalance `json:"transfer_balance,omitempty"`
	Error           *RedeemBalanceCodeError           `json:"error,omitempty"`
}

type RedeemBalanceCodeTransferBalance struct {
	TransferBalanceId *Id       `json:"transfer_balance_id"`
	StartTime         *Time     `json:"start_time"`
	EndTime           *Time     `json:"end_time"`
	BalanceByteCount  ByteCount `json:"balance_byte_count"`
}

type RedeemBalanceCodeError struct {
	Message string `json:"message"`
}

type RedeemBalanceCodeCallback connect.ApiCallback[*RedeemBalanceCodeResult]

func (self *Api) RedeemBalanceCode(args *RedeemBalanceCodeArgs, callback RedeemBalanceCodeCallback) {
	go connect.HandleError(func() {
		connect.HttpPostWithRawFunction(
			self.ctx,
			self.getHttpPostRaw(),
			fmt.Sprintf("%s/subscription/redeem-balance-code", self.apiUrl),
			args,
			self.GetByJwt(),
			&RedeemBalanceCodeResult{},
			callback,
		)
	})
}
