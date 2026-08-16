CREATE TABLE assets (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    network TEXT NOT NULL,
    asset_type TEXT NOT NULL,
    symbol TEXT NOT NULL,
    contract_address TEXT,
    decimals SMALLINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, symbol),
    CHECK (network = 'ethereum-sepolia'),
    CHECK (asset_type IN ('NATIVE', 'ERC20')),
    CHECK (symbol ~ '^[A-Z0-9]{2,12}$'),
    CHECK (decimals >= 1 AND decimals <= 18),
    CHECK (contract_address IS NULL OR contract_address ~ '^0x[0-9a-f]{40}$'),
    CHECK ((asset_type = 'NATIVE' AND contract_address IS NULL) OR
           (asset_type = 'ERC20' AND contract_address IS NOT NULL))
);

CREATE UNIQUE INDEX assets_network_contract_idx
    ON assets (network, contract_address)
    WHERE contract_address IS NOT NULL;
CREATE UNIQUE INDEX assets_native_network_idx
    ON assets (network)
    WHERE asset_type = 'NATIVE';

INSERT INTO assets (network, asset_type, symbol, decimals, enabled)
VALUES ('ethereum-sepolia', 'NATIVE', 'ETH', 18, TRUE);

ALTER TABLE asset_balances DROP CONSTRAINT asset_balances_asset_check;
ALTER TABLE asset_balances ADD COLUMN asset_id BIGINT;
UPDATE asset_balances
SET asset_id = (SELECT id FROM assets WHERE network = 'ethereum-sepolia' AND symbol = 'ETH');
ALTER TABLE asset_balances ALTER COLUMN asset_id SET NOT NULL;
ALTER TABLE asset_balances
    ADD CONSTRAINT asset_balances_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES assets(id),
    ADD CONSTRAINT asset_balances_user_asset_id_key UNIQUE (user_id, asset_id),
    ADD CONSTRAINT asset_balances_asset_symbol_check CHECK (asset ~ '^[A-Z0-9]{2,12}$');

ALTER TABLE balance_entries DROP CONSTRAINT balance_entries_asset_check;
ALTER TABLE balance_entries DROP CONSTRAINT balance_entries_reference_type_check;
ALTER TABLE balance_entries ADD COLUMN asset_id BIGINT;
UPDATE balance_entries
SET asset_id = (SELECT id FROM assets WHERE network = 'ethereum-sepolia' AND symbol = 'ETH');
ALTER TABLE balance_entries ALTER COLUMN asset_id SET NOT NULL;
ALTER TABLE balance_entries
    ADD CONSTRAINT balance_entries_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES assets(id),
    ADD CONSTRAINT balance_entries_asset_symbol_check CHECK (asset ~ '^[A-Z0-9]{2,12}$'),
    ADD CONSTRAINT balance_entries_reference_type_check
        CHECK (reference_type IN ('DEPOSIT', 'WITHDRAWAL', 'TOKEN_DEPOSIT', 'TOKEN_WITHDRAWAL'));

CREATE TABLE platform_wallets (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    network TEXT NOT NULL,
    role TEXT NOT NULL,
    address TEXT NOT NULL,
    derivation_path TEXT NOT NULL,
    next_nonce NUMERIC(20, 0) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, role),
    UNIQUE (network, address),
    CHECK (network = 'ethereum-sepolia'),
    CHECK (role = 'HOT'),
    CHECK (address ~ '^0x[0-9a-f]{40}$'),
    CHECK (derivation_path = 'm/44''/60''/0''/0/0'),
    CHECK (next_nonce >= 0)
);

CREATE TABLE token_deposits (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    address_id BIGINT NOT NULL REFERENCES wallet_addresses(id),
    asset_id BIGINT NOT NULL REFERENCES assets(id),
    tx_hash TEXT NOT NULL,
    log_index INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    block_hash TEXT NOT NULL,
    from_address TEXT NOT NULL,
    to_address TEXT NOT NULL,
    amount_units NUMERIC(78, 0) NOT NULL,
    confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (asset_id, tx_hash, log_index),
    CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (log_index >= 0),
    CHECK (block_number >= 0),
    CHECK (block_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (from_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (amount_units > 0),
    CHECK (confirmations >= 0),
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED'))
);

CREATE TABLE token_sweeps (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    address_id BIGINT NOT NULL REFERENCES wallet_addresses(id),
    asset_id BIGINT NOT NULL REFERENCES assets(id),
    trigger_deposit_id BIGINT NOT NULL REFERENCES token_deposits(id),
    recognized_amount_units NUMERIC(78, 0) NOT NULL,
    sweep_amount_units NUMERIC(78, 0),
    gas_topup_transfer_id BIGINT,
    nonce NUMERIC(20, 0),
    gas_limit BIGINT,
    max_fee_per_gas_wei NUMERIC(78, 0),
    max_priority_fee_per_gas_wei NUMERIC(78, 0),
    raw_tx BYTEA,
    tx_hash TEXT,
    block_number BIGINT,
    confirmations BIGINT NOT NULL DEFAULT 0,
    actual_fee_wei NUMERIC(78, 0),
    status TEXT NOT NULL,
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (recognized_amount_units > 0),
    CHECK (sweep_amount_units IS NULL OR sweep_amount_units > 0),
    CHECK (nonce IS NULL OR nonce >= 0),
    CHECK (gas_limit IS NULL OR gas_limit > 0),
    CHECK (max_fee_per_gas_wei IS NULL OR max_fee_per_gas_wei > 0),
    CHECK (max_priority_fee_per_gas_wei IS NULL OR max_priority_fee_per_gas_wei >= 0),
    CHECK (tx_hash IS NULL OR tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (block_number IS NULL OR block_number >= 0),
    CHECK (confirmations >= 0),
    CHECK (actual_fee_wei IS NULL OR actual_fee_wei >= 0),
    CHECK (status IN ('CREATED', 'WAITING_GAS', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'))
);

CREATE UNIQUE INDEX token_sweeps_active_address_asset_idx
    ON token_sweeps (address_id, asset_id)
    WHERE status IN ('CREATED', 'WAITING_GAS', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING');
CREATE UNIQUE INDEX token_sweeps_address_nonce_idx
    ON token_sweeps (address_id, nonce)
    WHERE nonce IS NOT NULL;
CREATE UNIQUE INDEX token_sweeps_tx_hash_idx
    ON token_sweeps (tx_hash)
    WHERE tx_hash IS NOT NULL;

CREATE TABLE internal_transfers (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    platform_wallet_id BIGINT NOT NULL REFERENCES platform_wallets(id),
    sweep_id BIGINT NOT NULL REFERENCES token_sweeps(id),
    transfer_type TEXT NOT NULL,
    from_address TEXT NOT NULL,
    to_address TEXT NOT NULL,
    amount_wei NUMERIC(78, 0) NOT NULL,
    nonce NUMERIC(20, 0),
    gas_limit BIGINT,
    max_fee_per_gas_wei NUMERIC(78, 0),
    max_priority_fee_per_gas_wei NUMERIC(78, 0),
    raw_tx BYTEA,
    tx_hash TEXT,
    block_number BIGINT,
    confirmations BIGINT NOT NULL DEFAULT 0,
    actual_fee_wei NUMERIC(78, 0),
    status TEXT NOT NULL,
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (sweep_id, transfer_type),
    CHECK (transfer_type = 'GAS_TOPUP'),
    CHECK (from_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (amount_wei > 0),
    CHECK (nonce IS NULL OR nonce >= 0),
    CHECK (gas_limit IS NULL OR gas_limit > 0),
    CHECK (max_fee_per_gas_wei IS NULL OR max_fee_per_gas_wei > 0),
    CHECK (max_priority_fee_per_gas_wei IS NULL OR max_priority_fee_per_gas_wei >= 0),
    CHECK (tx_hash IS NULL OR tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (block_number IS NULL OR block_number >= 0),
    CHECK (confirmations >= 0),
    CHECK (actual_fee_wei IS NULL OR actual_fee_wei >= 0),
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'))
);

CREATE UNIQUE INDEX internal_transfers_wallet_nonce_idx
    ON internal_transfers (platform_wallet_id, nonce)
    WHERE nonce IS NOT NULL;
CREATE UNIQUE INDEX internal_transfers_tx_hash_idx
    ON internal_transfers (tx_hash)
    WHERE tx_hash IS NOT NULL;

ALTER TABLE token_sweeps
    ADD CONSTRAINT token_sweeps_gas_topup_transfer_id_fkey
    FOREIGN KEY (gas_topup_transfer_id) REFERENCES internal_transfers(id),
    ADD CONSTRAINT token_sweeps_gas_topup_transfer_id_key UNIQUE (gas_topup_transfer_id);

CREATE TABLE token_withdrawals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    asset_id BIGINT NOT NULL REFERENCES assets(id),
    platform_wallet_id BIGINT NOT NULL REFERENCES platform_wallets(id),
    to_address TEXT NOT NULL,
    amount_units NUMERIC(78, 0) NOT NULL,
    nonce NUMERIC(20, 0),
    gas_limit BIGINT,
    max_fee_per_gas_wei NUMERIC(78, 0),
    max_priority_fee_per_gas_wei NUMERIC(78, 0),
    raw_tx BYTEA,
    tx_hash TEXT,
    block_number BIGINT,
    confirmations BIGINT NOT NULL DEFAULT 0,
    actual_fee_wei NUMERIC(78, 0),
    status TEXT NOT NULL,
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, idempotency_key),
    CHECK (length(btrim(idempotency_key)) > 0),
    CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (amount_units > 0),
    CHECK (nonce IS NULL OR nonce >= 0),
    CHECK (gas_limit IS NULL OR gas_limit > 0),
    CHECK (max_fee_per_gas_wei IS NULL OR max_fee_per_gas_wei > 0),
    CHECK (max_priority_fee_per_gas_wei IS NULL OR max_priority_fee_per_gas_wei >= 0),
    CHECK (tx_hash IS NULL OR tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (block_number IS NULL OR block_number >= 0),
    CHECK (confirmations >= 0),
    CHECK (actual_fee_wei IS NULL OR actual_fee_wei >= 0),
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'))
);

CREATE UNIQUE INDEX token_withdrawals_wallet_nonce_idx
    ON token_withdrawals (platform_wallet_id, nonce)
    WHERE nonce IS NOT NULL;
CREATE UNIQUE INDEX token_withdrawals_tx_hash_idx
    ON token_withdrawals (tx_hash)
    WHERE tx_hash IS NOT NULL;

COMMENT ON TABLE assets IS '系统支持的链上资产主数据表';
COMMENT ON COLUMN assets.id IS '资产自增主键';
COMMENT ON COLUMN assets.network IS '资产所在区块链网络';
COMMENT ON COLUMN assets.asset_type IS '资产类型：NATIVE 或 ERC20';
COMMENT ON COLUMN assets.symbol IS '资产展示代码';
COMMENT ON COLUMN assets.contract_address IS 'ERC-20 合约小写地址，原生资产为空';
COMMENT ON COLUMN assets.decimals IS '资产链上精度';
COMMENT ON COLUMN assets.enabled IS '是否允许该资产的 Worker 运行';
COMMENT ON COLUMN assets.created_at IS '资产创建时间';
COMMENT ON COLUMN assets.updated_at IS '资产最后更新时间';

COMMENT ON COLUMN asset_balances.asset_id IS '余额对应的资产主数据 ID';
COMMENT ON COLUMN asset_balances.asset IS '资产展示代码，保留用于兼容现有接口';
COMMENT ON COLUMN asset_balances.available_wei IS '已确认可用余额，单位为对应资产最小单位';
COMMENT ON COLUMN asset_balances.pending_deposit_wei IS '待确认充值余额，单位为对应资产最小单位';
COMMENT ON COLUMN asset_balances.pending_withdrawal_wei IS '提币处理中余额，单位为对应资产最小单位';

COMMENT ON COLUMN balance_entries.asset_id IS '流水对应的资产主数据 ID';
COMMENT ON COLUMN balance_entries.asset IS '发生变动的资产展示代码';
COMMENT ON COLUMN balance_entries.amount_wei IS '对用户余额的有符号变动，单位为对应资产最小单位';
COMMENT ON COLUMN balance_entries.reference_type IS '关联类型：DEPOSIT、WITHDRAWAL、TOKEN_DEPOSIT 或 TOKEN_WITHDRAWAL';

COMMENT ON TABLE platform_wallets IS '平台自有链上钱包表';
COMMENT ON COLUMN platform_wallets.id IS '平台钱包自增主键';
COMMENT ON COLUMN platform_wallets.network IS '平台钱包所在区块链网络';
COMMENT ON COLUMN platform_wallets.role IS '平台钱包角色，当前固定为 HOT';
COMMENT ON COLUMN platform_wallets.address IS '平台钱包规范化小写地址';
COMMENT ON COLUMN platform_wallets.derivation_path IS '平台钱包完整派生路径';
COMMENT ON COLUMN platform_wallets.next_nonce IS '平台钱包下一候选 Nonce';
COMMENT ON COLUMN platform_wallets.created_at IS '平台钱包创建时间';
COMMENT ON COLUMN platform_wallets.updated_at IS '平台钱包最后更新时间';

COMMENT ON TABLE token_deposits IS 'ERC-20 Transfer 充值 Event 记录表';
COMMENT ON COLUMN token_deposits.id IS 'Token 充值自增主键';
COMMENT ON COLUMN token_deposits.user_id IS '充值归属用户 ID';
COMMENT ON COLUMN token_deposits.address_id IS '接收 Token 的用户钱包地址 ID';
COMMENT ON COLUMN token_deposits.asset_id IS '充值 Token 资产 ID';
COMMENT ON COLUMN token_deposits.tx_hash IS '产生 Transfer Event 的交易哈希';
COMMENT ON COLUMN token_deposits.log_index IS 'Event 在交易 Receipt 中的日志索引';
COMMENT ON COLUMN token_deposits.block_number IS 'Event 所在区块高度';
COMMENT ON COLUMN token_deposits.block_hash IS 'Event 所在区块哈希，用于确认前复核';
COMMENT ON COLUMN token_deposits.from_address IS 'Transfer Event 发送方小写地址';
COMMENT ON COLUMN token_deposits.to_address IS 'Transfer Event 接收方小写地址';
COMMENT ON COLUMN token_deposits.amount_units IS 'Token 充值金额，单位为 Token 最小单位';
COMMENT ON COLUMN token_deposits.confirmations IS '最近一次计算得到的确认数';
COMMENT ON COLUMN token_deposits.status IS 'Token 充值生命周期状态';
COMMENT ON COLUMN token_deposits.created_at IS 'Token 充值记录创建时间';
COMMENT ON COLUMN token_deposits.updated_at IS 'Token 充值记录最后更新时间';

COMMENT ON TABLE token_sweeps IS '用户地址 Token 自动归集任务表';
COMMENT ON COLUMN token_sweeps.id IS 'Token 归集任务自增主键';
COMMENT ON COLUMN token_sweeps.user_id IS '归集所属用户 ID';
COMMENT ON COLUMN token_sweeps.address_id IS '归集来源用户钱包地址 ID';
COMMENT ON COLUMN token_sweeps.asset_id IS '归集 Token 资产 ID';
COMMENT ON COLUMN token_sweeps.trigger_deposit_id IS '首次触发该归集任务的 Token 充值 ID';
COMMENT ON COLUMN token_sweeps.recognized_amount_units IS '系统已识别且允许归集的 Token 数量';
COMMENT ON COLUMN token_sweeps.sweep_amount_units IS '签名前最终确定的实际归集数量';
COMMENT ON COLUMN token_sweeps.gas_topup_transfer_id IS '为该归集补充 Gas 的内部转账 ID';
COMMENT ON COLUMN token_sweeps.nonce IS '用户地址分配给归集交易的 Nonce';
COMMENT ON COLUMN token_sweeps.gas_limit IS '归集交易 Gas Limit';
COMMENT ON COLUMN token_sweeps.max_fee_per_gas_wei IS '归集交易每 Gas 最大费用，单位 Wei';
COMMENT ON COLUMN token_sweeps.max_priority_fee_per_gas_wei IS '归集交易每 Gas 最大优先费，单位 Wei';
COMMENT ON COLUMN token_sweeps.raw_tx IS '已签名归集交易原始字节';
COMMENT ON COLUMN token_sweeps.tx_hash IS '已签名归集交易哈希';
COMMENT ON COLUMN token_sweeps.block_number IS '归集交易打包区块高度';
COMMENT ON COLUMN token_sweeps.confirmations IS '归集交易确认数';
COMMENT ON COLUMN token_sweeps.actual_fee_wei IS '归集交易实际消耗的 ETH Gas';
COMMENT ON COLUMN token_sweeps.status IS 'Token 归集生命周期状态';
COMMENT ON COLUMN token_sweeps.error_code IS '归集失败的稳定错误码';
COMMENT ON COLUMN token_sweeps.error_message IS '不包含敏感信息的归集错误说明';
COMMENT ON COLUMN token_sweeps.created_at IS '归集任务创建时间';
COMMENT ON COLUMN token_sweeps.updated_at IS '归集任务最后更新时间';

COMMENT ON TABLE internal_transfers IS '平台内部 ETH 转账表，当前用于归集 Gas 补充';
COMMENT ON COLUMN internal_transfers.id IS '内部转账自增主键';
COMMENT ON COLUMN internal_transfers.platform_wallet_id IS '发送 ETH 的平台钱包 ID';
COMMENT ON COLUMN internal_transfers.sweep_id IS '需要 Gas 的 Token 归集任务 ID';
COMMENT ON COLUMN internal_transfers.transfer_type IS '内部转账类型，当前固定为 GAS_TOPUP';
COMMENT ON COLUMN internal_transfers.from_address IS '内部转账发送方小写地址';
COMMENT ON COLUMN internal_transfers.to_address IS '内部转账接收方小写地址';
COMMENT ON COLUMN internal_transfers.amount_wei IS '内部转账 ETH 金额，单位 Wei';
COMMENT ON COLUMN internal_transfers.nonce IS '平台钱包分配给内部转账的 Nonce';
COMMENT ON COLUMN internal_transfers.gas_limit IS '内部转账 Gas Limit';
COMMENT ON COLUMN internal_transfers.max_fee_per_gas_wei IS '内部转账每 Gas 最大费用，单位 Wei';
COMMENT ON COLUMN internal_transfers.max_priority_fee_per_gas_wei IS '内部转账每 Gas 最大优先费，单位 Wei';
COMMENT ON COLUMN internal_transfers.raw_tx IS '已签名内部转账原始字节';
COMMENT ON COLUMN internal_transfers.tx_hash IS '已签名内部转账交易哈希';
COMMENT ON COLUMN internal_transfers.block_number IS '内部转账打包区块高度';
COMMENT ON COLUMN internal_transfers.confirmations IS '内部转账确认数';
COMMENT ON COLUMN internal_transfers.actual_fee_wei IS '内部转账实际消耗的 ETH Gas';
COMMENT ON COLUMN internal_transfers.status IS '内部转账生命周期状态';
COMMENT ON COLUMN internal_transfers.error_code IS '内部转账失败的稳定错误码';
COMMENT ON COLUMN internal_transfers.error_message IS '不包含敏感信息的内部转账错误说明';
COMMENT ON COLUMN internal_transfers.created_at IS '内部转账记录创建时间';
COMMENT ON COLUMN internal_transfers.updated_at IS '内部转账记录最后更新时间';

COMMENT ON TABLE token_withdrawals IS '用户 ERC-20 提币请求及链上交易状态表';
COMMENT ON COLUMN token_withdrawals.id IS 'Token 提币自增主键';
COMMENT ON COLUMN token_withdrawals.idempotency_key IS '用户范围内唯一的提币幂等标识';
COMMENT ON COLUMN token_withdrawals.user_id IS 'Token 提币所属用户 ID';
COMMENT ON COLUMN token_withdrawals.asset_id IS '提币 Token 资产 ID';
COMMENT ON COLUMN token_withdrawals.platform_wallet_id IS '执行 Token 出款的平台热钱包 ID';
COMMENT ON COLUMN token_withdrawals.to_address IS 'Token 提币目标小写地址';
COMMENT ON COLUMN token_withdrawals.amount_units IS '提币 Token 数量，单位为 Token 最小单位';
COMMENT ON COLUMN token_withdrawals.nonce IS '平台热钱包分配给 Token 提币的 Nonce';
COMMENT ON COLUMN token_withdrawals.gas_limit IS 'Token 提币交易 Gas Limit';
COMMENT ON COLUMN token_withdrawals.max_fee_per_gas_wei IS 'Token 提币每 Gas 最大费用，单位 Wei';
COMMENT ON COLUMN token_withdrawals.max_priority_fee_per_gas_wei IS 'Token 提币每 Gas 最大优先费，单位 Wei';
COMMENT ON COLUMN token_withdrawals.raw_tx IS '已签名 Token 提币交易原始字节';
COMMENT ON COLUMN token_withdrawals.tx_hash IS '已签名 Token 提币交易哈希';
COMMENT ON COLUMN token_withdrawals.block_number IS 'Token 提币交易打包区块高度';
COMMENT ON COLUMN token_withdrawals.confirmations IS 'Token 提币交易确认数';
COMMENT ON COLUMN token_withdrawals.actual_fee_wei IS 'Token 提币实际消耗的 ETH Gas';
COMMENT ON COLUMN token_withdrawals.status IS 'Token 提币生命周期状态';
COMMENT ON COLUMN token_withdrawals.error_code IS 'Token 提币失败的稳定错误码';
COMMENT ON COLUMN token_withdrawals.error_message IS '不包含敏感信息的 Token 提币错误说明';
COMMENT ON COLUMN token_withdrawals.created_at IS 'Token 提币记录创建时间';
COMMENT ON COLUMN token_withdrawals.updated_at IS 'Token 提币记录最后更新时间';

CREATE INDEX assets_enabled_idx ON assets (enabled, id);
CREATE INDEX asset_balances_asset_user_idx ON asset_balances (asset_id, user_id);
CREATE INDEX token_deposits_status_block_idx ON token_deposits (status, block_number, log_index);
CREATE INDEX token_deposits_user_created_idx ON token_deposits (user_id, created_at DESC);
CREATE INDEX token_sweeps_status_created_idx ON token_sweeps (status, created_at);
CREATE INDEX internal_transfers_status_created_idx ON internal_transfers (status, created_at);
CREATE INDEX token_withdrawals_status_created_idx ON token_withdrawals (status, created_at);
