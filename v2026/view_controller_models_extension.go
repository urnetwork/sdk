//go:build ios_extension

package sdk

// These data models also appear in API responses and gomobile list wrappers,
// so the extension build keeps their wire shape while omitting the UI
// controller implementations that ordinarily own them.

type NetworkUser struct {
	UserId        *Id         `json:"userId"`
	UserName      string      `json:"user_name"`
	UserAuth      string      `json:"user_auth,omitempty"`
	Verified      bool        `json:"verified"`
	AuthType      string      `json:"auth_type"`
	NetworkName   string      `json:"network_name"`
	WalletAddress string      `json:"wallet_address,omitempty"`
	AuthTypes     *StringList `json:"auth_types,omitempty"`
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
	Canceled     bool  `json:"canceled"`
	CancelTime   *Time `json:"cancel_time,omitempty"`
}

type ProviderState = string

const (
	ProviderStateInEvaluation     ProviderState = "InEvaluation"
	ProviderStateEvaluationFailed ProviderState = "EvaluationFailed"
	ProviderStateNotAdded         ProviderState = "NotAdded"
	ProviderStateAdded            ProviderState = "Added"
	ProviderStateRemoved          ProviderState = "Removed"
)

type ProviderGridPoint struct {
	X        int32
	Y        int32
	ClientId *Id
	State    ProviderState
	EndTime  *Time
	Active   bool
}

type ThroughputSample struct {
	EgressByteCount    ByteCount
	IngressByteCount   ByteCount
	EgressPacketCount  int64
	IngressPacketCount int64
	EgressBitRate      int
	IngressBitRate     int
}

type ThroughputPoint struct {
	Time   int64
	Remote *ThroughputSample
	Local  *ThroughputSample
	Block  *ThroughputSample
}
