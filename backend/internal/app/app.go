package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

type App struct {
	config config.Config
	logger *slog.Logger
	pool   *pgxpool.Pool
	store  *postgres.Store
	keys   wallet.KeyProvider
	server *http.Server
}

// New 完成数据库迁移、密钥加载、演示用户初始化和 HTTP 服务装配。
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		return nil, errors.New("必须提供日志记录器")
	}
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.Timezone)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			pool.Close()
		}
	}()

	if err := migrations.NewRunner(pool).Up(ctx); err != nil {
		return nil, fmt.Errorf("执行数据库迁移失败：%w", err)
	}
	provider, err := wallet.LoadProvider(cfg.CustodyKeyStoreFile, cfg.CustodyPassword)
	if err != nil {
		return nil, fmt.Errorf("加载托管密钥提供器失败：%w", err)
	}
	store, err := postgres.NewStore(pool, cfg.Timezone)
	if err != nil {
		return nil, err
	}
	if err := store.BootstrapDemoUsers(ctx, provider); err != nil {
		return nil, fmt.Errorf("初始化演示用户失败：%w", err)
	}

	application := &App{
		config: cfg,
		logger: logger,
		pool:   pool,
		store:  store,
		keys:   provider,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", application.health)
	application.server = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	failed = false
	return application, nil
}

// Run 启动 HTTP 服务并在上下文取消时优雅关闭。
func (a *App) Run(ctx context.Context) error {
	defer a.pool.Close()
	serveErr := make(chan error, 1)
	go func() {
		a.logger.Info("Mini Custody Web 服务已启动", "address", a.server.Addr)
		err := a.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("关闭 Web 服务失败：%w", err)
		}
		return <-serveErr
	}
}

// health 检查数据库连接并返回服务健康状态。
func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status := http.StatusOK
	response := map[string]string{
		"status": "ok",
		"time":   time.Now().In(a.config.Timezone).Format(time.RFC3339),
	}
	if err := a.pool.Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		response["status"] = "unavailable"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
