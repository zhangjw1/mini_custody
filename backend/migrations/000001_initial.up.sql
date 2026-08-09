CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (code ~ '^[a-z][a-z0-9_-]{2,63}$'),
    CHECK (length(btrim(display_name)) > 0)
);

CREATE TABLE wallet_addresses (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    network TEXT NOT NULL,
    address TEXT NOT NULL,
    derivation_index BIGINT NOT NULL,
    derivation_path TEXT NOT NULL,
    next_nonce NUMERIC(20, 0) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, network),
    UNIQUE (network, address),
    UNIQUE (network, derivation_index),
    CHECK (network = 'ethereum-sepolia'),
    CHECK (address ~ '^0x[0-9a-f]{40}$'),
    CHECK (derivation_index >= 0 AND derivation_index <= 4294967295),
    CHECK (next_nonce >= 0)
);

CREATE TABLE asset_balances (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    asset TEXT NOT NULL,
    available_wei NUMERIC(78, 0) NOT NULL DEFAULT 0,
    pending_deposit_wei NUMERIC(78, 0) NOT NULL DEFAULT 0,
    pending_withdrawal_wei NUMERIC(78, 0) NOT NULL DEFAULT 0,
    version BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, asset),
    CHECK (asset = 'ETH'),
    CHECK (available_wei >= 0),
    CHECK (pending_deposit_wei >= 0),
    CHECK (pending_withdrawal_wei >= 0),
    CHECK (version >= 0)
);

CREATE TABLE deposits (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    address_id BIGINT NOT NULL REFERENCES wallet_addresses(id),
    network TEXT NOT NULL,
    asset TEXT NOT NULL,
    tx_hash TEXT NOT NULL,
    tx_index INTEGER NOT NULL,
    block_number BIGINT NOT NULL,
    block_hash TEXT NOT NULL,
    amount_wei NUMERIC(78, 0) NOT NULL,
    confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, tx_hash, address_id),
    CHECK (network = 'ethereum-sepolia'),
    CHECK (asset = 'ETH'),
    CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (block_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (tx_index >= 0),
    CHECK (block_number >= 0),
    CHECK (amount_wei > 0),
    CHECK (confirmations >= 0),
    CHECK (status IN ('DETECTED', 'CONFIRMING', 'CONFIRMED', 'CREDITED'))
);

CREATE TABLE withdrawals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    address_id BIGINT NOT NULL REFERENCES wallet_addresses(id),
    to_address TEXT NOT NULL,
    amount_wei NUMERIC(78, 0) NOT NULL,
    reserved_fee_wei NUMERIC(78, 0) NOT NULL,
    actual_fee_wei NUMERIC(78, 0),
    nonce NUMERIC(20, 0),
    gas_limit BIGINT,
    max_fee_per_gas_wei NUMERIC(78, 0),
    max_priority_fee_per_gas_wei NUMERIC(78, 0),
    raw_tx BYTEA,
    tx_hash TEXT,
    block_number BIGINT,
    confirmations BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error_code TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, idempotency_key),
    CHECK (length(btrim(idempotency_key)) > 0),
    CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (amount_wei > 0),
    CHECK (reserved_fee_wei >= 0),
    CHECK (actual_fee_wei IS NULL OR actual_fee_wei >= 0),
    CHECK (nonce IS NULL OR nonce >= 0),
    CHECK (gas_limit IS NULL OR gas_limit > 0),
    CHECK (max_fee_per_gas_wei IS NULL OR max_fee_per_gas_wei > 0),
    CHECK (max_priority_fee_per_gas_wei IS NULL OR max_priority_fee_per_gas_wei >= 0),
    CHECK (tx_hash IS NULL OR tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (block_number IS NULL OR block_number >= 0),
    CHECK (confirmations >= 0),
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTING', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'))
);

CREATE TABLE balance_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    asset TEXT NOT NULL,
    entry_type TEXT NOT NULL,
    amount_wei NUMERIC(78, 0) NOT NULL,
    reference_type TEXT NOT NULL,
    reference_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (entry_type, reference_type, reference_id),
    CHECK (asset = 'ETH'),
    CHECK (entry_type IN ('DEPOSIT_PENDING', 'DEPOSIT_CREDIT', 'WITHDRAW_RESERVE', 'WITHDRAW_FINALIZE', 'WITHDRAW_RELEASE', 'FEE_ADJUST')),
    CHECK (reference_type IN ('DEPOSIT', 'WITHDRAWAL'))
);

CREATE TABLE chain_checkpoints (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    network TEXT NOT NULL,
    scanner TEXT NOT NULL,
    last_scanned_block BIGINT NOT NULL,
    last_scanned_hash TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (network, scanner),
    CHECK (network = 'ethereum-sepolia'),
    CHECK (length(btrim(scanner)) > 0),
    CHECK (last_scanned_block >= 0),
    CHECK (last_scanned_hash ~ '^0x[0-9a-f]{64}$')
);

CREATE TABLE worker_errors (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    worker TEXT NOT NULL,
    stage TEXT NOT NULL,
    reference_type TEXT,
    reference_id BIGINT,
    error_code TEXT NOT NULL,
    error_message TEXT NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0,
    first_occurred_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_occurred_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (length(btrim(worker)) > 0),
    CHECK (length(btrim(stage)) > 0),
    CHECK (length(btrim(error_code)) > 0),
    CHECK (retry_count >= 0)
);

COMMENT ON TABLE users IS '演示用户表';
COMMENT ON COLUMN users.id IS '用户自增主键';
COMMENT ON COLUMN users.code IS '稳定的用户编码，用于程序识别和幂等初始化';
COMMENT ON COLUMN users.display_name IS '用户展示名称';
COMMENT ON COLUMN users.created_at IS '用户创建时间';

COMMENT ON TABLE wallet_addresses IS '平台托管的用户链上地址表';
COMMENT ON COLUMN wallet_addresses.id IS '钱包地址记录自增主键';
COMMENT ON COLUMN wallet_addresses.user_id IS '地址所属用户 ID';
COMMENT ON COLUMN wallet_addresses.network IS '区块链网络标识，当前固定为 ethereum-sepolia';
COMMENT ON COLUMN wallet_addresses.address IS '规范化为小写十六进制的 EVM 地址';
COMMENT ON COLUMN wallet_addresses.derivation_index IS 'BIP-44 外部地址分支中的派生索引';
COMMENT ON COLUMN wallet_addresses.derivation_path IS '从托管根密钥派生该地址的完整路径';
COMMENT ON COLUMN wallet_addresses.next_nonce IS '本系统为该地址维护的下一候选 Nonce';
COMMENT ON COLUMN wallet_addresses.created_at IS '钱包地址创建时间';

COMMENT ON TABLE asset_balances IS '用户资产余额汇总表';
COMMENT ON COLUMN asset_balances.id IS '资产余额记录自增主键';
COMMENT ON COLUMN asset_balances.user_id IS '余额所属用户 ID';
COMMENT ON COLUMN asset_balances.asset IS '资产代码，当前固定为 ETH';
COMMENT ON COLUMN asset_balances.available_wei IS '已确认且可用于提币的余额，单位 Wei';
COMMENT ON COLUMN asset_balances.pending_deposit_wei IS '已发现但尚未确认入账的充值金额，单位 Wei';
COMMENT ON COLUMN asset_balances.pending_withdrawal_wei IS '已被提币流程占用但尚未结算的金额，单位 Wei';
COMMENT ON COLUMN asset_balances.version IS '余额并发控制版本号';
COMMENT ON COLUMN asset_balances.updated_at IS '余额最后更新时间';

COMMENT ON TABLE deposits IS '链上充值记录表';
COMMENT ON COLUMN deposits.id IS '充值记录自增主键';
COMMENT ON COLUMN deposits.user_id IS '充值归属用户 ID';
COMMENT ON COLUMN deposits.address_id IS '收到充值的托管地址记录 ID';
COMMENT ON COLUMN deposits.network IS '充值所在区块链网络';
COMMENT ON COLUMN deposits.asset IS '充值资产代码';
COMMENT ON COLUMN deposits.tx_hash IS '充值交易哈希，规范化为小写十六进制';
COMMENT ON COLUMN deposits.tx_index IS '交易在所在区块中的位置索引';
COMMENT ON COLUMN deposits.block_number IS '首次发现充值的区块高度';
COMMENT ON COLUMN deposits.block_hash IS '首次发现充值的区块哈希，用于确认前校验重组';
COMMENT ON COLUMN deposits.amount_wei IS '充值金额，单位 Wei';
COMMENT ON COLUMN deposits.confirmations IS '最近一次计算得到的确认数';
COMMENT ON COLUMN deposits.status IS '充值状态：DETECTED、CONFIRMING、CONFIRMED 或 CREDITED';
COMMENT ON COLUMN deposits.created_at IS '充值记录创建时间';
COMMENT ON COLUMN deposits.updated_at IS '充值记录最后更新时间';

COMMENT ON TABLE withdrawals IS '用户提币请求及链上交易状态表';
COMMENT ON COLUMN withdrawals.id IS '提币记录自增主键';
COMMENT ON COLUMN withdrawals.idempotency_key IS '客户端提供的提币幂等标识，与用户 ID 组成唯一约束';
COMMENT ON COLUMN withdrawals.user_id IS '提币所属用户 ID';
COMMENT ON COLUMN withdrawals.address_id IS '执行出款的用户托管地址记录 ID';
COMMENT ON COLUMN withdrawals.to_address IS '提币目标 EVM 地址，规范化为小写十六进制';
COMMENT ON COLUMN withdrawals.amount_wei IS '收款方应收到的提币金额，单位 Wei';
COMMENT ON COLUMN withdrawals.reserved_fee_wei IS '创建提币时占用的最大网络费，单位 Wei';
COMMENT ON COLUMN withdrawals.actual_fee_wei IS '根据链上 Receipt 结算的实际网络费，单位 Wei';
COMMENT ON COLUMN withdrawals.nonce IS '分配给链上交易的发送地址 Nonce';
COMMENT ON COLUMN withdrawals.gas_limit IS '签名交易使用的 Gas Limit';
COMMENT ON COLUMN withdrawals.max_fee_per_gas_wei IS 'EIP-1559 每 Gas 最大费用，单位 Wei';
COMMENT ON COLUMN withdrawals.max_priority_fee_per_gas_wei IS 'EIP-1559 每 Gas 最大优先费，单位 Wei';
COMMENT ON COLUMN withdrawals.raw_tx IS '已签名交易的原始字节，可用于相同交易的幂等重播';
COMMENT ON COLUMN withdrawals.tx_hash IS '已签名交易哈希，规范化为小写十六进制';
COMMENT ON COLUMN withdrawals.block_number IS '交易被打包的区块高度';
COMMENT ON COLUMN withdrawals.confirmations IS '最近一次计算得到的交易确认数';
COMMENT ON COLUMN withdrawals.status IS '提币内部生命周期状态';
COMMENT ON COLUMN withdrawals.error_code IS '经过清洗且可对外映射的错误码';
COMMENT ON COLUMN withdrawals.error_message IS '不包含密钥和 RPC 凭据的错误信息';
COMMENT ON COLUMN withdrawals.created_at IS '提币记录创建时间';
COMMENT ON COLUMN withdrawals.updated_at IS '提币记录最后更新时间';

COMMENT ON TABLE balance_entries IS '用户资产余额变动流水表，只追加不修改';
COMMENT ON COLUMN balance_entries.id IS '余额流水自增主键';
COMMENT ON COLUMN balance_entries.user_id IS '流水所属用户 ID';
COMMENT ON COLUMN balance_entries.asset IS '发生变动的资产代码';
COMMENT ON COLUMN balance_entries.entry_type IS '余额变动类型';
COMMENT ON COLUMN balance_entries.amount_wei IS '对用户资产的有符号变动金额，单位 Wei';
COMMENT ON COLUMN balance_entries.reference_type IS '关联业务类型：DEPOSIT 或 WITHDRAWAL';
COMMENT ON COLUMN balance_entries.reference_id IS '关联充值或提币记录的自增主键';
COMMENT ON COLUMN balance_entries.created_at IS '余额流水创建时间';

COMMENT ON TABLE chain_checkpoints IS '链上扫描器持久化检查点表';
COMMENT ON COLUMN chain_checkpoints.id IS '扫描检查点自增主键';
COMMENT ON COLUMN chain_checkpoints.network IS '扫描的区块链网络';
COMMENT ON COLUMN chain_checkpoints.scanner IS '扫描器业务标识';
COMMENT ON COLUMN chain_checkpoints.last_scanned_block IS '最后一个完整处理成功的区块高度';
COMMENT ON COLUMN chain_checkpoints.last_scanned_hash IS '最后一个完整处理成功的区块哈希';
COMMENT ON COLUMN chain_checkpoints.updated_at IS '检查点最后更新时间';

COMMENT ON TABLE worker_errors IS '后台 Worker 可观察错误记录表';
COMMENT ON COLUMN worker_errors.id IS 'Worker 错误记录自增主键';
COMMENT ON COLUMN worker_errors.worker IS '发生错误的 Worker 标识';
COMMENT ON COLUMN worker_errors.stage IS '发生错误的处理阶段';
COMMENT ON COLUMN worker_errors.reference_type IS '可选的关联业务类型';
COMMENT ON COLUMN worker_errors.reference_id IS '可选的关联业务记录 ID';
COMMENT ON COLUMN worker_errors.error_code IS '稳定且经过清洗的错误码';
COMMENT ON COLUMN worker_errors.error_message IS '不包含敏感信息的错误描述';
COMMENT ON COLUMN worker_errors.retry_count IS '同一错误已经执行的重试次数';
COMMENT ON COLUMN worker_errors.first_occurred_at IS '错误首次发生时间';
COMMENT ON COLUMN worker_errors.last_occurred_at IS '错误最近一次发生时间';

CREATE INDEX deposits_status_block_idx ON deposits (status, block_number);
CREATE INDEX withdrawals_status_created_idx ON withdrawals (status, created_at);
CREATE INDEX balance_entries_user_created_idx ON balance_entries (user_id, created_at DESC);
CREATE INDEX worker_errors_last_occurred_idx ON worker_errors (last_occurred_at DESC);
