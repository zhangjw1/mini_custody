package btc

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
)

const (
	DepositDetected   = "DETECTED"
	DepositConfirming = "CONFIRMING"
	DepositConfirmed  = "CONFIRMED"
	DepositCredited   = "CREDITED"

	SweepCreated          = "CREATED"
	SweepSigning          = "SIGNING"
	SweepSigned           = "SIGNED"
	SweepBroadcastUnknown = "BROADCAST_UNKNOWN"
	SweepBroadcasted      = "BROADCASTED"
	SweepConfirming       = "CONFIRMING"
	SweepCompleted        = "COMPLETED"
	SweepFailed           = "FAILED"
	SweepManualReview     = "MANUAL_REVIEW"

	WithdrawalCreated          = "CREATED"
	WithdrawalInputsLocked     = "INPUTS_LOCKED"
	WithdrawalSigning          = "SIGNING"
	WithdrawalSigned           = "SIGNED"
	WithdrawalBroadcasting     = "BROADCASTING"
	WithdrawalBroadcastUnknown = "BROADCAST_UNKNOWN"
	WithdrawalBroadcasted      = "BROADCASTED"
	WithdrawalConfirming       = "CONFIRMING"
	WithdrawalCompleted        = "COMPLETED"
	WithdrawalFailed           = "FAILED"
	WithdrawalManualReview     = "MANUAL_REVIEW"

	UTXOStatusUnconfirmed = "UNCONFIRMED"
	UTXOStatusUnspent     = "UNSPENT"
	UTXOStatusLocked      = "LOCKED"
	UTXOStatusSpent       = "SPENT"
	UTXOStatusUnknown     = "UNKNOWN"

	AddressPurposeUserDeposit    = "USER_DEPOSIT"
	AddressPurposePlatformChange = "PLATFORM_CHANGE"

	SweepLockBy           = "btc-sweep"
	WithdrawalLockPrefix  = "withdrawal-"
	TargetOutputVout      = 0
	ChangeOutputVout      = 1
	DustThresholdSats     = int64(294)
	MaxSelectionInputs    = 100
	DefaultProcessBatch   = 100
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

// ChainParamsForNetwork 根据数据库网络标识返回对应的 Bitcoin 链参数。
func ChainParamsForNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "bitcoin-signet":
		return &chaincfg.SigNetParams, nil
	case "bitcoin-testnet4":
		return &chaincfg.TestNet3Params, nil
	default:
		return nil, errors.New("Bitcoin 网络无效")
	}
}

// WithdrawalLocker 生成提币独占锁定 UTXO 的租约持有者标识。
func WithdrawalLocker(withdrawalID int64) string {
	return fmt.Sprintf("%s%d", WithdrawalLockPrefix, withdrawalID)
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

// LockedInput 描述持久化在 selected_inputs_json 中的已锁定输入快照。
type LockedInput struct {
	ID           int64  `json:"id"`
	TxID         string `json:"txid"`
	Vout         uint32 `json:"vout"`
	ValueSats    int64  `json:"value_sats"`
	ScriptPubKey string `json:"script_pub_key"`
	BlockHeight  int64  `json:"block_height"`
	Path         string `json:"derivation_path"`
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
