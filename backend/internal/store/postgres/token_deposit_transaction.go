package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
)

type TokenDepositObservation struct {
	UserID      int64
	AddressID   int64
	AssetID     int64
	TxHash      string
	LogIndex    int32
	BlockNumber int64
	BlockHash   string
	FromAddress string
	ToAddress   string
	AmountUnits *big.Int
}

// RecordTokenDepositsAndCheckpoint 原子记录一批 Token Event、待确认余额和批次末检查点。
func (s *Store) RecordTokenDepositsAndCheckpoint(
	ctx context.Context,
	observations []TokenDepositObservation,
	checkpoint ChainCheckpoint,
) ([]TokenDeposit, int, error) {
	checkpoint.Network = strings.TrimSpace(checkpoint.Network)
	checkpoint.Scanner = strings.TrimSpace(checkpoint.Scanner)
	checkpoint.LastScannedHash = strings.ToLower(strings.TrimSpace(checkpoint.LastScannedHash))
	if checkpoint.Network != NetworkSepolia || checkpoint.Scanner == "" || checkpoint.LastScannedBlock < 0 ||
		!transactionHashPattern.MatchString(checkpoint.LastScannedHash) {
		return nil, 0, errors.New("Token 扫描检查点参数无效")
	}
	for index := range observations {
		if err := validateTokenDepositObservation(observations[index], checkpoint.LastScannedBlock); err != nil {
			return nil, 0, err
		}
		observations[index].TxHash = strings.ToLower(strings.TrimSpace(observations[index].TxHash))
		observations[index].BlockHash = strings.ToLower(strings.TrimSpace(observations[index].BlockHash))
		observations[index].FromAddress = strings.ToLower(common.HexToAddress(observations[index].FromAddress).Hex())
		observations[index].ToAddress = strings.ToLower(common.HexToAddress(observations[index].ToAddress).Hex())
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("开启 Token 充值扫描事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	items := make([]TokenDeposit, 0, len(observations))
	createdCount := 0
	for _, observation := range observations {
		item, created, err := s.recordTokenDepositInTx(ctx, tx, observation)
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
		return nil, 0, fmt.Errorf("提交 Token 充值扫描事务失败：%w", err)
	}
	return items, createdCount, nil
}

// recordTokenDepositInTx 在既有事务中幂等写入 Token Event、pending 余额和流水。
func (s *Store) recordTokenDepositInTx(ctx context.Context, tx pgx.Tx, observation TokenDepositObservation) (TokenDeposit, bool, error) {
	if err := ensureWalletOwnership(ctx, tx, observation.UserID, observation.AddressID); err != nil {
		return TokenDeposit{}, false, err
	}
	var assetType string
	if err := tx.QueryRow(ctx, `SELECT asset_type FROM assets WHERE id = $1 AND enabled FOR SHARE`, observation.AssetID).Scan(&assetType); err != nil {
		return TokenDeposit{}, false, mapNotFound(err)
	}
	if assetType != AssetTypeERC20 {
		return TokenDeposit{}, false, errors.New("Token 充值资产类型必须是 ERC20")
	}
	item, err := s.scanTokenDeposit(tx.QueryRow(ctx, `
		INSERT INTO token_deposits (
			user_id, address_id, asset_id, tx_hash, log_index, block_number, block_hash,
			from_address, to_address, amount_units, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11)
		ON CONFLICT (asset_id, tx_hash, log_index) WHERE status <> 'ORPHANED' DO NOTHING
		RETURNING `+tokenDepositColumns,
		observation.UserID, observation.AddressID, observation.AssetID, observation.TxHash,
		observation.LogIndex, observation.BlockNumber, observation.BlockHash, observation.FromAddress,
		observation.ToAddress, observation.AmountUnits.String(), DepositConfirming,
	))
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return TokenDeposit{}, false, fmt.Errorf("写入 Token 充值记录失败：%w", err)
	}
	if created {
		balance, err := tokenBalanceForUpdate(ctx, s, tx, observation.UserID, observation.AssetID)
		if err != nil {
			return TokenDeposit{}, false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE asset_balances SET
				pending_deposit_wei = pending_deposit_wei + $3::numeric,
				version = version + 1, updated_at = CURRENT_TIMESTAMP
			WHERE user_id = $1 AND asset_id = $2`,
			observation.UserID, observation.AssetID, observation.AmountUnits.String(),
		); err != nil {
			return TokenDeposit{}, false, fmt.Errorf("增加 Token 待确认余额失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO balance_entries (
				user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
			) VALUES ($1, $2, $3, $4, $5::numeric, 'TOKEN_DEPOSIT', $6)`,
			observation.UserID, observation.AssetID, balance.Asset, EntryDepositPending, observation.AmountUnits.String(), item.ID,
		); err != nil {
			return TokenDeposit{}, false, fmt.Errorf("写入 Token 待确认流水失败：%w", err)
		}
		return item, true, nil
	}

	existing, err := s.scanTokenDeposit(tx.QueryRow(ctx, `
		SELECT `+tokenDepositColumns+` FROM token_deposits
		WHERE asset_id = $1 AND tx_hash = $2 AND log_index = $3 AND status <> 'ORPHANED'`,
		observation.AssetID, observation.TxHash, observation.LogIndex,
	))
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("读取重复 Token 充值失败：%w", err)
	}
	if existing.UserID != observation.UserID || existing.AddressID != observation.AddressID ||
		existing.FromAddress != observation.FromAddress || existing.ToAddress != observation.ToAddress ||
		existing.AmountUnits.Cmp(observation.AmountUnits) != 0 {
		return TokenDeposit{}, false, ErrIdempotencyConflict
	}
	if existing.BlockNumber != observation.BlockNumber || existing.BlockHash != observation.BlockHash {
		if existing.Status == DepositCredited {
			refID := existing.ID
			if _, err := recordWorkerErrorWith(ctx, tx, WorkerError{
				Worker:        "token-deposit-scanner",
				Stage:         "reorg_check",
				ReferenceType: "TOKEN_DEPOSIT",
				ReferenceID:   &refID,
				ErrorCode:     "CREDITED_DEPOSIT_REORG",
				ErrorMessage:  fmt.Sprintf("已入账 Token 充值 %d 重组后位置变更为区块 %d (%s)，保持余额不变请人工核查", existing.ID, observation.BlockNumber, observation.BlockHash),
			}); err != nil {
				return TokenDeposit{}, false, err
			}
		} else {
			updated, err := s.scanTokenDeposit(tx.QueryRow(ctx, `
				UPDATE token_deposits SET
					block_number = $2, block_hash = $3, confirmations = 0, updated_at = CURRENT_TIMESTAMP
				WHERE id = $1
				RETURNING `+tokenDepositColumns,
				existing.ID, observation.BlockNumber, observation.BlockHash,
			))
			if err != nil {
				return TokenDeposit{}, false, fmt.Errorf("更新重组后 Token 充值区块信息失败：%w", err)
			}
			existing = updated
		}
	}
	return existing, false, nil
}

// OrphanTokenDeposit 在链重组确认 Token Event 不再属于主链时置为 ORPHANED 并写入冲正流水。
func (s *Store) OrphanTokenDeposit(ctx context.Context, depositID int64) (TokenDeposit, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("开启孤块 Token 充值冲正事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	item, err := s.scanTokenDeposit(tx.QueryRow(ctx, `SELECT `+tokenDepositColumns+` FROM token_deposits WHERE id = $1 FOR UPDATE`, depositID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenDeposit{}, false, ErrNotFound
	}
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("锁定待冲正 Token 充值失败：%w", err)
	}
	if item.Status == DepositOrphaned {
		if err := tx.Commit(ctx); err != nil {
			return TokenDeposit{}, false, fmt.Errorf("提交已孤立 Token 充值查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status == DepositCredited {
		return TokenDeposit{}, false, ErrInvalidState
	}
	balance, err := tokenBalanceForUpdate(ctx, s, tx, item.UserID, item.AssetID)
	if err != nil {
		return TokenDeposit{}, false, err
	}
	if balance.PendingDepositWei.Cmp(item.AmountUnits) < 0 {
		return TokenDeposit{}, false, ErrPendingBalance
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			pending_deposit_wei = pending_deposit_wei - $3::numeric,
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset_id = $2`,
		item.UserID, item.AssetID, item.AmountUnits.String(),
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("扣减孤块 Token 充值待确认余额失败：%w", err)
	}
	reversal := new(big.Int).Neg(item.AmountUnits).String()
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, $3, $4, $5::numeric, 'TOKEN_DEPOSIT', $6)
		ON CONFLICT (entry_type, reference_type, reference_id) DO NOTHING`,
		item.UserID, item.AssetID, balance.Asset, EntryDepositReversal, reversal, item.ID,
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("写入孤块 Token 充值冲正流水失败：%w", err)
	}
	item, err = s.scanTokenDeposit(tx.QueryRow(ctx, `
		UPDATE token_deposits SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenDepositColumns, item.ID, DepositOrphaned,
	))
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("更新 Token 充值为孤块状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("提交孤块 Token 充值冲正事务失败：%w", err)
	}
	return item, true, nil
}

// ListTokenDepositsAfterBlock 查询指定高度之后需复核主链一致性的非孤块 Token 充值记录。
func (s *Store) ListTokenDepositsAfterBlock(ctx context.Context, assetID, afterBlock int64, limit int) ([]TokenDeposit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenDepositColumns+` FROM token_deposits
		WHERE asset_id = $1 AND block_number > $2 AND status <> $3
		ORDER BY block_number, id LIMIT $4`,
		assetID, afterBlock, DepositOrphaned, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询重组区间 Token 充值记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenDeposit, 0)
	for rows.Next() {
		item, err := s.scanTokenDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取重组区间 Token 充值记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// UpdateTokenDepositConfirmations 单调更新 Token 充值确认数。
func (s *Store) UpdateTokenDepositConfirmations(ctx context.Context, depositID, confirmations int64) (TokenDeposit, error) {
	if confirmations < 0 {
		return TokenDeposit{}, errors.New("Token 充值确认数不能为负数")
	}
	item, err := s.scanTokenDeposit(s.pool.QueryRow(ctx, `
		UPDATE token_deposits SET confirmations = GREATEST(confirmations, $2), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenDepositColumns, depositID, confirmations,
	))
	return item, mapNotFound(err)
}

// CreditTokenDeposit 将确认充值转为可用余额，并幂等创建 Token 归集任务。
func (s *Store) CreditTokenDeposit(ctx context.Context, depositID, confirmations int64) (TokenDeposit, bool, error) {
	if confirmations <= 0 {
		return TokenDeposit{}, false, errors.New("Token 充值确认数必须大于零")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("开启 Token 充值入账事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenDeposit(tx.QueryRow(ctx, `SELECT `+tokenDepositColumns+` FROM token_deposits WHERE id = $1 FOR UPDATE`, depositID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenDeposit{}, false, ErrNotFound
	}
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("锁定 Token 充值失败：%w", err)
	}
	if item.Status == DepositCredited {
		if err := tx.Commit(ctx); err != nil {
			return TokenDeposit{}, false, fmt.Errorf("提交已入账 Token 充值查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != DepositConfirming && item.Status != DepositConfirmed {
		return TokenDeposit{}, false, ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `
		UPDATE token_deposits SET status = $2, confirmations = GREATEST(confirmations, $3), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`, item.ID, DepositConfirmed, confirmations,
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("确认 Token 充值记录失败：%w", err)
	}
	balance, err := tokenBalanceForUpdate(ctx, s, tx, item.UserID, item.AssetID)
	if err != nil {
		return TokenDeposit{}, false, err
	}
	if balance.PendingDepositWei.Cmp(item.AmountUnits) < 0 {
		return TokenDeposit{}, false, ErrPendingBalance
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			pending_deposit_wei = pending_deposit_wei - $3::numeric,
			available_wei = available_wei + $3::numeric,
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset_id = $2`, item.UserID, item.AssetID, item.AmountUnits.String(),
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("Token 充值金额转入可用余额失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, $3, 'DEPOSIT_CREDIT', $4::numeric, 'TOKEN_DEPOSIT', $5)`,
		item.UserID, item.AssetID, balance.Asset, item.AmountUnits.String(), item.ID,
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("写入 Token 充值入账流水失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO token_sweeps (
			user_id, address_id, asset_id, trigger_deposit_id, recognized_amount_units, status
		) VALUES ($1, $2, $3, $4, $5::numeric, $6)
		ON CONFLICT (address_id, asset_id)
			WHERE status IN ('CREATED', 'WAITING_GAS', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING')
		DO UPDATE SET
			recognized_amount_units = token_sweeps.recognized_amount_units + EXCLUDED.recognized_amount_units,
			updated_at = CURRENT_TIMESTAMP`, item.UserID, item.AddressID, item.AssetID, item.ID,
		item.AmountUnits.String(), TokenSweepCreated,
	); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("创建或合并 Token 归集任务失败：%w", err)
	}
	item, err = s.scanTokenDeposit(tx.QueryRow(ctx, `
		UPDATE token_deposits SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenDepositColumns, item.ID, DepositCredited,
	))
	if err != nil {
		return TokenDeposit{}, false, fmt.Errorf("更新 Token 充值为已入账失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenDeposit{}, false, fmt.Errorf("提交 Token 充值入账事务失败：%w", err)
	}
	return item, true, nil
}

// tokenBalanceForUpdate 查询并锁定用户指定 Token 的余额行。
func tokenBalanceForUpdate(ctx context.Context, s *Store, tx pgx.Tx, userID, assetID int64) (AssetBalance, error) {
	item, err := s.scanBalance(tx.QueryRow(ctx, `
		SELECT `+balanceColumns+` FROM asset_balances
		WHERE user_id = $1 AND asset_id = $2 FOR UPDATE`, userID, assetID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetBalance{}, ErrNotFound
	}
	if err != nil {
		return AssetBalance{}, fmt.Errorf("锁定 Token 资产余额失败：%w", err)
	}
	return item, nil
}

// validateTokenDepositObservation 校验扫描器传入的 Token Event 数据。
func validateTokenDepositObservation(observation TokenDepositObservation, checkpointBlock int64) error {
	if observation.UserID <= 0 || observation.AddressID <= 0 || observation.AssetID <= 0 {
		return errors.New("Token 充值用户、地址和资产 ID 必须大于零")
	}
	if observation.LogIndex < 0 || observation.BlockNumber < 0 || observation.BlockNumber > checkpointBlock {
		return errors.New("Token 充值日志索引或区块高度无效")
	}
	if err := amount.RequirePositive(observation.AmountUnits); err != nil {
		return err
	}
	if !transactionHashPattern.MatchString(strings.ToLower(strings.TrimSpace(observation.TxHash))) ||
		!transactionHashPattern.MatchString(strings.ToLower(strings.TrimSpace(observation.BlockHash))) {
		return errors.New("Token 充值必须提供有效交易哈希和区块哈希")
	}
	if !common.IsHexAddress(observation.FromAddress) || !common.IsHexAddress(observation.ToAddress) {
		return errors.New("Token 充值必须提供有效发送和接收地址")
	}
	return nil
}
