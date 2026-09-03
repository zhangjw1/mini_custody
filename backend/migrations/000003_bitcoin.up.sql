ALTER TABLE assets DROP CONSTRAINT assets_network_check;
ALTER TABLE assets DROP CONSTRAINT assets_asset_type_check;
ALTER TABLE assets ADD CONSTRAINT assets_network_check CHECK (network IN ('ethereum-sepolia', 'bitcoin-signet', 'bitcoin-testnet4'));
ALTER TABLE assets ADD CONSTRAINT assets_asset_type_check CHECK (asset_type IN ('NATIVE', 'ERC20'));
INSERT INTO assets (network, asset_type, symbol, decimals, enabled)
VALUES ('bitcoin-signet', 'NATIVE', 'BTC', 8, TRUE);

ALTER TABLE balance_entries DROP CONSTRAINT balance_entries_reference_type_check;
ALTER TABLE balance_entries ADD CONSTRAINT balance_entries_reference_type_check
    CHECK (reference_type IN ('DEPOSIT', 'WITHDRAWAL', 'TOKEN_DEPOSIT', 'TOKEN_WITHDRAWAL', 'BTC_DEPOSIT', 'BTC_WITHDRAWAL'));
ALTER TABLE chain_checkpoints DROP CONSTRAINT chain_checkpoints_network_check;
ALTER TABLE chain_checkpoints ADD CONSTRAINT chain_checkpoints_network_check
    CHECK (network IN ('ethereum-sepolia', 'bitcoin-signet', 'bitcoin-testnet4'));

CREATE TABLE btc_addresses (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT REFERENCES users(id), network TEXT NOT NULL, purpose TEXT NOT NULL,
    address TEXT NOT NULL, script_pub_key TEXT NOT NULL, derivation_index BIGINT NOT NULL,
    derivation_path TEXT NOT NULL, enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, address), UNIQUE (network, purpose, derivation_index),
    CHECK (network IN ('bitcoin-signet','bitcoin-testnet4')), CHECK (purpose IN ('USER_DEPOSIT', 'PLATFORM_CHANGE')),
    CHECK ((purpose = 'USER_DEPOSIT' AND user_id IS NOT NULL) OR (purpose = 'PLATFORM_CHANGE' AND user_id IS NULL)),
    CHECK (address ~ '^tb1q[023456789acdefghjklmnpqrstuvwxyz]{38}$'),
    CHECK (script_pub_key ~ '^0014[0-9a-f]{40}$'), CHECK (derivation_index BETWEEN 0 AND 4294967295)
);
CREATE UNIQUE INDEX btc_addresses_user_deposit_idx ON btc_addresses (user_id) WHERE purpose = 'USER_DEPOSIT';

CREATE TABLE btc_deposits (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id), address_id BIGINT NOT NULL REFERENCES btc_addresses(id),
    network TEXT NOT NULL, txid TEXT NOT NULL, vout INTEGER NOT NULL, block_hash TEXT NOT NULL,
    block_height BIGINT NOT NULL, amount_sats BIGINT NOT NULL, confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, txid, vout, address_id), CHECK (network IN ('bitcoin-signet','bitcoin-testnet4')),
    CHECK (txid ~ '^[0-9a-f]{64}$'), CHECK (block_hash ~ '^[0-9a-f]{64}$'),
    CHECK (vout >= 0), CHECK (block_height >= 0), CHECK (amount_sats > 0), CHECK (confirmations >= 0),
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED', 'REORG_RECHECK'))
);

CREATE TABLE btc_utxos (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deposit_id BIGINT UNIQUE REFERENCES btc_deposits(id), address_id BIGINT NOT NULL REFERENCES btc_addresses(id),
    network TEXT NOT NULL, txid TEXT NOT NULL, vout INTEGER NOT NULL, value_sats BIGINT NOT NULL,
    script_pub_key TEXT NOT NULL, block_height BIGINT NOT NULL, spend_txid TEXT, status TEXT NOT NULL,
    locked_by TEXT, locked_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, txid, vout), CHECK (network IN ('bitcoin-signet','bitcoin-testnet4')),
    CHECK (txid ~ '^[0-9a-f]{64}$'), CHECK (spend_txid IS NULL OR spend_txid ~ '^[0-9a-f]{64}$'),
    CHECK (script_pub_key ~ '^0014[0-9a-f]{40}$'), CHECK (vout >= 0), CHECK (value_sats > 0),
    CHECK (block_height >= 0), CHECK (status IN ('UNCONFIRMED', 'UNSPENT', 'LOCKED', 'SPENT', 'UNKNOWN')),
    CHECK ((status='LOCKED' AND locked_by IS NOT NULL AND locked_until IS NOT NULL) OR status<>'LOCKED')
);

CREATE TABLE btc_sweeps (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deposit_id BIGINT NOT NULL UNIQUE REFERENCES btc_deposits(id), utxo_id BIGINT NOT NULL UNIQUE REFERENCES btc_utxos(id),
    from_address_id BIGINT NOT NULL REFERENCES btc_addresses(id), to_address_id BIGINT NOT NULL REFERENCES btc_addresses(id),
    input_value_sats BIGINT NOT NULL, output_value_sats BIGINT, fee_sats BIGINT, fee_rate_sat_vb BIGINT,
    raw_tx BYTEA, txid TEXT, block_height BIGINT, confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL, error_code TEXT, error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (input_value_sats > 0), CHECK (output_value_sats IS NULL OR output_value_sats > 0),
    CHECK (fee_sats IS NULL OR fee_sats > 0), CHECK (fee_rate_sat_vb IS NULL OR fee_rate_sat_vb > 0),
    CHECK (txid IS NULL OR txid ~ '^[0-9a-f]{64}$'), CHECK (block_height IS NULL OR block_height >= 0),
    CHECK (confirmations >= 0), CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'))
);
CREATE UNIQUE INDEX btc_sweeps_txid_idx ON btc_sweeps (txid) WHERE txid IS NOT NULL;
CREATE INDEX btc_deposits_status_height_idx ON btc_deposits (status, block_height);
CREATE INDEX btc_sweeps_status_created_idx ON btc_sweeps (status, created_at);

CREATE TABLE btc_withdrawals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id), idempotency_key TEXT NOT NULL,
    to_address TEXT NOT NULL, amount_sats BIGINT NOT NULL, fee_sats BIGINT NOT NULL DEFAULT 0,
    change_sats BIGINT NOT NULL DEFAULT 0, fee_rate_sat_vb BIGINT NOT NULL,
    selected_inputs_json JSONB NOT NULL, outputs_json JSONB NOT NULL, psbt_hash TEXT,
    raw_tx_hash TEXT, txid TEXT, block_height BIGINT, confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL, error_code TEXT, error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, idempotency_key), CHECK (length(btrim(idempotency_key))>0),
    CHECK (to_address ~ '^tb1q[023456789acdefghjklmnpqrstuvwxyz]{38}$'), CHECK (amount_sats>0),
    CHECK (fee_sats>=0 AND change_sats>=0 AND fee_rate_sat_vb>0), CHECK (confirmations>=0),
    CHECK (status IN ('CREATED','INPUTS_LOCKED','SIGNING','SIGNED','BROADCASTING','BROADCAST_UNKNOWN','BROADCASTED','CONFIRMING','COMPLETED','FAILED'))
);
CREATE INDEX btc_withdrawals_status_created_idx ON btc_withdrawals(status,created_at);

COMMENT ON TABLE btc_addresses IS 'Bitcoin Signet 托管地址';
COMMENT ON TABLE btc_deposits IS 'Bitcoin Signet 充值输出';
COMMENT ON TABLE btc_utxos IS 'Bitcoin Signet 受控 UTXO';
COMMENT ON TABLE btc_sweeps IS 'Bitcoin Signet 自动归集任务';
COMMENT ON COLUMN btc_addresses.id IS '主键'; COMMENT ON COLUMN btc_addresses.user_id IS '用户 ID'; COMMENT ON COLUMN btc_addresses.network IS '网络'; COMMENT ON COLUMN btc_addresses.purpose IS '用途'; COMMENT ON COLUMN btc_addresses.address IS 'Bech32 地址'; COMMENT ON COLUMN btc_addresses.script_pub_key IS '输出脚本'; COMMENT ON COLUMN btc_addresses.derivation_index IS '派生索引'; COMMENT ON COLUMN btc_addresses.derivation_path IS '派生路径'; COMMENT ON COLUMN btc_addresses.enabled IS '是否启用'; COMMENT ON COLUMN btc_addresses.created_at IS '创建时间'; COMMENT ON COLUMN btc_addresses.updated_at IS '更新时间';
COMMENT ON COLUMN btc_deposits.id IS '主键'; COMMENT ON COLUMN btc_deposits.user_id IS '用户 ID'; COMMENT ON COLUMN btc_deposits.address_id IS '收款地址 ID'; COMMENT ON COLUMN btc_deposits.network IS '网络'; COMMENT ON COLUMN btc_deposits.txid IS '交易 ID'; COMMENT ON COLUMN btc_deposits.vout IS '输出序号'; COMMENT ON COLUMN btc_deposits.block_hash IS '区块哈希'; COMMENT ON COLUMN btc_deposits.block_height IS '区块高度'; COMMENT ON COLUMN btc_deposits.amount_sats IS '金额 satoshi'; COMMENT ON COLUMN btc_deposits.confirmations IS '确认数'; COMMENT ON COLUMN btc_deposits.status IS '状态'; COMMENT ON COLUMN btc_deposits.created_at IS '创建时间'; COMMENT ON COLUMN btc_deposits.updated_at IS '更新时间';
COMMENT ON COLUMN btc_utxos.id IS '主键'; COMMENT ON COLUMN btc_utxos.deposit_id IS '充值 ID'; COMMENT ON COLUMN btc_utxos.address_id IS '地址 ID'; COMMENT ON COLUMN btc_utxos.network IS '网络'; COMMENT ON COLUMN btc_utxos.txid IS '交易 ID'; COMMENT ON COLUMN btc_utxos.vout IS '输出序号'; COMMENT ON COLUMN btc_utxos.value_sats IS '金额 satoshi'; COMMENT ON COLUMN btc_utxos.script_pub_key IS '输出脚本'; COMMENT ON COLUMN btc_utxos.block_height IS '区块高度'; COMMENT ON COLUMN btc_utxos.spend_txid IS '花费交易 ID'; COMMENT ON COLUMN btc_utxos.status IS '状态'; COMMENT ON COLUMN btc_utxos.created_at IS '创建时间'; COMMENT ON COLUMN btc_utxos.updated_at IS '更新时间';
COMMENT ON COLUMN btc_utxos.locked_by IS 'UTXO 租约持有者'; COMMENT ON COLUMN btc_utxos.locked_until IS 'UTXO 租约过期时间';
COMMENT ON COLUMN btc_sweeps.id IS '主键'; COMMENT ON COLUMN btc_sweeps.deposit_id IS '充值 ID'; COMMENT ON COLUMN btc_sweeps.utxo_id IS 'UTXO ID'; COMMENT ON COLUMN btc_sweeps.from_address_id IS '来源地址 ID'; COMMENT ON COLUMN btc_sweeps.to_address_id IS '平台目标地址 ID'; COMMENT ON COLUMN btc_sweeps.input_value_sats IS '输入金额'; COMMENT ON COLUMN btc_sweeps.output_value_sats IS '输出金额'; COMMENT ON COLUMN btc_sweeps.fee_sats IS '网络费'; COMMENT ON COLUMN btc_sweeps.fee_rate_sat_vb IS '费率'; COMMENT ON COLUMN btc_sweeps.raw_tx IS '已签名原始交易'; COMMENT ON COLUMN btc_sweeps.txid IS '归集交易 ID'; COMMENT ON COLUMN btc_sweeps.block_height IS '确认区块高度'; COMMENT ON COLUMN btc_sweeps.confirmations IS '确认数'; COMMENT ON COLUMN btc_sweeps.status IS '状态'; COMMENT ON COLUMN btc_sweeps.error_code IS '错误码'; COMMENT ON COLUMN btc_sweeps.error_message IS '错误说明'; COMMENT ON COLUMN btc_sweeps.created_at IS '创建时间'; COMMENT ON COLUMN btc_sweeps.updated_at IS '更新时间';
COMMENT ON TABLE btc_withdrawals IS 'Bitcoin Signet 提币任务';
COMMENT ON COLUMN btc_withdrawals.id IS '主键'; COMMENT ON COLUMN btc_withdrawals.user_id IS '用户 ID'; COMMENT ON COLUMN btc_withdrawals.idempotency_key IS '幂等键'; COMMENT ON COLUMN btc_withdrawals.to_address IS '目标地址'; COMMENT ON COLUMN btc_withdrawals.amount_sats IS '提币金额'; COMMENT ON COLUMN btc_withdrawals.fee_sats IS '手续费'; COMMENT ON COLUMN btc_withdrawals.change_sats IS '找零金额'; COMMENT ON COLUMN btc_withdrawals.fee_rate_sat_vb IS '费率'; COMMENT ON COLUMN btc_withdrawals.selected_inputs_json IS '选中输入摘要'; COMMENT ON COLUMN btc_withdrawals.outputs_json IS '输出摘要'; COMMENT ON COLUMN btc_withdrawals.psbt_hash IS 'PSBT 摘要哈希'; COMMENT ON COLUMN btc_withdrawals.raw_tx_hash IS '原始交易哈希'; COMMENT ON COLUMN btc_withdrawals.txid IS '交易 ID'; COMMENT ON COLUMN btc_withdrawals.block_height IS '区块高度'; COMMENT ON COLUMN btc_withdrawals.confirmations IS '确认数'; COMMENT ON COLUMN btc_withdrawals.status IS '状态'; COMMENT ON COLUMN btc_withdrawals.error_code IS '错误码'; COMMENT ON COLUMN btc_withdrawals.error_message IS '错误说明'; COMMENT ON COLUMN btc_withdrawals.created_at IS '创建时间'; COMMENT ON COLUMN btc_withdrawals.updated_at IS '更新时间';
