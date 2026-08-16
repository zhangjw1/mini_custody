package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/deposit"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"github.com/xiaoqi/mini-custody/backend/internal/withdrawal"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

type App struct {
	config           config.Config
	logger           *slog.Logger
	pool             *pgxpool.Pool
	store            *postgres.Store
	apiStore         APIStore
	keys             wallet.KeyProvider
	chain            *evm.Client
	scanner          *deposit.Scanner
	withdrawals      *withdrawal.Service
	withdrawalWorker *withdrawal.Worker
	server           *http.Server
}

// componentResult 描述单体应用中一个长期运行组件的退出结果。
type componentResult struct {
	name string
	err  error
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
	chainClient, err := evm.NewFromConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("初始化 Sepolia RPC 失败：%w", err)
	}
	chainFailed := true
	defer func() {
		if chainFailed {
			chainClient.Close()
		}
	}()
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
	depositScanner, err := deposit.NewScanner(chainClient, store, logger, deposit.Config{
		StartBlock:    cfg.SepoliaScanStartBlock,
		Confirmations: cfg.SepoliaConfirmations,
		BatchSize:     cfg.SepoliaScanBatchSize,
		Interval:      cfg.SepoliaScanInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Sepolia 充值扫描器失败：%w", err)
	}
	if err := depositScanner.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("初始化 Sepolia 充值扫描器失败：%w", err)
	}
	withdrawalService, err := withdrawal.NewService(chainClient, store)
	if err != nil {
		return nil, fmt.Errorf("创建 Sepolia 提币服务失败：%w", err)
	}
	withdrawalWorker, err := withdrawal.NewWorker(chainClient, store, provider, logger, withdrawal.WorkerConfig{
		Interval:      cfg.SepoliaScanInterval,
		Confirmations: cfg.SepoliaConfirmations,
		ChainID:       big.NewInt(evm.SepoliaChainID),
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Sepolia 提币 Worker 失败：%w", err)
	}

	application := &App{
		config:           cfg,
		logger:           logger,
		pool:             pool,
		store:            store,
		apiStore:         store,
		keys:             provider,
		chain:            chainClient,
		scanner:          depositScanner,
		withdrawals:      withdrawalService,
		withdrawalWorker: withdrawalWorker,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", application.health)
	mux.HandleFunc("GET /api/v1/users", application.listUsers)
	mux.HandleFunc("GET /api/v1/users/{user_id}/wallet", application.getWallet)
	mux.HandleFunc("GET /api/v1/users/{user_id}/deposits", application.listDeposits)
	mux.HandleFunc("GET /api/v1/users/{user_id}/withdrawals", application.listWithdrawals)
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawal-quote", application.quoteWithdrawal)
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawals", application.createWithdrawal)
	mux.HandleFunc("GET /api/v1/withdrawals/{withdrawal_id}", application.getWithdrawal)
	mux.HandleFunc("GET /api/v1/transactions", application.listTransactions)
	mux.HandleFunc("GET /api/v1/system/chains/sepolia", application.getChainStatus)
	mux.HandleFunc("GET /api/v1/worker-errors", application.listWorkerErrors)
	application.server = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.requestContext(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	failed = false
	chainFailed = false
	return application, nil
}

// Run 启动 HTTP、充值扫描和提币 Worker，并在任一关键组件停止时统一关闭。
func (a *App) Run(ctx context.Context) error {
	defer a.pool.Close()
	defer a.chain.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan componentResult, 3)
	go func() {
		a.logger.Info("Mini Custody Web 服务已启动", "address", a.server.Addr)
		err := a.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- componentResult{name: "web", err: err}
	}()
	go func() {
		a.logger.Info("Sepolia 充值扫描器已启动")
		results <- componentResult{name: "deposit", err: a.scanner.Run(runCtx)}
	}()
	go func() {
		a.logger.Info("Sepolia 提币 Worker 已启动")
		results <- componentResult{name: "withdrawal", err: a.withdrawalWorker.Run(runCtx)}
	}()

	select {
	case result := <-results:
		cancel()
		if result.name != "web" {
			if shutdownErr := a.shutdownServer(); shutdownErr != nil {
				return shutdownErr
			}
		}
		<-results
		<-results
		if result.err != nil {
			return fmt.Errorf("应用组件 %s 已停止：%w", result.name, result.err)
		}
		return nil
	case <-ctx.Done():
		cancel()
		if err := a.shutdownServer(); err != nil {
			return err
		}
		first := <-results
		second := <-results
		third := <-results
		for _, result := range []componentResult{first, second, third} {
			if result.err != nil {
				return fmt.Errorf("关闭应用组件 %s 失败：%w", result.name, result.err)
			}
		}
		return nil
	}
}

// shutdownServer 在限定时间内优雅关闭 HTTP 服务。
func (a *App) shutdownServer() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭 Web 服务失败：%w", err)
	}
	return nil
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
	chainHealth := a.chain.Health(ctx)
	response["chain_status"] = chainHealth.Status
	response["chain_endpoint"] = chainHealth.Endpoint
	response["chain_id"] = chainHealth.ChainID
	response["network_height"] = fmt.Sprintf("%d", chainHealth.NetworkHeight)
	response["scan_height"] = fmt.Sprintf("%d", chainHealth.ScanHeight)
	if chainHealth.Status == "DOWN" {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
