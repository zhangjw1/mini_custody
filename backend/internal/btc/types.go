package btc

import "errors"

const (
	DepositDetected         = "DETECTED"
	DepositConfirming       = "CONFIRMING"
	DepositConfirmed        = "CONFIRMED"
	DepositCredited         = "CREDITED"
	SweepCreated            = "CREATED"
	SweepSigning            = "SIGNING"
	SweepSigned             = "SIGNED"
	SweepBroadcasted        = "BROADCASTED"
	SweepConfirming         = "CONFIRMING"
	SweepCompleted          = "COMPLETED"
	SweepFailed             = "FAILED"
	DustThresholdSats int64 = 294
)

var Network = "bitcoin-signet"

// ConfigureNetwork 设置单网络部署使用的 Bitcoin 数据库网络。
func ConfigureNetwork(network string) error {
	if network != "bitcoin-signet" && network != "bitcoin-testnet4" {
		return errors.New("Bitcoin 网络无效")
	}
	Network = network
	return nil
}

var ErrInsufficientSweepValue = errors.New("UTXO 金额不足以支付归集费用")
var ErrSweepDust = errors.New("归集输出低于 Bitcoin dust threshold")

type Address struct {
	ID           int64
	UserID       int64
	Address      string
	ScriptPubKey []byte
	Path         string
}
type DepositObservation struct {
	UserID, AddressID int64
	TxID              string
	Vout              uint32
	BlockHash         string
	BlockHeight       int64
	AmountSats        int64
	ScriptPubKey      []byte
}
type ConfirmingDeposit struct {
	ID, BlockHeight int64
	BlockHash       string
}
type DepositRecord struct {
	ID, UserID, BlockHeight, AmountSats, Confirmations int64
	TxID, BlockHash, Status                            string
	Vout                                               uint32
}
type Withdrawal struct {
	ID             int64  `json:"id"`
	UserID         int64  `json:"user_id"`
	AmountSats     int64  `json:"amount_sats"`
	FeeRateSatVB   int64  `json:"fee_rate_sat_vb"`
	IdempotencyKey string `json:"-"`
	ToAddress      string `json:"to_address"`
	Status         string `json:"status"`
	RawTx          []byte `json:"-"`
	TxID           string `json:"txid,omitempty"`
}
type UTXO struct {
	ID           int64
	AddressID    int64
	TxID         string
	Vout         uint32
	ValueSats    int64
	ScriptPubKey []byte
	BlockHeight  int64
}
type Sweep struct {
	ID                                                     int64
	UTXO                                                   UTXO
	From                                                   Address
	To                                                     Address
	InputValueSats, OutputValueSats, FeeSats, FeeRateSatVB int64
	RawTx                                                  []byte
	TxID                                                   string
	Status                                                 string
}
