package config

import (
	"fmt"
	"strings"
	"testing"
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
