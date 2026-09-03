package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/btc"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const btcSweepSelect = `SELECT s.id,s.status,s.input_value_sats,COALESCE(s.output_value_sats,0),COALESCE(s.fee_sats,0),COALESCE(s.fee_rate_sat_vb,0),COALESCE(s.raw_tx,''::bytea),COALESCE(s.txid,''),u.id,u.txid,u.vout,u.value_sats,u.script_pub_key,u.block_height,fa.id,fa.address,fa.script_pub_key,fa.derivation_path,ta.id,ta.address,ta.script_pub_key,ta.derivation_path FROM btc_sweeps s JOIN btc_utxos u ON u.id=s.utxo_id JOIN btc_addresses fa ON fa.id=s.from_address_id JOIN btc_addresses ta ON ta.id=s.to_address_id`

// ListProcessableBTCSweeps 查询需要归集 worker 继续处理的任务。
func (s *Store) ListProcessableBTCSweeps(ctx context.Context, limit int) ([]btc.Sweep, error) {
	rows, err := s.pool.Query(ctx, btcSweepSelect+` WHERE s.status IN ('CREATED','SIGNED','BROADCAST_UNKNOWN','BROADCASTED','CONFIRMING') ORDER BY s.id LIMIT $1`, normalizedLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.Sweep, 0)
	for rows.Next() {
		item, scanErr := scanBTCSweep(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SaveBTCSweepSigned 在广播前持久化不可变的已签名交易。
func (s *Store) SaveBTCSweepSigned(ctx context.Context, id int64, raw []byte, txid string, output, fee, rate int64) (btc.Sweep, error) {
	if id <= 0 || len(raw) == 0 || len(txid) != 64 || output <= 0 || fee <= 0 || rate <= 0 {
		return btc.Sweep{}, errors.New("BTC 归集签名参数无效")
	}
	command, err := s.pool.Exec(ctx, `UPDATE btc_sweeps SET raw_tx=$2,txid=$3,output_value_sats=$4,fee_sats=$5,fee_rate_sat_vb=$6,status='SIGNED',updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status='CREATED'`, id, raw, strings.ToLower(txid), output, fee, rate)
	if err != nil {
		return btc.Sweep{}, err
	}
	if command.RowsAffected() != 1 {
		return btc.Sweep{}, ErrInvalidState
	}
	return s.btcSweepByID(ctx, id)
}

// MarkBTCSweepBroadcasted 标记同一原始交易已广播。
func (s *Store) MarkBTCSweepBroadcasted(ctx context.Context, id int64, txid string) (btc.Sweep, error) {
	command, err := s.pool.Exec(ctx, `UPDATE btc_sweeps SET status='BROADCASTED',txid=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('SIGNED','BROADCAST_UNKNOWN') AND txid=$2`, id, strings.ToLower(txid))
	if err != nil {
		return btc.Sweep{}, err
	}
	if command.RowsAffected() != 1 {
		return btc.Sweep{}, ErrInvalidState
	}
	return s.btcSweepByID(ctx, id)
}

// MarkBTCSweepBroadcastUnknown 保留原始交易和 UTXO 锁并记录广播结果不明确。
func (s *Store) MarkBTCSweepBroadcastUnknown(ctx context.Context, id int64, message string) error {
	command, err := s.pool.Exec(ctx, `UPDATE btc_sweeps SET status='BROADCAST_UNKNOWN',error_code='BROADCAST_UNKNOWN',error_message=$2,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('SIGNED','BROADCAST_UNKNOWN')`, id, strings.TrimSpace(message))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

// FailBTCSweep 标记广播前确定性失败并释放未花费 UTXO。
func (s *Store) FailBTCSweep(ctx context.Context, id int64, code, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var utxoID int64
	if err = tx.QueryRow(ctx, `UPDATE btc_sweeps SET status='FAILED',error_code=$2,error_message=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('CREATED','SIGNED') RETURNING utxo_id`, id, strings.TrimSpace(code), strings.TrimSpace(message)).Scan(&utxoID); err != nil {
		return mapNotFound(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='UNSPENT',updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status='LOCKED'`, utxoID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompleteBTCSweep 原子结算归集交易并将输入 UTXO 标记为已花费。
func (s *Store) CompleteBTCSweep(ctx context.Context, id, confirmations, blockHeight int64) error {
	if id <= 0 || confirmations <= 0 || blockHeight < 0 {
		return errors.New("BTC 归集确认参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var utxoID int64
	if err = tx.QueryRow(ctx, `SELECT utxo_id FROM btc_sweeps WHERE id=$1 FOR UPDATE`, id).Scan(&utxoID); err != nil {
		return mapNotFound(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_sweeps SET status='COMPLETED',confirmations=GREATEST(confirmations,$2),block_height=$3,updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('BROADCASTED','CONFIRMING')`, id, confirmations, blockHeight); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='SPENT',spend_txid=(SELECT txid FROM btc_sweeps WHERE id=$1),updated_at=CURRENT_TIMESTAMP WHERE id=$2 AND status='LOCKED'`, id, utxoID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) SELECT NULL,s.to_address_id,$2,s.txid,0,s.output_value_sats,a.script_pub_key,$3,'UNSPENT' FROM btc_sweeps s JOIN btc_addresses a ON a.id=s.to_address_id WHERE s.id=$1 ON CONFLICT(network,txid,vout) DO NOTHING`, id, NetworkBitcoinSignet, blockHeight); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// btcSweepByID 查询单笔 BTC 归集任务。
func (s *Store) btcSweepByID(ctx context.Context, id int64) (btc.Sweep, error) {
	return scanBTCSweep(s.pool.QueryRow(ctx, btcSweepSelect+` WHERE s.id=$1`, id))
}

// scanBTCSweep 将关联查询转换为 BTC 归集领域模型。
func scanBTCSweep(row rowScanner) (btc.Sweep, error) {
	var item btc.Sweep
	var sourceHex, targetHex, utxoHex string
	var vout int32
	if err := row.Scan(&item.ID, &item.Status, &item.InputValueSats, &item.OutputValueSats, &item.FeeSats, &item.FeeRateSatVB, &item.RawTx, &item.TxID, &item.UTXO.ID, &item.UTXO.TxID, &vout, &item.UTXO.ValueSats, &utxoHex, &item.UTXO.BlockHeight, &item.From.ID, &item.From.Address, &sourceHex, &item.From.Path, &item.To.ID, &item.To.Address, &targetHex, &item.To.Path); err != nil {
		return btc.Sweep{}, mapNotFound(err)
	}
	item.UTXO.Vout = uint32(vout)
	var err error
	if item.UTXO.ScriptPubKey, err = hex.DecodeString(utxoHex); err != nil {
		return btc.Sweep{}, err
	}
	if item.From.ScriptPubKey, err = hex.DecodeString(sourceHex); err != nil {
		return btc.Sweep{}, err
	}
	if item.To.ScriptPubKey, err = hex.DecodeString(targetHex); err != nil {
		return btc.Sweep{}, err
	}
	return item, nil
}

// BTCCheckpoint 查询 BTC 扫描器检查点。
func (s *Store) BTCCheckpoint(ctx context.Context) (int64, string, error) {
	item, err := s.Checkpoint(ctx, NetworkBitcoinSignet, "btc-deposit")
	if errors.Is(err, ErrNotFound) {
		return -1, "", nil
	}
	if err != nil {
		return -1, "", err
	}
	return item.LastScannedBlock, item.LastScannedHash, nil
}

// RewindBTCCheckpoint 在检测到区块重组时回退检查点并标记受影响充值。
func (s *Store) RewindBTCCheckpoint(ctx context.Context, height int64, hash string) error {
	if height < 0 || len(hash) != 64 {
		return errors.New("BTC 回退高度无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var credited int64
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status='CREDITED'`, NetworkBitcoinSignet, height).Scan(&credited); err != nil {
		return err
	}
	if credited > 0 {
		return errors.New("Bitcoin 重组影响已入账充值，已停止自动回退")
	}
	var affectedUsers int64
	if err = tx.QueryRow(ctx, `SELECT COUNT(DISTINCT user_id) FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status<>'CREDITED'`, NetworkBitcoinSignet, height).Scan(&affectedUsers); err != nil {
		return err
	}
	command, updateErr := tx.Exec(ctx, `WITH affected AS (SELECT user_id,SUM(amount_sats)::numeric AS amount FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status<>'CREDITED' GROUP BY user_id) UPDATE asset_balances b SET pending_deposit_wei=b.pending_deposit_wei-affected.amount,version=b.version+1,updated_at=CURRENT_TIMESTAMP FROM affected WHERE b.user_id=affected.user_id AND b.asset='BTC' AND b.pending_deposit_wei>=affected.amount`, NetworkBitcoinSignet, height)
	if updateErr != nil {
		return updateErr
	}
	if command.RowsAffected() != affectedUsers {
		return ErrPendingBalance
	}
	if _, err = tx.Exec(ctx, `DELETE FROM balance_entries WHERE reference_type='BTC_DEPOSIT' AND entry_type='DEPOSIT_PENDING' AND reference_id IN (SELECT id FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status<>'CREDITED')`, NetworkBitcoinSignet, height); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM btc_utxos WHERE deposit_id IN (SELECT id FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status<>'CREDITED')`, NetworkBitcoinSignet, height); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM btc_deposits WHERE network=$1 AND block_height>$2 AND status<>'CREDITED'`, NetworkBitcoinSignet, height); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chain_checkpoints SET last_scanned_block=$2,last_scanned_hash=$3,updated_at=CURRENT_TIMESTAMP WHERE network=$1 AND scanner='btc-deposit'`, NetworkBitcoinSignet, height, strings.ToLower(hash)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListBTCAddresses 查询启用的用户 BTC 充值地址。
func (s *Store) ListBTCAddresses(ctx context.Context) (map[string]btc.Address, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,address,script_pub_key,derivation_path FROM btc_addresses WHERE network=$1 AND purpose='USER_DEPOSIT' AND enabled ORDER BY id`, NetworkBitcoinSignet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]btc.Address)
	for rows.Next() {
		var item btc.Address
		var script string
		if err = rows.Scan(&item.ID, &item.UserID, &item.Address, &script, &item.Path); err != nil {
			return nil, err
		}
		item.ScriptPubKey, err = hex.DecodeString(script)
		if err != nil {
			return nil, err
		}
		items[item.Address] = item
	}
	return items, rows.Err()
}

// BTCAddressByUser 查询用户的 Signet 充值地址。
func (s *Store) BTCAddressByUser(ctx context.Context, userID int64) (btc.Address, error) {
	var item btc.Address
	var script string
	err := s.pool.QueryRow(ctx, `SELECT id,user_id,address,script_pub_key,derivation_path FROM btc_addresses WHERE user_id=$1 AND network=$2 AND purpose='USER_DEPOSIT' AND enabled`, userID, NetworkBitcoinSignet).Scan(&item.ID, &item.UserID, &item.Address, &script, &item.Path)
	if err != nil {
		return btc.Address{}, mapNotFound(err)
	}
	item.ScriptPubKey, err = hex.DecodeString(script)
	if err != nil {
		return btc.Address{}, err
	}
	return item, nil
}

// ListBTCDepositsPage 分页查询用户 BTC 充值记录。
func (s *Store) ListBTCDepositsPage(ctx context.Context, userID int64, limit, offset int) ([]btc.DepositRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,txid,vout,block_height,block_hash,amount_sats,confirmations,status FROM btc_deposits WHERE user_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, userID, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.DepositRecord, 0)
	for rows.Next() {
		var item btc.DepositRecord
		var vout int32
		if err = rows.Scan(&item.ID, &item.UserID, &item.TxID, &vout, &item.BlockHeight, &item.BlockHash, &item.AmountSats, &item.Confirmations, &item.Status); err != nil {
			return nil, err
		}
		item.Vout = uint32(vout)
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListBTCSweepsPage 按主键倒序查询 BTC 归集任务。
func (s *Store) ListBTCSweepsPage(ctx context.Context, limit, offset int) ([]btc.Sweep, error) {
	rows, err := s.pool.Query(ctx, btcSweepSelect+` ORDER BY s.id DESC LIMIT $1 OFFSET $2`, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.Sweep, 0)
	for rows.Next() {
		item, scanErr := scanBTCSweep(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListConfirmingBTCDeposits 查询等待确认或入账的 BTC 充值。
func (s *Store) ListConfirmingBTCDeposits(ctx context.Context, limit int) ([]btc.ConfirmingDeposit, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,block_height,block_hash FROM btc_deposits WHERE status IN ('CONFIRMING','CONFIRMED') ORDER BY block_height,id LIMIT $1`, normalizedLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.ConfirmingDeposit, 0)
	for rows.Next() {
		var item btc.ConfirmingDeposit
		if err = rows.Scan(&item.ID, &item.BlockHeight, &item.BlockHash); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// UpdateBTCDepositConfirmations 单调更新 BTC 充值确认数。
func (s *Store) UpdateBTCDepositConfirmations(ctx context.Context, id, confirmations int64) error {
	if confirmations < 0 {
		return errors.New("BTC 确认数不能为负数")
	}
	command, err := s.pool.Exec(ctx, `UPDATE btc_deposits SET confirmations=GREATEST(confirmations,$2),updated_at=CURRENT_TIMESTAMP WHERE id=$1 AND status IN ('CONFIRMING','CONFIRMED')`, id, confirmations)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

// EnsureBitcoinWallets 初始化 BTC 资产、平台归集地址和演示用户地址。
func (s *Store) EnsureBitcoinWallets(ctx context.Context, provider *wallet.MnemonicKeyProvider) error {
	if provider == nil {
		return errors.New("BTC 钱包初始化必须提供密钥")
	}
	treasury, err := provider.BitcoinAddress(ctx, wallet.BitcoinTreasuryPath)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var assetID int64
	if _, err = tx.Exec(ctx, `INSERT INTO assets(network,asset_type,symbol,decimals,enabled) VALUES($1,'NATIVE','BTC',8,TRUE) ON CONFLICT(network,symbol) DO UPDATE SET enabled=TRUE,updated_at=CURRENT_TIMESTAMP`, NetworkBitcoinSignet); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET enabled=FALSE,updated_at=CURRENT_TIMESTAMP WHERE symbol='BTC' AND network<>$1`, NetworkBitcoinSignet); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM assets WHERE network=$1 AND symbol='BTC' FOR UPDATE`, NetworkBitcoinSignet).Scan(&assetID); err != nil {
		return fmt.Errorf("读取 BTC 资产失败：%w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO btc_addresses(network,purpose,address,script_pub_key,derivation_index,derivation_path) VALUES($1,'PLATFORM_CHANGE',$2,$3,0,$4) ON CONFLICT(network,purpose,derivation_index) DO NOTHING`, NetworkBitcoinSignet, treasury.Address, fmt.Sprintf("%x", treasury.ScriptPubKey), wallet.BitcoinTreasuryPath); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id,code FROM users ORDER BY id`)
	if err != nil {
		return err
	}
	type user struct {
		id   int64
		code string
	}
	users := make([]user, 0)
	for rows.Next() {
		var u user
		if err = rows.Scan(&u.id, &u.code); err != nil {
			rows.Close()
			return err
		}
		users = append(users, u)
	}
	rows.Close()
	for index, u := range users {
		derivationIndex := uint32(index + 1)
		path := wallet.BitcoinUserPath(derivationIndex)
		address, deriveErr := provider.BitcoinAddress(ctx, path)
		if deriveErr != nil {
			return deriveErr
		}
		if _, err = tx.Exec(ctx, `INSERT INTO btc_addresses(user_id,network,purpose,address,script_pub_key,derivation_index,derivation_path) VALUES($1,$2,'USER_DEPOSIT',$3,$4,$5,$6) ON CONFLICT(user_id) WHERE purpose='USER_DEPOSIT' DO NOTHING`, u.id, NetworkBitcoinSignet, address.Address, fmt.Sprintf("%x", address.ScriptPubKey), derivationIndex, path); err != nil {
			return fmt.Errorf("写入用户 %s BTC 地址失败：%w", u.code, err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO asset_balances(user_id,asset_id,asset) VALUES($1,$2,'BTC') ON CONFLICT(user_id,asset_id) DO NOTHING`, u.id, assetID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RecordBTCDepositsAndCheckpoint 原子写入 BTC 充值输出、UTXO 和扫描检查点。
func (s *Store) RecordBTCDepositsAndCheckpoint(ctx context.Context, observations []btc.DepositObservation, checkpoint btc.Checkpoint) (int, error) {
	if checkpoint.Network != NetworkBitcoinSignet || checkpoint.LastScannedBlock < 0 || len(checkpoint.LastScannedHash) != 64 {
		return 0, errors.New("BTC 检查点参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("开启 BTC 扫描事务失败：%w", err)
	}
	defer tx.Rollback(context.Background())
	created := 0
	for _, item := range observations {
		if item.UserID <= 0 || item.AddressID <= 0 || item.AmountSats <= 0 || item.BlockHeight < 0 || len(item.TxID) != 64 {
			return 0, errors.New("BTC 充值观察值无效")
		}
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO btc_deposits (user_id,address_id,network,txid,vout,block_hash,block_height,amount_sats,status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (network,txid,vout,address_id) DO NOTHING RETURNING id`, item.UserID, item.AddressID, NetworkBitcoinSignet, strings.ToLower(item.TxID), item.Vout, strings.ToLower(item.BlockHash), item.BlockHeight, item.AmountSats, DepositConfirming).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("写入 BTC 充值失败：%w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO btc_utxos (deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'UNCONFIRMED') ON CONFLICT DO NOTHING`, id, item.AddressID, NetworkBitcoinSignet, strings.ToLower(item.TxID), item.Vout, item.AmountSats, fmt.Sprintf("%x", item.ScriptPubKey), item.BlockHeight); err != nil {
			return 0, fmt.Errorf("写入 BTC UTXO 失败：%w", err)
		}
		assetID, assetErr := s.btcAssetID(ctx, tx)
		if assetErr != nil {
			return 0, assetErr
		}
		if _, err = tx.Exec(ctx, `UPDATE asset_balances SET pending_deposit_wei=pending_deposit_wei+$3,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE user_id=$1 AND asset_id=$2`, item.UserID, assetID, item.AmountSats); err != nil {
			return 0, fmt.Errorf("增加 BTC 待确认余额失败：%w", err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO balance_entries(user_id,asset_id,asset,entry_type,amount_wei,reference_type,reference_id) VALUES($1,$2,'BTC','DEPOSIT_PENDING',$3,'BTC_DEPOSIT',$4)`, item.UserID, assetID, item.AmountSats, id); err != nil {
			return 0, fmt.Errorf("写入 BTC 待确认流水失败：%w", err)
		}
		created++
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chain_checkpoints (network,scanner,last_scanned_block,last_scanned_hash) VALUES ($1,'btc-deposit',$2,$3) ON CONFLICT (network,scanner) DO UPDATE SET last_scanned_block=EXCLUDED.last_scanned_block,last_scanned_hash=EXCLUDED.last_scanned_hash,updated_at=CURRENT_TIMESTAMP`, NetworkBitcoinSignet, checkpoint.LastScannedBlock, strings.ToLower(checkpoint.LastScannedHash)); err != nil {
		return 0, fmt.Errorf("推进 BTC 检查点失败：%w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("提交 BTC 扫描事务失败：%w", err)
	}
	return created, nil
}

// CreditBTCDeposit 确认 BTC 充值并幂等增加 BTC 可用余额。
func (s *Store) CreditBTCDeposit(ctx context.Context, depositID, confirmations int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var userID, amount int64
	var status string
	if err = tx.QueryRow(ctx, `SELECT user_id,amount_sats,status FROM btc_deposits WHERE id=$1 FOR UPDATE`, depositID).Scan(&userID, &amount, &status); err != nil {
		return mapNotFound(err)
	}
	if status == DepositCredited {
		return tx.Commit(ctx)
	}
	if status != DepositConfirming && status != DepositConfirmed {
		return ErrInvalidState
	}
	assetID, err := s.btcAssetID(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_balances SET pending_deposit_wei=pending_deposit_wei-$3,available_wei=available_wei+$3,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE user_id=$1 AND asset_id=$2`, userID, assetID, amount); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO balance_entries(user_id,asset_id,asset,entry_type,amount_wei,reference_type,reference_id) VALUES($1,$2,'BTC','DEPOSIT_CREDIT',$3,'BTC_DEPOSIT',$4) ON CONFLICT DO NOTHING`, userID, assetID, amount, depositID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='UNSPENT',updated_at=CURRENT_TIMESTAMP WHERE deposit_id=$1 AND status='UNCONFIRMED'`, depositID); err != nil {
		return err
	}
	var utxoID, addressID, value, targetID int64
	if err = tx.QueryRow(ctx, `SELECT id,address_id,value_sats FROM btc_utxos WHERE deposit_id=$1 FOR UPDATE`, depositID).Scan(&utxoID, &addressID, &value); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE'`, NetworkBitcoinSignet).Scan(&targetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO btc_sweeps(deposit_id,utxo_id,from_address_id,to_address_id,input_value_sats,status) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(deposit_id) DO NOTHING`, depositID, utxoID, addressID, targetID, value, BTCSweepCreated); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_deposits SET status='CREDITED',confirmations=GREATEST(confirmations,$2),updated_at=CURRENT_TIMESTAMP WHERE id=$1`, depositID, confirmations); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateBTCSweepForDeposit 将已入账充值 UTXO 锁定并幂等创建归集任务。
func (s *Store) CreateBTCSweepForDeposit(ctx context.Context, depositID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var utxoID, addressID, value int64
	var status string
	if err = tx.QueryRow(ctx, `SELECT id,address_id,value_sats,status FROM btc_utxos WHERE deposit_id=$1 FOR UPDATE`, depositID).Scan(&utxoID, &addressID, &value, &status); err != nil {
		return mapNotFound(err)
	}
	if status == "LOCKED" || status == "SPENT" {
		return tx.Commit(ctx)
	}
	if status != "UNCONFIRMED" && status != "UNSPENT" {
		return ErrInvalidState
	}
	var targetID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE'`, NetworkBitcoinSignet).Scan(&targetID); err != nil {
		return mapNotFound(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO btc_sweeps(deposit_id,utxo_id,from_address_id,to_address_id,input_value_sats,status) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(deposit_id) DO NOTHING`, depositID, utxoID, addressID, targetID, value, BTCSweepCreated); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='LOCKED',locked_by='btc-sweep',locked_until=CURRENT_TIMESTAMP+INTERVAL '10 minutes',updated_at=CURRENT_TIMESTAMP WHERE id=$1`, utxoID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleaseExpiredBTCUTXOLocks 释放未广播且已过期的 UTXO 租约。
func (s *Store) ReleaseExpiredBTCUTXOLocks(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE btc_utxos SET status='UNSPENT',locked_by=NULL,locked_until=NULL,updated_at=CURRENT_TIMESTAMP WHERE status='LOCKED' AND locked_until<CURRENT_TIMESTAMP AND id IN (SELECT utxo_id FROM btc_sweeps WHERE status IN ('CREATED','FAILED'))`)
	return err
}

// LockBTCUTXOs 按租约原子锁定可用 UTXO，使用 SKIP LOCKED 避免并发 worker 互相等待。
func (s *Store) LockBTCUTXOs(ctx context.Context, targetSats, feeRateSatVB int64, locker string) ([]btc.UTXO, int64, error) {
	if targetSats <= 0 || feeRateSatVB <= 0 || strings.TrimSpace(locker) == "" {
		return nil, 0, errors.New("BTC UTXO 锁定参数无效")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT id,txid,vout,value_sats,script_pub_key,block_height,address_id FROM btc_utxos WHERE network=$1 AND status='UNSPENT' ORDER BY block_height,id FOR UPDATE SKIP LOCKED`, NetworkBitcoinSignet)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]btc.UTXO, 0)
	for rows.Next() {
		var item btc.UTXO
		var script string
		if err = rows.Scan(&item.ID, &item.TxID, &item.Vout, &item.ValueSats, &script, &item.BlockHeight, &item.AddressID); err != nil {
			return nil, 0, err
		}
		item.ScriptPubKey, err = hex.DecodeString(script)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
		chosen, fee, selectErr := btc.SelectUTXOs(items, targetSats, feeRateSatVB)
		if selectErr == nil {
			for _, u := range chosen {
				if _, err = tx.Exec(ctx, `UPDATE btc_utxos SET status='LOCKED',locked_by=$2,locked_until=CURRENT_TIMESTAMP+INTERVAL '10 minutes',updated_at=CURRENT_TIMESTAMP WHERE id=$1`, u.ID, locker); err != nil {
					return nil, 0, err
				}
			}
			if err = tx.Commit(ctx); err != nil {
				return nil, 0, err
			}
			return chosen, fee, nil
		}
	}
	return nil, 0, errors.New("BTC 可用 UTXO 不足")
}

// CreateBTCWithdrawal 幂等创建提币并在用户余额行锁下占用金额。
func (s *Store) CreateBTCWithdrawal(ctx context.Context, userID int64, key, address string, amountSats, feeRate int64) (btc.Withdrawal, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return btc.Withdrawal{}, false, err
	}
	defer tx.Rollback(context.Background())
	scan := func(row rowScanner, item *btc.Withdrawal) error {
		return row.Scan(&item.ID, &item.UserID, &item.IdempotencyKey, &item.ToAddress, &item.AmountSats, &item.FeeRateSatVB, &item.Status)
	}
	var item btc.Withdrawal
	err = scan(tx.QueryRow(ctx, `SELECT id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status FROM btc_withdrawals WHERE user_id=$1 AND idempotency_key=$2 FOR UPDATE`, userID, key), &item)
	if err == nil {
		if item.ToAddress != address || item.AmountSats != amountSats {
			return btc.Withdrawal{}, false, ErrIdempotencyConflict
		}
		return item, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return btc.Withdrawal{}, false, err
	}
	var balanceID, assetID int64
	var available string
	if err = tx.QueryRow(ctx, `SELECT id,asset_id,available_wei::text FROM asset_balances WHERE user_id=$1 AND asset='BTC' FOR UPDATE`, userID).Scan(&balanceID, &assetID, &available); err != nil {
		return btc.Withdrawal{}, false, mapNotFound(err)
	}
	parsed, ok := new(big.Int).SetString(available, 10)
	if !ok || parsed.Cmp(big.NewInt(amountSats)) < 0 {
		return btc.Withdrawal{}, false, ErrInsufficientBalance
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_balances SET available_wei=available_wei-$2,pending_withdrawal_wei=pending_withdrawal_wei+$2,version=version+1,updated_at=CURRENT_TIMESTAMP WHERE id=$1`, balanceID, amountSats); err != nil {
		return btc.Withdrawal{}, false, err
	}
	if err = scan(tx.QueryRow(ctx, `INSERT INTO btc_withdrawals(user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,selected_inputs_json,outputs_json,status) VALUES($1,$2,$3,$4,$5,'[]'::jsonb,'[]'::jsonb,'CREATED') RETURNING id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status`, userID, key, address, amountSats, feeRate), &item); err != nil {
		return btc.Withdrawal{}, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO balance_entries(user_id,asset_id,asset,entry_type,amount_wei,reference_type,reference_id) VALUES($1,$2,'BTC','WITHDRAW_RESERVE',$3,'BTC_WITHDRAWAL',$4)`, userID, assetID, -amountSats, item.ID); err != nil {
		return btc.Withdrawal{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return btc.Withdrawal{}, false, err
	}
	return item, true, nil
}

// BTCWithdrawalByID 查询 BTC 提币记录。
func (s *Store) BTCWithdrawalByID(ctx context.Context, id int64) (btc.Withdrawal, error) {
	var item btc.Withdrawal
	err := scanBTCWithdrawal(s.pool.QueryRow(ctx, `SELECT id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status FROM btc_withdrawals WHERE id=$1`, id), &item)
	return item, mapNotFound(err)
}

// ListBTCWithdrawalsPage 分页查询用户 BTC 提币记录。
func (s *Store) ListBTCWithdrawalsPage(ctx context.Context, userID int64, limit, offset int) ([]btc.Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,idempotency_key,to_address,amount_sats,fee_rate_sat_vb,status FROM btc_withdrawals WHERE user_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, userID, normalizedLimit(limit), normalizedOffset(offset))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]btc.Withdrawal, 0)
	for rows.Next() {
		var item btc.Withdrawal
		if err = scanBTCWithdrawal(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// scanBTCWithdrawal 读取 BTC 提币公共字段。
func scanBTCWithdrawal(row rowScanner, item *btc.Withdrawal) error {
	return row.Scan(&item.ID, &item.UserID, &item.IdempotencyKey, &item.ToAddress, &item.AmountSats, &item.FeeRateSatVB, &item.Status)
}

// btcAssetID 查询 BTC 原生资产主键。
func (s *Store) btcAssetID(ctx context.Context, tx pgx.Tx) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM assets WHERE network=$1 AND symbol='BTC'`, NetworkBitcoinSignet).Scan(&id)
	return id, mapNotFound(err)
}
