ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED', 'ORPHANED'));
ALTER TABLE deposits DROP CONSTRAINT deposits_network_tx_hash_address_id_key;
CREATE UNIQUE INDEX deposits_active_network_tx_hash_address_idx
    ON deposits (network, tx_hash, address_id)
    WHERE status <> 'ORPHANED';

ALTER TABLE token_deposits DROP CONSTRAINT token_deposits_status_check;
ALTER TABLE token_deposits ADD CONSTRAINT token_deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED', 'ORPHANED'));
ALTER TABLE token_deposits DROP CONSTRAINT token_deposits_asset_id_tx_hash_log_index_key;
CREATE UNIQUE INDEX token_deposits_active_asset_tx_log_idx
    ON token_deposits (asset_id, tx_hash, log_index)
    WHERE status <> 'ORPHANED';

ALTER TABLE btc_deposits DROP CONSTRAINT btc_deposits_status_check;
ALTER TABLE btc_deposits ADD CONSTRAINT btc_deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED', 'REORG_RECHECK', 'ORPHANED'));
ALTER TABLE btc_deposits DROP CONSTRAINT btc_deposits_network_txid_vout_address_id_key;
CREATE UNIQUE INDEX btc_deposits_active_network_txid_vout_addr_idx
    ON btc_deposits (network, txid, vout, address_id)
    WHERE status <> 'ORPHANED';

ALTER TABLE btc_utxos DROP CONSTRAINT btc_utxos_network_txid_vout_key;
CREATE UNIQUE INDEX btc_utxos_active_network_txid_vout_idx
    ON btc_utxos (network, txid, vout)
    WHERE status <> 'UNKNOWN';

ALTER TABLE balance_entries DROP CONSTRAINT balance_entries_entry_type_check;
ALTER TABLE balance_entries ADD CONSTRAINT balance_entries_entry_type_check
    CHECK (entry_type IN ('DEPOSIT_PENDING', 'DEPOSIT_CREDIT', 'DEPOSIT_REVERSAL', 'WITHDRAW_RESERVE', 'WITHDRAW_FINALIZE', 'WITHDRAW_RELEASE', 'FEE_ADJUST'));

COMMENT ON COLUMN deposits.status IS '充值状态：DETECTED、CONFIRMING、CONFIRMED、CREDITED 或 ORPHANED';
COMMENT ON COLUMN token_deposits.status IS 'Token 充值生命周期状态：DETECTED、CONFIRMING、CONFIRMED、CREDITED 或 ORPHANED';
COMMENT ON COLUMN btc_deposits.status IS 'BTC 充值状态：DETECTED、CONFIRMING、CONFIRMED、CREDITED、REORG_RECHECK 或 ORPHANED';
COMMENT ON COLUMN balance_entries.entry_type IS '余额变动类型：含 DEPOSIT_REVERSAL 孤块冲正流水';
