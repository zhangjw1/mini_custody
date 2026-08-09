package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
)

func main() {
	logger := logging.New(os.Stdout, slog.LevelInfo)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration validation failed", "error", err)
		os.Exit(1)
	}

	time.Local = cfg.Timezone
	logger.Info("mini-custody backend configuration validated", "config", cfg.SafeSummary())
}
