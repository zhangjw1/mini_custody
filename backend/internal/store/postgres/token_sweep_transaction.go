package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
)

// SignedTokenSweep 描述广播前必须持久化的 Token 归集签名结果。
type SignedTokenSweep struct {
	SweepID                 int64
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
}

// TokenSweepSettlement 描述 Token 归集 Receipt 的最终结算数据。
type TokenSweepSettlement struct {
	SweepID       int64
	ActualFeeWei  *big.Int
	Success       bool
	BlockNumber   int64
	Confirmations int64
	ErrorCode     string
	ErrorMessage  string
}

// AllocateTokenSweepNonce 在用户地址行锁下固定归集金额、分配 Nonce 并进入签名状态。
func (s *Store) AllocateTokenSweepNonce(ctx context.Context, sweepID int64, sweepAmountUnits *big.Int, chainPendingNonce uint64) (TokenSweep, bool, error) {
	if err := amount.RequirePositive(sweepAmountUnits); err != nil {
		return TokenSweep{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启 Token 归集 Nonce 事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, sweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定 Token 归集任务失败：%w", err)
	}
	if item.Status == TokenSweepSigning && item.Nonce != nil && item.SweepAmountUnits != nil {
		if item.SweepAmountUnits.Cmp(sweepAmountUnits) != 0 {
			return TokenSweep{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交已有 Token 归集 Nonce 查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != TokenSweepWaitingGas || item.Nonce != nil || item.SweepAmountUnits != nil {
		return TokenSweep{}, false, ErrInvalidState
	}
	if sweepAmountUnits.Cmp(item.RecognizedAmountUnits) > 0 {
		return TokenSweep{}, false, errors.New("归集金额超过系统已识别 Token 金额")
	}
	if item.GasTopupTransferID != nil {
		var gasStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM internal_transfers WHERE id = $1 FOR UPDATE`, *item.GasTopupTransferID).Scan(&gasStatus); err != nil {
			return TokenSweep{}, false, fmt.Errorf("锁定归集 Gas 补充交易失败：%w", err)
		}
		if gasStatus != InternalTransferDone {
			return TokenSweep{}, false, errors.New("归集 Gas 补充交易尚未完成")
		}
	}
	var databaseNonceText string
	if err := tx.QueryRow(ctx, `
		SELECT next_nonce::text FROM wallet_addresses
		WHERE id = $1 AND user_id = $2 AND network = $3 FOR UPDATE`,
		item.AddressID, item.UserID, NetworkSepolia,
	).Scan(&databaseNonceText); errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	} else if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定归集来源地址 Nonce 失败：%w", err)
	}
	databaseNonce, err := parseDatabaseWei(databaseNonceText, "token sweep wallet next_nonce")
	if err != nil {
		return TokenSweep{}, false, err
	}
	nonce := new(big.Int).SetUint64(chainPendingNonce)
	if databaseNonce.Cmp(nonce) > 0 {
		nonce.Set(databaseNonce)
	}
	nextNonce := new(big.Int).Add(new(big.Int).Set(nonce), big.NewInt(1))
	if _, err := tx.Exec(ctx, `UPDATE wallet_addresses SET next_nonce = $2::numeric WHERE id = $1`, item.AddressID, nextNonce.String()); err != nil {
		return TokenSweep{}, false, fmt.Errorf("推进归集来源地址 Nonce 失败：%w", err)
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET sweep_amount_units = $2::numeric, nonce = $3::numeric,
			status = $4, error_code = NULL, error_message = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns,
		item.ID, sweepAmountUnits.String(), nonce.String(), TokenSweepSigning,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("保存 Token 归集金额和 Nonce 失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交 Token 归集 Nonce 事务失败：%w", err)
	}
	return item, true, nil
}

// SaveSignedTokenSweep 在广播前幂等保存归集原始交易、哈希和 Gas 参数。
func (s *Store) SaveSignedTokenSweep(ctx context.Context, signed SignedTokenSweep) (TokenSweep, bool, error) {
	if signed.SweepID <= 0 || signed.GasLimit == 0 || signed.GasLimit > math.MaxInt64 || len(signed.RawTx) == 0 {
		return TokenSweep{}, false, errors.New("已签名 Token 归集参数无效")
	}
	if err := amount.RequirePositive(signed.MaxFeePerGasWei); err != nil {
		return TokenSweep{}, false, err
	}
	if err := amount.RequireNonNegative(signed.MaxPriorityFeePerGasWei); err != nil {
		return TokenSweep{}, false, err
	}
	signed.TxHash = strings.ToLower(strings.TrimSpace(signed.TxHash))
	if !transactionHashPattern.MatchString(signed.TxHash) {
		return TokenSweep{}, false, errors.New("已签名 Token 归集交易哈希无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启 Token 归集签名保存事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, signed.SweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定待签名 Token 归集失败：%w", err)
	}
	if item.Status == TokenSweepSigned {
		if !bytes.Equal(item.RawTx, signed.RawTx) || item.TxHash != signed.TxHash {
			return TokenSweep{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交已签名 Token 归集查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != TokenSweepSigning || item.Nonce == nil || item.SweepAmountUnits == nil {
		return TokenSweep{}, false, ErrInvalidState
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET gas_limit = $2, max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric, raw_tx = $5, tx_hash = $6,
			status = $7, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns,
		item.ID, int64(signed.GasLimit), signed.MaxFeePerGasWei.String(),
		signed.MaxPriorityFeePerGasWei.String(), signed.RawTx, signed.TxHash, TokenSweepSigned,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("保存已签名 Token 归集失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交 Token 归集签名保存事务失败：%w", err)
	}
	return item, true, nil
}

// TransitionTokenSweep 按归集状态机幂等迁移 Token 归集任务。
func (s *Store) TransitionTokenSweep(ctx context.Context, sweepID int64, target string) (TokenSweep, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, fmt.Errorf("开启 Token 归集状态事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, sweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, fmt.Errorf("锁定 Token 归集状态失败：%w", err)
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, fmt.Errorf("提交 Token 归集幂等状态失败：%w", err)
		}
		return item, nil
	}
	allowed := (item.Status == TokenSweepSigned && target == TokenSweepBroadcasted) ||
		(item.Status == TokenSweepBroadcasted && target == TokenSweepConfirming)
	if !allowed {
		return TokenSweep{}, ErrInvalidState
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns, item.ID, target,
	))
	if err != nil {
		return TokenSweep{}, fmt.Errorf("更新 Token 归集状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, fmt.Errorf("提交 Token 归集状态事务失败：%w", err)
	}
	return item, nil
}

// UpdateTokenSweepConfirmations 单调更新归集确认数并进入确认中状态。
func (s *Store) UpdateTokenSweepConfirmations(ctx context.Context, sweepID, confirmations int64) (TokenSweep, error) {
	if confirmations < 0 {
		return TokenSweep{}, errors.New("Token 归集确认数不能为负数")
	}
	item, err := s.scanTokenSweep(s.pool.QueryRow(ctx, `
		UPDATE token_sweeps SET confirmations = GREATEST(confirmations, $2),
			status = CASE WHEN status = $3 THEN $4 ELSE status END, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ($3, $4) RETURNING `+tokenSweepColumns,
		sweepID, confirmations, TokenSweepBroadcasted, TokenSweepConfirming,
	))
	return item, mapNotFound(err)
}

// FinalizeTokenSweep 保存归集 Receipt 和实际 Gas，成功时为签名后新增余额创建后继任务。
func (s *Store) FinalizeTokenSweep(ctx context.Context, settlement TokenSweepSettlement) (TokenSweep, bool, error) {
	settlement.ErrorCode = strings.TrimSpace(settlement.ErrorCode)
	settlement.ErrorMessage = strings.TrimSpace(settlement.ErrorMessage)
	if settlement.SweepID <= 0 || settlement.BlockNumber < 0 || settlement.Confirmations <= 0 {
		return TokenSweep{}, false, errors.New("Token 归集结算参数无效")
	}
	if err := amount.RequireNonNegative(settlement.ActualFeeWei); err != nil {
		return TokenSweep{}, false, err
	}
	if settlement.Success && (settlement.ErrorCode != "" || settlement.ErrorMessage != "") {
		return TokenSweep{}, false, errors.New("成功的 Token 归集不能包含错误信息")
	}
	if !settlement.Success && (settlement.ErrorCode == "" || settlement.ErrorMessage == "") {
		return TokenSweep{}, false, errors.New("失败的 Token 归集必须包含错误信息")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启 Token 归集结算事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, settlement.SweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定待结算 Token 归集失败：%w", err)
	}
	if item.Status == TokenSweepCompleted || item.Status == TokenSweepFailed {
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交已结算 Token 归集查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != TokenSweepBroadcasted && item.Status != TokenSweepConfirming || item.SweepAmountUnits == nil {
		return TokenSweep{}, false, ErrInvalidState
	}
	status := TokenSweepCompleted
	if !settlement.Success {
		status = TokenSweepFailed
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET block_number = $2, confirmations = GREATEST(confirmations, $3),
			actual_fee_wei = $4::numeric, status = $5, error_code = NULLIF($6, ''),
			error_message = NULLIF($7, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns,
		item.ID, settlement.BlockNumber, settlement.Confirmations, settlement.ActualFeeWei.String(),
		status, settlement.ErrorCode, settlement.ErrorMessage,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("保存 Token 归集结算失败：%w", err)
	}
	if settlement.Success {
		remaining := new(big.Int).Sub(item.RecognizedAmountUnits, item.SweepAmountUnits)
		if remaining.Sign() > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO token_sweeps (
					user_id, address_id, asset_id, trigger_deposit_id, recognized_amount_units, status
				) VALUES ($1, $2, $3, $4, $5::numeric, $6)`,
				item.UserID, item.AddressID, item.AssetID, item.TriggerDepositID,
				remaining.String(), TokenSweepCreated,
			); err != nil {
				return TokenSweep{}, false, fmt.Errorf("创建剩余 Token 后继归集任务失败：%w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交 Token 归集结算事务失败：%w", err)
	}
	return item, true, nil
}

// FailWaitingTokenSweep 在分配 Nonce 前将确定性不可执行的归集任务标记失败。
func (s *Store) FailWaitingTokenSweep(ctx context.Context, sweepID int64, errorCode, errorMessage string) (TokenSweep, bool, error) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if sweepID <= 0 || errorCode == "" || errorMessage == "" {
		return TokenSweep{}, false, errors.New("Token 归集失败参数无效")
	}
	item, err := s.scanTokenSweep(s.pool.QueryRow(ctx, `
		UPDATE token_sweeps SET status = $2, error_code = $3, error_message = $4,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $5 AND nonce IS NULL AND raw_tx IS NULL
		RETURNING `+tokenSweepColumns,
		sweepID, TokenSweepFailed, errorCode, errorMessage, TokenSweepWaitingGas,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		current, currentErr := s.TokenSweepByID(ctx, sweepID)
		if currentErr != nil {
			return TokenSweep{}, false, currentErr
		}
		if current.Status == TokenSweepFailed {
			return current, false, nil
		}
		return TokenSweep{}, false, ErrInvalidState
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("标记等待中 Token 归集失败：%w", err)
	}
	return item, true, nil
}
