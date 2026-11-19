package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const (
	userCreationLockID int64 = 0x4d494e4955534552
	userColumns              = `id, code, display_name, status, note, created_at`
	adminColumns             = `id, username, password_hash, display_name, role, disabled, last_login_at, created_at, updated_at`
	walletColumns      = `id, user_id, network, address, derivation_index, derivation_path, next_nonce::text, created_at`
	balanceColumns     = `id, user_id, asset_id, asset, available_wei::text, pending_deposit_wei::text, pending_withdrawal_wei::text, version, updated_at`
	entryColumns       = `id, user_id, asset_id, asset, entry_type, amount_wei::text, reference_type, reference_id, created_at`
	depositColumns     = `id, user_id, address_id, network, asset, tx_hash, tx_index, block_number, block_hash, amount_wei::text, confirmations, status, created_at, updated_at`
	withdrawalColumns  = `id, idempotency_key, user_id, address_id, to_address, amount_wei::text, reserved_fee_wei::text, actual_fee_wei::text, nonce::text, gas_limit, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, raw_tx, tx_hash, block_number, confirmations, status, error_code, error_message, created_at, updated_at`
	checkpointColumns  = `id, network, scanner, last_scanned_block, last_scanned_hash, updated_at`
	workerErrorColumns = `id, worker, stage, reference_type, reference_id, error_code, error_message, retry_count, first_occurred_at, last_occurred_at`
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

var userCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,63}$`)

// EnsureBootstrapAdmin 在管理员表为空时幂等写入首个管理员账户，已存在管理员则不覆盖密码。
func (s *Store) EnsureBootstrapAdmin(ctx context.Context, username, passwordHash, displayName string) error {
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if username == "" || passwordHash == "" || displayName == "" {
		return errors.New("初始化管理员参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启管理员初始化事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var count int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return fmt.Errorf("查询管理员数量失败：%w", err)
	}
	if count == 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO admin_users (username, password_hash, display_name, role)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (username) DO NOTHING`,
			username, passwordHash, displayName, AdminRole,
		); err != nil {
			return fmt.Errorf("写入首个管理员失败：%w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交管理员初始化事务失败：%w", err)
	}
	return nil
}

// AdminByUsername 根据登录用户名查询管理员账户。
func (s *Store) AdminByUsername(ctx context.Context, username string) (AdminUser, error) {
	var item AdminUser
	err := s.pool.QueryRow(ctx, `SELECT `+adminColumns+` FROM admin_users WHERE username = $1`, strings.TrimSpace(username)).Scan(
		&item.ID, &item.Username, &item.PasswordHash, &item.DisplayName, &item.Role,
		&item.Disabled, &item.LastLoginAt, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return AdminUser{}, mapNotFound(err)
	}
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	if item.LastLoginAt != nil {
		local := s.localTime(*item.LastLoginAt)
		item.LastLoginAt = &local
	}
	return item, nil
}

// AdminByID 根据主键查询管理员账户。
func (s *Store) AdminByID(ctx context.Context, id int64) (AdminUser, error) {
	var item AdminUser
	err := s.pool.QueryRow(ctx, `SELECT `+adminColumns+` FROM admin_users WHERE id = $1`, id).Scan(
		&item.ID, &item.Username, &item.PasswordHash, &item.DisplayName, &item.Role,
		&item.Disabled, &item.LastLoginAt, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return AdminUser{}, mapNotFound(err)
	}
	item.CreatedAt = s.localTime(item.CreatedAt)
	item.UpdatedAt = s.localTime(item.UpdatedAt)
	if item.LastLoginAt != nil {
		local := s.localTime(*item.LastLoginAt)
		item.LastLoginAt = &local
	}
	return item, nil
}

// UpdateAdminLastLogin 更新管理员最近登录时间。
func (s *Store) UpdateAdminLastLogin(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE admin_users SET last_login_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = $1`, id)
	return err
}

// CreateManagedUser 在事务内串行分配派生索引，创建托管用户并派生 EVM（及可选 BTC）托管地址与初始余额行。
func (s *Store) CreateManagedUser(
	ctx context.Context,
	code, displayName, note string,
	evmProvider wallet.KeyProvider,
	btcProvider *wallet.MnemonicKeyProvider,
) (User, WalletAddress, error) {
	code = strings.TrimSpace(code)
	displayName = strings.TrimSpace(displayName)
	note = strings.TrimSpace(note)
	if !userCodePattern.MatchString(code) || displayName == "" || evmProvider == nil {
		return User{}, WalletAddress{}, ErrInvalidUserInput
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("开启创建托管用户事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, userCreationLockID); err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("获取用户开户锁失败：%w", err)
	}

	var nextEVMIndex int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(derivation_index), 0) + 1
		FROM wallet_addresses
		WHERE network = $1`, NetworkSepolia,
	).Scan(&nextEVMIndex); err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("计算 EVM 派生索引失败：%w", err)
	}
	if nextEVMIndex <= 0 || nextEVMIndex > int64(^uint32(0)) {
		return User{}, WalletAddress{}, errors.New("EVM 派生索引超出范围")
	}

	evmIndex := uint32(nextEVMIndex)
	evmPath := wallet.UserPath(evmIndex)
	derivedAddr, err := evmProvider.Address(ctx, evmPath)
	if err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("派生 EVM 托管地址失败：%w", err)
	}

	user, err := s.scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (code, display_name, status, note)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING `+userColumns,
		code, displayName, UserStatusActive, note,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, WalletAddress{}, ErrUserAlreadyExists
		}
		return User{}, WalletAddress{}, fmt.Errorf("写入托管用户失败：%w", err)
	}

	walletRow, err := s.scanWalletAddress(tx.QueryRow(ctx, `
		INSERT INTO wallet_addresses (user_id, network, address, derivation_index, derivation_path)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+walletColumns,
		user.ID, NetworkSepolia, strings.ToLower(derivedAddr.Hex()), evmIndex, evmPath,
	))
	if err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("写入 EVM 托管地址失败：%w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO asset_balances (user_id, asset_id, asset)
		SELECT $1, id, symbol
		FROM assets
		WHERE enabled = TRUE
		ON CONFLICT (user_id, asset_id) DO NOTHING`, user.ID,
	); err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("初始化托管用户余额失败：%w", err)
	}

	if btcProvider != nil {
		var nextBTCIndex int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(derivation_index), 0) + 1
			FROM btc_addresses
			WHERE network = $1 AND purpose = 'USER_DEPOSIT'`, NetworkBitcoinSignet,
		).Scan(&nextBTCIndex); err != nil {
			return User{}, WalletAddress{}, fmt.Errorf("计算 BTC 派生索引失败：%w", err)
		}
		if nextBTCIndex <= 0 || nextBTCIndex > int64(^uint32(0)) {
			return User{}, WalletAddress{}, errors.New("BTC 派生索引超出范围")
		}
		btcIndex := uint32(nextBTCIndex)
		btcPath := wallet.BitcoinUserPath(btcIndex)
		btcAddr, err := btcProvider.BitcoinAddress(ctx, btcPath)
		if err != nil {
			return User{}, WalletAddress{}, fmt.Errorf("派生 BTC 托管地址失败：%w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO btc_addresses (user_id, network, purpose, address, script_pub_key, derivation_index, derivation_path)
			VALUES ($1, $2, 'USER_DEPOSIT', $3, $4, $5, $6)
			ON CONFLICT (user_id) WHERE purpose = 'USER_DEPOSIT' DO NOTHING`,
			user.ID, NetworkBitcoinSignet, btcAddr.Address, fmt.Sprintf("%x", btcAddr.ScriptPubKey), btcIndex, btcPath,
		); err != nil {
			return User{}, WalletAddress{}, fmt.Errorf("写入 BTC 托管地址失败：%w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, WalletAddress{}, fmt.Errorf("提交创建托管用户事务失败：%w", err)
	}
	return user, walletRow, nil
}

// UpdateManagedUser 更新托管用户的展示名称、备注与启用状态。
func (s *Store) UpdateManagedUser(ctx context.Context, id int64, displayName, note, status string) (User, error) {
	if id <= 0 {
		return User{}, ErrInvalidUserInput
	}
	displayName = strings.TrimSpace(displayName)
	note = strings.TrimSpace(note)
	status = strings.ToUpper(strings.TrimSpace(status))
	if displayName == "" || (status != UserStatusActive && status != UserStatusDisabled) {
		return User{}, ErrInvalidUserInput
	}
	item, err := s.scanUser(s.pool.QueryRow(ctx, `
		UPDATE users
		SET display_name = $2, note = NULLIF($3, ''), status = $4
		WHERE id = $1
		RETURNING `+userColumns,
		id, displayName, note, status,
	))
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

// ListDepositsPage 按页倒序查询用户充值记录。
func (s *Store) ListDepositsPage(ctx context.Context, userID int64, limit, offset int) ([]Deposit, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+depositColumns+` FROM deposits WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
		userID, normalizedLimit(limit), normalizedOffset(offset),
	)
	if err != nil {
		return nil, fmt.Errorf("分页查询充值记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Deposit, 0)
	for rows.Next() {
		item, err := s.scanDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取分页充值记录失败：%w", err)
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

// WithdrawalByIdempotencyKey 根据用户和幂等标识查询原始提币请求。
func (s *Store) WithdrawalByIdempotencyKey(ctx context.Context, userID int64, idempotencyKey string) (Withdrawal, error) {
	item, err := s.scanWithdrawal(s.pool.QueryRow(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`,
		userID, strings.TrimSpace(idempotencyKey),
	))
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

// ListWithdrawalsPage 按页倒序查询用户提币记录。
func (s *Store) ListWithdrawalsPage(ctx context.Context, userID int64, limit, offset int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+withdrawalColumns+` FROM withdrawals WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`,
		userID, normalizedLimit(limit), normalizedOffset(offset),
	)
	if err != nil {
		return nil, fmt.Errorf("分页查询提币记录失败：%w", err)
	}
	defer rows.Close()
	items := make([]Withdrawal, 0)
	for rows.Next() {
		item, err := s.scanWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("读取分页提币记录失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListTransactionsPage 按时间倒序联合查询全部资产和任务流水。
func (s *Store) ListTransactionsPage(ctx context.Context, limit, offset int) ([]TransactionRecord, error) {
	return s.ListTransactionsFilteredPage(ctx, "", "", limit, offset)
}

// ListTransactionsFilteredPage 按资产和任务类型筛选统一流水投影。
func (s *Store) ListTransactionsFilteredPage(ctx context.Context, asset, transactionType string, limit, offset int) ([]TransactionRecord, error) {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	transactionType = strings.ToUpper(strings.TrimSpace(transactionType))
	rows, err := s.pool.Query(ctx, `
		SELECT transaction_type, id, user_id, asset, decimals, tx_hash, amount_wei, block_number,
		       confirmations, status, created_at, updated_at
		FROM (
			SELECT 'DEPOSIT'::text AS transaction_type, id, user_id, asset, 18::smallint AS decimals,
			       tx_hash, amount_wei::text AS amount_wei, block_number, confirmations, status, created_at, updated_at
			FROM deposits
			UNION ALL
			SELECT 'WITHDRAWAL'::text, id, user_id, 'ETH'::text, 18::smallint,
			       COALESCE(tx_hash, ''), amount_wei::text, block_number, confirmations, status, created_at, updated_at
			FROM withdrawals
			UNION ALL
			SELECT 'TOKEN_DEPOSIT'::text, deposit.id, deposit.user_id, asset.symbol, asset.decimals,
			       deposit.tx_hash, deposit.amount_units::text, deposit.block_number,
			       deposit.confirmations, deposit.status, deposit.created_at, deposit.updated_at
			FROM token_deposits deposit JOIN assets asset ON asset.id = deposit.asset_id
			UNION ALL
			SELECT 'TOKEN_WITHDRAWAL'::text, withdrawal.id, withdrawal.user_id, asset.symbol, asset.decimals,
			       COALESCE(withdrawal.tx_hash, ''), withdrawal.amount_units::text, withdrawal.block_number,
			       withdrawal.confirmations, withdrawal.status, withdrawal.created_at, withdrawal.updated_at
			FROM token_withdrawals withdrawal JOIN assets asset ON asset.id = withdrawal.asset_id
			UNION ALL
			SELECT 'TOKEN_SWEEP'::text, sweep.id, sweep.user_id, asset.symbol, asset.decimals,
			       COALESCE(sweep.tx_hash, ''), COALESCE(sweep.sweep_amount_units, sweep.recognized_amount_units)::text,
			       sweep.block_number, sweep.confirmations, sweep.status, sweep.created_at, sweep.updated_at
			FROM token_sweeps sweep JOIN assets asset ON asset.id = sweep.asset_id
			UNION ALL
			SELECT 'GAS_TOPUP'::text, transfer.id, sweep.user_id, 'ETH'::text, 18::smallint,
			       COALESCE(transfer.tx_hash, ''), transfer.amount_wei::text, transfer.block_number,
			       transfer.confirmations, transfer.status, transfer.created_at, transfer.updated_at
			FROM internal_transfers transfer JOIN token_sweeps sweep ON sweep.id = transfer.sweep_id
		) transactions
		WHERE ($1 = '' OR asset = $1) AND ($2 = '' OR transaction_type = $2)
		ORDER BY created_at DESC, transaction_type, id DESC
		LIMIT $3 OFFSET $4`, asset, transactionType, normalizedLimit(limit), normalizedOffset(offset),
	)
	if err != nil {
		return nil, fmt.Errorf("分页查询全局交易失败：%w", err)
	}
	defer rows.Close()
	items := make([]TransactionRecord, 0)
	for rows.Next() {
		item, err := s.scanTransactionRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("读取全局交易失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListWorkerErrorsPage 按最近发生时间倒序查询后台任务错误。
func (s *Store) ListWorkerErrorsPage(ctx context.Context, limit, offset int) ([]WorkerError, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+workerErrorColumns+` FROM worker_errors ORDER BY last_occurred_at DESC, id DESC LIMIT $1 OFFSET $2`,
		normalizedLimit(limit), normalizedOffset(offset),
	)
	if err != nil {
		return nil, fmt.Errorf("分页查询 Worker 错误失败：%w", err)
	}
	defer rows.Close()
	items := make([]WorkerError, 0)
	for rows.Next() {
		item, err := s.scanWorkerError(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 Worker 错误失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListProcessableWithdrawals 查询需要提币 Worker 继续处理或恢复的记录。
func (s *Store) ListProcessableWithdrawals(ctx context.Context, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+withdrawalColumns+`
		 FROM withdrawals
		 WHERE status IN ($1, $2, $3, $4, $5, $6, $7)
		 ORDER BY id
		 LIMIT $8`,
		WithdrawalCreated, WithdrawalSigning, WithdrawalSigned, WithdrawalBroadcasting,
		WithdrawalBroadcastUnknown, WithdrawalBroadcasted, WithdrawalConfirming, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待处理提币失败：%w", err)
	}
	defer rows.Close()
	items := make([]Withdrawal, 0)
	for rows.Next() {
		item, err := s.scanWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("读取待处理提币失败：%w", err)
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

// normalizedOffset 将分页偏移量限制为非负整数。
func normalizedOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

// mapNotFound 将 pgx 未找到错误映射为稳定的领域错误。
func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ensureUserActiveInTx 在事务内校验托管用户存在且处于启用状态。
func ensureUserActiveInTx(ctx context.Context, tx pgx.Tx, userID int64) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR SHARE`, userID).Scan(&status); err != nil {
		return mapNotFound(err)
	}
	if status != UserStatusActive {
		return ErrUserDisabled
	}
	return nil
}

// isUniqueViolation 判断 PostgreSQL 错误是否为唯一约束冲突。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
