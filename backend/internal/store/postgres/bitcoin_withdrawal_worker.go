package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/xiaoqi/mini-custody/backend/internal/btc"
)

// ListProcessableBTCWithdrawals 查询可恢复 BTC 提币任务。
func (s *Store) ListProcessableBTCWithdrawals(ctx context.Context, limit int) ([]btc.Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status,COALESCE(raw_tx,''::bytea),COALESCE(txid,'') FROM btc_withdrawals WHERE status IN ('CREATED','SIGNED','BROADCAST_UNKNOWN','BROADCASTED','CONFIRMING') ORDER BY id LIMIT $1`, normalizedLimit(limit))
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

// LockWithdrawalInputs 原子锁定平台 UTXO 并返回提币目标和找零脚本。
func (s *Store) LockWithdrawalInputs(ctx context.Context, id, amount int64, locker string) ([]btc.UTXO, []btc.Address, []byte, []byte, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer tx.Rollback(context.Background())
	var targetAddress string
	var rate int64
	if err = tx.QueryRow(ctx, `SELECT to_address,fee_rate_sat_vb FROM btc_withdrawals WHERE id=$1 AND status='CREATED' FOR UPDATE`, id).Scan(&targetAddress, &rate); err != nil {
		return nil, nil, nil, nil, mapNotFound(err)
	}
	target, err := btcutil.DecodeAddress(targetAddress, &chaincfg.SigNetParams)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	targetScript, err := txscript.PayToAddrScript(target)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT u.id,u.txid,u.vout,u.value_sats,u.script_pub_key,u.block_height,a.derivation_path FROM btc_utxos u JOIN btc_addresses a ON a.id=u.address_id WHERE u.network=$1 AND u.status='UNSPENT' ORDER BY u.block_height,u.id FOR UPDATE SKIP LOCKED`, NetworkBitcoinSignet)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer rows.Close()
	all := make([]btc.UTXO, 0)
	paths := make(map[string]string)
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
		paths[u.TxID+":"+string(rune(u.Vout))] = path
	}
	chosen, _, err := btc.SelectUTXOs(all, amount, rate)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	addresses := make([]btc.Address, 0, len(chosen))
	for _, u := range chosen {
		path := paths[u.TxID+":"+string(rune(u.Vout))]
		addresses = append(addresses, btc.Address{Path: path})
		if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='LOCKED',locked_by=$2,locked_until=CURRENT_TIMESTAMP+INTERVAL '10 minutes' WHERE id=$1`, u.ID, locker); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	var changeHex string
	if err = tx.QueryRow(ctx, `SELECT script_pub_key FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE' AND enabled`, NetworkBitcoinSignet).Scan(&changeHex); err != nil {
		return nil, nil, nil, nil, err
	}
	changeScript, err := hex.DecodeString(changeHex)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	return chosen, addresses, targetScript, changeScript, nil
}

// SaveBTCWithdrawalSigned 保存已签名提币交易并返回更新后的任务。
func (s *Store) SaveBTCWithdrawalSigned(ctx context.Context, id int64, raw []byte, fee, change int64) (btc.Withdrawal, error) {
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return btc.Withdrawal{}, err
	}
	hash := tx.TxHash().String()
	var item btc.Withdrawal
	err := s.pool.QueryRow(ctx, `UPDATE btc_withdrawals SET raw_tx=$2,raw_tx_hash=$3,fee_sats=$4,change_sats=$5,status='SIGNED',updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status='CREATED' RETURNING id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status,raw_tx,COALESCE(txid,'')`, id, raw, hash, fee, change).Scan(&item.ID, &item.UserID, &item.IdempotencyKey, &item.ToAddress, &item.AmountSats, &item.FeeRateSatVB, &item.Status, &item.RawTx, &item.TxID)
	return item, err
}

// MarkBTCWithdrawalBroadcastUnknown 标记广播结果未知并保留原始交易。
func (s *Store) MarkBTCWithdrawalBroadcastUnknown(ctx context.Context, id int64, message string) error {
	_, err := s.pool.Exec(ctx, `UPDATE btc_withdrawals SET status='BROADCAST_UNKNOWN',error_code='BROADCAST_UNKNOWN',error_message=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('SIGNED','BROADCAST_UNKNOWN')`, id, strings.TrimSpace(message))
	return err
}

// MarkBTCWithdrawalBroadcasted 标记提币交易已广播。
func (s *Store) MarkBTCWithdrawalBroadcasted(ctx context.Context, id int64, txid string) error {
	_, err := s.pool.Exec(ctx, `UPDATE btc_withdrawals SET status='BROADCASTED',txid=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('SIGNED','BROADCAST_UNKNOWN')`, id, strings.ToLower(txid))
	return err
}

// CompleteBTCWithdrawal 结算 BTC 提币并释放输入 UTXO 为已花费。
func (s *Store) CompleteBTCWithdrawal(ctx context.Context, id, confirmations, height int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var userID, amount int64
	if err = tx.QueryRow(ctx, `SELECT user_id,amount_sats FROM btc_withdrawals WHERE id=$1 FOR UPDATE`, id).Scan(&userID, &amount); err != nil {
		return mapNotFound(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_withdrawals SET status='COMPLETED',confirmations=GREATEST(confirmations,$2),block_height=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, id, confirmations, height); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_balances SET pending_withdrawal_wei=pending_withdrawal_wei-$2,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE user_id=$1 AND asset='BTC'`, userID, amount); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='SPENT',spend_txid=(SELECT txid FROM btc_withdrawals WHERE id=$1),locked_by=NULL,locked_until=NULL,updated_at=CURRENT_TIMESTAMP WHERE status='LOCKED' AND locked_by=$2`, id, "withdrawal-"+fmt.Sprint(id)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FailBTCWithdrawal 失败提币并释放其余额和 UTXO 锁。
func (s *Store) FailBTCWithdrawal(ctx context.Context, id int64, code, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var userID, amount int64
	if err = tx.QueryRow(ctx, `UPDATE btc_withdrawals SET status='FAILED',error_code=$2,error_message=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('CREATED','SIGNED') RETURNING user_id,amount_sats`, id, code, message).Scan(&userID, &amount); err != nil {
		return mapNotFound(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_balances SET available_wei=available_wei+$2,pending_withdrawal_wei=pending_withdrawal_wei-$2,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE user_id=$1 AND asset='BTC'`, userID, amount); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='UNSPENT',locked_by=NULL,locked_until=NULL,updated_at=CURRENT_TIMESTAMP WHERE status='LOCKED' AND locked_by=$1`, `withdrawal-`+fmt.Sprint(id)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
