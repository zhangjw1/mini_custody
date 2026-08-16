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

// TestLoadPreflightDoesNotRequireCustodySecrets 验证只读预检无需密钥文件和密码。
func TestLoadPreflightDoesNotRequireCustodySecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "")
	if _, err := LoadPreflight(); err != nil {
		t.Fatalf("LoadPreflight() error = %v", err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want missing custody config")
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

// TestConfigKeepsERC20DisabledByDefault 验证未启用 Token 时现有 ETH 配置仍可正常加载。
func TestConfigKeepsERC20DisabledByDefault(t *testing.T) {
	setRequiredConfigEnvironment(t)
	t.Setenv("ERC20_ENABLED", "")
	t.Setenv("ERC20_SWEEP_ENABLED", "")
	t.Setenv("ERC20_CONTRACT_ADDRESS", "not-an-address")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ERC20.Enabled || cfg.ERC20.SweepEnabled {
		t.Fatalf("ERC20 config = %+v, want disabled", cfg.ERC20)
	}
}

// TestConfigLoadsERC20Settings 验证 Sepolia USDC、扫描、归集和平台热钱包配置解析。
func TestConfigLoadsERC20Settings(t *testing.T) {
	setRequiredConfigEnvironment(t)
	t.Setenv("ERC20_ENABLED", "true")
	t.Setenv("ERC20_CONTRACT_ADDRESS", "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238")
	t.Setenv("ERC20_SYMBOL", "USDC")
	t.Setenv("ERC20_DECIMALS", "6")
	t.Setenv("ERC20_SCAN_START_BLOCK", "789")
	t.Setenv("ERC20_SCAN_BATCH_SIZE", "250")
	t.Setenv("ERC20_CONFIRMATIONS", "5")
	t.Setenv("ERC20_SWEEP_ENABLED", "true")
	t.Setenv("ERC20_SWEEP_INTERVAL", "30s")
	t.Setenv("ERC20_GAS_SAFETY_BPS", "1500")
	t.Setenv("ERC20_GAS_TOPUP_MAX_WEI", "4000000000000000")
	t.Setenv("PLATFORM_HOT_WALLET_PATH", "m/44'/60'/0'/0/0")
	t.Setenv("PLATFORM_MIN_ETH_BALANCE_WEI", "9000000000000000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.ERC20.Enabled || !cfg.ERC20.SweepEnabled || cfg.ERC20.Symbol != "USDC" || cfg.ERC20.Decimals != 6 {
		t.Fatalf("ERC20 config = %+v", cfg.ERC20)
	}
	if cfg.ERC20.ScanStartBlock == nil || *cfg.ERC20.ScanStartBlock != 789 || cfg.ERC20.ScanBatchSize != 250 ||
		cfg.ERC20.Confirmations != 5 || cfg.ERC20.SweepInterval != 30*time.Second {
		t.Fatalf("ERC20 scanner config = %+v", cfg.ERC20)
	}
	if cfg.ERC20.GasTopupMaxWei.String() != "4000000000000000" || cfg.PlatformWallet.MinETHBalanceWei.String() != "9000000000000000" {
		t.Fatalf("Gas config = %+v, platform = %+v", cfg.ERC20, cfg.PlatformWallet)
	}
}

// TestConfigRejectsInvalidERC20Settings 验证 Token 开启后会拒绝危险或无法解释的配置。
func TestConfigRejectsInvalidERC20Settings(t *testing.T) {
	testCases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "合约地址", key: "ERC20_CONTRACT_ADDRESS", value: "not-an-address"},
		{name: "零地址", key: "ERC20_CONTRACT_ADDRESS", value: "0x0000000000000000000000000000000000000000"},
		{name: "精度", key: "ERC20_DECIMALS", value: "19"},
		{name: "扫描批次", key: "ERC20_SCAN_BATCH_SIZE", value: "1001"},
		{name: "Gas 安全余量", key: "ERC20_GAS_SAFETY_BPS", value: "10001"},
		{name: "Gas 补充上限", key: "ERC20_GAS_TOPUP_MAX_WEI", value: "0"},
		{name: "热钱包路径", key: "PLATFORM_HOT_WALLET_PATH", value: "m/44'/60'/0'/0/1"},
		{name: "平台余额阈值", key: "PLATFORM_MIN_ETH_BALANCE_WEI", value: "invalid"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			setRequiredConfigEnvironment(t)
			t.Setenv("ERC20_ENABLED", "true")
			t.Setenv(testCase.key, testCase.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() error = nil, want invalid %s", testCase.key)
			}
		})
	}
}

// TestConfigRejectsSweepWithoutERC20 验证不能在 Token 关闭时单独启动归集 Worker。
func TestConfigRejectsSweepWithoutERC20(t *testing.T) {
	setRequiredConfigEnvironment(t)
	t.Setenv("ERC20_ENABLED", "false")
	t.Setenv("ERC20_SWEEP_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want sweep dependency error")
	}
}

// setRequiredConfigEnvironment 设置加载配置所需的最小非敏感测试值。
func setRequiredConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://configured")
	t.Setenv("CUSTODY_KEYSTORE_FILE", "root.age")
	t.Setenv("CUSTODY_KEYSTORE_PASSWORD", "password")
	t.Setenv("SEPOLIA_RPC_URL", "https://primary.example")
	for _, key := range []string{
		"ERC20_CONTRACT_ADDRESS", "ERC20_SYMBOL", "ERC20_DECIMALS", "ERC20_SCAN_START_BLOCK",
		"ERC20_SCAN_BATCH_SIZE", "ERC20_CONFIRMATIONS", "ERC20_SWEEP_INTERVAL", "ERC20_GAS_SAFETY_BPS",
		"ERC20_GAS_TOPUP_MAX_WEI", "PLATFORM_HOT_WALLET_PATH", "PLATFORM_MIN_ETH_BALANCE_WEI",
	} {
		t.Setenv(key, "")
	}
}
