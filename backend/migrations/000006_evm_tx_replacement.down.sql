DROP TABLE IF EXISTS evm_tx_attempts;

ALTER TABLE internal_transfers DROP CONSTRAINT internal_transfers_status_check;
ALTER TABLE internal_transfers ADD CONSTRAINT internal_transfers_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'));
ALTER TABLE internal_transfers
    DROP COLUMN IF EXISTS bump_count,
    DROP COLUMN IF EXISTS last_broadcast_at,
    DROP COLUMN IF EXISTS broadcasted_at;

ALTER TABLE token_sweeps DROP CONSTRAINT token_sweeps_status_check;
ALTER TABLE token_sweeps ADD CONSTRAINT token_sweeps_status_check
    CHECK (status IN ('CREATED', 'WAITING_GAS', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'));
ALTER TABLE token_sweeps
    DROP COLUMN IF EXISTS bump_count,
    DROP COLUMN IF EXISTS last_broadcast_at,
    DROP COLUMN IF EXISTS broadcasted_at;

ALTER TABLE token_withdrawals DROP CONSTRAINT token_withdrawals_status_check;
ALTER TABLE token_withdrawals ADD CONSTRAINT token_withdrawals_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'));
ALTER TABLE token_withdrawals
    DROP COLUMN IF EXISTS bump_count,
    DROP COLUMN IF EXISTS last_broadcast_at,
    DROP COLUMN IF EXISTS broadcasted_at;

ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCASTING', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED'));
ALTER TABLE withdrawals
    DROP COLUMN IF EXISTS bump_count,
    DROP COLUMN IF EXISTS last_broadcast_at,
    DROP COLUMN IF EXISTS broadcasted_at;
