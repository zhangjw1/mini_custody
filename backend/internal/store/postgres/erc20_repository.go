package postgres

import (
	"context"
	"fmt"
	"strings"
)

const (
	assetColumns            = `id, network, asset_type, symbol, contract_address, decimals, enabled, created_at, updated_at`
	platformWalletColumns   = `id, network, role, address, derivation_path, next_nonce::text, created_at, updated_at`
	tokenDepositColumns     = `id, user_id, address_id, asset_id, tx_hash, log_index, block_number, block_hash, from_address, to_address, amount_units::text, confirmations, status, created_at, updated_at`
	tokenSweepColumns       = `id, user_id, address_id, asset_id, trigger_deposit_id, recognized_amount_units::text, sweep_amount_units::text, gas_topup_transfer_id, nonce::text, gas_limit, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, raw_tx, tx_hash, block_number, confirmations, actual_fee_wei::text, status, error_code, error_message, created_at, updated_at`
	internalTransferColumns = `id, platform_wallet_id, sweep_id, transfer_type, from_address, to_address, amount_wei::text, nonce::text, gas_limit, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, raw_tx, tx_hash, block_number, confirmations, actual_fee_wei::text, status, error_code, error_message, created_at, updated_at`
	tokenWithdrawalColumns  = `id, idempotency_key, user_id, asset_id, platform_wallet_id, to_address, amount_units::text, nonce::text, gas_limit, max_fee_per_gas_wei::text, max_priority_fee_per_gas_wei::text, raw_tx, tx_hash, block_number, confirmations, actual_fee_wei::text, status, error_code, error_message, created_at, updated_at`
)

// ListAssets 按主键顺序查询全部资产配置。
func (s *Store) ListAssets(ctx context.Context) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assetColumns+` FROM assets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询资产列表失败：%w", err)
	}
	defer rows.Close()
	items := make([]Asset, 0)
	for rows.Next() {
		item, err := s.scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("读取资产数据失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// AssetBySymbol 根据网络和 symbol 查询资产配置。
func (s *Store) AssetBySymbol(ctx context.Context, network, symbol string) (Asset, error) {
	item, err := s.scanAsset(s.pool.QueryRow(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE network = $1 AND symbol = $2`,
		strings.TrimSpace(network), strings.ToUpper(strings.TrimSpace(symbol)),
	))
	return item, mapNotFound(err)
}

// AssetByContract 根据网络和小写合约地址查询 ERC-20 资产。
func (s *Store) AssetByContract(ctx context.Context, network, contractAddress string) (Asset, error) {
	item, err := s.scanAsset(s.pool.QueryRow(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE network = $1 AND contract_address = $2`,
		strings.TrimSpace(network), strings.ToLower(strings.TrimSpace(contractAddress)),
	))
	return item, mapNotFound(err)
}

// BalanceByUserAndAsset 查询用户指定资产的余额汇总。
func (s *Store) BalanceByUserAndAsset(ctx context.Context, userID, assetID int64) (AssetBalance, error) {
	item, err := s.scanBalance(s.pool.QueryRow(ctx,
		`SELECT `+balanceColumns+` FROM asset_balances WHERE user_id = $1 AND asset_id = $2`,
		userID, assetID,
	))
	return item, mapNotFound(err)
}

// PlatformWalletByRole 查询指定网络和角色的平台钱包。
func (s *Store) PlatformWalletByRole(ctx context.Context, network, role string) (PlatformWallet, error) {
	item, err := s.scanPlatformWallet(s.pool.QueryRow(ctx,
		`SELECT `+platformWalletColumns+` FROM platform_wallets WHERE network = $1 AND role = $2`,
		strings.TrimSpace(network), strings.ToUpper(strings.TrimSpace(role)),
	))
	return item, mapNotFound(err)
}

// TokenDepositByID 根据主键查询 Token 充值记录。
func (s *Store) TokenDepositByID(ctx context.Context, id int64) (TokenDeposit, error) {
	item, err := s.scanTokenDeposit(s.pool.QueryRow(ctx, `SELECT `+tokenDepositColumns+` FROM token_deposits WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// ListConfirmingTokenDeposits 查询仍需确认或入账的 Token 充值记录。
func (s *Store) ListConfirmingTokenDeposits(ctx context.Context, assetID int64, limit int) ([]TokenDeposit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenDepositColumns+` FROM token_deposits
		WHERE asset_id = $1 AND status IN ($2, $3)
		ORDER BY block_number, log_index, id LIMIT $4`,
		assetID, DepositConfirming, DepositConfirmed, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待确认 Token 充值失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenDeposit, 0)
	for rows.Next() {
		item, err := s.scanTokenDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取待确认 Token 充值失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// TokenSweepByID 根据主键查询 Token 归集任务。
func (s *Store) TokenSweepByID(ctx context.Context, id int64) (TokenSweep, error) {
	item, err := s.scanTokenSweep(s.pool.QueryRow(ctx, `SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// ListProcessableTokenSweeps 查询需要归集 Worker 继续处理的任务。
func (s *Store) ListProcessableTokenSweeps(ctx context.Context, limit int) ([]TokenSweep, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenSweepColumns+` FROM token_sweeps
		WHERE status IN ($1, $2, $3, $4) OR (
			status = $5 AND (
				gas_topup_transfer_id IS NULL OR EXISTS (
					SELECT 1 FROM internal_transfers WHERE id = gas_topup_transfer_id AND status = $6
				)
			)
		)
		ORDER BY id LIMIT $7`,
		TokenSweepSigning, TokenSweepSigned, TokenSweepBroadcasted, TokenSweepConfirming,
		TokenSweepWaitingGas, InternalTransferDone, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待处理 Token 归集失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenSweep, 0)
	for rows.Next() {
		item, err := s.scanTokenSweep(rows)
		if err != nil {
			return nil, fmt.Errorf("读取待处理 Token 归集失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// InternalTransferByID 根据主键查询平台内部转账。
func (s *Store) InternalTransferByID(ctx context.Context, id int64) (InternalTransfer, error) {
	item, err := s.scanInternalTransfer(s.pool.QueryRow(ctx, `SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// InternalTransferByTxHash 根据交易哈希查询平台内部转账。
func (s *Store) InternalTransferByTxHash(ctx context.Context, txHash string) (InternalTransfer, error) {
	item, err := s.scanInternalTransfer(s.pool.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE tx_hash = $1`,
		strings.ToLower(strings.TrimSpace(txHash)),
	))
	return item, mapNotFound(err)
}

// TokenWithdrawalByID 根据主键查询 Token 提币任务。
func (s *Store) TokenWithdrawalByID(ctx context.Context, id int64) (TokenWithdrawal, error) {
	item, err := s.scanTokenWithdrawal(s.pool.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1`, id))
	return item, mapNotFound(err)
}

// TokenWithdrawalByIdempotencyKey 根据用户和幂等标识查询 Token 提币任务。
func (s *Store) TokenWithdrawalByIdempotencyKey(ctx context.Context, userID int64, idempotencyKey string) (TokenWithdrawal, error) {
	item, err := s.scanTokenWithdrawal(s.pool.QueryRow(ctx,
		`SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE user_id = $1 AND idempotency_key = $2`,
		userID, strings.TrimSpace(idempotencyKey),
	))
	return item, mapNotFound(err)
}

// ListProcessableTokenWithdrawals 查询需要 Token 提币 Worker 继续处理的任务。
func (s *Store) ListProcessableTokenWithdrawals(ctx context.Context, limit int) ([]TokenWithdrawal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals
		WHERE status IN ($1, $2, $3, $4, $5)
		ORDER BY id LIMIT $6`,
		WithdrawalCreated, WithdrawalSigning, WithdrawalSigned, WithdrawalBroadcasted,
		WithdrawalConfirming, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待处理 Token 提币失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenWithdrawal, 0)
	for rows.Next() {
		item, err := s.scanTokenWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("读取待处理 Token 提币失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListTokenDepositsPage 按页倒序查询用户 Token 充值记录。
func (s *Store) ListTokenDepositsPage(ctx context.Context, userID int64, limit, offset int) ([]TokenDeposit, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenDepositColumns+` FROM token_deposits WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`, userID, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, fmt.Errorf("分页查询 Token 充值失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenDeposit, 0)
	for rows.Next() {
		item, err := s.scanTokenDeposit(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 Token 充值失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListTokenWithdrawalsPage 按页倒序查询用户 Token 提币记录。
func (s *Store) ListTokenWithdrawalsPage(ctx context.Context, userID int64, limit, offset int) ([]TokenWithdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3`, userID, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, fmt.Errorf("分页查询 Token 提币失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenWithdrawal, 0)
	for rows.Next() {
		item, err := s.scanTokenWithdrawal(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 Token 提币失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListTokenSweepsPage 按页倒序查询 Token 归集任务。
func (s *Store) ListTokenSweepsPage(ctx context.Context, limit, offset int) ([]TokenSweep, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenSweepColumns+` FROM token_sweeps ORDER BY id DESC LIMIT $1 OFFSET $2`, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, fmt.Errorf("分页查询 Token 归集任务失败：%w", err)
	}
	defer rows.Close()
	items := make([]TokenSweep, 0)
	for rows.Next() {
		item, err := s.scanTokenSweep(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 Token 归集任务失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListInternalTransfersPage 按页倒序查询平台内部 Gas 转账。
func (s *Store) ListInternalTransfersPage(ctx context.Context, limit, offset int) ([]InternalTransfer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+internalTransferColumns+` FROM internal_transfers ORDER BY id DESC LIMIT $1 OFFSET $2`, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, fmt.Errorf("分页查询内部转账失败：%w", err)
	}
	defer rows.Close()
	items := make([]InternalTransfer, 0)
	for rows.Next() {
		item, err := s.scanInternalTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("读取内部转账失败：%w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
