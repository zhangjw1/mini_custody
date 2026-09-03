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

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiaoqi/mini-custody/backend/internal/btc"
	bitcoinchain "github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/deposit"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	tokendeposit "github.com/xiaoqi/mini-custody/backend/internal/token/deposit"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/token/gasstation"
	tokensweep "github.com/xiaoqi/mini-custody/backend/internal/token/sweep"
	tokenwithdrawal "github.com/xiaoqi/mini-custody/backend/internal/token/withdrawal"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"github.com/xiaoqi/mini-custody/backend/internal/withdrawal"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

type App struct {
	config                  config.Config
	logger                  *slog.Logger
	pool                    *pgxpool.Pool
	store                   *postgres.Store
	apiStore                APIStore
	keys                    wallet.KeyProvider
	chain                   *evm.Client
	tokenContract           *erc20.Contract
	tokenScanner            *tokendeposit.Scanner
	gasStation              *gasstation.Worker
	tokenSweeper            *tokensweep.Worker
	tokenAsset              postgres.Asset
	tokenWithdrawals        *tokenwithdrawal.Service
	tokenWithdrawalWorker   *tokenwithdrawal.Worker
	bitcoinClient           *bitcoinchain.Client
	bitcoinScanner          *btc.Scanner
	bitcoinSweeper          *btc.SweepWorker
	bitcoinWithdrawals      *btc.WithdrawalService
	bitcoinWithdrawalWorker *btc.WithdrawalWorker
	scanner                 *deposit.Scanner
	withdrawals             *withdrawal.Service
	withdrawalWorker        *withdrawal.Worker
	server                  *http.Server
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
	var tokenContract *erc20.Contract
	if cfg.ERC20.Enabled {
		tokenContract, err = erc20.NewContract(chainClient, common.HexToAddress(cfg.ERC20.ContractAddress))
		if err != nil {
			return nil, fmt.Errorf("创建 ERC-20 合约适配器失败：%w", err)
		}
		if _, err := tokenContract.Validate(ctx, cfg.ERC20.Symbol, cfg.ERC20.Decimals); err != nil {
			return nil, fmt.Errorf("校验 ERC-20 合约失败：%w", err)
		}
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
	var bitcoinClient *bitcoinchain.Client
	var bitcoinScanner *btc.Scanner
	var bitcoinSweeper *btc.SweepWorker
	var bitcoinWithdrawals *btc.WithdrawalService
	var bitcoinWithdrawalWorker *btc.WithdrawalWorker
	if cfg.Bitcoin.Enabled {
		profile, profileErr := bitcoinchain.ResolveNetwork(cfg.Bitcoin.Network)
		if profileErr != nil {
			return nil, profileErr
		}
		if err = postgres.ConfigureBitcoinNetwork(profile.DatabaseNetwork); err != nil {
			return nil, err
		}
		if err = btc.ConfigureNetwork(profile.DatabaseNetwork); err != nil {
			return nil, err
		}
		bitcoinClient, err = bitcoinchain.NewClient(cfg.Bitcoin.RPCURL, cfg.Bitcoin.RPCUser, cfg.Bitcoin.RPCPassword, cfg.Bitcoin.RPCTimeout)
		if err != nil {
			return nil, fmt.Errorf("初始化 Bitcoin RPC 失败：%w", err)
		}
		if err = bitcoinClient.VerifyNetwork(ctx, profile.RPCChain); err != nil {
			return nil, fmt.Errorf("校验 Bitcoin 网络失败：%w", err)
		}
		if err = store.EnsureBitcoinWallets(ctx, provider); err != nil {
			return nil, fmt.Errorf("初始化 Bitcoin 钱包失败：%w", err)
		}
		addresses, loadErr := store.ListBTCAddresses(ctx)
		if loadErr != nil {
			return nil, fmt.Errorf("加载 Bitcoin 地址失败：%w", loadErr)
		}
		start := int64(0)
		if cfg.Bitcoin.ScanStartBlock != nil {
			start = int64(*cfg.Bitcoin.ScanStartBlock)
		}
		bitcoinScanner, err = btc.NewScanner(bitcoinClient, store, addresses, start, int64(cfg.Bitcoin.Confirmations), int64(cfg.Bitcoin.ScanBatchSize), cfg.Bitcoin.ScanInterval, logger)
		if err != nil {
			return nil, err
		}
		bitcoinSweeper, err = btc.NewSweepWorker(bitcoinClient, store, provider, cfg.Bitcoin.SweepInterval, cfg.Bitcoin.SweepFeeRateSatVB, int64(cfg.Bitcoin.Confirmations), logger)
		if err != nil {
			return nil, err
		}
		bitcoinWithdrawals, err = btc.NewWithdrawalService(store, cfg.Bitcoin.SweepFeeRateSatVB)
		if err != nil {
			return nil, err
		}
		bitcoinWithdrawalWorker, err = btc.NewWithdrawalWorker(bitcoinClient, store, provider, cfg.Bitcoin.SweepInterval, int64(cfg.Bitcoin.Confirmations), logger)
		if err != nil {
			return nil, err
		}
	}
	var tokenAsset postgres.Asset
	if cfg.ERC20.Enabled {
		tokenAsset, err = store.EnsureERC20Asset(ctx, postgres.Asset{
			Network:         postgres.NetworkSepolia,
			Symbol:          cfg.ERC20.Symbol,
			ContractAddress: cfg.ERC20.ContractAddress,
			Decimals:        cfg.ERC20.Decimals,
			Enabled:         true,
		})
		if err != nil {
			return nil, fmt.Errorf("初始化 ERC-20 资产失败：%w", err)
		}
		if _, err := store.EnsurePlatformHotWallet(ctx, provider, cfg.PlatformWallet.HotWalletPath); err != nil {
			return nil, fmt.Errorf("初始化平台热钱包失败：%w", err)
		}
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
	var tokenScanner *tokendeposit.Scanner
	if cfg.ERC20.Enabled {
		tokenScanner, err = tokendeposit.NewScanner(chainClient, tokenContract, store, logger, tokendeposit.Config{
			AssetID: tokenAsset.ID, StartBlock: cfg.ERC20.ScanStartBlock,
			BatchSize: cfg.ERC20.ScanBatchSize, Confirmations: cfg.ERC20.Confirmations,
			Interval: cfg.SepoliaScanInterval,
		})
		if err != nil {
			return nil, fmt.Errorf("创建 Token 充值扫描器失败：%w", err)
		}
		if err := tokenScanner.Initialize(ctx); err != nil {
			return nil, fmt.Errorf("初始化 Token 充值扫描器失败：%w", err)
		}
	}
	var gasStationWorker *gasstation.Worker
	var tokenSweepWorker *tokensweep.Worker
	var tokenWithdrawalService *tokenwithdrawal.Service
	var tokenWithdrawalWorker *tokenwithdrawal.Worker
	if cfg.ERC20.SweepEnabled {
		gasStationWorker, err = gasstation.NewWorker(chainClient, tokenContract, store, provider, logger, gasstation.Config{
			Interval: cfg.ERC20.SweepInterval, Confirmations: cfg.ERC20.Confirmations,
			ChainID: big.NewInt(evm.SepoliaChainID), GasSafetyBPS: cfg.ERC20.GasSafetyBPS,
			GasTopupMaxWei: cfg.ERC20.GasTopupMaxWei, PlatformMinBalanceWei: cfg.PlatformWallet.MinETHBalanceWei,
		})
		if err != nil {
			return nil, fmt.Errorf("创建 Gas Station Worker 失败：%w", err)
		}
		tokenSweepWorker, err = tokensweep.NewWorker(chainClient, tokenContract, store, provider, logger, tokensweep.Config{
			Interval: cfg.ERC20.SweepInterval, Confirmations: cfg.ERC20.Confirmations,
			ChainID: big.NewInt(evm.SepoliaChainID), Symbol: cfg.ERC20.Symbol,
		})
		if err != nil {
			return nil, fmt.Errorf("创建 Token Sweep Worker 失败：%w", err)
		}
	}
	if cfg.ERC20.Enabled {
		tokenWithdrawalService, err = tokenwithdrawal.NewService(chainClient, tokenContract, store, tokenAsset)
		if err != nil {
			return nil, fmt.Errorf("创建 Token 提币服务失败：%w", err)
		}
		tokenWithdrawalWorker, err = tokenwithdrawal.NewWorker(chainClient, tokenContract, store, provider, logger, tokenwithdrawal.WorkerConfig{
			Interval: cfg.ERC20.SweepInterval, Confirmations: cfg.ERC20.Confirmations,
			ChainID: big.NewInt(evm.SepoliaChainID),
		})
		if err != nil {
			return nil, fmt.Errorf("创建 Token 提币 Worker 失败：%w", err)
		}
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
		config:                cfg,
		logger:                logger,
		pool:                  pool,
		store:                 store,
		apiStore:              store,
		keys:                  provider,
		chain:                 chainClient,
		tokenContract:         tokenContract,
		tokenScanner:          tokenScanner,
		gasStation:            gasStationWorker,
		tokenSweeper:          tokenSweepWorker,
		tokenAsset:            tokenAsset,
		tokenWithdrawals:      tokenWithdrawalService,
		tokenWithdrawalWorker: tokenWithdrawalWorker,
		bitcoinClient:         bitcoinClient, bitcoinScanner: bitcoinScanner, bitcoinSweeper: bitcoinSweeper,
		bitcoinWithdrawals:      bitcoinWithdrawals,
		bitcoinWithdrawalWorker: bitcoinWithdrawalWorker,
		scanner:                 depositScanner,
		withdrawals:             withdrawalService,
		withdrawalWorker:        withdrawalWorker,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", application.health)
	mux.HandleFunc("GET /api/v1/users", application.listUsers)
	mux.HandleFunc("GET /api/v1/assets", application.listAssets)
	mux.HandleFunc("GET /api/v1/users/{user_id}/wallet", application.getWallet)
	mux.HandleFunc("GET /api/v1/users/{user_id}/btc-wallet", application.getBitcoinWallet)
	mux.HandleFunc("GET /api/v1/users/{user_id}/btc-deposits", application.listBitcoinDeposits)
	mux.HandleFunc("GET /api/v1/users/{user_id}/balances", application.listUserBalances)
	mux.HandleFunc("GET /api/v1/users/{user_id}/deposits", application.listDeposits)
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-deposits", application.listTokenDeposits)
	mux.HandleFunc("GET /api/v1/users/{user_id}/withdrawals", application.listWithdrawals)
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-withdrawals", application.listTokenWithdrawals)
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawal-quote", application.quoteWithdrawal)
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawals", application.createWithdrawal)
	if application.tokenWithdrawals != nil {
		mux.HandleFunc("POST /api/v1/users/{user_id}/token-withdrawal-quote", application.quoteTokenWithdrawal)
		mux.HandleFunc("POST /api/v1/users/{user_id}/token-withdrawals", application.createTokenWithdrawal)
	}
	mux.HandleFunc("GET /api/v1/withdrawals/{withdrawal_id}", application.getWithdrawal)
	mux.HandleFunc("GET /api/v1/token-withdrawals/{withdrawal_id}", application.getTokenWithdrawal)
	mux.HandleFunc("GET /api/v1/transactions", application.listTransactions)
	mux.HandleFunc("GET /api/v1/sweeps", application.listTokenSweeps)
	mux.HandleFunc("GET /api/v1/btc/sweeps", application.listBitcoinSweeps)
	if application.bitcoinWithdrawals != nil {
		mux.HandleFunc("POST /api/v1/users/{user_id}/btc-withdrawals", application.createBitcoinWithdrawal)
		mux.HandleFunc("GET /api/v1/users/{user_id}/btc-withdrawals", application.listBitcoinWithdrawals)
		mux.HandleFunc("GET /api/v1/btc-withdrawals/{withdrawal_id}", application.getBitcoinWithdrawal)
	}
	mux.HandleFunc("GET /api/v1/internal-transfers", application.listInternalTransfers)
	mux.HandleFunc("GET /api/v1/system/platform-wallet", application.getPlatformWalletStatus)
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
	type component struct {
		name string
		run  func() error
	}
	components := []component{
		{name: "web", run: func() error {
			a.logger.Info("Mini Custody Web 服务已启动", "address", a.server.Addr)
			err := a.server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}},
		{name: "deposit", run: func() error {
			a.logger.Info("Sepolia 充值扫描器已启动")
			return a.scanner.Run(runCtx)
		}},
		{name: "withdrawal", run: func() error {
			a.logger.Info("Sepolia 提币 Worker 已启动")
			return a.withdrawalWorker.Run(runCtx)
		}},
	}
	if a.tokenScanner != nil {
		components = append(components, component{name: "token-deposit", run: func() error {
			a.logger.Info("Token 充值扫描器已启动")
			return a.tokenScanner.Run(runCtx)
		}})
	}
	if a.bitcoinScanner != nil {
		components = append(components, component{name: "bitcoin-deposit", run: func() error { a.logger.Info("Bitcoin 充值扫描器已启动"); return a.bitcoinScanner.Run(runCtx) }})
	}
	if a.bitcoinSweeper != nil {
		components = append(components, component{name: "bitcoin-sweep", run: func() error { a.logger.Info("Bitcoin 归集 Worker 已启动"); return a.bitcoinSweeper.Run(runCtx) }})
	}
	if a.bitcoinWithdrawalWorker != nil {
		components = append(components, component{name: "bitcoin-withdrawal", run: func() error {
			a.logger.Info("Bitcoin 提币 Worker 已启动")
			return a.bitcoinWithdrawalWorker.Run(runCtx)
		}})
	}
	if a.gasStation != nil {
		components = append(components, component{name: "gas-station", run: func() error {
			a.logger.Info("Gas Station Worker 已启动")
			return a.gasStation.Run(runCtx)
		}})
	}
	if a.tokenSweeper != nil {
		components = append(components, component{name: "token-sweep", run: func() error {
			a.logger.Info("Token Sweep Worker 已启动")
			return a.tokenSweeper.Run(runCtx)
		}})
	}
	if a.tokenWithdrawalWorker != nil {
		components = append(components, component{name: "token-withdrawal", run: func() error {
			a.logger.Info("Token 提币 Worker 已启动")
			return a.tokenWithdrawalWorker.Run(runCtx)
		}})
	}
	results := make(chan componentResult, len(components))
	for _, item := range components {
		item := item
		go func() { results <- componentResult{name: item.name, err: item.run()} }()
	}

	select {
	case result := <-results:
		cancel()
		if result.name != "web" {
			if shutdownErr := a.shutdownServer(); shutdownErr != nil {
				return shutdownErr
			}
		}
		for index := 1; index < len(components); index++ {
			other := <-results
			if result.err == nil && other.err != nil {
				result = other
			}
		}
		if result.err != nil {
			return fmt.Errorf("应用组件 %s 已停止：%w", result.name, result.err)
		}
		return nil
	case <-ctx.Done():
		cancel()
		if err := a.shutdownServer(); err != nil {
			return err
		}
		for range components {
			result := <-results
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
	if a.tokenScanner != nil {
		tokenHealth := a.tokenScanner.Snapshot()
		response["token_status"] = tokenHealth.Status
		response["token_network_height"] = fmt.Sprintf("%d", tokenHealth.NetworkHeight)
		response["token_scan_height"] = fmt.Sprintf("%d", tokenHealth.ScanHeight)
		response["token_lag_blocks"] = fmt.Sprintf("%d", tokenHealth.LagBlocks)
		if tokenHealth.LastError != "" {
			response["token_last_error"] = tokenHealth.LastError
		}
		if tokenHealth.Status == "DOWN" {
			status = http.StatusServiceUnavailable
		}
	}
	if a.gasStation != nil {
		gasHealth := a.gasStation.Snapshot()
		response["gas_station_status"] = gasHealth.Status
		response["gas_station_balance_wei"] = gasHealth.BalanceWei.String()
		response["gas_station_minimum_wei"] = gasHealth.MinBalanceWei.String()
		if gasHealth.LastError != "" {
			response["gas_station_last_error"] = gasHealth.LastError
		}
		if gasHealth.Status == "DOWN" || gasHealth.Status == "LOW_BALANCE" {
			status = http.StatusServiceUnavailable
			response["status"] = "unavailable"
		}
	}
	if a.tokenSweeper != nil {
		sweepHealth := a.tokenSweeper.Snapshot()
		response["token_inventory_status"] = sweepHealth.Status
		response["token_inventory_balance_units"] = sweepHealth.BalanceUnits.String()
		if sweepHealth.LastError != "" {
			response["token_inventory_last_error"] = sweepHealth.LastError
		}
		if sweepHealth.Status == "DOWN" {
			status = http.StatusServiceUnavailable
			response["status"] = "unavailable"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
