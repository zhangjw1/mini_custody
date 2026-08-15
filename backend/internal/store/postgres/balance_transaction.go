package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
)

var transactionHashPattern = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

type DepositObservation struct {
	UserID      int64
	AddressID   int64
	TxHash      string
	TxIndex     int32
	BlockNumber int64
	BlockHash   string
	AmountWei   *big.Int
	Checkpoint  ChainCheckpoint
}

type WithdrawalRequest struct {
	IdempotencyKey string
	UserID         int64
	AddressID      int64
	ToAddress      string
	AmountWei      *big.Int
	ReservedFeeWei *big.Int
}

type WithdrawalSettlement struct {
	WithdrawalID  int64
	ActualFeeWei  *big.Int
	Success       bool
	BlockNumber   int64
	Confirmations int64
}

// RecordDepositAndCheckpoint 兼容单笔调用，在同一事务中记录充值并推进扫描点。
func (s *Store) RecordDepositAndCheckpoint(ctx context.Context, observation DepositObservation) (Deposit, bool, error) {
	if observation.Checkpoint.Network == "" {
		observation.Checkpoint.Network = NetworkSepolia
	}
	if observation.Checkpoint.LastScannedBlock < observation.BlockNumber {
		return Deposit{}, false, errors.New("扫描检查点落后于已发现充值的区块")
	}
	items, created, err := s.RecordDepositsAndCheckpoint(ctx, []DepositObservation{observation}, observation.Checkpoint)
	if err != nil {
		return Deposit{}, false, err
	}
	return items[0], created == 1, nil
}

// RecordDepositsAndCheckpoint 原子记录完整区块内的充值并在最后推进扫描检查点。
func (s *Store) RecordDepositsAndCheckpoint(
	ctx context.Context,
	observations []DepositObservation,
	checkpoint ChainCheckpoint,
) ([]Deposit, int, error) {
	checkpoint.Network = strings.TrimSpace(checkpoint.Network)
	if checkpoint.Network == "" {
		checkpoint.Network = NetworkSepolia
	}
	checkpoint.Scanner = strings.TrimSpace(checkpoint.Scanner)
	checkpoint.LastScannedHash = strings.ToLower(strings.TrimSpace(checkpoint.LastScannedHash))
	if checkpoint.Network != NetworkSepolia || checkpoint.Scanner == "" || checkpoint.LastScannedBlock < 0 ||
		!transactionHashPattern.MatchString(checkpoint.LastScannedHash) {
		return nil, 0, errors.New("扫描检查点参数无效")
	}
	for index := range observations {
		if err := validateDepositObservation(observations[index]); err != nil {
			return nil, 0, err
		}
		observations[index].TxHash = strings.ToLower(strings.TrimSpace(observations[index].TxHash))
		observations[index].BlockHash = strings.ToLower(strings.TrimSpace(observations[index].BlockHash))
		if observations[index].BlockNumber != checkpoint.LastScannedBlock ||
			observations[index].BlockHash != checkpoint.LastScannedHash {
			return nil, 0, errors.New("充值记录与扫描检查点不属于同一区块")
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("开启充值事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	items := make([]Deposit, 0, len(observations))
	createdCount := 0
	for _, observation := range observations {
		item, created, err := s.recordDepositInTx(ctx, tx, observation)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
		if created {
			createdCount++
		}
	}
	if err := advanceCheckpoint(ctx, tx, checkpoint); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("提交区块充值事务失败：%w", err)
	}
	return items, createdCount, nil
}

// recordDepositInTx 在既有事务中幂等写入一笔充值及其待确认余额流水。
func (s *Store) recordDepositInTx(ctx context.Context, tx pgx.Tx, observation DepositObservation) (Deposit, bool, error) {
	if err := ensureWalletOwnership(ctx, tx, observation.UserID, observation.AddressID); err != nil {
		return Deposit{}, false, err
	}
	var depositID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO deposits (
			user_id, address_id, network, asset, tx_hash, tx_index,
			block_number, block_hash, amount_wei, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10)
		ON CONFLICT (network, tx_hash, address_id) DO NOTHING
		RETURNING id`,
		observation.UserID, observation.AddressID, NetworkSepolia, AssetETH,
		observation.TxHash, observation.TxIndex, observation.BlockNumber,
		observation.BlockHash, observation.AmountWei.String(), DepositDetected,
	).Scan(&depositID)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Deposit{}, false, fmt.Errorf("写入充值记录失败：%w", err)
	}

	if created {
		if _, err := balanceForUpdate(ctx, s, tx, observation.UserID); err != nil {
			return Deposit{}, false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE asset_balances SET
				pending_deposit_wei = pending_deposit_wei + $2::numeric,
				version = version + 1,
				updated_at = CURRENT_TIMESTAMP
			WHERE user_id = $1 AND asset = $3`,
			observation.UserID, observation.AmountWei.String(), AssetETH,
		); err != nil {
			return Deposit{}, false, fmt.Errorf("增加待确认充值余额失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO balance_entries (
				user_id, asset, entry_type, amount_wei, reference_type, reference_id
			) VALUES ($1, $2, 'DEPOSIT_PENDING', $3::numeric, 'DEPOSIT', $4)`,
			observation.UserID, AssetETH, observation.AmountWei.String(), depositID,
		); err != nil {
			return Deposit{}, false, fmt.Errorf("写入待确认充值流水失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deposits SET status = $2, updated_at = CURRENT_TIMESTAMP WHERE id = $1`,
			depositID, DepositConfirming,
		); err != nil {
			return Deposit{}, false, fmt.Errorf("更新充值为确认中失败：%w", err)
		}
	} else {
		existing, err := s.scanDeposit(tx.QueryRow(ctx, `
			SELECT `+depositColumns+`
			FROM deposits
			WHERE network = $1 AND tx_hash = $2 AND address_id = $3`,
			NetworkSepolia, observation.TxHash, observation.AddressID,
		))
		if err != nil {
			return Deposit{}, false, fmt.Errorf("读取重复充值记录失败：%w", err)
		}
		if existing.UserID != observation.UserID || existing.TxIndex != observation.TxIndex ||
			existing.BlockNumber != observation.BlockNumber || existing.BlockHash != observation.BlockHash ||
			existing.AmountWei.Cmp(observation.AmountWei) != 0 {
			return Deposit{}, false, ErrIdempotencyConflict
		}
		depositID = existing.ID
	}

	item, err := s.scanDeposit(tx.QueryRow(ctx, `SELECT `+depositColumns+` FROM deposits WHERE id = $1`, depositID))
	if err != nil {
		return Deposit{}, false, fmt.Errorf("读取已记录充值失败：%w", err)
	}
	return item, created, nil
}

// UpdateDepositConfirmations 单调更新充值确认数和确认中状态。
func (s *Store) UpdateDepositConfirmations(ctx context.Context, depositID, confirmations int64) (Deposit, error) {
	if confirmations < 0 {
		return Deposit{}, errors.New("充值确认数不能为负数")
	}
	item, err := s.scanDeposit(s.pool.QueryRow(ctx, `
		UPDATE deposits SET
			confirmations = GREATEST(confirmations, $2),
			status = CASE WHEN status = $3 THEN $4 ELSE status END,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1
		RETURNING `+depositColumns,
		depositID, confirmations, DepositDetected, DepositConfirming,
	))
	return item, mapNotFound(err)
}

// CreditDeposit 将达到确认要求的充值从待确认余额转入可用余额。
func (s *Store) CreditDeposit(ctx context.Context, depositID, confirmations int64) (Deposit, bool, error) {
	if confirmations <= 0 {
		return Deposit{}, false, errors.New("充值确认数必须大于零")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deposit{}, false, fmt.Errorf("开启充值入账事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	item, err := s.scanDeposit(tx.QueryRow(ctx,
		`SELECT `+depositColumns+` FROM deposits WHERE id = $1 FOR UPDATE`, depositID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deposit{}, false, ErrNotFound
	}
	if err != nil {
		return Deposit{}, false, fmt.Errorf("锁定充值记录失败：%w", err)
	}
	if item.Status == DepositCredited {
		if err := tx.Commit(ctx); err != nil {
			return Deposit{}, false, fmt.Errorf("提交已入账充值查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != DepositConfirming && item.Status != DepositConfirmed {
		return Deposit{}, false, ErrInvalidState
	}

	if _, err := tx.Exec(ctx, `
		UPDATE deposits SET status = $2, confirmations = GREATEST(confirmations, $3), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`, depositID, DepositConfirmed, confirmations,
	); err != nil {
		return Deposit{}, false, fmt.Errorf("确认充值记录失败：%w", err)
	}
	balance, err := balanceForUpdate(ctx, s, tx, item.UserID)
	if err != nil {
		return Deposit{}, false, err
	}
	if balance.PendingDepositWei.Cmp(item.AmountWei) < 0 {
		return Deposit{}, false, ErrPendingBalance
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			pending_deposit_wei = pending_deposit_wei - $2::numeric,
			available_wei = available_wei + $2::numeric,
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset = $3`,
		item.UserID, item.AmountWei.String(), AssetETH,
	); err != nil {
		return Deposit{}, false, fmt.Errorf("充值金额转入可用余额失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, 'DEPOSIT_CREDIT', $3::numeric, 'DEPOSIT', $4)`,
		item.UserID, AssetETH, item.AmountWei.String(), item.ID,
	); err != nil {
		return Deposit{}, false, fmt.Errorf("写入充值入账流水失败：%w", err)
	}
	item, err = s.scanDeposit(tx.QueryRow(ctx, `
		UPDATE deposits SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1
		RETURNING `+depositColumns, depositID, DepositCredited,
	))
	if err != nil {
		return Deposit{}, false, fmt.Errorf("更新充值为已入账失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Deposit{}, false, fmt.Errorf("提交充值入账事务失败：%w", err)
	}
	return item, true, nil
}

// ReserveWithdrawal 幂等创建提币并原子占用金额和预估网络费。
func (s *Store) ReserveWithdrawal(ctx context.Context, request WithdrawalRequest) (Withdrawal, bool, error) {
	if err := validateWithdrawalRequest(request); err != nil {
		return Withdrawal{}, false, err
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.ToAddress = strings.ToLower(common.HexToAddress(request.ToAddress).Hex())
	total := new(big.Int).Add(request.AmountWei, request.ReservedFeeWei)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币占用事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := ensureWalletOwnership(ctx, tx, request.UserID, request.AddressID); err != nil {
		return Withdrawal{}, false, err
	}
	balance, err := balanceForUpdate(ctx, s, tx, request.UserID)
	if err != nil {
		return Withdrawal{}, false, err
	}

	var withdrawalID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO withdrawals (
			idempotency_key, user_id, address_id, to_address,
			amount_wei, reserved_fee_wei, status
		) VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING id`,
		request.IdempotencyKey, request.UserID, request.AddressID, request.ToAddress,
		request.AmountWei.String(), request.ReservedFeeWei.String(), WithdrawalCreated,
	).Scan(&withdrawalID)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, fmt.Errorf("写入提币记录失败：%w", err)
	}
	if !created {
		existing, err := s.scanWithdrawal(tx.QueryRow(ctx, `
			SELECT `+withdrawalColumns+`
			FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`,
			request.UserID, request.IdempotencyKey,
		))
		if err != nil {
			return Withdrawal{}, false, fmt.Errorf("读取幂等提币记录失败：%w", err)
		}
		if existing.AddressID != request.AddressID || existing.ToAddress != request.ToAddress ||
			existing.AmountWei.Cmp(request.AmountWei) != 0 || existing.ReservedFeeWei.Cmp(request.ReservedFeeWei) != 0 {
			return Withdrawal{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交幂等提币查询事务失败：%w", err)
		}
		return existing, false, nil
	}
	if balance.AvailableWei.Cmp(total) < 0 {
		return Withdrawal{}, false, ErrInsufficientBalance
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			available_wei = available_wei - $2::numeric,
			pending_withdrawal_wei = pending_withdrawal_wei + $2::numeric,
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset = $3`, request.UserID, total.String(), AssetETH,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("占用提币余额失败：%w", err)
	}
	reservedEntry := new(big.Int).Neg(new(big.Int).Set(total))
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, 'WITHDRAW_RESERVE', $3::numeric, 'WITHDRAWAL', $4)`,
		request.UserID, AssetETH, reservedEntry.String(), withdrawalID,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("写入提币占用流水失败：%w", err)
	}
	item, err := s.scanWithdrawal(tx.QueryRow(ctx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1`, withdrawalID))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("读取已占用余额的提币失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币占用事务失败：%w", err)
	}
	return item, true, nil
}

// TransitionWithdrawal 按允许的状态机迁移提币状态。
func (s *Store) TransitionWithdrawal(ctx context.Context, withdrawalID int64, target string) (Withdrawal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("开启提币状态迁移事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, fmt.Errorf("锁定提币记录失败：%w", err)
	}
	if !allowedWithdrawalTransition(item.Status, target) {
		return Withdrawal{}, ErrInvalidState
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns, withdrawalID, target,
	))
	if err != nil {
		return Withdrawal{}, fmt.Errorf("迁移提币状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, fmt.Errorf("提交提币状态迁移事务失败：%w", err)
	}
	return item, nil
}

// ReleaseWithdrawal 对明确未广播的失败提币释放占用余额。
func (s *Store) ReleaseWithdrawal(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (Withdrawal, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币余额释放事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("锁定提币记录失败：%w", err)
	}
	if item.Status == WithdrawalFailed {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交已释放提币查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalCreated && item.Status != WithdrawalSigning {
		return Withdrawal{}, false, ErrUnsafeRelease
	}
	total := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	if _, err := lockPendingWithdrawal(ctx, s, tx, item.UserID, total); err != nil {
		return Withdrawal{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			available_wei = available_wei + $2::numeric,
			pending_withdrawal_wei = pending_withdrawal_wei - $2::numeric,
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset = $3`, item.UserID, total.String(), AssetETH,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("释放提币占用余额失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, 'WITHDRAW_RELEASE', $3::numeric, 'WITHDRAWAL', $4)`,
		item.UserID, AssetETH, total.String(), item.ID,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("写入提币释放流水失败：%w", err)
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET
			status = $2, error_code = NULLIF($3, ''), error_message = NULLIF($4, ''),
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, WithdrawalFailed, strings.TrimSpace(errorCode), strings.TrimSpace(errorMessage),
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("更新提币为失败状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币余额释放事务失败：%w", err)
	}
	return item, true, nil
}

// FinalizeWithdrawal 根据链上执行结果结算提币金额和实际网络费。
func (s *Store) FinalizeWithdrawal(ctx context.Context, settlement WithdrawalSettlement) (Withdrawal, bool, error) {
	if err := amount.RequireNonNegative(settlement.ActualFeeWei); err != nil {
		return Withdrawal{}, false, err
	}
	if settlement.BlockNumber < 0 || settlement.Confirmations <= 0 {
		return Withdrawal{}, false, errors.New("必须提供有效区块高度且确认数必须大于零")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币结算事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, settlement.WithdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("锁定提币记录失败：%w", err)
	}
	if item.Status == WithdrawalCompleted || (item.Status == WithdrawalFailed && item.ActualFeeWei != nil) {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交已结算提币查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalBroadcasted && item.Status != WithdrawalConfirming {
		return Withdrawal{}, false, ErrInvalidState
	}
	if settlement.ActualFeeWei.Cmp(item.ReservedFeeWei) > 0 {
		return Withdrawal{}, false, ErrActualFeeExceedsHold
	}
	total := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	if _, err := lockPendingWithdrawal(ctx, s, tx, item.UserID, total); err != nil {
		return Withdrawal{}, false, err
	}
	refund := new(big.Int).Sub(item.ReservedFeeWei, settlement.ActualFeeWei)
	netSpent := new(big.Int).Add(item.AmountWei, settlement.ActualFeeWei)
	status := WithdrawalCompleted
	errorCode := ""
	errorMessage := ""
	if !settlement.Success {
		refund.Add(refund, item.AmountWei)
		netSpent.Set(settlement.ActualFeeWei)
		status = WithdrawalFailed
		errorCode = "CHAIN_EXECUTION_FAILED"
		errorMessage = "链上交易执行失败"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			available_wei = available_wei + $2::numeric,
			pending_withdrawal_wei = pending_withdrawal_wei - $3::numeric,
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset = $4`, item.UserID, refund.String(), total.String(), AssetETH,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("结算提币余额失败：%w", err)
	}
	entryAmount := new(big.Int).Neg(new(big.Int).Set(netSpent))
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, 'WITHDRAW_FINALIZE', $3::numeric, 'WITHDRAWAL', $4)`,
		item.UserID, AssetETH, entryAmount.String(), item.ID,
	); err != nil {
		return Withdrawal{}, false, fmt.Errorf("写入提币结算流水失败：%w", err)
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET
			actual_fee_wei = $2::numeric, block_number = $3, confirmations = $4,
			status = $5, error_code = NULLIF($6, ''), error_message = NULLIF($7, ''),
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, settlement.ActualFeeWei.String(), settlement.BlockNumber, settlement.Confirmations,
		status, errorCode, errorMessage,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("记录提币结算结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币结算事务失败：%w", err)
	}
	return item, true, nil
}

// validateDepositObservation 校验扫描器发现的充值数据。
func validateDepositObservation(observation DepositObservation) error {
	if observation.UserID <= 0 || observation.AddressID <= 0 {
		return errors.New("用户 ID 和地址 ID 必须大于零")
	}
	if observation.TxIndex < 0 || observation.BlockNumber < 0 {
		return errors.New("交易索引和区块高度不能为负数")
	}
	if err := amount.RequirePositive(observation.AmountWei); err != nil {
		return err
	}
	if !transactionHashPattern.MatchString(strings.ToLower(strings.TrimSpace(observation.TxHash))) ||
		!transactionHashPattern.MatchString(strings.ToLower(strings.TrimSpace(observation.BlockHash))) {
		return errors.New("必须提供有效的交易哈希和区块哈希")
	}
	return nil
}

// validateWithdrawalRequest 校验提币占用请求。
func validateWithdrawalRequest(request WithdrawalRequest) error {
	if request.UserID <= 0 || request.AddressID <= 0 {
		return errors.New("用户 ID 和地址 ID 必须大于零")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return errors.New("必须提供幂等标识")
	}
	if !common.IsHexAddress(strings.TrimSpace(request.ToAddress)) {
		return errors.New("必须提供有效的目标地址")
	}
	if err := amount.RequirePositive(request.AmountWei); err != nil {
		return err
	}
	return amount.RequireNonNegative(request.ReservedFeeWei)
}

// balanceForUpdate 查询并锁定用户 ETH 余额行。
func balanceForUpdate(ctx context.Context, s *Store, tx pgx.Tx, userID int64) (AssetBalance, error) {
	item, err := s.scanBalance(tx.QueryRow(ctx, `
		SELECT `+balanceColumns+`
		FROM asset_balances WHERE user_id = $1 AND asset = $2 FOR UPDATE`, userID, AssetETH,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetBalance{}, ErrNotFound
	}
	if err != nil {
		return AssetBalance{}, fmt.Errorf("锁定资产余额失败：%w", err)
	}
	return item, nil
}

// lockPendingWithdrawal 锁定余额并校验待处理提币金额是否足够结算。
func lockPendingWithdrawal(ctx context.Context, s *Store, tx pgx.Tx, userID int64, required *big.Int) (AssetBalance, error) {
	item, err := balanceForUpdate(ctx, s, tx, userID)
	if err != nil {
		return AssetBalance{}, err
	}
	if item.PendingWithdrawalWei.Cmp(required) < 0 {
		return AssetBalance{}, ErrPendingBalance
	}
	return item, nil
}

// ensureWalletOwnership 校验钱包地址属于指定用户和 Sepolia 网络。
func ensureWalletOwnership(ctx context.Context, tx pgx.Tx, userID, addressID int64) error {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wallet_addresses
			WHERE id = $1 AND user_id = $2 AND network = $3
		)`, addressID, userID, NetworkSepolia).Scan(&exists); err != nil {
		return fmt.Errorf("校验钱包地址归属失败：%w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// allowedWithdrawalTransition 判断提币状态迁移是否合法。
func allowedWithdrawalTransition(current, target string) bool {
	allowed := map[string]map[string]bool{
		WithdrawalCreated:          {WithdrawalSigning: true},
		WithdrawalSigning:          {WithdrawalSigned: true},
		WithdrawalSigned:           {WithdrawalBroadcasting: true},
		WithdrawalBroadcasting:     {WithdrawalBroadcasted: true, WithdrawalBroadcastUnknown: true},
		WithdrawalBroadcastUnknown: {WithdrawalBroadcasted: true},
		WithdrawalBroadcasted:      {WithdrawalConfirming: true},
	}
	return allowed[current][target]
}
