ALTER TABLE withdrawals
    ADD COLUMN signing_retry_count INTEGER NOT NULL DEFAULT 0 CHECK (signing_retry_count >= 0),
    ADD COLUMN bump_blocked_at TIMESTAMPTZ,
    ADD COLUMN bump_blocked_reason TEXT;

ALTER TABLE token_withdrawals
    ADD COLUMN signing_retry_count INTEGER NOT NULL DEFAULT 0 CHECK (signing_retry_count >= 0),
    ADD COLUMN bump_blocked_at TIMESTAMPTZ,
    ADD COLUMN bump_blocked_reason TEXT;

ALTER TABLE token_sweeps
    ADD COLUMN signing_retry_count INTEGER NOT NULL DEFAULT 0 CHECK (signing_retry_count >= 0),
    ADD COLUMN bump_blocked_at TIMESTAMPTZ,
    ADD COLUMN bump_blocked_reason TEXT;

ALTER TABLE internal_transfers
    ADD COLUMN signing_retry_count INTEGER NOT NULL DEFAULT 0 CHECK (signing_retry_count >= 0),
    ADD COLUMN bump_blocked_at TIMESTAMPTZ,
    ADD COLUMN bump_blocked_reason TEXT;

ALTER TABLE evm_tx_attempts
    ADD COLUMN backfilled BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN withdrawals.signing_retry_count IS '分配 Nonce 后连续签名失败的持久化计数';
COMMENT ON COLUMN withdrawals.bump_blocked_at IS '加价替换首次被余额或费率上限阻塞的时间戳';
COMMENT ON COLUMN withdrawals.bump_blocked_reason IS '当前加价替换阻塞原因编码，用于单次告警去重';

COMMENT ON COLUMN token_withdrawals.signing_retry_count IS '分配 Nonce 后连续签名失败的持久化计数';
COMMENT ON COLUMN token_withdrawals.bump_blocked_at IS '加价替换首次被余额或费率上限阻塞的时间戳';
COMMENT ON COLUMN token_withdrawals.bump_blocked_reason IS '当前加价替换阻塞原因编码，用于单次告警去重';

COMMENT ON COLUMN token_sweeps.signing_retry_count IS '分配 Nonce 后连续签名失败的持久化计数';
COMMENT ON COLUMN token_sweeps.bump_blocked_at IS '加价替换首次被余额或费率上限阻塞的时间戳';
COMMENT ON COLUMN token_sweeps.bump_blocked_reason IS '当前加价替换阻塞原因编码，用于单次告警去重';

COMMENT ON COLUMN internal_transfers.signing_retry_count IS '分配 Nonce 后连续签名失败的持久化计数';
COMMENT ON COLUMN internal_transfers.bump_blocked_at IS '加价替换首次被余额或费率上限阻塞的时间戳';
COMMENT ON COLUMN internal_transfers.bump_blocked_reason IS '当前加价替换阻塞原因编码，用于单次告警去重';

COMMENT ON COLUMN evm_tx_attempts.backfilled IS '是否为 000009 迁移从存量业务表回填的尝试记录';

-- 回填存量 EVM 业务记录缺失的首个候选尝试（attempt_no = 1）。
INSERT INTO evm_tx_attempts (
    business_type, business_id, from_address, nonce, attempt_no,
    tx_hash, raw_tx, max_fee_per_gas_wei, max_priority_fee_per_gas_wei, gas_limit, backfilled
)
SELECT 'WITHDRAWAL', w.id, LOWER(a.address), w.nonce, 1,
       LOWER(w.tx_hash), COALESCE(w.raw_tx, ''::bytea),
       GREATEST(COALESCE(w.max_fee_per_gas_wei, 1), 1),
       GREATEST(COALESCE(w.max_priority_fee_per_gas_wei, 0), 0),
       GREATEST(COALESCE(w.gas_limit, 21000), 1),
       TRUE
FROM withdrawals w
JOIN wallet_addresses a ON a.id = w.address_id
WHERE COALESCE(w.tx_hash, '') ~* '^0x[0-9a-f]{64}$'
  AND w.nonce IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM evm_tx_attempts e
      WHERE e.business_type = 'WITHDRAWAL' AND e.business_id = w.id
  )
ON CONFLICT (tx_hash) DO NOTHING;

INSERT INTO evm_tx_attempts (
    business_type, business_id, from_address, nonce, attempt_no,
    tx_hash, raw_tx, max_fee_per_gas_wei, max_priority_fee_per_gas_wei, gas_limit, backfilled
)
SELECT 'TOKEN_WITHDRAWAL', tw.id, LOWER(p.address), tw.nonce, 1,
       LOWER(tw.tx_hash), COALESCE(tw.raw_tx, ''::bytea),
       GREATEST(COALESCE(tw.max_fee_per_gas_wei, 1), 1),
       GREATEST(COALESCE(tw.max_priority_fee_per_gas_wei, 0), 0),
       GREATEST(COALESCE(tw.gas_limit, 65000), 1),
       TRUE
FROM token_withdrawals tw
JOIN platform_wallets p ON p.id = tw.platform_wallet_id
WHERE COALESCE(tw.tx_hash, '') ~* '^0x[0-9a-f]{64}$'
  AND tw.nonce IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM evm_tx_attempts e
      WHERE e.business_type = 'TOKEN_WITHDRAWAL' AND e.business_id = tw.id
  )
ON CONFLICT (tx_hash) DO NOTHING;

INSERT INTO evm_tx_attempts (
    business_type, business_id, from_address, nonce, attempt_no,
    tx_hash, raw_tx, max_fee_per_gas_wei, max_priority_fee_per_gas_wei, gas_limit, backfilled
)
SELECT 'TOKEN_SWEEP', ts.id, LOWER(a.address), ts.nonce, 1,
       LOWER(ts.tx_hash), COALESCE(ts.raw_tx, ''::bytea),
       GREATEST(COALESCE(ts.max_fee_per_gas_wei, 1), 1),
       GREATEST(COALESCE(ts.max_priority_fee_per_gas_wei, 0), 0),
       GREATEST(COALESCE(ts.gas_limit, 65000), 1),
       TRUE
FROM token_sweeps ts
JOIN wallet_addresses a ON a.id = ts.address_id
WHERE COALESCE(ts.tx_hash, '') ~* '^0x[0-9a-f]{64}$'
  AND ts.nonce IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM evm_tx_attempts e
      WHERE e.business_type = 'TOKEN_SWEEP' AND e.business_id = ts.id
  )
ON CONFLICT (tx_hash) DO NOTHING;

INSERT INTO evm_tx_attempts (
    business_type, business_id, from_address, nonce, attempt_no,
    tx_hash, raw_tx, max_fee_per_gas_wei, max_priority_fee_per_gas_wei, gas_limit, backfilled
)
SELECT 'INTERNAL_TRANSFER', it.id, LOWER(it.from_address), it.nonce, 1,
       LOWER(it.tx_hash), COALESCE(it.raw_tx, ''::bytea),
       GREATEST(COALESCE(it.max_fee_per_gas_wei, 1), 1),
       GREATEST(COALESCE(it.max_priority_fee_per_gas_wei, 0), 0),
       GREATEST(COALESCE(it.gas_limit, 21000), 1),
       TRUE
FROM internal_transfers it
WHERE COALESCE(it.tx_hash, '') ~* '^0x[0-9a-f]{64}$'
  AND it.nonce IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM evm_tx_attempts e
      WHERE e.business_type = 'INTERNAL_TRANSFER' AND e.business_id = it.id
  )
ON CONFLICT (tx_hash) DO NOTHING;

-- 回填已广播及终态存量记录缺失的 broadcasted_at 与 last_broadcast_at。
UPDATE withdrawals
SET broadcasted_at = COALESCE(broadcasted_at, updated_at),
    last_broadcast_at = COALESCE(last_broadcast_at, broadcasted_at, updated_at)
WHERE status IN ('BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW')
  AND COALESCE(tx_hash, '') <> ''
  AND (broadcasted_at IS NULL OR last_broadcast_at IS NULL);

UPDATE token_withdrawals
SET broadcasted_at = COALESCE(broadcasted_at, updated_at),
    last_broadcast_at = COALESCE(last_broadcast_at, broadcasted_at, updated_at)
WHERE status IN ('BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW')
  AND COALESCE(tx_hash, '') <> ''
  AND (broadcasted_at IS NULL OR last_broadcast_at IS NULL);

UPDATE token_sweeps
SET broadcasted_at = COALESCE(broadcasted_at, updated_at),
    last_broadcast_at = COALESCE(last_broadcast_at, broadcasted_at, updated_at)
WHERE status IN ('BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW')
  AND COALESCE(tx_hash, '') <> ''
  AND (broadcasted_at IS NULL OR last_broadcast_at IS NULL);

UPDATE internal_transfers
SET broadcasted_at = COALESCE(broadcasted_at, updated_at),
    last_broadcast_at = COALESCE(last_broadcast_at, broadcasted_at, updated_at)
WHERE status IN ('BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW')
  AND COALESCE(tx_hash, '') <> ''
  AND (broadcasted_at IS NULL OR last_broadcast_at IS NULL);
