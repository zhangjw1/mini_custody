package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/btc"
)

// ListProcessableBTCWithdrawals 查询可恢复 BTC 提币任务。
func (s *Store) ListProcessableBTCWithdrawals(ctx context.Context, limit int) ([]btc.Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, idempotency_key, to_address, amount_sats, fee_rate_sat_vb, status,
		       COALESCE(raw_tx, ''::bytea), COALESCE(txid, '')
		FROM btc_withdrawals
		WHERE status IN ('CREATED', 'INPUTS_LOCKED', 'SIGNED', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING')
		ORDER BY id LIMIT $1`, normalizedLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.Withdrawal, 0)
	for rows.Next() {
		var item btc.Withdrawal
		if err = rows.Scan(&item.ID, &item.UserID, &item.IdempotencyKey, &item.ToAddress, &item.AmountSats, &item.FeeRateSatVB, &item.Status, &item.RawTx, &item.TxID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// LockWithdrawalInputs 原子锁定平台可用 UTXO 并推进到 INPUTS_LOCKED，重入时直接复用已锁定输入。
func (s *Store) LockWithdrawalInputs(ctx context.Context, id, amount int64, locker string) ([]btc.UTXO, []btc.Address, []byte, []byte, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer tx.Rollback(context.Background())

	var targetAddress, status string
	var rate int64
	var selectedJSON []byte
	if err = tx.QueryRow(ctx, `
		SELECT to_address, fee_rate_sat_vb, status, selected_inputs_json
		FROM btc_withdrawals
		WHERE id = $1
		FOR UPDATE`, id).Scan(&targetAddress, &rate, &status, &selectedJSON); err != nil {
		return nil, nil, nil, nil, mapNotFound(err)
	}

	params, err := btc.ChainParamsForNetwork(NetworkBitcoinSignet)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	target, err := btcutil.DecodeAddress(targetAddress, params)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	targetScript, err := txscript.PayToAddrScript(target)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var changeHex string
	if err = tx.QueryRow(ctx, `
		SELECT script_pub_key
		FROM btc_addresses
		WHERE network = $1 AND purpose = $2 AND enabled
		ORDER BY derivation_index
		LIMIT 1`, NetworkBitcoinSignet, btc.AddressPurposePlatformChange).Scan(&changeHex); err != nil {
		return nil, nil, nil, nil, mapNotFound(err)
	}
	changeScript, err := hex.DecodeString(changeHex)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if status == btc.WithdrawalInputsLocked {
		chosen, addresses, err := decodeLockedInputs(selectedJSON)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, nil, nil, nil, err
		}
		return chosen, addresses, targetScript, changeScript, nil
	}
	if status != btc.WithdrawalCreated {
		return nil, nil, nil, nil, ErrInvalidState
	}

	rows, err := tx.Query(ctx, `
		SELECT u.id, u.txid, u.vout, u.value_sats, u.script_pub_key, u.block_height, a.derivation_path
		FROM btc_utxos u
		JOIN btc_addresses a ON a.id = u.address_id
		WHERE u.network = $1
		  AND u.status = 'UNSPENT'
		  AND a.purpose = $2
		  AND a.enabled
		ORDER BY u.block_height, u.id
		FOR UPDATE OF u SKIP LOCKED`, NetworkBitcoinSignet, btc.AddressPurposePlatformChange)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer rows.Close()

	all := make([]btc.UTXO, 0)
	paths := make(map[int64]string)
	for rows.Next() {
		var u btc.UTXO
		var script, path string
		if err = rows.Scan(&u.ID, &u.TxID, &u.Vout, &u.ValueSats, &script, &u.BlockHeight, &path); err != nil {
			return nil, nil, nil, nil, err
		}
		u.ScriptPubKey, err = hex.DecodeString(script)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		all = append(all, u)
		paths[u.ID] = path
	}
	if err = rows.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	rows.Close()

	chosen, _, err := btc.SelectUTXOs(all, amount, rate)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	addresses := make([]btc.Address, 0, len(chosen))
	snapshots := make([]btc.LockedInput, 0, len(chosen))
	for _, u := range chosen {
		path := paths[u.ID]
		addresses = append(addresses, btc.Address{Path: path})
		snapshots = append(snapshots, btc.LockedInput{
			ID:           u.ID,
			TxID:         u.TxID,
			Vout:         u.Vout,
			ValueSats:    u.ValueSats,
			ScriptPubKey: hex.EncodeToString(u.ScriptPubKey),
			BlockHeight:  u.BlockHeight,
			Path:         path,
		})
		command, execErr := tx.Exec(ctx, `
			UPDATE btc_utxos
			SET status = 'LOCKED', locked_by = $2, locked_until = CURRENT_TIMESTAMP + INTERVAL '10 minutes', updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 AND status = 'UNSPENT'`, u.ID, locker)
		if execErr != nil {
			return nil, nil, nil, nil, execErr
		}
		if command.RowsAffected() != 1 {
			return nil, nil, nil, nil, ErrInvalidState
		}
	}

	encodedInputs, err := json.Marshal(snapshots)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("编码已锁定 BTC 输入失败：%w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE btc_withdrawals
		SET selected_inputs_json = $2::jsonb, status = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status = $4`,
		id, string(encodedInputs), btc.WithdrawalInputsLocked, btc.WithdrawalCreated,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if command.RowsAffected() != 1 {
		return nil, nil, nil, nil, ErrInvalidState
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	return chosen, addresses, targetScript, changeScript, nil
}

// decodeLockedInputs 将持久化在 selected_inputs_json 中的输入快照还原为签名所需切片。
func decodeLockedInputs(raw []byte) ([]btc.UTXO, []btc.Address, error) {
	var snapshots []btc.LockedInput
	if err := json.Unmarshal(raw, &snapshots); err != nil || len(snapshots) == 0 {
		return nil, nil, errors.New("已锁定 BTC 提币输入快照无效")
	}
	chosen := make([]btc.UTXO, 0, len(snapshots))
	addresses := make([]btc.Address, 0, len(snapshots))
	for _, snap := range snapshots {
		script, err := hex.DecodeString(snap.ScriptPubKey)
		if err != nil {
			return nil, nil, fmt.Errorf("解码已锁定 BTC 输入脚本失败：%w", err)
		}
		chosen = append(chosen, btc.UTXO{
			ID:           snap.ID,
			TxID:         snap.TxID,
			Vout:         snap.Vout,
			ValueSats:    snap.ValueSats,
			ScriptPubKey: script,
			BlockHeight:  snap.BlockHeight,
		})
		addresses = append(addresses, btc.Address{Path: snap.Path})
	}
	return chosen, addresses, nil
}

// SaveBTCWithdrawalSigned 持久化已签名提币交易和计算出的 txid，并返回更新后的任务。
func (s *Store) SaveBTCWithdrawalSigned(ctx context.Context, id int64, raw []byte, fee, change int64) (btc.Withdrawal, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return btc.Withdrawal{}, err
	}
	txid := strings.ToLower(tx.TxHash().String())
	var item btc.Withdrawal
	err := s.pool.QueryRow(ctx, `
		UPDATE btc_withdrawals
		SET raw_tx = $2, raw_tx_hash = $3, txid = $3, fee_sats = $4, change_sats = $5, status = 'SIGNED', updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ('CREATED', 'INPUTS_LOCKED')
		RETURNING id, user_id, idempotency_key, to_address, amount_sats, fee_rate_sat_vb, status, raw_tx, COALESCE(txid, '')`,
		id, raw, txid, fee, change,
	).Scan(&item.ID, &item.UserID, &item.IdempotencyKey, &item.ToAddress, &item.AmountSats, &item.FeeRateSatVB, &item.Status, &item.RawTx, &item.TxID)
	return item, mapNotFound(err)
}

// MarkBTCWithdrawalBroadcastUnknown 标记广播结果未知并保留原始交易。
func (s *Store) MarkBTCWithdrawalBroadcastUnknown(ctx context.Context, id int64, message string) error {
	command, err := s.pool.Exec(ctx, `
		UPDATE btc_withdrawals
		SET status = 'BROADCAST_UNKNOWN', error_code = 'BROADCAST_UNKNOWN', error_message = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ('SIGNED', 'BROADCAST_UNKNOWN')`, id, strings.TrimSpace(message))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

// MarkBTCWithdrawalBroadcasted 标记提币交易已广播。
func (s *Store) MarkBTCWithdrawalBroadcasted(ctx context.Context, id int64, txid string) error {
	command, err := s.pool.Exec(ctx, `
		UPDATE btc_withdrawals
		SET status = 'BROADCASTED', txid = $2, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ('SIGNED', 'BROADCAST_UNKNOWN')`, id, strings.ToLower(strings.TrimSpace(txid)))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

// MarkBTCWithdrawalManualReview 将已签名但被节点拒绝且链上未发现的提币转入人工审核，不释放余额或 UTXO。
func (s *Store) MarkBTCWithdrawalManualReview(ctx context.Context, id int64, code, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	command, err := tx.Exec(ctx, `
		UPDATE btc_withdrawals
		SET status = 'MANUAL_REVIEW', error_code = $2, error_message = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ('SIGNED', 'BROADCAST_UNKNOWN')`,
		id, strings.TrimSpace(code), strings.TrimSpace(message),
	)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	if err := insertWorkerErrorInTx(ctx, tx, WorkerError{
		Worker:        "btc-withdrawal-worker",
		Stage:         "broadcast",
		ReferenceType: "BTC_WITHDRAWAL",
		ReferenceID:   &id,
		ErrorCode:     strings.TrimSpace(code),
		ErrorMessage:  strings.TrimSpace(message),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompleteBTCWithdrawal 幂等结算已广播 BTC 提币、写入结算流水、消耗输入 UTXO 并将找零加入平台 UTXO 池。
func (s *Store) CompleteBTCWithdrawal(ctx context.Context, id, confirmations, height int64) error {
	if id <= 0 || confirmations <= 0 || height < 0 {
		return errors.New("BTC 提币结算参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	var userID, amount, changeSats int64
	var status, txid string
	if err = tx.QueryRow(ctx, `
		SELECT user_id, amount_sats, change_sats, status, COALESCE(txid, '')
		FROM btc_withdrawals
		WHERE id = $1
		FOR UPDATE`, id).Scan(&userID, &amount, &changeSats, &status, &txid); err != nil {
		return mapNotFound(err)
	}
	if status == btc.WithdrawalCompleted {
		return tx.Commit(ctx)
	}
	if status != btc.WithdrawalBroadcasted && status != btc.WithdrawalConfirming {
		return ErrInvalidState
	}

	command, err := tx.Exec(ctx, `
		UPDATE btc_withdrawals
		SET status = 'COMPLETED', confirmations = GREATEST(confirmations, $2), block_height = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ('BROADCASTED', 'CONFIRMING')`, id, confirmations, height)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}

	assetID, err := s.btcAssetID(ctx, tx)
	if err != nil {
		return err
	}
	var pendingStr string
	if err = tx.QueryRow(ctx, `
		SELECT pending_withdrawal_wei::text
		FROM asset_balances
		WHERE user_id = $1 AND asset_id = $2
		FOR UPDATE`, userID, assetID).Scan(&pendingStr); err != nil {
		return mapNotFound(err)
	}
	pending, ok := new(big.Int).SetString(pendingStr, 10)
	if !ok || pending.Cmp(big.NewInt(amount)) < 0 {
		return ErrPendingBalance
	}

	amt := big.NewInt(amount)
	if err = adjustBalance(ctx, tx, balanceDelta{
		UserID:                 userID,
		AssetID:                &assetID,
		Asset:                  AssetBTC,
		PendingWithdrawalDelta: new(big.Int).Neg(amt),
	}, "扣减 BTC 待结算提币余额"); err != nil {
		return err
	}
	if err = insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:         userID,
		AssetID:        &assetID,
		Asset:          AssetBTC,
		EntryType:      EntryWithdrawFinalize,
		Amount:         new(big.Int).Neg(amt),
		ReferenceType:  RefBTCWithdrawal,
		ReferenceID:    id,
		ConflictPolicy: entryConflictDoNothing,
	}, "写入 BTC 提币结算流水"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE btc_utxos
		SET status = $3, spend_txid = $2, locked_by = NULL, locked_until = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status = $4 AND locked_by = $1`, btc.WithdrawalLocker(id), strings.ToLower(txid), btc.UTXOStatusSpent, btc.UTXOStatusLocked); err != nil {
		return err
	}

	if changeSats > 0 {
		if len(txid) != 64 {
			return errors.New("BTC 提币找零入库缺少有效 txid")
		}
		var changeAddrID int64
		var changeScript string
		if err = tx.QueryRow(ctx, `
			SELECT id, script_pub_key
			FROM btc_addresses
			WHERE network = $1 AND purpose = $2 AND enabled
			ORDER BY derivation_index
			LIMIT 1`, NetworkBitcoinSignet, btc.AddressPurposePlatformChange).Scan(&changeAddrID, &changeScript); err != nil {
			return mapNotFound(err)
		}
		if _, err = tx.Exec(ctx, `
			INSERT INTO btc_utxos (deposit_id, address_id, network, txid, vout, value_sats, script_pub_key, block_height, status)
			VALUES (NULL, $1, $2, $3, $4, $5, $6, $7, 'UNSPENT')
			ON CONFLICT (network, txid, vout) WHERE status <> 'UNKNOWN' DO NOTHING`,
			changeAddrID, NetworkBitcoinSignet, strings.ToLower(txid), btc.ChangeOutputVout, changeSats, changeScript, height,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// FailBTCWithdrawal 仅允许在广播前（CREATED/INPUTS_LOCKED）失败提币并释放余额、流水和 UTXO 锁。
func (s *Store) FailBTCWithdrawal(ctx context.Context, id int64, code, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	var userID, amount int64
	if err = tx.QueryRow(ctx, `
		UPDATE btc_withdrawals
		SET status = $4, error_code = $2, error_message = $3, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status IN ($5, $6)
		RETURNING user_id, amount_sats`, id, strings.TrimSpace(code), strings.TrimSpace(message),
		btc.WithdrawalFailed, btc.WithdrawalCreated, btc.WithdrawalInputsLocked).Scan(&userID, &amount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if checkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM btc_withdrawals WHERE id = $1)`, id).Scan(&exists); checkErr == nil && exists {
				return ErrInvalidState
			}
			return ErrNotFound
		}
		return err
	}

	assetID, err := s.btcAssetID(ctx, tx)
	if err != nil {
		return err
	}
	amt := big.NewInt(amount)
	if err = adjustBalance(ctx, tx, balanceDelta{
		UserID:                 userID,
		AssetID:                &assetID,
		Asset:                  AssetBTC,
		AvailableDelta:         amt,
		PendingWithdrawalDelta: new(big.Int).Neg(amt),
	}, "释放 BTC 提币占用余额"); err != nil {
		return err
	}
	if err = insertBalanceEntry(ctx, tx, balanceEntryInput{
		UserID:         userID,
		AssetID:        &assetID,
		Asset:          AssetBTC,
		EntryType:      EntryWithdrawRelease,
		Amount:         amt,
		ReferenceType:  RefBTCWithdrawal,
		ReferenceID:    id,
		ConflictPolicy: entryConflictDoNothing,
	}, "写入 BTC 提币释放流水"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE btc_utxos
		SET status = $2, locked_by = NULL, locked_until = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status = $3 AND locked_by = $1`, btc.WithdrawalLocker(id), btc.UTXOStatusUnspent, btc.UTXOStatusLocked); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// insertWorkerErrorInTx 在既有事务中幂等记录或累加后台任务错误，便于人工介入排查。
func insertWorkerErrorInTx(ctx context.Context, tx pgx.Tx, item WorkerError) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO worker_errors (
			worker, stage, reference_type, reference_id, error_code, error_message, retry_count
		) VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, 1)`,
		item.Worker, item.Stage, item.ReferenceType, item.ReferenceID, item.ErrorCode, item.ErrorMessage,
	)
	return err
}
