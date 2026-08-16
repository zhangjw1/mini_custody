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

// GasTopupRequest 描述创建 Gas 补充内部转账所需的确定性参数。
type GasTopupRequest struct {
	SweepID           int64
	PlatformWalletID  int64
	FromAddress       string
	ToAddress         string
	AmountWei         *big.Int
	ChainPendingNonce uint64
}

// SignedInternalTransfer 描述广播前必须持久化的内部转账签名结果。
type SignedInternalTransfer struct {
	TransferID              int64
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
}

// InternalTransferSettlement 描述内部转账 Receipt 的最终结算数据。
type InternalTransferSettlement struct {
	TransferID    int64
	ActualFeeWei  *big.Int
	Success       bool
	BlockNumber   int64
	Confirmations int64
}

// ListGasStationSweeps 查询尚未准备 Gas 或仍有补气交易需要恢复的归集任务。
func (s *Store) ListGasStationSweeps(ctx context.Context, limit int) ([]TokenSweep, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sweep.id FROM token_sweeps sweep
		LEFT JOIN internal_transfers transfer ON transfer.id = sweep.gas_topup_transfer_id
		WHERE sweep.status = $1 OR (
			sweep.status = $2 AND transfer.status IN ($3, $4, $5, $6, $7)
		)
		ORDER BY sweep.id LIMIT $8`,
		TokenSweepCreated, TokenSweepWaitingGas, InternalTransferCreated, InternalTransferSigning,
		InternalTransferSigned, InternalTransferSent, InternalTransferChecking, normalizedLimit(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("查询待处理 Gas 补充任务失败：%w", err)
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取 Gas 补充任务 ID 失败：%w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("遍历 Gas 补充任务失败：%w", err)
	}
	rows.Close()
	items := make([]TokenSweep, 0, len(ids))
	for _, id := range ids {
		item, err := s.TokenSweepByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("读取 Gas 补充归集任务失败：%w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

// MarkTokenSweepGasReady 将无需补气的归集任务推进到等待归集状态。
func (s *Store) MarkTokenSweepGasReady(ctx context.Context, sweepID int64) (TokenSweep, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("开启归集 Gas 状态事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, sweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSweep{}, false, ErrNotFound
	}
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("锁定归集任务失败：%w", err)
	}
	if item.Status == TokenSweepWaitingGas && item.GasTopupTransferID == nil {
		if err := tx.Commit(ctx); err != nil {
			return TokenSweep{}, false, fmt.Errorf("提交归集 Gas 幂等事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != TokenSweepCreated || item.GasTopupTransferID != nil {
		return TokenSweep{}, false, ErrInvalidState
	}
	item, err = s.scanTokenSweep(tx.QueryRow(ctx, `
		UPDATE token_sweeps SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+tokenSweepColumns, item.ID, TokenSweepWaitingGas,
	))
	if err != nil {
		return TokenSweep{}, false, fmt.Errorf("更新归集 Gas 状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenSweep{}, false, fmt.Errorf("提交归集 Gas 状态事务失败：%w", err)
	}
	return item, true, nil
}

// CreateOrGetGasTopup 在归集任务和平台钱包行锁下幂等创建补气交易并分配 Nonce。
func (s *Store) CreateOrGetGasTopup(ctx context.Context, request GasTopupRequest) (InternalTransfer, bool, error) {
	request.FromAddress = strings.ToLower(strings.TrimSpace(request.FromAddress))
	request.ToAddress = strings.ToLower(strings.TrimSpace(request.ToAddress))
	if request.SweepID <= 0 || request.PlatformWalletID <= 0 || !common.IsHexAddress(request.FromAddress) ||
		!common.IsHexAddress(request.ToAddress) || request.FromAddress == request.ToAddress {
		return InternalTransfer{}, false, errors.New("Gas 补充请求参数无效")
	}
	if err := amount.RequirePositive(request.AmountWei); err != nil {
		return InternalTransfer{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("开启 Gas 补充创建事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	sweep, err := s.scanTokenSweep(tx.QueryRow(ctx,
		`SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE id = $1 FOR UPDATE`, request.SweepID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定 Gas 补充归集任务失败：%w", err)
	}
	if sweep.GasTopupTransferID != nil {
		item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
			`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1`, *sweep.GasTopupTransferID,
		))
		if err != nil {
			return InternalTransfer{}, false, fmt.Errorf("读取已有 Gas 补充交易失败：%w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("提交已有 Gas 补充查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if sweep.Status != TokenSweepCreated {
		return InternalTransfer{}, false, ErrInvalidState
	}
	platform, err := s.scanPlatformWallet(tx.QueryRow(ctx,
		`SELECT `+platformWalletColumns+` FROM platform_wallets WHERE id = $1 FOR UPDATE`, request.PlatformWalletID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定平台热钱包失败：%w", err)
	}
	if platform.Network != NetworkSepolia || platform.Role != PlatformRoleHot || platform.Address != request.FromAddress {
		return InternalTransfer{}, false, ErrWalletKeyMismatch
	}
	nonce := new(big.Int).SetUint64(request.ChainPendingNonce)
	if platform.NextNonce.Cmp(nonce) > 0 {
		nonce.Set(platform.NextNonce)
	}
	nextNonce := new(big.Int).Add(new(big.Int).Set(nonce), big.NewInt(1))
	if _, err := tx.Exec(ctx, `
		UPDATE platform_wallets SET next_nonce = $2::numeric, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`, platform.ID, nextNonce.String()); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("推进平台热钱包 Nonce 失败：%w", err)
	}
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx, `
		INSERT INTO internal_transfers (
			platform_wallet_id, sweep_id, transfer_type, from_address, to_address,
			amount_wei, nonce, status
		) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8)
		RETURNING `+internalTransferColumns,
		platform.ID, sweep.ID, InternalTransferGasTopup, request.FromAddress, request.ToAddress,
		request.AmountWei.String(), nonce.String(), InternalTransferSigning,
	))
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("创建 Gas 补充内部转账失败：%w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE token_sweeps SET gas_topup_transfer_id = $2, status = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1`, sweep.ID, item.ID, TokenSweepWaitingGas); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("关联归集任务和 Gas 补充交易失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("提交 Gas 补充创建事务失败：%w", err)
	}
	return item, true, nil
}

// SaveSignedInternalTransfer 在广播前幂等保存内部转账原始交易、哈希和 Gas 参数。
func (s *Store) SaveSignedInternalTransfer(ctx context.Context, signed SignedInternalTransfer) (InternalTransfer, bool, error) {
	if signed.TransferID <= 0 || signed.GasLimit == 0 || signed.GasLimit > math.MaxInt64 || len(signed.RawTx) == 0 {
		return InternalTransfer{}, false, errors.New("已签名内部转账参数无效")
	}
	if err := amount.RequirePositive(signed.MaxFeePerGasWei); err != nil {
		return InternalTransfer{}, false, err
	}
	if err := amount.RequireNonNegative(signed.MaxPriorityFeePerGasWei); err != nil {
		return InternalTransfer{}, false, err
	}
	signed.TxHash = strings.ToLower(strings.TrimSpace(signed.TxHash))
	if !transactionHashPattern.MatchString(signed.TxHash) {
		return InternalTransfer{}, false, errors.New("已签名内部转账哈希无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("开启内部转账签名保存事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1 FOR UPDATE`, signed.TransferID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定内部转账失败：%w", err)
	}
	if item.Status == InternalTransferSigned {
		if !bytes.Equal(item.RawTx, signed.RawTx) || item.TxHash != signed.TxHash {
			return InternalTransfer{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("提交已签名内部转账查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != InternalTransferSigning || item.Nonce == nil {
		return InternalTransfer{}, false, ErrInvalidState
	}
	item, err = s.scanInternalTransfer(tx.QueryRow(ctx, `
		UPDATE internal_transfers SET gas_limit = $2, max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric, raw_tx = $5, tx_hash = $6,
			status = $7, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+internalTransferColumns,
		item.ID, int64(signed.GasLimit), signed.MaxFeePerGasWei.String(),
		signed.MaxPriorityFeePerGasWei.String(), signed.RawTx, signed.TxHash, InternalTransferSigned,
	))
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("保存已签名内部转账失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("提交内部转账签名保存事务失败：%w", err)
	}
	return item, true, nil
}

// TransitionInternalTransfer 按补气状态机幂等迁移内部转账。
func (s *Store) TransitionInternalTransfer(ctx context.Context, transferID int64, target string) (InternalTransfer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, fmt.Errorf("开启内部转账状态事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1 FOR UPDATE`, transferID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, fmt.Errorf("锁定内部转账状态失败：%w", err)
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, fmt.Errorf("提交内部转账幂等状态失败：%w", err)
		}
		return item, nil
	}
	allowed := (item.Status == InternalTransferSigned && target == InternalTransferSent) ||
		(item.Status == InternalTransferSent && target == InternalTransferChecking)
	if !allowed {
		return InternalTransfer{}, ErrInvalidState
	}
	item, err = s.scanInternalTransfer(tx.QueryRow(ctx, `
		UPDATE internal_transfers SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+internalTransferColumns, item.ID, target,
	))
	if err != nil {
		return InternalTransfer{}, fmt.Errorf("更新内部转账状态失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, fmt.Errorf("提交内部转账状态事务失败：%w", err)
	}
	return item, nil
}

// UpdateInternalTransferConfirmations 单调更新补气交易确认数并进入确认中状态。
func (s *Store) UpdateInternalTransferConfirmations(ctx context.Context, transferID, confirmations int64) (InternalTransfer, error) {
	if confirmations < 0 {
		return InternalTransfer{}, errors.New("内部转账确认数不能为负数")
	}
	item, err := s.scanInternalTransfer(s.pool.QueryRow(ctx, `
		UPDATE internal_transfers SET confirmations = GREATEST(confirmations, $2),
			status = CASE WHEN status = $3 THEN $4 ELSE status END, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ($3, $4) RETURNING `+internalTransferColumns,
		transferID, confirmations, InternalTransferSent, InternalTransferChecking,
	))
	return item, mapNotFound(err)
}

// FinalizeInternalTransfer 保存补气 Receipt 和实际费用，失败时同时终止对应归集任务。
func (s *Store) FinalizeInternalTransfer(ctx context.Context, settlement InternalTransferSettlement) (InternalTransfer, bool, error) {
	if settlement.TransferID <= 0 || settlement.BlockNumber < 0 || settlement.Confirmations <= 0 {
		return InternalTransfer{}, false, errors.New("内部转账结算参数无效")
	}
	if err := amount.RequireNonNegative(settlement.ActualFeeWei); err != nil {
		return InternalTransfer{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("开启内部转账结算事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := s.scanInternalTransfer(tx.QueryRow(ctx,
		`SELECT `+internalTransferColumns+` FROM internal_transfers WHERE id = $1 FOR UPDATE`, settlement.TransferID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return InternalTransfer{}, false, ErrNotFound
	}
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("锁定待结算内部转账失败：%w", err)
	}
	if item.Status == InternalTransferDone || item.Status == InternalTransferFailed {
		if err := tx.Commit(ctx); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("提交已结算内部转账查询失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != InternalTransferSent && item.Status != InternalTransferChecking {
		return InternalTransfer{}, false, ErrInvalidState
	}
	status := InternalTransferDone
	errorCode := ""
	errorMessage := ""
	if !settlement.Success {
		status = InternalTransferFailed
		errorCode = "GAS_TOPUP_REVERTED"
		errorMessage = "Gas 补充交易链上执行失败"
	}
	item, err = s.scanInternalTransfer(tx.QueryRow(ctx, `
		UPDATE internal_transfers SET block_number = $2, confirmations = GREATEST(confirmations, $3),
			actual_fee_wei = $4::numeric, status = $5, error_code = NULLIF($6, ''),
			error_message = NULLIF($7, ''), updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+internalTransferColumns,
		item.ID, settlement.BlockNumber, settlement.Confirmations, settlement.ActualFeeWei.String(),
		status, errorCode, errorMessage,
	))
	if err != nil {
		return InternalTransfer{}, false, fmt.Errorf("保存内部转账结算失败：%w", err)
	}
	if !settlement.Success {
		if _, err := tx.Exec(ctx, `
			UPDATE token_sweeps SET status = $2, error_code = $3, error_message = $4,
				updated_at = CURRENT_TIMESTAMP WHERE id = $1`,
			item.SweepID, TokenSweepFailed, errorCode, errorMessage,
		); err != nil {
			return InternalTransfer{}, false, fmt.Errorf("更新补气失败归集任务失败：%w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return InternalTransfer{}, false, fmt.Errorf("提交内部转账结算事务失败：%w", err)
	}
	return item, true, nil
}

// IsInternalTransferTx 判断交易哈希是否属于已登记的平台内部转账。
func (s *Store) IsInternalTransferTx(ctx context.Context, txHash string) (bool, error) {
	txHash = strings.ToLower(strings.TrimSpace(txHash))
	if !transactionHashPattern.MatchString(txHash) {
		return false, errors.New("内部转账查询哈希无效")
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM internal_transfers WHERE tx_hash = $1)`, txHash).Scan(&exists); err != nil {
		return false, fmt.Errorf("查询内部转账哈希失败：%w", err)
	}
	return exists, nil
}
