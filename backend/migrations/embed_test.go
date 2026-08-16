package migrations

import (
	"strings"
	"testing"
)

// TestEmbeddedMigrationsHaveUpAndDownPairs 验证每个迁移版本都有升级和回退文件。
func TestEmbeddedMigrationsHaveUpAndDownPairs(t *testing.T) {
	items, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() error = %v", err)
	}
	if len(items) == 0 {
		t.Fatal("loadMigrations() returned no migrations")
	}
	for i, item := range items {
		if item.version != int64(i+1) {
			t.Fatalf("migration[%d].version = %d, want %d", i, item.version, i+1)
		}
	}
}

// TestInitialMigrationDocumentsEveryBusinessColumn 验证全部业务表和字段都有注释。
func TestInitialMigrationDocumentsEveryBusinessColumn(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000001_initial.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	tables := map[string][]string{
		"users":             {"id", "code", "display_name", "created_at"},
		"wallet_addresses":  {"id", "user_id", "network", "address", "derivation_index", "derivation_path", "next_nonce", "created_at"},
		"asset_balances":    {"id", "user_id", "asset", "available_wei", "pending_deposit_wei", "pending_withdrawal_wei", "version", "updated_at"},
		"deposits":          {"id", "user_id", "address_id", "network", "asset", "tx_hash", "tx_index", "block_number", "block_hash", "amount_wei", "confirmations", "status", "created_at", "updated_at"},
		"withdrawals":       {"id", "idempotency_key", "user_id", "address_id", "to_address", "amount_wei", "reserved_fee_wei", "actual_fee_wei", "nonce", "gas_limit", "max_fee_per_gas_wei", "max_priority_fee_per_gas_wei", "raw_tx", "tx_hash", "block_number", "confirmations", "status", "error_code", "error_message", "created_at", "updated_at"},
		"balance_entries":   {"id", "user_id", "asset", "entry_type", "amount_wei", "reference_type", "reference_id", "created_at"},
		"chain_checkpoints": {"id", "network", "scanner", "last_scanned_block", "last_scanned_hash", "updated_at"},
		"worker_errors":     {"id", "worker", "stage", "reference_type", "reference_id", "error_code", "error_message", "retry_count", "first_occurred_at", "last_occurred_at"},
	}
	for table, columns := range tables {
		if !strings.Contains(sql, "COMMENT ON TABLE "+table+" IS") {
			t.Errorf("table %s has no comment", table)
		}
		for _, column := range columns {
			if !strings.Contains(sql, "COMMENT ON COLUMN "+table+"."+column+" IS") {
				t.Errorf("column %s.%s has no comment", table, column)
			}
		}
	}
}

// TestERC20MigrationDocumentsEveryNewBusinessColumn 验证 ERC-20 迁移新增表和字段都有中文注释。
func TestERC20MigrationDocumentsEveryNewBusinessColumn(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000002_erc20.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	tables := map[string][]string{
		"assets":             {"id", "network", "asset_type", "symbol", "contract_address", "decimals", "enabled", "created_at", "updated_at"},
		"platform_wallets":   {"id", "network", "role", "address", "derivation_path", "next_nonce", "created_at", "updated_at"},
		"token_deposits":     {"id", "user_id", "address_id", "asset_id", "tx_hash", "log_index", "block_number", "block_hash", "from_address", "to_address", "amount_units", "confirmations", "status", "created_at", "updated_at"},
		"token_sweeps":       {"id", "user_id", "address_id", "asset_id", "trigger_deposit_id", "recognized_amount_units", "sweep_amount_units", "gas_topup_transfer_id", "nonce", "gas_limit", "max_fee_per_gas_wei", "max_priority_fee_per_gas_wei", "raw_tx", "tx_hash", "block_number", "confirmations", "actual_fee_wei", "status", "error_code", "error_message", "created_at", "updated_at"},
		"internal_transfers": {"id", "platform_wallet_id", "sweep_id", "transfer_type", "from_address", "to_address", "amount_wei", "nonce", "gas_limit", "max_fee_per_gas_wei", "max_priority_fee_per_gas_wei", "raw_tx", "tx_hash", "block_number", "confirmations", "actual_fee_wei", "status", "error_code", "error_message", "created_at", "updated_at"},
		"token_withdrawals":  {"id", "idempotency_key", "user_id", "asset_id", "platform_wallet_id", "to_address", "amount_units", "nonce", "gas_limit", "max_fee_per_gas_wei", "max_priority_fee_per_gas_wei", "raw_tx", "tx_hash", "block_number", "confirmations", "actual_fee_wei", "status", "error_code", "error_message", "created_at", "updated_at"},
	}
	for table, columns := range tables {
		if !strings.Contains(sql, "COMMENT ON TABLE "+table+" IS") {
			t.Errorf("table %s has no comment", table)
		}
		for _, column := range columns {
			if !strings.Contains(sql, "COMMENT ON COLUMN "+table+"."+column+" IS") {
				t.Errorf("column %s.%s has no comment", table, column)
			}
		}
	}
}

// TestERC20MigrationContainsCriticalConstraints 验证迁移包含 Event、活动归集、Nonce 和提币幂等约束。
func TestERC20MigrationContainsCriticalConstraints(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000002_erc20.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"UNIQUE (asset_id, tx_hash, log_index)",
		"token_sweeps_active_address_asset_idx",
		"internal_transfers_wallet_nonce_idx",
		"UNIQUE (user_id, idempotency_key)",
		"token_withdrawals_wallet_nonce_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("critical constraint %q is missing", fragment)
		}
	}
}
