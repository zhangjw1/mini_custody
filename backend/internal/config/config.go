package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const defaultTimezone = "Asia/Shanghai"

type Config struct {
	AppEnv              string
	HTTPAddr            string
	Timezone            *time.Location
	DatabaseURL         string
	CustodyKeyStoreFile string
	CustodyPassword     string
	SepoliaRPCURL       string
}

// Load 从环境变量加载并校验应用配置。
func Load() (Config, error) {
	timezoneName := envOrDefault("APP_TIMEZONE", defaultTimezone)
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return Config{}, fmt.Errorf("APP_TIMEZONE 配置无效：%w", err)
	}

	cfg := Config{
		AppEnv:              envOrDefault("APP_ENV", "development"),
		HTTPAddr:            envOrDefault("HTTP_ADDR", ":8080"),
		Timezone:            timezone,
		DatabaseURL:         strings.TrimSpace(os.Getenv("DATABASE_URL")),
		CustodyKeyStoreFile: strings.TrimSpace(os.Getenv("CUSTODY_KEYSTORE_FILE")),
		CustodyPassword:     os.Getenv("CUSTODY_KEYSTORE_PASSWORD"),
		SepoliaRPCURL:       strings.TrimSpace(os.Getenv("SEPOLIA_RPC_URL")),
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
	return nil
}

// SafeSummary 返回不会泄露密码和连接凭据的配置摘要。
func (c Config) SafeSummary() map[string]string {
	return map[string]string{
		"app_env":       c.AppEnv,
		"http_addr":     c.HTTPAddr,
		"timezone":      c.Timezone.String(),
		"keystore_file": c.CustodyKeyStoreFile,
		"database":      configured(c.DatabaseURL),
		"sepolia_rpc":   configured(c.SepoliaRPCURL),
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
