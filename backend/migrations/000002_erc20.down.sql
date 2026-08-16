ALTER TABLE token_sweeps DROP CONSTRAINT IF EXISTS token_sweeps_gas_topup_transfer_id_fkey;
ALTER TABLE token_sweeps DROP CONSTRAINT IF EXISTS token_sweeps_gas_topup_transfer_id_key;

DROP TABLE IF EXISTS token_withdrawals;
DROP TABLE IF EXISTS internal_transfers;
DROP TABLE IF EXISTS token_sweeps;
DROP TABLE IF EXISTS token_deposits;
DROP TABLE IF EXISTS platform_wallets;

DELETE FROM balance_entries WHERE asset <> 'ETH';
DELETE FROM asset_balances WHERE asset <> 'ETH';

ALTER TABLE balance_entries DROP CONSTRAINT IF EXISTS balance_entries_asset_id_fkey;
ALTER TABLE balance_entries DROP CONSTRAINT IF EXISTS balance_entries_asset_symbol_check;
ALTER TABLE balance_entries DROP CONSTRAINT IF EXISTS balance_entries_reference_type_check;
ALTER TABLE balance_entries DROP COLUMN IF EXISTS asset_id;
ALTER TABLE balance_entries
    ADD CONSTRAINT balance_entries_asset_check CHECK (asset = 'ETH'),
    ADD CONSTRAINT balance_entries_reference_type_check CHECK (reference_type IN ('DEPOSIT', 'WITHDRAWAL'));
COMMENT ON COLUMN balance_entries.asset IS '发生变动的资产代码';
COMMENT ON COLUMN balance_entries.amount_wei IS '对用户资产的有符号变动金额，单位 Wei';
COMMENT ON COLUMN balance_entries.reference_type IS '关联业务类型：DEPOSIT 或 WITHDRAWAL';

ALTER TABLE asset_balances DROP CONSTRAINT IF EXISTS asset_balances_user_asset_id_key;
ALTER TABLE asset_balances DROP CONSTRAINT IF EXISTS asset_balances_asset_id_fkey;
ALTER TABLE asset_balances DROP CONSTRAINT IF EXISTS asset_balances_asset_symbol_check;
ALTER TABLE asset_balances DROP COLUMN IF EXISTS asset_id;
ALTER TABLE asset_balances ADD CONSTRAINT asset_balances_asset_check CHECK (asset = 'ETH');
COMMENT ON COLUMN asset_balances.asset IS '资产代码，当前固定为 ETH';
COMMENT ON COLUMN asset_balances.available_wei IS '已确认且可用于提币的余额，单位 Wei';
COMMENT ON COLUMN asset_balances.pending_deposit_wei IS '已发现但尚未确认入账的充值金额，单位 Wei';
COMMENT ON COLUMN asset_balances.pending_withdrawal_wei IS '已被提币流程占用但尚未结算的金额，单位 Wei';

DROP TABLE IF EXISTS assets;
