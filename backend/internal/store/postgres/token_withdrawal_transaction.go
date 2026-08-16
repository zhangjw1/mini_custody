package postgres

import (
	"bytes"
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

// TokenWithdrawalRequest 描述创建 Token 提币所需的确定性参数。
type TokenWithdrawalRequest struct {
	IdempotencyKey   string
	UserID           int64
	AssetID          int64
	PlatformWalletID int64
	ToAddress        string
	AmountUnits      *big.Int
}

// SignedTokenWithdrawal 描述广播前必须持久化的 Token 提币签名结果。
type SignedTokenWithdrawal struct {
	WithdrawalID            int64
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
}

// TokenWithdrawalSettlement 描述 Token 提币 Receipt 的最终结算数据。
type TokenWithdrawalSettlement struct {
	WithdrawalID  int64
	ActualFeeWei  *big.Int
	Success       bool
	BlockNumber   int64
	Confirmations int64
	ErrorCode     string
	ErrorMessage  string
}

// ReserveTokenWithdrawal 幂等创建 Token 提币并原子占用用户 Token 可用余额。
func (s *Store) ReserveTokenWithdrawal(ctx context.Context, request TokenWithdrawalRequest) (TokenWithdrawal, bool, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.ToAddress = strings.ToLower(strings.TrimSpace(request.ToAddress))
	if request.IdempotencyKey == "" || request.UserID <= 0 || request.AssetID <= 0 || request.PlatformWalletID <= 0 ||
		!common.IsHexAddress(request.ToAddress) || common.HexToAddress(request.ToAddress) == (common.Address{}) {
		return TokenWithdrawal{}, false, errors.New("Token 提币请求参数无效")
	}
	if err := amount.RequirePositive(request.AmountUnits); err != nil {
		return TokenWithdrawal{}, false, err
	}
	request.ToAddress = strings.ToLower(common.HexToAddress(request.ToAddress).Hex())
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币占用事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	balance, err := tokenBalanceForUpdate(ctx, s, tx, request.UserID, request.AssetID)
	if err != nil {
		return TokenWithdrawal{}, false, err
	}
	var assetType string
	if err := tx.QueryRow(ctx, `SELECT asset_type FROM assets WHERE id = $1 AND enabled FOR SHARE`, request.AssetID).Scan(&assetType); err != nil {
		return TokenWithdrawal{}, false, mapNotFound(err)
	}
	if assetType != AssetTypeERC20 {
		return TokenWithdrawal{}, false, errors.New("Token 提币资产类型必须是 ERC20")
	}
	var platformNetwork, platformRole string
	if err := tx.QueryRow(ctx, `SELECT network, role FROM platform_wallets WHERE id = $1 FOR SHARE`, request.PlatformWalletID).Scan(&platformNetwork, &platformRole); err != nil {
		return TokenWithdrawal{}, false, mapNotFound(err)
	}
	if platformNetwork != NetworkSepolia || platformRole != PlatformRoleHot {
		return TokenWithdrawal{}, false, errors.New("Token 提币平台钱包配置无效")
	}
	var withdrawalID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO token_withdrawals (
			idempotency_key, user_id, asset_id, platform_wallet_id, to_address, amount_units, status
		) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING id`, request.IdempotencyKey, request.UserID, request.AssetID, request.PlatformWalletID,
		request.ToAddress, request.AmountUnits.String(), WithdrawalCreated,
	).Scan(&withdrawalID)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, fmt.Errorf("写入 Token 提币记录失败：%w", err)
	}
	if !created {
		existing, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `
			SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals
			WHERE user_id = $1 AND idempotency_key = $2`, request.UserID, request.IdempotencyKey,
		))
		if err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("读取幂等 Token 提币失败：%w", err)
		}
		if existing.AssetID != request.AssetID || existing.PlatformWalletID != request.PlatformWalletID ||
			existing.ToAddress != request.ToAddress || existing.AmountUnits.Cmp(request.AmountUnits) != 0 {
			return TokenWithdrawal{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交幂等 Token 提币查询失败：%w", err)
		}
		return existing, false, nil
	}
	if balance.AvailableWei.Cmp(request.AmountUnits) < 0 {
		return TokenWithdrawal{}, false, ErrInsufficientBalance
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET
			available_wei = available_wei - $3::numeric,
			pending_withdrawal_wei = pending_withdrawal_wei + $3::numeric,
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset_id = $2`, request.UserID, request.AssetID, request.AmountUnits.String(),
	); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("占用 Token 提币余额失败：%w", err)
	}
	entryAmount := new(big.Int).Neg(new(big.Int).Set(request.AmountUnits))
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, $3, 'WITHDRAW_RESERVE', $4::numeric, 'TOKEN_WITHDRAWAL', $5)`,
		request.UserID, request.AssetID, balance.Asset, entryAmount.String(), withdrawalID,
	); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("写入 Token 提币占用流水失败：%w", err)
	}
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1`, withdrawalID))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("读取已创建 Token 提币失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币占用事务失败：%w", err)
	}
	return item, true, nil
}

// AllocateTokenWithdrawalNonce 在平台钱包行锁下分配共享 Nonce 并推进到签名状态。
func (s *Store) AllocateTokenWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64) (TokenWithdrawal, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币 Nonce 事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定 Token 提币失败：%w", err)
	}
	if item.Status == WithdrawalSigning && item.Nonce != nil {
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交已有 Token 提币 Nonce 查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalCreated {
		return TokenWithdrawal{}, false, ErrInvalidState
	}
	platform, err := s.scanPlatformWallet(tx.QueryRow(ctx, `SELECT `+platformWalletColumns+` FROM platform_wallets WHERE id = $1 FOR UPDATE`, item.PlatformWalletID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定 Token 提币平台钱包失败：%w", err)
	}
	if platform.Network != NetworkSepolia || platform.Role != PlatformRoleHot {
		return TokenWithdrawal{}, false, ErrWalletKeyMismatch
	}
	nonce := new(big.Int).SetUint64(chainPendingNonce)
	if platform.NextNonce.Cmp(nonce) > 0 {
		nonce.Set(platform.NextNonce)
	}
	nextNonce := new(big.Int).Add(new(big.Int).Set(nonce), big.NewInt(1))
	if _, err := tx.Exec(ctx, `UPDATE platform_wallets SET next_nonce = $2::numeric, updated_at = CURRENT_TIMESTAMP WHERE id = $1`, platform.ID, nextNonce.String()); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("推进 Token 提币平台钱包 Nonce 失败：%w", err)
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET nonce = $2::numeric, status = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns, item.ID, nonce.String(), WithdrawalSigning,
	))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("保存 Token 提币 Nonce 失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币 Nonce 事务失败：%w", err)
	}
	return item, true, nil
}

// SaveSignedTokenWithdrawal 在广播前幂等保存 Token 提币原始交易、哈希和 Gas 参数。
func (s *Store) SaveSignedTokenWithdrawal(ctx context.Context, signed SignedTokenWithdrawal) (TokenWithdrawal, bool, error) {
	if signed.WithdrawalID <= 0 || signed.GasLimit == 0 || signed.GasLimit > math.MaxInt64 || len(signed.RawTx) == 0 {
		return TokenWithdrawal{}, false, errors.New("已签名 Token 提币参数无效")
	}
	if err := amount.RequirePositive(signed.MaxFeePerGasWei); err != nil {
		return TokenWithdrawal{}, false, err
	}
	if err := amount.RequireNonNegative(signed.MaxPriorityFeePerGasWei); err != nil {
		return TokenWithdrawal{}, false, err
	}
	signed.TxHash = strings.ToLower(strings.TrimSpace(signed.TxHash))
	if !transactionHashPattern.MatchString(signed.TxHash) {
		return TokenWithdrawal{}, false, errors.New("已签名 Token 提币交易哈希无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币签名保存事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, signed.WithdrawalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定待签名 Token 提币失败：%w", err)
	}
	if item.Status == WithdrawalSigned {
		if !bytes.Equal(item.RawTx, signed.RawTx) || item.TxHash != signed.TxHash {
			return TokenWithdrawal{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交已签名 Token 提币查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalSigning || item.Nonce == nil {
		return TokenWithdrawal{}, false, ErrInvalidState
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET gas_limit = $2, max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric, raw_tx = $5, tx_hash = $6,
			status = $7, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns, item.ID, int64(signed.GasLimit),
		signed.MaxFeePerGasWei.String(), signed.MaxPriorityFeePerGasWei.String(), signed.RawTx,
		signed.TxHash, WithdrawalSigned,
	))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("保存已签名 Token 提币失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币签名保存事务失败：%w", err)
	}
	return item, true, nil
}

// TransitionTokenWithdrawal 按允许的状态机迁移 Token 提币状态。
func (s *Store) TransitionTokenWithdrawal(ctx context.Context, withdrawalID int64, target string) (TokenWithdrawal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, fmt.Errorf("开启 Token 提币状态事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, fmt.Errorf("锁定 Token 提币状态失败：%w", err)
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, fmt.Errorf("提交幂等 Token 提币状态失败：%w", err)
		}
		return item, nil
	}
	allowed := (item.Status == WithdrawalSigned && target == WithdrawalBroadcasted) ||
		(item.Status == WithdrawalBroadcasted && target == WithdrawalConfirming)
	if !allowed {
		return TokenWithdrawal{}, ErrInvalidState
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns, withdrawalID, target,
	))
	if err != nil {
		return TokenWithdrawal{}, fmt.Errorf("迁移 Token 提币状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, fmt.Errorf("提交 Token 提币状态事务失败：%w", err)
	}
	return item, nil
}

// UpdateTokenWithdrawalConfirmations 单调更新 Token 提币确认数并进入确认中状态。
func (s *Store) UpdateTokenWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (TokenWithdrawal, error) {
	if confirmations < 0 {
		return TokenWithdrawal{}, errors.New("Token 提币确认数不能为负数")
	}
	item, err := s.scanTokenWithdrawal(s.pool.QueryRow(ctx, `
		UPDATE token_withdrawals SET confirmations = GREATEST(confirmations, $2),
			status = CASE WHEN status = $3 THEN $4 ELSE status END, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ($3, $4) RETURNING `+tokenWithdrawalColumns,
		withdrawalID, confirmations, WithdrawalBroadcasted, WithdrawalConfirming,
	))
	return item, mapNotFound(err)
}

// FinalizeTokenWithdrawal 根据 Receipt 结算用户 Token 占用并记录平台实际 Gas。
func (s *Store) FinalizeTokenWithdrawal(ctx context.Context, settlement TokenWithdrawalSettlement) (TokenWithdrawal, bool, error) {
	if err := amount.RequireNonNegative(settlement.ActualFeeWei); err != nil {
		return TokenWithdrawal{}, false, err
	}
	if settlement.WithdrawalID <= 0 || settlement.BlockNumber < 0 || settlement.Confirmations <= 0 {
		return TokenWithdrawal{}, false, errors.New("Token 提币结算参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("开启 Token 提币结算事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenWithdrawal(tx.QueryRow(ctx, `SELECT `+tokenWithdrawalColumns+` FROM token_withdrawals WHERE id = $1 FOR UPDATE`, settlement.WithdrawalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenWithdrawal{}, false, ErrNotFound
	}
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("锁定待结算 Token 提币失败：%w", err)
	}
	if item.Status == WithdrawalCompleted || (item.Status == WithdrawalFailed && item.ActualFeeWei != nil) {
		if err := tx.Commit(ctx); err != nil {
			return TokenWithdrawal{}, false, fmt.Errorf("提交已结算 Token 提币查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalBroadcasted && item.Status != WithdrawalConfirming {
		return TokenWithdrawal{}, false, ErrInvalidState
	}
	balance, err := tokenBalanceForUpdate(ctx, s, tx, item.UserID, item.AssetID)
	if err != nil {
		return TokenWithdrawal{}, false, err
	}
	if balance.PendingWithdrawalWei.Cmp(item.AmountUnits) < 0 {
		return TokenWithdrawal{}, false, ErrPendingBalance
	}
	status := WithdrawalCompleted
	entryType := "WITHDRAW_FINALIZE"
	entryAmount := new(big.Int).Neg(new(big.Int).Set(item.AmountUnits))
	availableIncrease := new(big.Int)
	errorCode := strings.TrimSpace(settlement.ErrorCode)
	errorMessage := strings.TrimSpace(settlement.ErrorMessage)
	if !settlement.Success {
		status = WithdrawalFailed
		entryType = "WITHDRAW_RELEASE"
		entryAmount.Set(item.AmountUnits)
		availableIncrease.Set(item.AmountUnits)
		if errorCode == "" {
			errorCode = "TOKEN_WITHDRAWAL_REVERTED"
		}
		if errorMessage == "" {
			errorMessage = "Token 提币交易链上执行失败"
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE asset_balances SET available_wei = available_wei + $3::numeric,
			pending_withdrawal_wei = pending_withdrawal_wei - $4::numeric,
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE user_id = $1 AND asset_id = $2`, item.UserID, item.AssetID,
		availableIncrease.String(), item.AmountUnits.String(),
	); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("结算 Token 提币余额失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO balance_entries (
			user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
		) VALUES ($1, $2, $3, $4, $5::numeric, 'TOKEN_WITHDRAWAL', $6)`,
		item.UserID, item.AssetID, balance.Asset, entryType, entryAmount.String(), item.ID,
	); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("写入 Token 提币结算流水失败：%w", err)
	}
	item, err = s.scanTokenWithdrawal(tx.QueryRow(ctx, `
		UPDATE token_withdrawals SET actual_fee_wei = $2::numeric, block_number = $3,
			confirmations = $4, status = $5, error_code = NULLIF($6, ''),
			error_message = NULLIF($7, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenWithdrawalColumns, item.ID, settlement.ActualFeeWei.String(),
		settlement.BlockNumber, settlement.Confirmations, status, errorCode, errorMessage,
	))
	if err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("保存 Token 提币结算结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWithdrawal{}, false, fmt.Errorf("提交 Token 提币结算事务失败：%w", err)
	}
	return item, true, nil
}
