package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/app"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
)

// main 加载配置并启动 Mini Custody 单体 Web 服务。
func main() {
	logger := logging.New(os.Stdout, slog.LevelInfo)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("配置校验失败", "error", err)
		os.Exit(1)
	}

	time.Local = cfg.Timezone
	logger.Info("Mini Custody 后端配置校验通过", "config", cfg.SafeSummary())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("应用初始化失败", "error", err)
		os.Exit(1)
	}
	if err := application.Run(ctx); err != nil {
		logger.Error("应用异常停止", "error", err)
		os.Exit(1)
	}
}
