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

// TestBitcoinMigrationContainsCriticalConstraints 验证 BTC UTXO、锁租约和归集唯一约束。
func TestBitcoinMigrationContainsCriticalConstraints(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000003_bitcoin.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"UNIQUE (network, txid, vout)",
		"deposit_id BIGINT UNIQUE REFERENCES btc_deposits(id)",
		"btc_sweeps_txid_idx",
		"locked_until",
		"UNIQUE (user_id, idempotency_key)",
		"btc_withdrawals_status_created_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("critical BTC constraint %q is missing", fragment)
		}
	}
}

// TestBitcoinFixesMigrationContainsRawTxAndManualReview 验证 000004 迁移补全了 raw_tx 列与 MANUAL_REVIEW 约束。
func TestBitcoinFixesMigrationContainsRawTxAndManualReview(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000004_btc_fixes.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"ADD COLUMN raw_tx BYTEA",
		"COMMENT ON COLUMN btc_withdrawals.raw_tx IS",
		"MANUAL_REVIEW",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("BTC fixes migration fragment %q is missing", fragment)
		}
	}
}

// TestAdminAuthMigrationDocumentsEveryNewColumn 验证 000005 迁移的表与字段都有中文注释。
func TestAdminAuthMigrationDocumentsEveryNewColumn(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000005_admin_auth_and_users.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, col := range []string{"id", "username", "password_hash", "display_name", "role", "disabled", "last_login_at", "created_at", "updated_at"} {
		if !strings.Contains(sql, "COMMENT ON COLUMN admin_users."+col+" IS") {
			t.Errorf("column admin_users.%s has no comment", col)
		}
	}
	for _, col := range []string{"status", "note"} {
		if !strings.Contains(sql, "COMMENT ON COLUMN users."+col+" IS") {
			t.Errorf("column users.%s has no comment", col)
		}
	}
}

// TestEVMReplacementMigrationDocumentsEveryColumn 验证 000006 迁移新增表与字段都有中文注释。
func TestEVMReplacementMigrationDocumentsEveryColumn(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000006_evm_tx_replacement.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	if !strings.Contains(sql, "COMMENT ON TABLE evm_tx_attempts IS") {
		t.Error("table evm_tx_attempts has no comment")
	}
	for _, col := range []string{
		"id", "business_type", "business_id", "from_address", "nonce",
		"attempt_no", "tx_hash", "raw_tx", "max_fee_per_gas_wei",
		"max_priority_fee_per_gas_wei", "gas_limit", "created_at",
	} {
		if !strings.Contains(sql, "COMMENT ON COLUMN evm_tx_attempts."+col+" IS") {
			t.Errorf("column evm_tx_attempts.%s has no comment", col)
		}
	}
	for _, table := range []string{"withdrawals", "token_withdrawals", "token_sweeps", "internal_transfers"} {
		for _, col := range []string{"broadcasted_at", "last_broadcast_at", "bump_count"} {
			if !strings.Contains(sql, "COMMENT ON COLUMN "+table+"."+col+" IS") {
				t.Errorf("column %s.%s has no comment", table, col)
			}
		}
	}
}

// TestReorgOrphanedMigrationContainsConstraintsAndComments 验证 000007 迁移补全 ORPHANED 状态、部分唯一索引与 DEPOSIT_REVERSAL 约束。
func TestReorgOrphanedMigrationContainsConstraintsAndComments(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000007_reorg_orphaned.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"'ORPHANED'",
		"'DEPOSIT_REVERSAL'",
		"deposits_active_network_tx_hash_address_idx",
		"token_deposits_active_asset_tx_log_idx",
		"btc_deposits_active_network_txid_vout_addr_idx",
		"btc_utxos_active_network_txid_vout_idx",
		"COMMENT ON COLUMN deposits.status IS",
		"COMMENT ON COLUMN token_deposits.status IS",
		"COMMENT ON COLUMN btc_deposits.status IS",
		"COMMENT ON COLUMN balance_entries.entry_type IS",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("reorg orphaned migration fragment %q is missing", fragment)
		}
	}
}

// TestPerformanceIndexesMigrationContainsExpectedIndexes 验证 000008 迁移包含性能索引及对齐后的排序索引。
func TestPerformanceIndexesMigrationContainsExpectedIndexes(t *testing.T) {
	sqlBytes, err := migrationFiles.ReadFile("000008_performance_indexes.up.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(sqlBytes)
	for _, fragment := range []string{
		"deposits_user_id_desc_idx",
		"btc_deposits_user_id_desc_idx",
		"btc_utxos_network_status_height_id_idx",
		"btc_utxos_locked_by_idx",
		"balance_entries_user_id_desc_idx",
		"token_deposits_user_id_desc_idx",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("performance indexes migration fragment %q is missing", fragment)
		}
	}
}
