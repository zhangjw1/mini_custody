package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	userColumns       = `id, code, display_name, created_at`
	walletColumns     = `id, user_id, network, address, derivation_index, derivation_path, next_nonce::text, created_at`
	balanceColumns    = `id, user_id, asset, available_wei::text, pending_deposit_wei::text, pending_withdrawal_wei::text, version, updated_at`
	entryColumns      = `id, user_id, asset, entry_type, amount_wei::text, reference_type, reference_id, created_at`
	depositColumns    = `id, user_id, address_id, network, asset, tx_hash, tx_index, block_number, block_hash, amount_wei::text, confirmations, status, created_at, updated_at`
	withdrawalColumns = `id, idempotency_key, user_id, address_id, to_address, amount_wei::text, reserved_fee_wei::text, actual_fee_wei::text, nonce::text, gas_limit, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, raw_tx, tx_hash, block_number, confirmations, status, error_code, error_message, created_at, updated_at`
	checkpointColumns = `id, network, scanner, last_scanned_block, last_scanned_hash, updated_at`
)

// ListUsers 按主键顺序查询全部演示用户。
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表失败：%w", err)
	}
	defer rows.Close()
	items := make([]User, 0)
	for rows.Next() {
		item, err := s.scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("读取用户数据失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// UserByID 根据自增主键查询用户。
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	item, err := s.scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// WalletAddressByUser 查询用户在 Sepolia 网络上的托管地址。
func (s *Store) WalletAddressByUser(ctx context.Context, userID int64) (WalletAddress, error) {
	item, err := s.scanWalletAddress(s.pool.QueryRow(ctx,
		`SELECT `+walletColumns+` FROM wallet_addresses WHERE user_id = $1 AND network = $2`,
		userID, NetworkSepolia,
	))
	return item, mapNotFound(err)
}

// ListWalletAddresses 查询 Sepolia 网络上的全部托管地址，供充值扫描器构建地址索引。
func (s *Store) ListWalletAddresses(ctx context.Context) ([]WalletAddress, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+walletColumns+` FROM wallet_addresses WHERE network = $1 ORDER BY id`,
		NetworkSepolia,
	)
	if err != nil {
		return nil, fmt.Errorf("查询托管钱包地址失败：%w", err)
	}
	defer rows.Close()
	items := make([]WalletAddress, 0)
	for rows.Next() {
		item, err := s.scanWalletAddress(rows)
		if err != nil {
			return nil, fmt.Errorf("读取托管钱包地址失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// BalanceByUser 查询用户的 ETH 余额汇总。
func (s *Store) BalanceByUser(ctx context.Context, userID int64) (AssetBalance, error) {
	item, err := s.scanBalance(s.pool.QueryRow(ctx,
		`SELECT `+balanceColumns+` FROM asset_balances WHERE user_id = $1 AND asset = $2`,
		userID, AssetETH,
	))
	return item, mapNotFound(err)
}

// ListBalanceEntries 倒序查询用户余额流水。
func (s *Store) ListBalanceEntries(ctx context.Context, userID int64, limit int) ([]BalanceEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+entryColumns+` FROM balance_entries WHERE user_id = $1 ORDER BY id DESC LIMIT $2`,
		userID, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询余额流水失败：%w", err)
	}
	defer rows.Close()
	items := make([]BalanceEntry, 0)
	for rows.Next() {
		item, err := s.scanBalanceEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("读取余额流水失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// DepositByID 根据自增主键查询充值记录。
func (s *Store) DepositByID(ctx context.Context, id int64) (Deposit, error) {
	item, err := s.scanDeposit(s.pool.QueryRow(ctx, `SELECT `+depositColumns+` FROM deposits WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// ListDeposits 倒序查询用户充值记录。
func (s *Store) ListDeposits(ctx context.Context, userID int64, limit int) ([]Deposit, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+depositColumns+` FROM deposits WHERE user_id = $1 ORDER BY id DESC LIMIT $2`,
		userID, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询充值记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Deposit, 0)
	for rows.Next() {
		item, err := s.scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取充值记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListConfirmingDeposits 查询仍需更新确认数或执行入账的充值记录。
func (s *Store) ListConfirmingDeposits(ctx context.Context, limit int) ([]Deposit, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+depositColumns+`
		 FROM deposits
		 WHERE network = $1 AND status IN ($2, $3)
		 ORDER BY block_number, id
		 LIMIT $4`,
		NetworkSepolia, DepositConfirming, DepositConfirmed, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待确认充值失败：%w", err)
	}
	defer rows.Close()
	items := make([]Deposit, 0)
	for rows.Next() {
		item, err := s.scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取待确认充值失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// WithdrawalByID 根据自增主键查询提币记录。
func (s *Store) WithdrawalByID(ctx context.Context, id int64) (Withdrawal, error) {
	item, err := s.scanWithdrawal(s.pool.QueryRow(ctx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// ListWithdrawals 倒序查询用户提币记录。
func (s *Store) ListWithdrawals(ctx context.Context, userID int64, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE user_id = $1 ORDER BY id DESC LIMIT $2`,
		userID, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询提币记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Withdrawal, 0)
	for rows.Next() {
		item, err := s.scanWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("读取提币记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Checkpoint 查询指定网络和扫描器的持久化检查点。
func (s *Store) Checkpoint(ctx context.Context, network, scanner string) (ChainCheckpoint, error) {
	item, err := s.scanCheckpoint(s.pool.QueryRow(ctx,
		`SELECT `+checkpointColumns+` FROM chain_checkpoints WHERE network = $1 AND scanner = $2`,
		network, scanner,
	))
	return item, mapNotFound(err)
}

// AdvanceCheckpoint 在事务中单调推进链扫描检查点。
func (s *Store) AdvanceCheckpoint(ctx context.Context, checkpoint ChainCheckpoint) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启扫描检查点事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := advanceCheckpoint(ctx, tx, checkpoint); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交扫描检查点事务失败：%w", err)
	}
	return nil
}

// advanceCheckpoint 在现有事务内写入或推进扫描检查点。
func advanceCheckpoint(ctx context.Context, tx pgx.Tx, checkpoint ChainCheckpoint) error {
	checkpoint.Network = strings.TrimSpace(checkpoint.Network)
	checkpoint.Scanner = strings.TrimSpace(checkpoint.Scanner)
	checkpoint.LastScannedHash = strings.ToLower(strings.TrimSpace(checkpoint.LastScannedHash))
	if checkpoint.Network == "" || checkpoint.Scanner == "" || checkpoint.LastScannedBlock < 0 || checkpoint.LastScannedHash == "" {
		return errors.New("扫描检查点参数无效")
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO chain_checkpoints (network, scanner, last_scanned_block, last_scanned_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (network, scanner) DO UPDATE SET
			last_scanned_block = EXCLUDED.last_scanned_block,
			last_scanned_hash = EXCLUDED.last_scanned_hash,
			updated_at = CURRENT_TIMESTAMP
		WHERE chain_checkpoints.last_scanned_block < EXCLUDED.last_scanned_block
		   OR (chain_checkpoints.last_scanned_block = EXCLUDED.last_scanned_block
		       AND chain_checkpoints.last_scanned_hash = EXCLUDED.last_scanned_hash)`,
		checkpoint.Network, checkpoint.Scanner, checkpoint.LastScannedBlock, checkpoint.LastScannedHash,
	)
	if err != nil {
		return fmt.Errorf("推进扫描检查点失败：%w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCheckpointConflict
	}
	return nil
}

// RecordWorkerError 保存经过清洗的后台任务错误信息。
func (s *Store) RecordWorkerError(ctx context.Context, item WorkerError) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO worker_errors (
			worker, stage, reference_type, reference_id, error_code, error_message, retry_count
		) VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)
		RETURNING id`,
		strings.TrimSpace(item.Worker), strings.TrimSpace(item.Stage), strings.TrimSpace(item.ReferenceType),
		item.ReferenceID, strings.TrimSpace(item.ErrorCode), strings.TrimSpace(item.ErrorMessage), item.RetryCount,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("记录后台任务错误失败：%w", err)
	}
	return id, nil
}

// normalizedLimit 将分页数量限制在安全范围内。
func normalizedLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// mapNotFound 将 pgx 未找到错误映射为稳定的领域错误。
func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// isUniqueViolation 判断 PostgreSQL 错误是否为唯一约束冲突。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
