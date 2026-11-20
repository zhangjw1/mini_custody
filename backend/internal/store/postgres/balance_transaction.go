package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

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
	return s.recordDepositsWithCheckpoint(ctx, observations, checkpoint, true)
}

// RecordDepositsBatchAndCheckpoint 原子记录一段连续区块内的充值并在最后推进扫描检查点。
func (s *Store) RecordDepositsBatchAndCheckpoint(
	ctx context.Context,
	observations []DepositObservation,
	checkpoint ChainCheckpoint,
) ([]Deposit, int, error) {
	return s.recordDepositsWithCheckpoint(ctx, observations, checkpoint, false)
}

// recordDepositsWithCheckpoint 在单个事务中批量记录充值观测并原子推进检查点。
func (s *Store) recordDepositsWithCheckpoint(
	ctx context.Context,
	observations []DepositObservation,
	checkpoint ChainCheckpoint,
	requireSameBlock bool,
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
		if requireSameBlock {
			if observations[index].BlockNumber != checkpoint.LastScannedBlock ||
				observations[index].BlockHash != checkpoint.LastScannedHash {
				return nil, 0, errors.New("充值记录与扫描检查点不属于同一区块")
			}
		} else if observations[index].BlockNumber > checkpoint.LastScannedBlock ||
			(observations[index].BlockNumber == checkpoint.LastScannedBlock && observations[index].BlockHash != checkpoint.LastScannedHash) {
			return nil, 0, errors.New("充值记录超出批量扫描检查点范围")
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
	item, err := s.scanDeposit(tx.QueryRow(ctx, `
		INSERT INTO deposits (
			user_id, address_id, network, asset, tx_hash, tx_index,
			block_number, block_hash, amount_wei, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10)
		ON CONFLICT (network, tx_hash, address_id) WHERE status <> 'ORPHANED' DO NOTHING
		RETURNING `+depositColumns,
		observation.UserID, observation.AddressID, NetworkSepolia, AssetETH,
		observation.TxHash, observation.TxIndex, observation.BlockNumber,
		observation.BlockHash, observation.AmountWei.String(), DepositConfirming,
	))
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Deposit{}, false, fmt.Errorf("写入充值记录失败：%w", err)
	}

	if created {
		if _, err := balanceForUpdate(ctx, s, tx, observation.UserID); err != nil {
			return Deposit{}, false, err
		}
		if err := adjustBalance(ctx, tx, balanceDelta{
			UserID:              observation.UserID,
			Asset:               AssetETH,
			PendingDepositDelta: observation.AmountWei,
		}, "增加待确认充值余额"); err != nil {
			return Deposit{}, false, err
		}
		if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
			UserID:        observation.UserID,
			Asset:         AssetETH,
			EntryType:     EntryDepositPending,
			Amount:        observation.AmountWei,
			ReferenceType: RefDeposit,
			ReferenceID:   item.ID,
		}, "写入待确认充值流水"); err != nil {
			return Deposit{}, false, err
		}
		return item, true, nil
	}

	existing, err := s.scanDeposit(tx.QueryRow(ctx, `
		SELECT `+depositColumns+`
		FROM deposits
		WHERE network = $1 AND tx_hash = $2 AND address_id = $3 AND status <> 'ORPHANED'`,
		NetworkSepolia, observation.TxHash, observation.AddressID,
	))
	if err != nil {
		return Deposit{}, false, fmt.Errorf("读取重复充值记录失败：%w", err)
	}
	if existing.UserID != observation.UserID || existing.AmountWei.Cmp(observation.AmountWei) != 0 {
		return Deposit{}, false, ErrIdempotencyConflict
	}
	if existing.TxIndex != observation.TxIndex || existing.BlockNumber != observation.BlockNumber || existing.BlockHash != observation.BlockHash {
		if existing.Status == DepositCredited {
			refID := existing.ID
			if _, err := recordWorkerErrorWith(ctx, tx, WorkerError{
				Worker:        "deposit-scanner",
				Stage:         "reorg_check",
				ReferenceType: RefDeposit,
				ReferenceID:   &refID,
				ErrorCode:     "CREDITED_DEPOSIT_REORG",
				ErrorMessage:  fmt.Sprintf("已入账充值 %d 在重组后位置变更为区块 %d (%s)，系统保持余额不变请人工核查", existing.ID, observation.BlockNumber, observation.BlockHash),
			}); err != nil {
				return Deposit{}, false, err
			}
		} else {
			updated, err := s.scanDeposit(tx.QueryRow(ctx, `
				UPDATE deposits SET
					tx_index = $2, block_number = $3, block_hash = $4, confirmations = 0, updated_at = CURRENT_TIMESTAMP
				WHERE id = $1
				RETURNING `+depositColumns,
				existing.ID, observation.TxIndex, observation.BlockNumber, observation.BlockHash,
			))
			if err != nil {
				return Deposit{}, false, fmt.Errorf("更新重组后充值区块信息失败：%w", err)
			}
			existing = updated
		}
	}
	return existing, false, nil
}

// OrphanDeposit 在链重组确认交易不再属于主链时将未入账充值置为 ORPHANED 并写入冲正流水。
func (s *Store) OrphanDeposit(ctx context.Context, depositID int64) (Deposit, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Deposit{}, false, fmt.Errorf("开启孤块充值冲正事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	item, err := lockRowForUpdate(ctx, tx, `SELECT `+depositColumns+` FROM deposits WHERE id = $1 FOR UPDATE`, depositID, s.scanDeposit, "锁定待冲正充值")
	if err != nil {
		return Deposit{}, false, err
	}
	if item.Status == DepositOrphaned {
		if err := tx.Commit(ctx); err != nil {
			return Deposit{}, false, fmt.Errorf("提交已孤立充值查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status == DepositCredited {
		return Deposit{}, false, ErrInvalidState
	}
	balance, err := balanceForUpdate(ctx, s, tx, item.UserID)
	if err != nil {
		return Deposit{}, false, err
	}
	if balance.PendingDepositWei.Cmp(item.AmountWei) < 0 {
		return Deposit{}, false, ErrPendingBalance
	}
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:              item.UserID,
		Asset:               AssetETH,
		PendingDepositDelta: new(big.Int).Neg(item.AmountWei),
	}, "扣减孤块充值待确认余额"); err != nil {
		return Deposit{}, false, err
	}
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:         item.UserID,
		Asset:          AssetETH,
		EntryType:      EntryDepositReversal,
		Amount:         new(big.Int).Neg(item.AmountWei),
		ReferenceType:  RefDeposit,
		ReferenceID:    item.ID,
		ConflictPolicy: entryConflictDoNothing,
	}, "写入孤块充值冲正流水"); err != nil {
		return Deposit{}, false, err
	}
	item, err = s.scanDeposit(tx.QueryRow(ctx, `
		UPDATE deposits SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+depositColumns, item.ID, DepositOrphaned,
	))
	if err != nil {
		return Deposit{}, false, fmt.Errorf("更新充值为孤块状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Deposit{}, false, fmt.Errorf("提交孤块充值冲正事务失败：%w", err)
	}
	return item, true, nil
}

// ListDepositsAfterBlock 查询指定区块高度之后需要复核主链一致性的非孤块充值记录。
func (s *Store) ListDepositsAfterBlock(ctx context.Context, afterBlock int64, limit int) ([]Deposit, error) {
	return s.ListDepositsAfterBlockCursor(ctx, afterBlock+1, afterBlock, 0, limit)
}

// ListDepositsAfterBlockCursor 使用键集游标分页查询不早于最小高度的非孤块充值记录。
func (s *Store) ListDepositsAfterBlockCursor(ctx context.Context, minBlock, afterBlock, afterID int64, limit int) ([]Deposit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+depositColumns+` FROM deposits
		WHERE block_number >= $1 AND status <> $2
		  AND (block_number > $3 OR (block_number = $3 AND id > $4))
		ORDER BY block_number, id LIMIT $5`,
		minBlock, DepositOrphaned, afterBlock, afterID, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询重组区间充值记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Deposit, 0)
	for rows.Next() {
		item, err := s.scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取重组区间充值记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListCreditedDepositsAfterCursor 使用键集游标分页查询需要独立审计主链一致性的已入账充值记录。
func (s *Store) ListCreditedDepositsAfterCursor(ctx context.Context, minBlock, afterBlock, afterID int64, limit int) ([]Deposit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+depositColumns+` FROM deposits
		WHERE block_number >= $1 AND status = $2
		  AND (block_number > $3 OR (block_number = $3 AND id > $4))
		ORDER BY block_number, id LIMIT $5`,
		minBlock, DepositCredited, afterBlock, afterID, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询已入账充值审计记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Deposit, 0)
	for rows.Next() {
		item, err := s.scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取已入账充值审计记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
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

	item, err := lockRowForUpdate(ctx, tx, `SELECT `+depositColumns+` FROM deposits WHERE id = $1 FOR UPDATE`, depositID, s.scanDeposit, "锁定充值记录")
	if err != nil {
		return Deposit{}, false, err
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
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:              item.UserID,
		Asset:               AssetETH,
		AvailableDelta:      item.AmountWei,
		PendingDepositDelta: new(big.Int).Neg(item.AmountWei),
	}, "充值金额转入可用余额"); err != nil {
		return Deposit{}, false, err
	}
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:        item.UserID,
		Asset:         AssetETH,
		EntryType:     EntryDepositCredit,
		Amount:        item.AmountWei,
		ReferenceType: RefDeposit,
		ReferenceID:   item.ID,
	}, "写入充值入账流水"); err != nil {
		return Deposit{}, false, err
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
