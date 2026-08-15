package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestConfigSafeSummaryDoesNotExposeSecrets 验证配置摘要不会泄露敏感值。
func TestConfigSafeSummaryDoesNotExposeSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret-user:secret-pass@localhost/custody")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "./secrets/custody-root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "top-secret-password")
	t.Setenv("SEPOLIA_RPC_URL", "https://rpc.example/v1/secret-api-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	summary := fmt.Sprint(cfg.SafeSummary())
	for _, secret := range []string{"secret-user", "secret-pass", "top-secret-password", "secret-api-key"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("safe summary contains secret %q", secret)
		}
	}
}

// TestConfigUsesAsiaShanghaiByDefault 验证默认业务时区为 Asia/Shanghai。
func TestConfigUsesAsiaShanghaiByDefault(t *testing.T) {
	t.Setenv("APP_TIMEZONE", "")
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://rpc.example")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Timezone.String(); got != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q, want Asia/Shanghai", got)
	}
}

// TestConfigLoadsSepoliaRPCRetrySettings 验证 Sepolia RPC 主备和重试配置解析。
func TestConfigLoadsSepoliaRPCRetrySettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	t.Setenv("SEPOLIA_RPC_FALLBACK_URL", "https://fallback.example")
	t.Setenv("SEPOLIA_RPC_TIMEOUT", "2s")
	t.Setenv("SEPOLIA_RPC_MAX_ATTEMPTS", "4")
	t.Setenv("SEPOLIA_RPC_BASE_DELAY", "100ms")
	t.Setenv("SEPOLIA_RPC_MAX_DELAY", "1s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SepoliaRPCFallbackURL != "https://fallback.example" || cfg.SepoliaRPCTimeout.String() != "2s" || cfg.SepoliaRPCMaxAttempts != 4 {
		t.Fatalf("RPC config = %+v", cfg)
	}
}

// TestConfigRejectsInvalidSepoliaRPCRetrySettings 验证无效重试配置会阻止启动。
func TestConfigRejectsInvalidSepoliaRPCRetrySettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	t.Setenv("SEPOLIA_RPC_TIMEOUT", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid retry config")
	}
}

// TestConfigLoadsSepoliaScannerSettings 验证充值扫描起点和轮询配置解析。
func TestConfigLoadsSepoliaScannerSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	t.Setenv("SEPOLIA_SCAN_START_BLOCK", "123")
	t.Setenv("SEPOLIA_CONFIRMATIONS", "5")
	t.Setenv("SEPOLIA_SCAN_INTERVAL", "4s")
	t.Setenv("SEPOLIA_SCAN_BATCH_SIZE", "25")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SepoliaScanStartBlock == nil || *cfg.SepoliaScanStartBlock != 123 || cfg.SepoliaConfirmations != 5 ||
		cfg.SepoliaScanInterval != 4*time.Second || cfg.SepoliaScanBatchSize != 25 {
		t.Fatalf("scanner config = %+v", cfg)
	}
}

// TestConfigRejectsInvalidSepoliaScannerSettings 验证无效充值扫描配置会阻止启动。
func TestConfigRejectsInvalidSepoliaScannerSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	t.Setenv("SEPOLIA_SCAN_START_BLOCK", "invalid")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid scanner config")
	}
}
