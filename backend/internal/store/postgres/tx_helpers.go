package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
)

type entryConflictPolicy int

const (
	entryConflictNone entryConflictPolicy = iota
	entryConflictDoNothing
	entryConflictAddAmount
)

// balanceDelta 描述对单个用户资产余额三栏位的原子增减量。
type balanceDelta struct {
	UserID                 int64
	AssetID                *int64
	Asset                  string
	AvailableDelta         *big.Int
	PendingDepositDelta    *big.Int
	PendingWithdrawalDelta *big.Int
}

// balanceEntryInput 描述写入 balance_entries 账本流水所需参数与幂等冲突策略。
type balanceEntryInput struct {
	UserID         int64
	AssetID        *int64
	Asset          string
	EntryType      string
	Amount         *big.Int
	ReferenceType  string
	ReferenceID    int64
	ConflictPolicy entryConflictPolicy
}

// lockRowForUpdate 在事务中按主键加排他锁读取单条记录，不存在时返回 ErrNotFound。
func lockRowForUpdate[T any](ctx context.Context, tx pgx.Tx, query string, id int64, scan func(rowScanner) (T, error), action string) (T, error) {
	item, err := scan(tx.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		var zero T
		return zero, ErrNotFound
	}
	if err != nil {
		var zero T
		return zero, fmt.Errorf("数据库锁定记录失败（%s）：%w", action, err)
	}
	return item, nil
}

// adjustBalance 在同一事务内原子更新用户可用余额、待确认充值和待结算提币字段。
func adjustBalance(ctx context.Context, tx pgx.Tx, delta balanceDelta, action string) error {
	available := deltaString(delta.AvailableDelta)
	pendingDeposit := deltaString(delta.PendingDepositDelta)
	pendingWithdrawal := deltaString(delta.PendingWithdrawalDelta)

	var err error
	if delta.AssetID != nil {
		_, err = tx.Exec(ctx, `
			UPDATE asset_balances SET
				available_wei = available_wei + $2::numeric,
				pending_deposit_wei = pending_deposit_wei + $3::numeric,
				pending_withdrawal_wei = pending_withdrawal_wei + $4::numeric,
				version = version + 1,
				updated_at = CURRENT_TIMESTAMP
			WHERE user_id = $1 AND asset_id = $5`,
			delta.UserID, available, pendingDeposit, pendingWithdrawal, *delta.AssetID,
		)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE asset_balances SET
				available_wei = available_wei + $2::numeric,
				pending_deposit_wei = pending_deposit_wei + $3::numeric,
				pending_withdrawal_wei = pending_withdrawal_wei + $4::numeric,
				version = version + 1,
				updated_at = CURRENT_TIMESTAMP
			WHERE user_id = $1 AND asset = $5`,
			delta.UserID, available, pendingDeposit, pendingWithdrawal, delta.Asset,
		)
	}
	if err != nil {
		return fmt.Errorf("更新资产余额失败（%s）：%w", action, err)
	}
	return nil
}

// insertBalanceEntry 在同一事务内写入一条账本流水并按需处理幂等冲突。
func insertBalanceEntry(ctx context.Context, tx pgx.Tx, in balanceEntryInput, action string) error {
	conflictClause := ""
	switch in.ConflictPolicy {
	case entryConflictDoNothing:
		conflictClause = ` ON CONFLICT (entry_type, reference_type, reference_id) DO NOTHING`
	case entryConflictAddAmount:
		conflictClause = ` ON CONFLICT (entry_type, reference_type, reference_id) DO UPDATE SET amount_wei = balance_entries.amount_wei + EXCLUDED.amount_wei`
	}

	var err error
	if in.AssetID != nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO balance_entries (
				user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
			) VALUES ($1, $2, $3, $4, $5::numeric, $6, $7)`+conflictClause,
			in.UserID, *in.AssetID, in.Asset, in.EntryType, in.Amount.String(), in.ReferenceType, in.ReferenceID,
		)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO balance_entries (
				user_id, asset_id, asset, entry_type, amount_wei, reference_type, reference_id
			) VALUES ($1, (SELECT id FROM assets WHERE symbol = $2), $2, $3, $4::numeric, $5, $6)`+conflictClause,
			in.UserID, in.Asset, in.EntryType, in.Amount.String(), in.ReferenceType, in.ReferenceID,
		)
	}
	if err != nil {
		return fmt.Errorf("写入账本流水失败（%s）：%w", action, err)
	}
	return nil
}

// deltaString 将可选金额增量格式化为十进制数字字符串。
func deltaString(value *big.Int) string {
	if value == nil {
		return "0"
	}
	return value.String()
}
