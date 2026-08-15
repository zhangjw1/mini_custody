package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultTimezone = "Asia/Shanghai"

type Config struct {
	AppEnv                string
	HTTPAddr              string
	Timezone              *time.Location
	DatabaseURL           string
	CustodyKeyStoreFile   string
	CustodyPassword       string
	SepoliaRPCURL         string
	SepoliaRPCFallbackURL string
	SepoliaRPCTimeout     time.Duration
	SepoliaRPCMaxAttempts int
	SepoliaRPCBaseDelay   time.Duration
	SepoliaRPCMaxDelay    time.Duration
	SepoliaScanStartBlock *uint64
	SepoliaConfirmations  uint64
	SepoliaScanInterval   time.Duration
	SepoliaScanBatchSize  uint64
}

// Load 从环境变量加载并校验应用配置。
func Load() (Config, error) {
	timezoneName := envOrDefault("APP_TIMEZONE", defaultTimezone)
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return Config{}, fmt.Errorf("APP_TIMEZONE 配置无效：%w", err)
	}
	scanStartBlock, err := optionalUint64("SEPOLIA_SCAN_START_BLOCK")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		AppEnv:                envOrDefault("APP_ENV", "development"),
		HTTPAddr:              envOrDefault("HTTP_ADDR", ":8080"),
		Timezone:              timezone,
		DatabaseURL:           strings.TrimSpace(os.Getenv("DATABASE_URL")),
		CustodyKeyStoreFile:   strings.TrimSpace(os.Getenv("CUSTODY_KEYSTORE_FILE")),
		CustodyPassword:       os.Getenv("CUSTODY_KEYSTORE_PASSWORD"),
		SepoliaRPCURL:         strings.TrimSpace(os.Getenv("SEPOLIA_RPC_URL")),
		SepoliaRPCFallbackURL: strings.TrimSpace(os.Getenv("SEPOLIA_RPC_FALLBACK_URL")),
		SepoliaRPCTimeout:     durationOrDefault("SEPOLIA_RPC_TIMEOUT", 10*time.Second),
		SepoliaRPCMaxAttempts: intOrDefault("SEPOLIA_RPC_MAX_ATTEMPTS", 3),
		SepoliaRPCBaseDelay:   durationOrDefault("SEPOLIA_RPC_BASE_DELAY", 250*time.Millisecond),
		SepoliaRPCMaxDelay:    durationOrDefault("SEPOLIA_RPC_MAX_DELAY", 3*time.Second),
		SepoliaScanStartBlock: scanStartBlock,
		SepoliaConfirmations:  uint64OrDefault("SEPOLIA_CONFIRMATIONS", 3),
		SepoliaScanInterval:   durationOrDefault("SEPOLIA_SCAN_INTERVAL", 12*time.Second),
		SepoliaScanBatchSize:  uint64OrDefault("SEPOLIA_SCAN_BATCH_SIZE", 20),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 校验启动所需的配置是否完整。
func (c Config) Validate() error {
	missing := make([]string, 0, 4)
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.CustodyKeyStoreFile == "" {
		missing = append(missing, "CUSTODY_KEYSTORE_FILE")
	}
	if c.CustodyPassword == "" {
		missing = append(missing, "CUSTODY_KEYSTORE_PASSWORD")
	}
	if c.SepoliaRPCURL == "" {
		missing = append(missing, "SEPOLIA_RPC_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需配置：%s", strings.Join(missing, ", "))
	}
	if c.Timezone == nil {
		return errors.New("必须配置时区")
	}
	if c.SepoliaRPCTimeout <= 0 || c.SepoliaRPCMaxAttempts <= 0 || c.SepoliaRPCBaseDelay < 0 || c.SepoliaRPCMaxDelay < c.SepoliaRPCBaseDelay {
		return errors.New("Sepolia RPC 重试配置无效")
	}
	if c.SepoliaConfirmations == 0 || c.SepoliaScanInterval <= 0 || c.SepoliaScanBatchSize == 0 || c.SepoliaScanBatchSize > 100 {
		return errors.New("Sepolia 充值扫描配置无效")
	}
	return nil
}

// SafeSummary 返回不会泄露密码和连接凭据的配置摘要。
func (c Config) SafeSummary() map[string]string {
	return map[string]string{
		"app_env":               c.AppEnv,
		"http_addr":             c.HTTPAddr,
		"timezone":              c.Timezone.String(),
		"keystore_file":         c.CustodyKeyStoreFile,
		"database":              configured(c.DatabaseURL),
		"sepolia_rpc":           configured(c.SepoliaRPCURL),
		"sepolia_rpc_fallback":  configured(c.SepoliaRPCFallbackURL),
		"sepolia_rpc_timeout":   c.SepoliaRPCTimeout.String(),
		"sepolia_confirmations": strconv.FormatUint(c.SepoliaConfirmations, 10),
		"sepolia_scan_interval": c.SepoliaScanInterval.String(),
		"sepolia_scan_batch":    strconv.FormatUint(c.SepoliaScanBatchSize, 10),
	}
}

// envOrDefault 读取环境变量，空值时返回默认值。
func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// configured 将敏感配置转换为不包含原值的状态文本。
func configured(value string) string {
	if value == "" {
		return "缺失"
	}
	return "已配置"
}

// durationOrDefault 读取时长配置，格式错误时返回默认值，由 Validate 负责最终校验范围。
func durationOrDefault(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return parsed
}

// intOrDefault 读取整数配置，格式错误时返回零，由 Validate 负责最终校验范围。
func intOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// uint64OrDefault 读取无符号整数配置，格式错误时返回零并交由 Validate 拒绝。
func uint64OrDefault(key string, fallback uint64) uint64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// optionalUint64 读取可选无符号整数，空值表示继续使用数据库检查点。
func optionalUint64(key string) (*uint64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s 配置必须是非负整数", key)
	}
	return &parsed, nil
}
