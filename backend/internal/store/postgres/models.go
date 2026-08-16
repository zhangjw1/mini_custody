package postgres

import (
	"errors"
	"math/big"
	"time"
)

const (
	NetworkSepolia  = "ethereum-sepolia"
	AssetETH        = "ETH"
	AssetTypeNative = "NATIVE"
	AssetTypeERC20  = "ERC20"
	PlatformRoleHot = "HOT"
)

const (
	DepositDetected   = "DETECTED"
	DepositConfirming = "CONFIRMING"
	DepositConfirmed  = "CONFIRMED"
	DepositCredited   = "CREDITED"
)

const (
	TokenSweepCreated     = "CREATED"
	TokenSweepWaitingGas  = "WAITING_GAS"
	TokenSweepSigning     = "SIGNING"
	TokenSweepSigned      = "SIGNED"
	TokenSweepBroadcasted = "BROADCASTED"
	TokenSweepConfirming  = "CONFIRMING"
	TokenSweepCompleted   = "COMPLETED"
	TokenSweepFailed      = "FAILED"
)

const (
	InternalTransferGasTopup = "GAS_TOPUP"
	InternalTransferCreated  = "CREATED"
	InternalTransferSigning  = "SIGNING"
	InternalTransferSigned   = "SIGNED"
	InternalTransferSent     = "BROADCASTED"
	InternalTransferChecking = "CONFIRMING"
	InternalTransferDone     = "COMPLETED"
	InternalTransferFailed   = "FAILED"
)

const (
	WithdrawalCreated          = "CREATED"
	WithdrawalSigning          = "SIGNING"
	WithdrawalSigned           = "SIGNED"
	WithdrawalBroadcasting     = "BROADCASTING"
	WithdrawalBroadcastUnknown = "BROADCAST_UNKNOWN"
	WithdrawalBroadcasted      = "BROADCASTED"
	WithdrawalConfirming       = "CONFIRMING"
	WithdrawalCompleted        = "COMPLETED"
	WithdrawalFailed           = "FAILED"
)

var (
	ErrNotFound             = errors.New("未找到记录")
	ErrInsufficientBalance  = errors.New("可用余额不足")
	ErrIdempotencyConflict  = errors.New("幂等标识对应的请求内容不一致")
	ErrCheckpointConflict   = errors.New("扫描检查点不能后退或变更同高度区块哈希")
	ErrInvalidState         = errors.New("状态迁移无效")
	ErrUnsafeRelease        = errors.New("当前提币状态不能安全释放余额")
	ErrWalletKeyMismatch    = errors.New("数据库钱包地址与托管根密钥不匹配")
	ErrPendingBalance       = errors.New("待处理余额小于待结算金额")
	ErrActualFeeExceedsHold = errors.New("实际网络费超过预留网络费")
	ErrAssetConfigMismatch  = errors.New("数据库资产配置与启动配置不一致")
)

type User struct {
	ID          int64
	Code        string
	DisplayName string
	CreatedAt   time.Time
}

type WalletAddress struct {
	ID              int64
	UserID          int64
	Network         string
	Address         string
	DerivationIndex uint32
	DerivationPath  string
	NextNonce       *big.Int
	CreatedAt       time.Time
}

type Asset struct {
	ID              int64
	Network         string
	AssetType       string
	Symbol          string
	ContractAddress string
	Decimals        uint8
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type AssetBalance struct {
	ID                   int64
	UserID               int64
	AssetID              int64
	Asset                string
	AvailableWei         *big.Int
	PendingDepositWei    *big.Int
	PendingWithdrawalWei *big.Int
	Version              int64
	UpdatedAt            time.Time
}

type BalanceEntry struct {
	ID            int64
	UserID        int64
	AssetID       int64
	Asset         string
	EntryType     string
	AmountWei     *big.Int
	ReferenceType string
	ReferenceID   int64
	CreatedAt     time.Time
}

type Deposit struct {
	ID            int64
	UserID        int64
	AddressID     int64
	Network       string
	Asset         string
	TxHash        string
	TxIndex       int32
	BlockNumber   int64
	BlockHash     string
	AmountWei     *big.Int
	Confirmations int64
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Withdrawal struct {
	ID                      int64
	IdempotencyKey          string
	UserID                  int64
	AddressID               int64
	ToAddress               string
	AmountWei               *big.Int
	ReservedFeeWei          *big.Int
	ActualFeeWei            *big.Int
	Nonce                   *big.Int
	GasLimit                *int64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
	BlockNumber             *int64
	Confirmations           int64
	Status                  string
	ErrorCode               string
	ErrorMessage            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type ChainCheckpoint struct {
	ID               int64
	Network          string
	Scanner          string
	LastScannedBlock int64
	LastScannedHash  string
	UpdatedAt        time.Time
}

type WorkerError struct {
	ID              int64
	Worker          string
	Stage           string
	ReferenceType   string
	ReferenceID     *int64
	ErrorCode       string
	ErrorMessage    string
	RetryCount      int32
	FirstOccurredAt time.Time
	LastOccurredAt  time.Time
}

type PlatformWallet struct {
	ID             int64
	Network        string
	Role           string
	Address        string
	DerivationPath string
	NextNonce      *big.Int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type TokenDeposit struct {
	ID            int64
	UserID        int64
	AddressID     int64
	AssetID       int64
	TxHash        string
	LogIndex      int32
	BlockNumber   int64
	BlockHash     string
	FromAddress   string
	ToAddress     string
	AmountUnits   *big.Int
	Confirmations int64
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type TokenSweep struct {
	ID                      int64
	UserID                  int64
	AddressID               int64
	AssetID                 int64
	TriggerDepositID        int64
	RecognizedAmountUnits   *big.Int
	SweepAmountUnits        *big.Int
	GasTopupTransferID      *int64
	Nonce                   *big.Int
	GasLimit                *int64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
	BlockNumber             *int64
	Confirmations           int64
	ActualFeeWei            *big.Int
	Status                  string
	ErrorCode               string
	ErrorMessage            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type InternalTransfer struct {
	ID                      int64
	PlatformWalletID        int64
	SweepID                 int64
	TransferType            string
	FromAddress             string
	ToAddress               string
	AmountWei               *big.Int
	Nonce                   *big.Int
	GasLimit                *int64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
	BlockNumber             *int64
	Confirmations           int64
	ActualFeeWei            *big.Int
	Status                  string
	ErrorCode               string
	ErrorMessage            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type TokenWithdrawal struct {
	ID                      int64
	IdempotencyKey          string
	UserID                  int64
	AssetID                 int64
	PlatformWalletID        int64
	ToAddress               string
	AmountUnits             *big.Int
	Nonce                   *big.Int
	GasLimit                *int64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
	BlockNumber             *int64
	Confirmations           int64
	ActualFeeWei            *big.Int
	Status                  string
	ErrorCode               string
	ErrorMessage            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type TransactionRecord struct {
	Type          string
	ID            int64
	UserID        int64
	Asset         string
	Decimals      uint8
	TxHash        string
	AmountWei     *big.Int
	BlockNumber   *int64
	Confirmations int64
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
