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

func Load() (Config, error) {
	timezoneName := envOrDefault("APP_TIMEZONE", defaultTimezone)
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return Config{}, fmt.Errorf("invalid APP_TIMEZONE: %w", err)
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
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.Timezone == nil {
		return errors.New("timezone is required")
	}
	return nil
}

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

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func configured(value string) string {
	if value == "" {
		return "missing"
	}
	return "configured"
}
