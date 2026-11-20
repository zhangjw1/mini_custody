DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM balance_entries WHERE entry_type = 'DEPOSIT_REVERSAL')
        OR EXISTS (SELECT 1 FROM btc_deposits WHERE status = 'ORPHANED')
        OR EXISTS (SELECT 1 FROM token_deposits WHERE status = 'ORPHANED')
        OR EXISTS (SELECT 1 FROM deposits WHERE status = 'ORPHANED') THEN
        RAISE EXCEPTION '存在 ORPHANED 充值或 DEPOSIT_REVERSAL 冲正流水，禁止回滚 000007 迁移以免破坏账本历史';
    END IF;
END $$;

DELETE FROM balance_entries WHERE entry_type = 'DEPOSIT_REVERSAL';
DELETE FROM btc_utxos WHERE status = 'UNKNOWN';
DELETE FROM btc_deposits WHERE status = 'ORPHANED';
DELETE FROM token_deposits WHERE status = 'ORPHANED';
DELETE FROM deposits WHERE status = 'ORPHANED';

ALTER TABLE balance_entries DROP CONSTRAINT balance_entries_entry_type_check;
ALTER TABLE balance_entries ADD CONSTRAINT balance_entries_entry_type_check
    CHECK (entry_type IN ('DEPOSIT_PENDING', 'DEPOSIT_CREDIT', 'WITHDRAW_RESERVE', 'WITHDRAW_FINALIZE', 'WITHDRAW_RELEASE', 'FEE_ADJUST'));

DROP INDEX btc_utxos_active_network_txid_vout_idx;
ALTER TABLE btc_utxos ADD CONSTRAINT btc_utxos_network_txid_vout_key UNIQUE (network, txid, vout);

DROP INDEX btc_deposits_active_network_txid_vout_addr_idx;
ALTER TABLE btc_deposits ADD CONSTRAINT btc_deposits_network_txid_vout_address_id_key UNIQUE (network, txid, vout, address_id);
ALTER TABLE btc_deposits DROP CONSTRAINT btc_deposits_status_check;
ALTER TABLE btc_deposits ADD CONSTRAINT btc_deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED', 'REORG_RECHECK'));

DROP INDEX token_deposits_active_asset_tx_log_idx;
ALTER TABLE token_deposits ADD CONSTRAINT token_deposits_asset_id_tx_hash_log_index_key UNIQUE (asset_id, tx_hash, log_index);
ALTER TABLE token_deposits DROP CONSTRAINT token_deposits_status_check;
ALTER TABLE token_deposits ADD CONSTRAINT token_deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED'));

DROP INDEX deposits_active_network_tx_hash_address_idx;
ALTER TABLE deposits ADD CONSTRAINT deposits_network_tx_hash_address_id_key UNIQUE (network, tx_hash, address_id);
ALTER TABLE deposits DROP CONSTRAINT deposits_status_check;
ALTER TABLE deposits ADD CONSTRAINT deposits_status_check
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED'));
