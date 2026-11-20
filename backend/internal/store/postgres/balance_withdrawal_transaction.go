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

// WithdrawalRequest 描述创建 ETH 提币及其费用占用所需的请求参数。
type WithdrawalRequest struct {
	IdempotencyKey string
	UserID         int64
	AddressID      int64
	ToAddress      string
	AmountWei      *big.Int
	ReservedFeeWei *big.Int
}

// WithdrawalSettlement 描述 ETH 提币回执最终结算参数。
type WithdrawalSettlement struct {
	WithdrawalID  int64
	ActualFeeWei  *big.Int
	Success       bool
	BlockNumber   int64
	Confirmations int64
	TxHash        string
}

// SignedWithdrawal 描述广播前需要落库的 ETH 提币签名交易参数。
type SignedWithdrawal struct {
	WithdrawalID            int64
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	RawTx                   []byte
	TxHash                  string
}

var allowedWithdrawalTransitions = map[string]map[string]bool{
	WithdrawalCreated:          {WithdrawalSigning: true},
	WithdrawalSigning:          {WithdrawalSigned: true},
	WithdrawalSigned:           {WithdrawalBroadcasting: true, WithdrawalManualReview: true},
	WithdrawalBroadcasting:     {WithdrawalBroadcasted: true, WithdrawalBroadcastUnknown: true, WithdrawalManualReview: true},
	WithdrawalBroadcastUnknown: {WithdrawalBroadcasted: true, WithdrawalManualReview: true},
	WithdrawalBroadcasted:      {WithdrawalConfirming: true, WithdrawalManualReview: true},
	WithdrawalConfirming:       {WithdrawalManualReview: true},
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
	if err := ensureUserActiveInTx(ctx, tx, request.UserID); err != nil {
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
			existing.AmountWei.Cmp(request.AmountWei) != 0 {
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
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:                 request.UserID,
		Asset:                  AssetETH,
		AvailableDelta:         new(big.Int).Neg(total),
		PendingWithdrawalDelta: total,
	}, "占用提币余额"); err != nil {
		return Withdrawal{}, false, err
	}
	reservedEntry := new(big.Int).Neg(new(big.Int).Set(total))
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:        request.UserID,
		Asset:         AssetETH,
		EntryType:     EntryWithdrawReserve,
		Amount:        reservedEntry,
		ReferenceType: RefWithdrawal,
		ReferenceID:   withdrawalID,
	}, "写入提币占用流水"); err != nil {
		return Withdrawal{}, false, err
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

// IncreaseWithdrawalFee 在签名前原子补充更高的最大网络费占用。
func (s *Store) IncreaseWithdrawalFee(ctx context.Context, withdrawalID int64, newReservedFeeWei *big.Int) (Withdrawal, bool, error) {
	if err := amount.RequireNonNegative(newReservedFeeWei); err != nil {
		return Withdrawal{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币费用调整事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID, s.scanWithdrawal, "锁定提币记录")
	if err != nil {
		return Withdrawal{}, false, err
	}
	if item.Status != WithdrawalCreated && item.Status != WithdrawalSigning {
		return Withdrawal{}, false, ErrInvalidState
	}
	if newReservedFeeWei.Cmp(item.ReservedFeeWei) <= 0 {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交无需调整的提币费用事务失败：%w", err)
		}
		return item, false, nil
	}
	difference := new(big.Int).Sub(newReservedFeeWei, item.ReservedFeeWei)
	balance, err := balanceForUpdate(ctx, s, tx, item.UserID)
	if err != nil {
		return Withdrawal{}, false, err
	}
	if balance.AvailableWei.Cmp(difference) < 0 {
		return Withdrawal{}, false, ErrInsufficientBalance
	}
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:                 item.UserID,
		Asset:                  AssetETH,
		AvailableDelta:         new(big.Int).Neg(difference),
		PendingWithdrawalDelta: difference,
	}, "补充提币费用占用"); err != nil {
		return Withdrawal{}, false, err
	}
	entryAmount := new(big.Int).Neg(new(big.Int).Set(difference))
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:         item.UserID,
		Asset:          AssetETH,
		EntryType:      EntryFeeAdjust,
		Amount:         entryAmount,
		ReferenceType:  RefWithdrawal,
		ReferenceID:    item.ID,
		ConflictPolicy: entryConflictAddAmount,
	}, "写入提币费用调整流水"); err != nil {
		return Withdrawal{}, false, err
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET reserved_fee_wei = $2::numeric, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns, item.ID, newReservedFeeWei.String(),
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("更新提币预留费用失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币费用调整事务失败：%w", err)
	}
	return item, true, nil
}

// AllocateWithdrawalNonce 在地址行锁下选择链上与数据库中的较大 Nonce 并推进提币状态。
func (s *Store) AllocateWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64) (Withdrawal, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启提币 Nonce 分配事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID, s.scanWithdrawal, "锁定提币记录")
	if err != nil {
		return Withdrawal{}, false, err
	}
	if item.Status == WithdrawalSigning && item.Nonce != nil {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交已有 Nonce 的提币事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalCreated {
		return Withdrawal{}, false, ErrInvalidState
	}
	var databaseNonceText string
	if err := tx.QueryRow(ctx, `
		SELECT next_nonce::text FROM wallet_addresses
		WHERE id = $1 AND user_id = $2 AND network = $3 FOR UPDATE`,
		item.AddressID, item.UserID, NetworkSepolia,
	).Scan(&databaseNonceText); errors.Is(err, pgx.ErrNoRows) {
		return Withdrawal{}, false, ErrNotFound
	} else if err != nil {
		return Withdrawal{}, false, fmt.Errorf("锁定钱包地址 Nonce 失败：%w", err)
	}
	databaseNonce, err := parseDatabaseWei(databaseNonceText, "next_nonce")
	if err != nil {
		return Withdrawal{}, false, err
	}
	nonce := new(big.Int).SetUint64(chainPendingNonce)
	if databaseNonce.Cmp(nonce) > 0 {
		nonce.Set(databaseNonce)
	}
	nextNonce := new(big.Int).Add(nonce, big.NewInt(1))
	if _, err := tx.Exec(ctx, `UPDATE wallet_addresses SET next_nonce = $2::numeric WHERE id = $1`, item.AddressID, nextNonce.String()); err != nil {
		return Withdrawal{}, false, fmt.Errorf("推进钱包地址 Nonce 失败：%w", err)
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET nonce = $2::numeric, status = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, nonce.String(), WithdrawalSigning,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("保存提币 Nonce 失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币 Nonce 分配事务失败：%w", err)
	}
	return item, true, nil
}

// SaveSignedWithdrawal 在广播前持久化已签名原始交易、哈希和费用参数。
func (s *Store) SaveSignedWithdrawal(ctx context.Context, signed SignedWithdrawal) (Withdrawal, bool, error) {
	if signed.WithdrawalID <= 0 || signed.GasLimit == 0 || signed.GasLimit > uint64(math.MaxInt64) || len(signed.RawTx) == 0 {
		return Withdrawal{}, false, errors.New("已签名提币参数无效")
	}
	if err := amount.RequirePositive(signed.MaxFeePerGasWei); err != nil {
		return Withdrawal{}, false, err
	}
	if err := amount.RequireNonNegative(signed.MaxPriorityFeePerGasWei); err != nil {
		return Withdrawal{}, false, err
	}
	signed.TxHash = strings.ToLower(strings.TrimSpace(signed.TxHash))
	if !transactionHashPattern.MatchString(signed.TxHash) {
		return Withdrawal{}, false, errors.New("已签名提币交易哈希无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("开启签名交易保存事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, signed.WithdrawalID, s.scanWithdrawal, "锁定待签名提币")
	if err != nil {
		return Withdrawal{}, false, err
	}
	if item.Status == WithdrawalSigned {
		if !bytes.Equal(item.RawTx, signed.RawTx) || item.TxHash != signed.TxHash {
			return Withdrawal{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交已签名提币查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Status != WithdrawalSigning || item.Nonce == nil {
		return Withdrawal{}, false, ErrInvalidState
	}
	var fromAddress string
	if err := tx.QueryRow(ctx, `SELECT address FROM wallet_addresses WHERE id = $1`, item.AddressID).Scan(&fromAddress); err != nil {
		return Withdrawal{}, false, fmt.Errorf("查询提币发送地址失败：%w", mapNotFound(err))
	}
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET
			gas_limit = $2, max_fee_per_gas_wei = $3::numeric,
			max_priority_fee_per_gas_wei = $4::numeric, raw_tx = $5,
			tx_hash = $6, status = $7, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, int64(signed.GasLimit), signed.MaxFeePerGasWei.String(),
		signed.MaxPriorityFeePerGasWei.String(), signed.RawTx, signed.TxHash, WithdrawalSigned,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("保存已签名提币失败：%w", err)
	}
	if err := insertEVMTxAttemptInTx(ctx, tx, EVMTxAttempt{
		BusinessType:            EVMBusinessWithdrawal,
		BusinessID:              item.ID,
		FromAddress:             fromAddress,
		Nonce:                   item.Nonce,
		AttemptNo:               1,
		TxHash:                  signed.TxHash,
		RawTx:                   signed.RawTx,
		MaxFeePerGasWei:         signed.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: signed.MaxPriorityFeePerGasWei,
		GasLimit:                int64(signed.GasLimit),
	}); err != nil {
		return Withdrawal{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交签名交易保存事务失败：%w", err)
	}
	return item, true, nil
}

// UpdateWithdrawalConfirmations 单调更新已广播提币的确认数并进入确认中状态。
func (s *Store) UpdateWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (Withdrawal, error) {
	if confirmations < 0 {
		return Withdrawal{}, errors.New("提币确认数不能为负数")
	}
	item, err := s.scanWithdrawal(s.pool.QueryRow(ctx, `
		UPDATE withdrawals SET
			confirmations = GREATEST(confirmations, $2),
			status = CASE WHEN status = $3 THEN $4 ELSE status END,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ($3, $4)
		RETURNING `+withdrawalColumns,
		withdrawalID, confirmations, WithdrawalBroadcasted, WithdrawalConfirming,
	))
	return item, mapNotFound(err)
}

// TransitionWithdrawal 按允许的状态机迁移提币状态。
func (s *Store) TransitionWithdrawal(ctx context.Context, withdrawalID int64, target string) (Withdrawal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("开启提币状态迁移事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID, s.scanWithdrawal, "锁定提币记录")
	if err != nil {
		return Withdrawal{}, err
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, fmt.Errorf("提交幂等提币状态事务失败：%w", err)
		}
		return item, nil
	}
	if !allowedWithdrawalTransition(item.Status, target) {
		return Withdrawal{}, ErrInvalidState
	}
	if target == WithdrawalBroadcasted {
		item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
			UPDATE withdrawals SET
				status = $2,
				broadcasted_at = COALESCE(broadcasted_at, CURRENT_TIMESTAMP),
				last_broadcast_at = CURRENT_TIMESTAMP,
				updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 RETURNING `+withdrawalColumns, withdrawalID, target,
		))
	} else {
		item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
			UPDATE withdrawals SET status = $2, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 RETURNING `+withdrawalColumns, withdrawalID, target,
		))
	}
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
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID, s.scanWithdrawal, "锁定提币记录")
	if err != nil {
		return Withdrawal{}, false, err
	}
	if item.Status == WithdrawalFailed {
		if err := tx.Commit(ctx); err != nil {
			return Withdrawal{}, false, fmt.Errorf("提交已释放提币查询事务失败：%w", err)
		}
		return item, false, nil
	}
	if item.Nonce != nil || item.Status != WithdrawalCreated {
		return Withdrawal{}, false, ErrUnsafeRelease
	}
	total := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	if _, err := lockPendingWithdrawal(ctx, s, tx, item.UserID, total); err != nil {
		return Withdrawal{}, false, err
	}
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:                 item.UserID,
		Asset:                  AssetETH,
		AvailableDelta:         total,
		PendingWithdrawalDelta: new(big.Int).Neg(total),
	}, "释放提币占用余额"); err != nil {
		return Withdrawal{}, false, err
	}
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:        item.UserID,
		Asset:         AssetETH,
		EntryType:     EntryWithdrawRelease,
		Amount:        total,
		ReferenceType: RefWithdrawal,
		ReferenceID:   item.ID,
	}, "写入提币释放流水"); err != nil {
		return Withdrawal{}, false, err
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
	item, err := lockRowForUpdate(ctx, tx, `SELECT `+withdrawalColumns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, settlement.WithdrawalID, s.scanWithdrawal, "锁定提币记录")
	if err != nil {
		return Withdrawal{}, false, err
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
	if err := adjustBalance(ctx, tx, balanceDelta{
		UserID:                 item.UserID,
		Asset:                  AssetETH,
		AvailableDelta:         refund,
		PendingWithdrawalDelta: new(big.Int).Neg(total),
	}, "结算提币余额"); err != nil {
		return Withdrawal{}, false, err
	}
	entryAmount := new(big.Int).Neg(new(big.Int).Set(netSpent))
	if err := insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:        item.UserID,
		Asset:         AssetETH,
		EntryType:     EntryWithdrawFinalize,
		Amount:        entryAmount,
		ReferenceType: RefWithdrawal,
		ReferenceID:   item.ID,
	}, "写入提币结算流水"); err != nil {
		return Withdrawal{}, false, err
	}
	matchedTxHash := strings.ToLower(strings.TrimSpace(settlement.TxHash))
	item, err = s.scanWithdrawal(tx.QueryRow(ctx, `
		UPDATE withdrawals SET
			actual_fee_wei = $2::numeric, block_number = $3, confirmations = $4,
			status = $5, error_code = NULLIF($6, ''), error_message = NULLIF($7, ''),
			tx_hash = COALESCE(NULLIF($8, ''), tx_hash),
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 RETURNING `+withdrawalColumns,
		item.ID, settlement.ActualFeeWei.String(), settlement.BlockNumber, settlement.Confirmations,
		status, errorCode, errorMessage, matchedTxHash,
	))
	if err != nil {
		return Withdrawal{}, false, fmt.Errorf("记录提币结算结果失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Withdrawal{}, false, fmt.Errorf("提交提币结算事务失败：%w", err)
	}
	return item, true, nil
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

// allowedWithdrawalTransition 判断提币状态迁移是否合法。
func allowedWithdrawalTransition(current, target string) bool {
	return allowedWithdrawalTransitions[current][target]
}
