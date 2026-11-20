CREATE INDEX deposits_user_id_desc_idx ON deposits (user_id, id DESC);
CREATE INDEX btc_deposits_user_id_desc_idx ON btc_deposits (user_id, id DESC);
CREATE INDEX btc_utxos_network_status_height_id_idx ON btc_utxos (network, status, block_height, id);
CREATE INDEX btc_utxos_locked_by_idx ON btc_utxos (locked_by) WHERE locked_by IS NOT NULL;

DROP INDEX IF EXISTS balance_entries_user_created_idx;
CREATE INDEX balance_entries_user_id_desc_idx ON balance_entries (user_id, id DESC);

DROP INDEX IF EXISTS token_deposits_user_created_idx;
CREATE INDEX token_deposits_user_id_desc_idx ON token_deposits (user_id, id DESC);

CREATE INDEX deposits_created_id_desc_idx ON deposits (created_at DESC, id DESC);
CREATE INDEX withdrawals_created_id_desc_idx ON withdrawals (created_at DESC, id DESC);
CREATE INDEX token_deposits_created_id_desc_idx ON token_deposits (created_at DESC, id DESC);
CREATE INDEX token_withdrawals_created_id_desc_idx ON token_withdrawals (created_at DESC, id DESC);
CREATE INDEX token_sweeps_created_id_desc_idx ON token_sweeps (created_at DESC, id DESC);
CREATE INDEX internal_transfers_created_id_desc_idx ON internal_transfers (created_at DESC, id DESC);
CREATE INDEX btc_deposits_created_id_desc_idx ON btc_deposits (created_at DESC, id DESC);
CREATE INDEX btc_withdrawals_created_id_desc_idx ON btc_withdrawals (created_at DESC, id DESC);
