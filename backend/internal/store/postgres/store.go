package postgres

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
)

type Store struct {
	pool     *pgxpool.Pool
	timezone *time.Location
}

// NewStore 创建 PostgreSQL 数据访问对象。
func NewStore(pool *pgxpool.Pool, timezone *time.Location) (*Store, error) {
	if pool == nil {
		return nil, errors.New("必须提供数据库连接池")
	}
	if timezone == nil {
		return nil, errors.New("必须提供数据访问层时区")
	}
	return &Store{pool: pool, timezone: timezone}, nil
}

type rowScanner interface {
	// Scan 将当前查询行写入目标变量。
	Scan(dest ...any) error
}

// localTime 将数据库时间转换为业务时区。
func (s *Store) localTime(value time.Time) time.Time {
	return value.In(s.timezone)
}

// parseDatabaseWei 将数据库无符号金额文本解析为大整数。
func parseDatabaseWei(value, field string) (*big.Int, error) {
	parsed, err := amount.ParseWei(value)
	if err != nil {
		return nil, fmt.Errorf("数据库字段 %s 的金额无效：%w", field, err)
	}
	return parsed, nil
}

// parseSignedDatabaseWei 将数据库有符号金额文本解析为大整数。
func parseSignedDatabaseWei(value, field string) (*big.Int, error) {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("数据库字段 %s 的金额无效", field)
	}
	return parsed, nil
}

// optionalWei 将可空金额文本转换为大整数指针。
func optionalWei(value pgtype.Text, field string) (*big.Int, error) {
	if !value.Valid {
		return nil, nil
	}
	return parseDatabaseWei(value.String, field)
}

// optionalInt64 将可空 int8 转换为 int64 指针。
func optionalInt64(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

// optionalString 将可空文本转换为空字符串或实际值。
func optionalString(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

// scanUser 从查询结果读取用户模型。
func (s *Store) scanUser(row rowScanner) (User, error) {
	var item User
	if err := row.Scan(&item.ID, &item.Code, &item.DisplayName, &item.CreatedAt); err != nil {
		return User{}, err
	}
	item.CreatedAt = s.localTime(item.CreatedAt)
	return item, nil
}

// scanWalletAddress 从查询结果读取钱包地址模型。
func (s *Store) scanWalletAddress(row rowScanner) (WalletAddress, error) {
	var item WalletAddress
	var derivationIndex int64
	var nextNonce string
	if err := row.Scan(
		&item.ID, &item.UserID, &item.Network, &item.Address, &derivationIndex,
		&item.DerivationPath, &nextNonce, &item.CreatedAt,
	); err != nil {
		return WalletAddress{}, err
	}
	if derivationIndex < 0 || derivationIndex > int64(^uint32(0)) {
		return WalletAddress{}, errors.New("数据库中的派生索引无效")
	}
	nonce, err := parseDatabaseWei(nextNonce, "next_nonce")
	if err != nil {
		return WalletAddress{}, err
	}
	item.DerivationIndex = uint32(derivationIndex)
	item.NextNonce = nonce
	item.CreatedAt = s.localTime(item.CreatedAt)
	return item, nil
}

// scanBalance 从查询结果读取用户资产余额模型。
func (s *Store) scanBalance(row rowScanner) (AssetBalance, error) {
	var item AssetBalance
	var available, pendingDeposit, pendingWithdrawal string
	if err := row.Scan(
		&item.ID, &item.UserID, &item.AssetID, &item.Asset, &available, &pendingDeposit,
		&pendingWithdrawal, &item.Version, &item.UpdatedAt,
	); err != nil {
		return AssetBalance{}, err
	}
	var err error
	if item.AvailableWei, err = parseDatabaseWei(available, "available_wei"); err != nil {
		return AssetBalance{}, err
	}
	if item.PendingDepositWei, err = parseDatabaseWei(pendingDeposit, "pending_deposit_wei"); err != nil {
		return AssetBalance{}, err
	}
	if item.PendingWithdrawalWei, err = parseDatabaseWei(pendingWithdrawal, "pending_withdrawal_wei"); err != nil {
		return AssetBalance{}, err
	}
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanBalanceEntry 从查询结果读取余额流水模型。
func (s *Store) scanBalanceEntry(row rowScanner) (BalanceEntry, error) {
	var item BalanceEntry
	var value string
	if err := row.Scan(
		&item.ID, &item.UserID, &item.AssetID, &item.Asset, &item.EntryType, &value,
		&item.ReferenceType, &item.ReferenceID, &item.CreatedAt,
	); err != nil {
		return BalanceEntry{}, err
	}
	amountWei, err := parseSignedDatabaseWei(value, "balance entry amount_wei")
	if err != nil {
		return BalanceEntry{}, err
	}
	item.AmountWei = amountWei
	item.CreatedAt = s.localTime(item.CreatedAt)
	return item, nil
}

// scanDeposit 从查询结果读取充值模型。
func (s *Store) scanDeposit(row rowScanner) (Deposit, error) {
	var item Deposit
	var value string
	if err := row.Scan(
		&item.ID, &item.UserID, &item.AddressID, &item.Network, &item.Asset,
		&item.TxHash, &item.TxIndex, &item.BlockNumber, &item.BlockHash, &value,
		&item.Confirmations, &item.Status, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return Deposit{}, err
	}
	amountWei, err := parseDatabaseWei(value, "deposit amount_wei")
	if err != nil {
		return Deposit{}, err
	}
	item.AmountWei = amountWei
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanWithdrawal 从查询结果读取提币模型。
func (s *Store) scanWithdrawal(row rowScanner) (Withdrawal, error) {
	var item Withdrawal
	var amountValue, reservedFeeValue string
	var actualFee, nonce, maxFee, maxPriorityFee pgtype.Text
	var gasLimit, blockNumber pgtype.Int8
	var txHash, errorCode, errorMessage pgtype.Text
	if err := row.Scan(
		&item.ID, &item.IdempotencyKey, &item.UserID, &item.AddressID, &item.ToAddress,
		&amountValue, &reservedFeeValue, &actualFee, &nonce, &gasLimit, &maxFee,
		&maxPriorityFee, &item.RawTx, &txHash, &blockNumber, &item.Confirmations,
		&item.Status, &errorCode, &errorMessage, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return Withdrawal{}, err
	}
	var err error
	if item.AmountWei, err = parseDatabaseWei(amountValue, "withdrawal amount_wei"); err != nil {
		return Withdrawal{}, err
	}
	if item.ReservedFeeWei, err = parseDatabaseWei(reservedFeeValue, "withdrawal reserved_fee_wei"); err != nil {
		return Withdrawal{}, err
	}
	if item.ActualFeeWei, err = optionalWei(actualFee, "withdrawal actual_fee_wei"); err != nil {
		return Withdrawal{}, err
	}
	if item.Nonce, err = optionalWei(nonce, "withdrawal nonce"); err != nil {
		return Withdrawal{}, err
	}
	if item.MaxFeePerGasWei, err = optionalWei(maxFee, "withdrawal max_fee_per_gas_wei"); err != nil {
		return Withdrawal{}, err
	}
	if item.MaxPriorityFeePerGasWei, err = optionalWei(maxPriorityFee, "withdrawal max_priority_fee_per_gas_wei"); err != nil {
		return Withdrawal{}, err
	}
	item.GasLimit = optionalInt64(gasLimit)
	item.BlockNumber = optionalInt64(blockNumber)
	item.TxHash = optionalString(txHash)
	item.ErrorCode = optionalString(errorCode)
	item.ErrorMessage = optionalString(errorMessage)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanCheckpoint 从查询结果读取链扫描检查点模型。
func (s *Store) scanCheckpoint(row rowScanner) (ChainCheckpoint, error) {
	var item ChainCheckpoint
	if err := row.Scan(
		&item.ID, &item.Network, &item.Scanner, &item.LastScannedBlock,
		&item.LastScannedHash, &item.UpdatedAt,
	); err != nil {
		return ChainCheckpoint{}, err
	}
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}

// scanWorkerError 从查询结果读取后台 Worker 错误模型。
func (s *Store) scanWorkerError(row rowScanner) (WorkerError, error) {
	var item WorkerError
	var referenceType pgtype.Text
	var referenceID pgtype.Int8
	if err := row.Scan(
		&item.ID, &item.Worker, &item.Stage, &referenceType, &referenceID,
		&item.ErrorCode, &item.ErrorMessage, &item.RetryCount,
		&item.FirstOccurredAt, &item.LastOccurredAt,
	); err != nil {
		return WorkerError{}, err
	}
	item.ReferenceType = optionalString(referenceType)
	item.ReferenceID = optionalInt64(referenceID)
	item.FirstOccurredAt = s.localTime(item.FirstOccurredAt)
	item.LastOccurredAt = s.localTime(item.LastOccurredAt)
	return item, nil
}

// scanTransactionRecord 从充值和提币联合查询中读取统一交易模型。
func (s *Store) scanTransactionRecord(row rowScanner) (TransactionRecord, error) {
	var item TransactionRecord
	var amountValue string
	var blockNumber pgtype.Int8
	if err := row.Scan(
		&item.Type, &item.ID, &item.UserID, &item.Asset, &item.Decimals, &item.TxHash,
		&amountValue, &blockNumber, &item.Confirmations, &item.Status,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return TransactionRecord{}, err
	}
	amountWei, err := parseDatabaseWei(amountValue, "transaction amount_wei")
	if err != nil {
		return TransactionRecord{}, err
	}
	item.AmountWei = amountWei
	item.BlockNumber = optionalInt64(blockNumber)
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	return item, nil
}
