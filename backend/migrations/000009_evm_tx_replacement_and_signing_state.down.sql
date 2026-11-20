DELETE FROM evm_tx_attempts WHERE backfilled = TRUE;

ALTER TABLE evm_tx_attempts DROP COLUMN backfilled;

ALTER TABLE internal_transfers
    DROP COLUMN bump_blocked_reason,
    DROP COLUMN bump_blocked_at,
    DROP COLUMN signing_retry_count;

ALTER TABLE token_sweeps
    DROP COLUMN bump_blocked_reason,
    DROP COLUMN bump_blocked_at,
    DROP COLUMN signing_retry_count;

ALTER TABLE token_withdrawals
    DROP COLUMN bump_blocked_reason,
    DROP COLUMN bump_blocked_at,
    DROP COLUMN signing_retry_count;

ALTER TABLE withdrawals
    DROP COLUMN bump_blocked_reason,
    DROP COLUMN bump_blocked_at,
    DROP COLUMN signing_retry_count;
