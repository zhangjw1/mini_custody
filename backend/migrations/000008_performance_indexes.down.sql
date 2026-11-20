DROP INDEX IF EXISTS btc_withdrawals_created_id_desc_idx;
DROP INDEX IF EXISTS btc_deposits_created_id_desc_idx;
DROP INDEX IF EXISTS internal_transfers_created_id_desc_idx;
DROP INDEX IF EXISTS token_sweeps_created_id_desc_idx;
DROP INDEX IF EXISTS token_withdrawals_created_id_desc_idx;
DROP INDEX IF EXISTS token_deposits_created_id_desc_idx;
DROP INDEX IF EXISTS withdrawals_created_id_desc_idx;
DROP INDEX IF EXISTS deposits_created_id_desc_idx;

DROP INDEX IF EXISTS token_deposits_user_id_desc_idx;
CREATE INDEX token_deposits_user_created_idx ON token_deposits (user_id, created_at DESC);

DROP INDEX IF EXISTS balance_entries_user_id_desc_idx;
CREATE INDEX balance_entries_user_created_idx ON balance_entries (user_id, created_at DESC);

DROP INDEX IF EXISTS btc_utxos_locked_by_idx;
DROP INDEX IF EXISTS btc_utxos_network_status_height_id_idx;
DROP INDEX IF EXISTS btc_deposits_user_id_desc_idx;
DROP INDEX IF EXISTS deposits_user_id_desc_idx;
