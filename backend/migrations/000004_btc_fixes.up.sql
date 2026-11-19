ALTER TABLE btc_withdrawals ADD COLUMN raw_tx BYTEA;

ALTER TABLE btc_withdrawals DROP CONSTRAINT btc_withdrawals_status_check;
ALTER TABLE btc_withdrawals ADD CONSTRAINT btc_withdrawals_status_check
    CHECK (status IN ('CREATED', 'INPUTS_LOCKED', 'SIGNING', 'SIGNED', 'BROADCASTING', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

ALTER TABLE btc_sweeps DROP CONSTRAINT btc_sweeps_status_check;
ALTER TABLE btc_sweeps ADD CONSTRAINT btc_sweeps_status_check
    CHECK (status IN ('CREATED', 'SIGNING', 'SIGNED', 'BROADCAST_UNKNOWN', 'BROADCASTED', 'CONFIRMING', 'COMPLETED', 'FAILED', 'MANUAL_REVIEW'));

ALTER TABLE chain_checkpoints DROP CONSTRAINT chain_checkpoints_last_scanned_hash_check;
ALTER TABLE chain_checkpoints ADD CONSTRAINT chain_checkpoints_last_scanned_hash_check
    CHECK ((network = 'ethereum-sepolia' AND last_scanned_hash ~ '^0x[0-9a-f]{64}$') OR (network IN ('bitcoin-signet', 'bitcoin-testnet4') AND last_scanned_hash ~ '^[0-9a-f]{64}$'));

COMMENT ON COLUMN btc_withdrawals.raw_tx IS '已签名原始交易字节';
