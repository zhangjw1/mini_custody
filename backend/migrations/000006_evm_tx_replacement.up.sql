ALTER TABLE withdrawals
    ADD COLUMN broadcasted_at TIMESTAMPTZ,
    ADD COLUMN last_broadcast_at TIMESTAMPTZ,
    ADD COLUMN bump_count INTEGER NOT NULL DEFAULT 0 CHECK (bump_count >= 0);

ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTING', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

ALTER TABLE token_withdrawals
    ADD COLUMN broadcasted_at TIMESTAMPTZ,
    ADD COLUMN last_broadcast_at TIMESTAMPTZ,
    ADD COLUMN bump_count INTEGER NOT NULL DEFAULT 0 CHECK (bump_count >= 0);

ALTER TABLE token_withdrawals DROP CONSTRAINT token_withdrawals_status_check;
ALTER TABLE token_withdrawals ADD CONSTRAINT token_withdrawals_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

ALTER TABLE token_sweeps
    ADD COLUMN broadcasted_at TIMESTAMPTZ,
    ADD COLUMN last_broadcast_at TIMESTAMPTZ,
    ADD COLUMN bump_count INTEGER NOT NULL DEFAULT 0 CHECK (bump_count >= 0);

ALTER TABLE token_sweeps DROP CONSTRAINT token_sweeps_status_check;
ALTER TABLE token_sweeps ADD CONSTRAINT token_sweeps_status_check
    CHECK (status IN ('CREATED', 'WAITING_GAS', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

ALTER TABLE internal_transfers
    ADD COLUMN broadcasted_at TIMESTAMPTZ,
    ADD COLUMN last_broadcast_at TIMESTAMPTZ,
    ADD COLUMN bump_count INTEGER NOT NULL DEFAULT 0 CHECK (bump_count >= 0);

ALTER TABLE internal_transfers DROP CONSTRAINT internal_transfers_status_check;
ALTER TABLE internal_transfers ADD CONSTRAINT internal_transfers_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

CREATE TABLE evm_tx_attempts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_type TEXT NOT NULL,
    business_id BIGINT NOT NULL,
    from_address TEXT NOT NULL,
    nonce NUMERIC(20, 0) NOT NULL,
    attempt_no INTEGER NOT NULL,
    tx_hash TEXT NOT NULL,
    raw_tx BYTEA NOT NULL,
    max_fee_per_gas_wei NUMERIC(78, 0) NOT NULL,
    max_priority_fee_per_gas_wei NUMERIC(78, 0) NOT NULL,
    gas_limit BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (business_type, business_id, attempt_no),
    UNIQUE (tx_hash),
    CHECK (business_type IN ('WITHDRAWAL', 'TOKEN_WITHDRAWAL', 'TOKEN_SWEEP', 'INTERNAL_TRANSFER')),
    CHECK (business_id > 0),
    CHECK (from_address ~ '^0x[0-9a-f]{40}$'),
    CHECK (nonce >= 0),
    CHECK (attempt_no >= 1),
    CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    CHECK (max_fee_per_gas_wei > 0),
    CHECK (max_priority_fee_per_gas_wei >= 0),
    CHECK (gas_limit > 0)
);

COMMENT ON COLUMN withdrawals.broadcasted_at IS '首次广播时间戳，重播与加价计时起点';
COMMENT ON COLUMN withdrawals.last_broadcast_at IS '最近一次广播或重播时间戳';
COMMENT ON COLUMN withdrawals.bump_count IS '已执行 Gas 加价替换次数';

COMMENT ON COLUMN token_withdrawals.broadcasted_at IS '首次广播时间戳，重播与加价计时起点';
COMMENT ON COLUMN token_withdrawals.last_broadcast_at IS '最近一次广播或重播时间戳';
COMMENT ON COLUMN token_withdrawals.bump_count IS '已执行 Gas 加价替换次数';

COMMENT ON COLUMN token_sweeps.broadcasted_at IS '首次广播时间戳，重播与加价计时起点';
COMMENT ON COLUMN token_sweeps.last_broadcast_at IS '最近一次广播或重播时间戳';
COMMENT ON COLUMN token_sweeps.bump_count IS '已执行 Gas 加价替换次数';

COMMENT ON COLUMN internal_transfers.broadcasted_at IS '首次广播时间戳，重播与加价计时起点';
COMMENT ON COLUMN internal_transfers.last_broadcast_at IS '最近一次广播或重播时间戳';
COMMENT ON COLUMN internal_transfers.bump_count IS '已执行 Gas 加价替换次数';

COMMENT ON TABLE evm_tx_attempts IS 'EVM 同 Nonce 交易广播与加价替换历史表';
COMMENT ON COLUMN evm_tx_attempts.id IS '交易尝试记录自增主键';
COMMENT ON COLUMN evm_tx_attempts.business_type IS '业务类型：WITHDRAWAL、TOKEN_WITHDRAWAL、TOKEN_SWEEP 或 INTERNAL_TRANSFER';
COMMENT ON COLUMN evm_tx_attempts.business_id IS '关联业务记录自增主键';
COMMENT ON COLUMN evm_tx_attempts.from_address IS '签名交易发送方规范化小写地址';
COMMENT ON COLUMN evm_tx_attempts.nonce IS '交易使用的发送地址 Nonce';
COMMENT ON COLUMN evm_tx_attempts.attempt_no IS '同一业务记录从 1 开始递增的签名尝试序号';
COMMENT ON COLUMN evm_tx_attempts.tx_hash IS '对应版本的交易哈希，规范化为小写十六进制';
COMMENT ON COLUMN evm_tx_attempts.raw_tx IS '对应版本的已签名交易原始字节';
COMMENT ON COLUMN evm_tx_attempts.max_fee_per_gas_wei IS '该尝试版本的 EIP-1559 每 Gas 最大费用，单位 Wei';
COMMENT ON COLUMN evm_tx_attempts.max_priority_fee_per_gas_wei IS '该尝试版本的 EIP-1559 每 Gas 优先费，单位 Wei';
COMMENT ON COLUMN evm_tx_attempts.gas_limit IS '该尝试版本使用的 Gas Limit';
COMMENT ON COLUMN evm_tx_attempts.created_at IS '尝试记录创建时间';
