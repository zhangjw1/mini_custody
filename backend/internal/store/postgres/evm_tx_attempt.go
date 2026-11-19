package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
)

const evmTxAttemptColumns = `id, business_type, business_id, from_address, nonce::text, attempt_no, tx_hash, raw_tx, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, gas_limit, created_at`

// ListEVMTxAttempts 按尝试序号倒序查询同一笔 EVM 业务交易的所有候选发送版本。
func (s *Store) ListEVMTxAttempts(ctx context.Context, businessType string, businessID int64) ([]EVMTxAttempt, error) {
	businessType = strings.ToUpper(strings.TrimSpace(businessType))
	if businessID <= 0 || !isValidEVMBusinessType(businessType) {
		return nil, errors.New("查询 EVM 交易尝试参数无效")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+evmTxAttemptColumns+`
		FROM evm_tx_attempts
		WHERE business_type = $1 AND business_id = $2
		ORDER BY attempt_no DESC`,
		businessType, businessID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 EVM 交易尝试记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]EVMTxAttempt, 0)
	for rows.Next() {
		item, err := s.scanEVMTxAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 EVM 交易尝试记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceWithdrawalTx 在行锁事务中按同一 Nonce 保存 ETH 提币加价替换交易并同步追加费用占用。
func (s *Store) ReplaceWithdrawalTx(ctx context.Context, replacement SignedWithdrawal, newReservedFeeWei *big.Int) (Withdrawal, bool, error) {
	if err := validateSignedTxParams(replacement.WithdrawalID, replacement.GasLimit, replacement.MaxFeePerGasWei, replacement.MaxPriorityFeePerGasWei, replacement.RawTx, replacement.TxHash); err != nil {
		return Withdrawal{}, false, err
	}
	replacement.TxHash = strings.ToLower(strings.TrimSpace(replacement.TxHash))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币加价替换事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, replacement.WithdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("锁定待替换提币失败：%w", err)
	}
	if item.Status != WithdrawalBroadcasted || item.Nonce == nil {
		return Withdrawal{}, false, ErrInvalidState
	}
	if item.TxHash == replacement.TxHash {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交幂等提币替换事务失败：%w", err)
		}
		return item, false, nil
	}
	if newReservedFeeWei != nil && newReservedFeeWei.Cmp(item.ReservedFeeWei) > 0 {
		difference := new(big.Int).Sub(newReservedFeeWei, item.ReservedFeeWei)
		balance, err := balanceForUpdate(ctx, s, tx, item.UserID)
		if err != nil {
			return Withdrawal{}, false, err
		}
		if balance.AvailableWei.Cmp(difference) < 0 {
			return Withdrawal{}, false, ErrInsufficientBalance
		}
		if _, err := tx.Exec(ctx, `
			UPDATE asset_balances SET
				available_wei = available_wei - $2::numeric,
				pending_withdrawal_wei = pending_withdrawal_wei + $2::numeric,
				version = version + 1,
				updated_at = CURRENT_TIMESTAMP
			WHERE user_id = $1 AND asset = $3`, item.UserID, difference.String(), AssetETH,
		); err != nil {
			return Withdrawal{}, false, fmt.Errorf("追加提币替换费用占用失败：%w", err)
		}
		entryAmount := new(big.Int).Neg(new(big.Int).Set(difference))
		if _, err := tx.Exec(ctx, `
			INSERT INTO balance_entries (
				user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
			) VALUES ($1, (SELECT id FROM assets WHERE symbol = $2), $2, 'FEE_ADJUST', $3::numeric, 'WITHDRAWAL', $4)
			ON CONFLICT (entry_type, reference_type, reference_id) DO UPDATE SET
				amount_wei = balance_entries.amount_wei + EXCLUDED.amount_wei`,
			item.UserID, AssetETH, entryAmount.String(), item.ID,
		); err != nil {
			return Withdrawal{}, false, fmt.Errorf("写入提币替换费用调整流水失败：%w", err)
		}
	} else {
		newReservedFeeWei = new(big.Int).Set(item.ReservedFeeWei)
	}
	var fromAddress string
	if err := tx.QueryRow(ctx, `SELECT address FROM wallet_addresses WHERE id = $1`, item.AddressID).Scan(&fromAddress); err != nil {
		return Withdrawal{}, false, fmt.Errorf("查询提币发送地址失败：%w", mapNotFound(err))
	}
	attemptNo, err := nextAttemptNoInTx(ctx, tx, EVMBusinessWithdrawal, item.ID, item.BumpCount)
	if err != nil {
		return Withdrawal{}, false, err
	}
	if err := insertEVMTxAttemptInTx(ctx, tx, EVMTxAttempt{
		BusinessType:            EVMBusinessWithdrawal,
		BusinessID:              item.ID,
		FromAddress:             fromAddress,
		Nonce:                   item.Nonce,
		AttemptNo:               attemptNo,
		TxHash:                  replacement.TxHash,
		RawTx:                   replacement.RawTx,
		MaxFeePerGasWei:         replacement.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: replacement.MaxPriorityFeePerGasWei,
		GasLimit:                int64(replacement.GasLimit),
	}); err != nil {
		return Withdrawal{}, false, err
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET
			reserved_fee_wei = $2::numeric,
			gas_limit = $3,
			max_fee_per_gas_wei = $4::numeric,
			max_priority_fee_per_gas_wei = $5::numeric,
			raw_tx = $6,
			tx_hash = $7,
			bump_count = bump_count + 1,
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, newReservedFeeWei.String(), int64(replacement.GasLimit),
		replacement.MaxFeePerGasWei.String(), replacement.MaxPriorityFeePerGasWei.String(),
		replacement.RawTx, replacement.TxHash,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("保存提币加价替换结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币加价替换事务失败：%w", err)
	}
	return item, true, nil
}

// ReplaceTokenWithdrawalTx 在行锁事务中按同一 Nonce 保存 Token 提币加价替换交易。
func (s *Store) ReplaceTokenWithdrawalTx(ctx context.Context, replacement SignedTokenWithdrawal) (TokenWithdrawal, bool, error) {
	if err := validateSignedTxParams(replacement.WithdrawalID, replacement.GasLimit, replacement.MaxFeePerGasWei, replacement.MaxPriorityFeePerGasWei, replacement.RawTx, replacement.TxHash); err != nil {
		return TokenWithdrawal{}, false, err
	}
	replacement.TxHash = strings.ToLower(strings.TrimSpace(replacement.TxHash))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币加价替换事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx,
		`SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, replacement.WithdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定待替换 Token 提币失败：%w", err)
	}
	if item.Status != WithdrawalBroadcasted || item.Nonce == nil {
		return TokenWithdrawal{}, false, ErrInvalidState
	}
	if item.TxHash == replacement.TxHash {
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交幂等 Token 提币替换事务失败：%w", err)
		}
		return item, false, nil
	}
	var fromAddress string
	if err := tx.QueryRow(ctx, `SELECT address FROM platform_wallets WHERE id = $1`, item.PlatformWalletID).Scan(&fromAddress); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("查询 Token 提币平台热钱包地址失败：%w", mapNotFound(err))
	}
	attemptNo, err := nextAttemptNoInTx(ctx, tx, EVMBusinessTokenWithdrawal, item.ID, item.BumpCount)
	if err != nil {
		return TokenWithdrawal{}, false, err
	}
	if err := insertEVMTxAttemptInTx(ctx, tx, EVMTxAttempt{
		BusinessType:            EVMBusinessTokenWithdrawal,
		BusinessID:              item.ID,
		FromAddress:             fromAddress,
		Nonce:                   item.Nonce,
		AttemptNo:               attemptNo,
		TxHash:                  replacement.TxHash,
		RawTx:                   replacement.RawTx,
		MaxFeePerGasWei:         replacement.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: replacement.MaxPriorityFeePerGasWei,
		GasLimit:                int64(replacement.GasLimit),
	}); err != nil {
		return TokenWithdrawal{}, false, err
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET
			gas_limit = $2,
			max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric,
			raw_tx = $5,
			tx_hash = $6,
			bump_count = bump_count + 1,
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns,
		item.ID, int64(replacement.GasLimit),
		replacement.MaxFeePerGasWei.String(), replacement.MaxPriorityFeePerGasWei.String(),
		replacement.RawTx, replacement.TxHash,
	))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("保存 Token 提币加价替换结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币加价替换事务失败：%w", err)
	}
	return item, true, nil
}

// ReplaceTokenSweepTx 在行锁事务中按同一 Nonce 保存 Token 归集加价替换交易。
func (s *Store) ReplaceTokenSweepTx(ctx context.Context, replacement SignedTokenSweep) (TokenSweep, bool, error) {
	if err := validateSignedTxParams(replacement.SweepID, replacement.GasLimit, replacement.MaxFeePerGasWei, replacement.MaxPriorityFeePerGasWei, replacement.RawTx, replacement.TxHash); err != nil {
		return TokenSweep{}, false, err
	}
	replacement.TxHash = strings.ToLower(strings.TrimSpace(replacement.TxHash))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启 Token 归集加价替换事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, replacement.SweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定待替换 Token 归集失败：%w", err)
	}
	if item.Status != TokenSweepBroadcasted || item.Nonce == nil {
		return TokenSweep{}, false, ErrInvalidState
	}
	if item.TxHash == replacement.TxHash {
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交幂等 Token 归集替换事务失败：%w", err)
		}
		return item, false, nil
	}
	var fromAddress string
	if err := tx.QueryRow(ctx, `SELECT address FROM wallet_addresses WHERE id = $1`, item.AddressID).Scan(&fromAddress); err != nil {
		return TokenSweep{}, false, fmt.Errorf("查询 Token 归集来源地址失败：%w", mapNotFound(err))
	}
	attemptNo, err := nextAttemptNoInTx(ctx, tx, EVMBusinessTokenSweep, item.ID, item.BumpCount)
	if err != nil {
		return TokenSweep{}, false, err
	}
	if err := insertEVMTxAttemptInTx(ctx, tx, EVMTxAttempt{
		BusinessType:            EVMBusinessTokenSweep,
		BusinessID:              item.ID,
		FromAddress:             fromAddress,
		Nonce:                   item.Nonce,
		AttemptNo:               attemptNo,
		TxHash:                  replacement.TxHash,
		RawTx:                   replacement.RawTx,
		MaxFeePerGasWei:         replacement.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: replacement.MaxPriorityFeePerGasWei,
		GasLimit:                int64(replacement.GasLimit),
	}); err != nil {
		return TokenSweep{}, false, err
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET
			gas_limit = $2,
			max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric,
			raw_tx = $5,
			tx_hash = $6,
			bump_count = bump_count + 1,
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns,
		item.ID, int64(replacement.GasLimit),
		replacement.MaxFeePerGasWei.String(), replacement.MaxPriorityFeePerGasWei.String(),
		replacement.RawTx, replacement.TxHash,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("保存 Token 归集加价替换结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交 Token 归集加价替换事务失败：%w", err)
	}
	return item, true, nil
}

// ReplaceInternalTransferTx 在行锁事务中按同一 Nonce 保存 Gas 补充加价替换交易。
func (s *Store) ReplaceInternalTransferTx(ctx context.Context, replacement SignedInternalTransfer) (InternalTransfer, bool, error) {
	if err := validateSignedTxParams(replacement.TransferID, replacement.GasLimit, replacement.MaxFeePerGasWei, replacement.MaxPriorityFeePerGasWei, replacement.RawTx, replacement.TxHash); err != nil {
		return InternalTransfer{}, false, err
	}
	replacement.TxHash = strings.ToLower(strings.TrimSpace(replacement.TxHash))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("开启内部转账加价替换事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1 FOR UPDATE`, replacement.TransferID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定待替换内部转账失败：%w", err)
	}
	if item.Status != InternalTransferSent || item.Nonce == nil {
		return InternalTransfer{}, false, ErrInvalidState
	}
	if item.TxHash == replacement.TxHash {
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("提交幂等内部转账替换事务失败：%w", err)
		}
		return item, false, nil
	}
	attemptNo, err := nextAttemptNoInTx(ctx, tx, EVMBusinessInternalTransfer, item.ID, item.BumpCount)
	if err != nil {
		return InternalTransfer{}, false, err
	}
	if err := insertEVMTxAttemptInTx(ctx, tx, EVMTxAttempt{
		BusinessType:            EVMBusinessInternalTransfer,
		BusinessID:              item.ID,
		FromAddress:             item.FromAddress,
		Nonce:                   item.Nonce,
		AttemptNo:               attemptNo,
		TxHash:                  replacement.TxHash,
		RawTx:                   replacement.RawTx,
		MaxFeePerGasWei:         replacement.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: replacement.MaxPriorityFeePerGasWei,
		GasLimit:                int64(replacement.GasLimit),
	}); err != nil {
		return InternalTransfer{}, false, err
	}
	item, err = s.scanInternalTransfer(tx.QueryRow(ctx, `
		UPDATE internal_transfers SET
			gas_limit = $2,
			max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric,
			raw_tx = $5,
			tx_hash = $6,
			bump_count = bump_count + 1,
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+internalTransferColumns,
		item.ID, int64(replacement.GasLimit),
		replacement.MaxFeePerGasWei.String(), replacement.MaxPriorityFeePerGasWei.String(),
		replacement.RawTx, replacement.TxHash,
	))
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("保存内部转账加价替换结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("提交内部转账加价替换事务失败：%w", err)
	}
	return item, true, nil
}

// TouchWithdrawalBroadcast 更新已广播 ETH 提币的最近重播时间。
func (s *Store) TouchWithdrawalBroadcast(ctx context.Context, withdrawalID int64) (Withdrawal, error) {
	item, err := s.scanWithdrawal(s.pool.QueryRow(ctx, `
		UPDATE withdrawals SET
			broadcasted_at = COALESCE(broadcasted_at, CURRENT_TIMESTAMP),
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $2
		RETURNING `+withdrawalColumns,
		withdrawalID, WithdrawalBroadcasted,
	))
	return item, mapNotFound(err)
}

// TouchTokenWithdrawalBroadcast 更新已广播 Token 提币的最近重播时间。
func (s *Store) TouchTokenWithdrawalBroadcast(ctx context.Context, withdrawalID int64) (TokenWithdrawal, error) {
	item, err := s.scanTokenWithdrawal(s.pool.QueryRow(ctx, `
		UPDATE token_withdrawals SET
			broadcasted_at = COALESCE(broadcasted_at, CURRENT_TIMESTAMP),
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $2
		RETURNING `+tokenWithdrawalColumns,
		withdrawalID, WithdrawalBroadcasted,
	))
	return item, mapNotFound(err)
}

// TouchTokenSweepBroadcast 更新已广播 Token 归集的最近重播时间。
func (s *Store) TouchTokenSweepBroadcast(ctx context.Context, sweepID int64) (TokenSweep, error) {
	item, err := s.scanTokenSweep(s.pool.QueryRow(ctx, `
		UPDATE token_sweeps SET
			broadcasted_at = COALESCE(broadcasted_at, CURRENT_TIMESTAMP),
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $2
		RETURNING `+tokenSweepColumns,
		sweepID, TokenSweepBroadcasted,
	))
	return item, mapNotFound(err)
}

// TouchInternalTransferBroadcast 更新已广播 Gas 补充内部转账的最近重播时间。
func (s *Store) TouchInternalTransferBroadcast(ctx context.Context, transferID int64) (InternalTransfer, error) {
	item, err := s.scanInternalTransfer(s.pool.QueryRow(ctx, `
		UPDATE internal_transfers SET
			broadcasted_at = COALESCE(broadcasted_at, CURRENT_TIMESTAMP),
			last_broadcast_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $2
		RETURNING `+internalTransferColumns,
		transferID, InternalTransferSent,
	))
	return item, mapNotFound(err)
}

// MarkWithdrawalManualReview 将已越过 Nonce 且无法确认上链的 ETH 提币转入人工核查状态。
func (s *Store) MarkWithdrawalManualReview(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (Withdrawal, bool, error) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if withdrawalID <= 0 || errorCode == "" || errorMessage == "" {
		return Withdrawal{}, false, errors.New("人工核查参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币人工核查事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanWithdrawal(tx.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("锁定待核查提币失败：%w", err)
	}
	if item.Status == WithdrawalManualReview {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交提币人工核查查询事务失败：%w", err)
		}
		return item, false, nil
	}
	switch item.Status {
	case WithdrawalSigned, WithdrawalBroadcasting, WithdrawalBroadcastUnknown, WithdrawalBroadcasted, WithdrawalConfirming:
	default:
		return Withdrawal{}, false, ErrInvalidState
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET status = $2, error_code = $3, error_message = $4, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, WithdrawalManualReview, errorCode, errorMessage,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("更新提币为人工核查状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币人工核查事务失败：%w", err)
	}
	return item, true, nil
}

// MarkTokenWithdrawalManualReview 将已越过 Nonce 且无法确认上链的 Token 提币转入人工核查状态。
func (s *Store) MarkTokenWithdrawalManualReview(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (TokenWithdrawal, bool, error) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if withdrawalID <= 0 || errorCode == "" || errorMessage == "" {
		return TokenWithdrawal{}, false, errors.New("Token 提币人工核查参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币人工核查事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx,
		`SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定待核查 Token 提币失败：%w", err)
	}
	if item.Status == WithdrawalManualReview {
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币人工核查查询事务失败：%w", err)
		}
		return item, false, nil
	}
	switch item.Status {
	case WithdrawalSigned, WithdrawalBroadcasted, WithdrawalConfirming:
	default:
		return TokenWithdrawal{}, false, ErrInvalidState
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET status = $2, error_code = $3, error_message = $4, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns,
		item.ID, WithdrawalManualReview, errorCode, errorMessage,
	))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("更新 Token 提币为人工核查状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币人工核查事务失败：%w", err)
	}
	return item, true, nil
}

// MarkTokenSweepManualReview 将已越过 Nonce 且无法确认上链的 Token 归集转入人工核查状态。
func (s *Store) MarkTokenSweepManualReview(ctx context.Context, sweepID int64, errorCode, errorMessage string) (TokenSweep, bool, error) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if sweepID <= 0 || errorCode == "" || errorMessage == "" {
		return TokenSweep{}, false, errors.New("Token 归集人工核查参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启 Token 归集人工核查事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, sweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定待核查 Token 归集失败：%w", err)
	}
	if item.Status == TokenSweepManualReview {
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交 Token 归集人工核查查询事务失败：%w", err)
		}
		return item, false, nil
	}
	switch item.Status {
	case TokenSweepSigned, TokenSweepBroadcasted, TokenSweepConfirming:
	default:
		return TokenSweep{}, false, ErrInvalidState
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET status = $2, error_code = $3, error_message = $4, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns,
		item.ID, TokenSweepManualReview, errorCode, errorMessage,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("更新 Token 归集为人工核查状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交 Token 归集人工核查事务失败：%w", err)
	}
	return item, true, nil
}

// MarkInternalTransferManualReview 将已越过 Nonce 且无法确认上链的内部转账转入人工核查状态。
func (s *Store) MarkInternalTransferManualReview(ctx context.Context, transferID int64, errorCode, errorMessage string) (InternalTransfer, bool, error) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if transferID <= 0 || errorCode == "" || errorMessage == "" {
		return InternalTransfer{}, false, errors.New("内部转账人工核查参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("开启内部转账人工核查事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1 FOR UPDATE`, transferID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定待核查内部转账失败：%w", err)
	}
	if item.Status == InternalTransferManualReview {
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("提交内部转账人工核查查询事务失败：%w", err)
		}
		return item, false, nil
	}
	switch item.Status {
	case InternalTransferSigned, InternalTransferSent, InternalTransferChecking:
	default:
		return InternalTransfer{}, false, ErrInvalidState
	}
	item, err = s.scanInternalTransfer(tx.QueryRow(ctx, `
		UPDATE internal_transfers SET status = $2, error_code = $3, error_message = $4, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+internalTransferColumns,
		item.ID, InternalTransferManualReview, errorCode, errorMessage,
	))
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("更新内部转账为人工核查状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("提交内部转账人工核查事务失败：%w", err)
	}
	return item, true, nil
}

// nextAttemptNoInTx 在行锁事务中计算下一条 EVM 交易尝试序号。
func nextAttemptNoInTx(ctx context.Context, tx pgx.Tx, businessType string, businessID int64, currentBumpCount int32) (int32, error) {
	var maxAttempt int32
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(attempt_no), 0)
		FROM evm_tx_attempts
		WHERE business_type = $1 AND business_id = $2`,
		businessType, businessID,
	).Scan(&maxAttempt); err != nil {
		return 0, fmt.Errorf("查询 EVM 交易历史尝试序号失败：%w", err)
	}
	expected := currentBumpCount + 2
	if maxAttempt+1 > expected {
		expected = maxAttempt + 1
	}
	return expected, nil
}

// insertEVMTxAttemptInTx 在现有事务内幂等写入一条 EVM 候选交易记录。
func insertEVMTxAttemptInTx(ctx context.Context, tx pgx.Tx, attempt EVMTxAttempt) error {
	attempt.BusinessType = strings.ToUpper(strings.TrimSpace(attempt.BusinessType))
	attempt.FromAddress = strings.ToLower(strings.TrimSpace(attempt.FromAddress))
	attempt.TxHash = strings.ToLower(strings.TrimSpace(attempt.TxHash))
	if attempt.BusinessID <= 0 || !isValidEVMBusinessType(attempt.BusinessType) ||
		!common.IsHexAddress(attempt.FromAddress) || attempt.Nonce == nil || attempt.Nonce.Sign() < 0 ||
		attempt.AttemptNo < 1 || !transactionHashPattern.MatchString(attempt.TxHash) ||
		len(attempt.RawTx) == 0 || attempt.GasLimit <= 0 {
		return errors.New("EVM 交易尝试记录参数无效")
	}
	if err := amount.RequirePositive(attempt.MaxFeePerGasWei); err != nil {
		return err
	}
	if err := amount.RequireNonNegative(attempt.MaxPriorityFeePerGasWei); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO evm_tx_attempts (
			business_type, business_id, from_address, nonce, attempt_no,
			tx_hash, raw_tx, max_fee_per_gas_wei, max_priority_fee_per_gas_wei, gas_limit
		) VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8::numeric, $9::numeric, $10)
		ON CONFLICT (tx_hash) DO NOTHING`,
		attempt.BusinessType, attempt.BusinessID, attempt.FromAddress, attempt.Nonce.String(),
		attempt.AttemptNo, attempt.TxHash, attempt.RawTx,
		attempt.MaxFeePerGasWei.String(), attempt.MaxPriorityFeePerGasWei.String(), attempt.GasLimit,
	); err != nil {
		return fmt.Errorf("写入 EVM 交易尝试记录失败：%w", err)
	}
	return nil
}

// scanEVMTxAttempt 从查询结果读取 EVM 交易尝试模型。
func (s *Store) scanEVMTxAttempt(row rowScanner) (EVMTxAttempt, error) {
	var item EVMTxAttempt
	var nonceText, maxFeeText, maxPriorityFeeText string
	if err := row.Scan(
		&item.ID, &item.BusinessType, &item.BusinessID, &item.FromAddress,
		&nonceText, &item.AttemptNo, &item.TxHash, &item.RawTx,
		&maxFeeText, &maxPriorityFeeText, &item.GasLimit, &item.CreatedAt,
	); err != nil {
		return EVMTxAttempt{}, err
	}
	var err error
	if item.Nonce, err = parseDatabaseWei(nonceText, "evm_tx_attempts nonce"); err != nil {
		return EVMTxAttempt{}, err
	}
	if item.MaxFeePerGasWei, err = parseDatabaseWei(maxFeeText, "evm_tx_attempts max_fee_per_gas_wei"); err != nil {
		return EVMTxAttempt{}, err
	}
	if item.MaxPriorityFeePerGasWei, err = parseDatabaseWei(maxPriorityFeeText, "evm_tx_attempts max_priority_fee_per_gas_wei"); err != nil {
		return EVMTxAttempt{}, err
	}
	item.CreatedAt = s.localTime(item.CreatedAt)
	return item, nil
}

// validateSignedTxParams 校验替换签名交易的基础参数合法性。
func validateSignedTxParams(id int64, gasLimit uint64, maxFee, maxTip *big.Int, rawTx []byte, txHash string) error {
	if id <= 0 || gasLimit == 0 || gasLimit > uint64(math.MaxInt64) || len(rawTx) == 0 {
		return errors.New("替换签名交易参数无效")
	}
	if err := amount.RequirePositive(maxFee); err != nil {
		return err
	}
	if err := amount.RequireNonNegative(maxTip); err != nil {
		return err
	}
	if !transactionHashPattern.MatchString(strings.ToLower(strings.TrimSpace(txHash))) {
		return errors.New("替换签名交易哈希无效")
	}
	return nil
}

// isValidEVMBusinessType 校验 EVM 交易尝试记录的业务类型是否受支持。
func isValidEVMBusinessType(value string) bool {
	switch value {
	case EVMBusinessWithdrawal, EVMBusinessTokenWithdrawal, EVMBusinessTokenSweep, EVMBusinessInternalTransfer:
		return true
	default:
		return false
	}
}
