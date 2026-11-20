package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiaoqi/mini-custody/backend/internal/auth"
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
	authStore               AuthStore
	jwtSecret               []byte
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
	// 初始化数据库连接池。
	pool, err := postgres.OpenWithConfig(ctx, cfg.DatabaseURL, cfg.Timezone, postgres.PoolConfig{
		MaxConns:        cfg.DB.MaxConns,
		MinConns:        cfg.DB.MinConns,
		MaxConnLifetime: cfg.DB.MaxConnLifetime,
		MaxConnIdleTime: cfg.DB.MaxConnIdleTime,
	})
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
	// 初始化 EVM 客户端。
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
	// 初始化 ERC-20 合约适配器。
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
	// 初始化 HD钱包适配器
	provider, err := wallet.LoadProvider(cfg.CustodyKeyStoreFile, cfg.CustodyPassword)
	if err != nil {
		return nil, fmt.Errorf("加载托管密钥提供器失败：%w", err)
	}
	store, err := postgres.NewStore(pool, cfg.Timezone)
	if err != nil {
		return nil, err
	}
	jwtSecret, err := resolveJWTSecret(cfg, logger)
	if err != nil {
		return nil, err
	}
	if err := ensureBootstrapAdmin(ctx, store, cfg, logger); err != nil {
		return nil, err
	}
	application := &App{
		config:        cfg,
		logger:        logger,
		pool:          pool,
		store:         store,
		apiStore:      store,
		authStore:     store,
		jwtSecret:     jwtSecret,
		keys:          provider,
		chain:         chainClient,
		tokenContract: tokenContract,
	}
	if err := application.wireBTC(ctx, cfg, store, provider, logger); err != nil {
		return nil, err
	}
	if err := application.wireEVM(ctx, cfg, chainClient, store, provider, logger); err != nil {
		return nil, err
	}
	if err := application.wireERC20(ctx, cfg, chainClient, tokenContract, store, provider, logger); err != nil {
		return nil, err
	}
	application.server = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.requestContext(application.buildRouter()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
	failed = false
	chainFailed = false
	return application, nil
}

// wireBTC 根据配置装配 Bitcoin RPC 客户端、扫描器、归集和提币组件。
func (a *App) wireBTC(ctx context.Context, cfg config.Config, store *postgres.Store, provider *wallet.MnemonicKeyProvider, logger *slog.Logger) error {
	if !cfg.Bitcoin.Enabled {
		return nil
	}
	profile, err := bitcoinchain.ResolveNetwork(cfg.Bitcoin.Network)
	if err != nil {
		return err
	}
	if err = postgres.ConfigureBitcoinNetwork(profile.DatabaseNetwork); err != nil {
		return err
	}
	if err = btc.ConfigureNetwork(profile.DatabaseNetwork); err != nil {
		return err
	}
	bitcoinClient, err := bitcoinchain.NewClient(cfg.Bitcoin.RPCURL, cfg.Bitcoin.RPCUser, cfg.Bitcoin.RPCPassword, cfg.Bitcoin.RPCTimeout)
	if err != nil {
		return fmt.Errorf("初始化 Bitcoin RPC 失败：%w", err)
	}
	if err = bitcoinClient.VerifyNetwork(ctx, profile.RPCChain); err != nil {
		return fmt.Errorf("校验 Bitcoin 网络失败：%w", err)
	}
	if err = store.EnsureBitcoinWallets(ctx, provider); err != nil {
		return fmt.Errorf("初始化 Bitcoin 钱包失败：%w", err)
	}
	addresses, err := store.ListBTCAddresses(ctx)
	if err != nil {
		return fmt.Errorf("加载 Bitcoin 地址失败：%w", err)
	}
	start := int64(0)
	if cfg.Bitcoin.ScanStartBlock != nil {
		start = int64(*cfg.Bitcoin.ScanStartBlock)
	}
	bitcoinScanner, err := btc.NewScanner(bitcoinClient, store, addresses, start, int64(cfg.Bitcoin.Confirmations), int64(cfg.Bitcoin.ScanBatchSize), cfg.Bitcoin.ScanInterval, logger)
	if err != nil {
		return err
	}
	bitcoinSweeper, err := btc.NewSweepWorker(bitcoinClient, store, provider, cfg.Bitcoin.SweepInterval, cfg.Bitcoin.SweepFeeRateSatVB, int64(cfg.Bitcoin.Confirmations), logger)
	if err != nil {
		return err
	}
	bitcoinWithdrawals, err := btc.NewWithdrawalService(store, cfg.Bitcoin.SweepFeeRateSatVB)
	if err != nil {
		return err
	}
	bitcoinWithdrawalWorker, err := btc.NewWithdrawalWorker(bitcoinClient, store, provider, cfg.Bitcoin.SweepInterval, int64(cfg.Bitcoin.Confirmations), logger)
	if err != nil {
		return err
	}
	a.bitcoinClient = bitcoinClient
	a.bitcoinScanner = bitcoinScanner
	a.bitcoinSweeper = bitcoinSweeper
	a.bitcoinWithdrawals = bitcoinWithdrawals
	a.bitcoinWithdrawalWorker = bitcoinWithdrawalWorker
	return nil
}

// wireEVM 装配 Sepolia ETH 充值扫描器和提币服务及 Worker。
func (a *App) wireEVM(ctx context.Context, cfg config.Config, chainClient *evm.Client, store *postgres.Store, provider *wallet.MnemonicKeyProvider, logger *slog.Logger) error {
	depositScanner, err := deposit.NewScanner(chainClient, store, logger, deposit.Config{
		StartBlock:        cfg.SepoliaScanStartBlock,
		Confirmations:     cfg.SepoliaConfirmations,
		SafeDepth:         cfg.SepoliaSafeDepth,
		ReorgRewindBlocks: cfg.SepoliaReorgRewindBlocks,
		BatchSize:         cfg.SepoliaScanBatchSize,
		Interval:          cfg.SepoliaScanInterval,
	})
	if err != nil {
		return fmt.Errorf("创建 Sepolia 充值扫描器失败：%w", err)
	}
	if err := depositScanner.Initialize(ctx); err != nil {
		return fmt.Errorf("初始化 Sepolia 充值扫描器失败：%w", err)
	}
	withdrawalService, err := withdrawal.NewService(chainClient, store)
	if err != nil {
		return fmt.Errorf("创建 Sepolia 提币服务失败：%w", err)
	}
	withdrawalWorker, err := withdrawal.NewWorker(chainClient, store, provider, logger, withdrawal.WorkerConfig{
		Interval:                     cfg.SepoliaScanInterval,
		Confirmations:                cfg.SepoliaConfirmations,
		ChainID:                      big.NewInt(evm.SepoliaChainID),
		RebroadcastAfter:             cfg.EVMReplacement.RebroadcastAfter,
		BumpAfter:                    cfg.EVMReplacement.BumpAfter,
		BumpBlockedManualReviewAfter: cfg.EVMReplacement.BumpBlockedManualReviewAfter,
		BumpPercent:                  cfg.EVMReplacement.BumpPercent,
		MaxBumps:                     cfg.EVMReplacement.MaxBumps,
		MaxFeeCapWei:                 cfg.EVMReplacement.MaxFeeCapWei,
	})
	if err != nil {
		return fmt.Errorf("创建 Sepolia 提币 Worker 失败：%w", err)
	}
	a.scanner = depositScanner
	a.withdrawals = withdrawalService
	a.withdrawalWorker = withdrawalWorker
	return nil
}

// wireERC20 根据配置装配 ERC-20 资产、平台热钱包及 Token 充值、归集、补气和提币组件。
func (a *App) wireERC20(ctx context.Context, cfg config.Config, chainClient *evm.Client, tokenContract *erc20.Contract, store *postgres.Store, provider *wallet.MnemonicKeyProvider, logger *slog.Logger) error {
	if cfg.ERC20.Enabled {
		tokenAsset, err := store.EnsureERC20Asset(ctx, postgres.Asset{
			Network:         postgres.NetworkSepolia,
			Symbol:          cfg.ERC20.Symbol,
			ContractAddress: cfg.ERC20.ContractAddress,
			Decimals:        cfg.ERC20.Decimals,
			Enabled:         true,
		})
		if err != nil {
			return fmt.Errorf("初始化 ERC-20 资产失败：%w", err)
		}
		if _, err := store.EnsurePlatformHotWallet(ctx, provider, cfg.PlatformWallet.HotWalletPath); err != nil {
			return fmt.Errorf("初始化平台热钱包失败：%w", err)
		}
		tokenScanner, err := tokendeposit.NewScanner(chainClient, tokenContract, store, logger, tokendeposit.Config{
			AssetID: tokenAsset.ID, StartBlock: cfg.ERC20.ScanStartBlock,
			BatchSize: cfg.ERC20.ScanBatchSize, Confirmations: cfg.ERC20.Confirmations,
			SafeDepth: cfg.ERC20.SafeDepth, ReorgRewindBlocks: cfg.ERC20.ReorgRewindBlocks,
			Interval: cfg.SepoliaScanInterval,
		})
		if err != nil {
			return fmt.Errorf("创建 Token 充值扫描器失败：%w", err)
		}
		if err := tokenScanner.Initialize(ctx); err != nil {
			return fmt.Errorf("初始化 Token 充值扫描器失败：%w", err)
		}
		tokenWithdrawalService, err := tokenwithdrawal.NewService(chainClient, tokenContract, store, tokenAsset)
		if err != nil {
			return fmt.Errorf("创建 Token 提币服务失败：%w", err)
		}
		tokenWithdrawalWorker, err := tokenwithdrawal.NewWorker(chainClient, tokenContract, store, provider, logger, tokenwithdrawal.WorkerConfig{
			Interval:                     cfg.ERC20.SweepInterval,
			Confirmations:                cfg.ERC20.Confirmations,
			ChainID:                      big.NewInt(evm.SepoliaChainID),
			RebroadcastAfter:             cfg.EVMReplacement.RebroadcastAfter,
			BumpAfter:                    cfg.EVMReplacement.BumpAfter,
			BumpBlockedManualReviewAfter: cfg.EVMReplacement.BumpBlockedManualReviewAfter,
			BumpPercent:                  cfg.EVMReplacement.BumpPercent,
			MaxBumps:                     cfg.EVMReplacement.MaxBumps,
			MaxFeeCapWei:                 cfg.EVMReplacement.MaxFeeCapWei,
		})
		if err != nil {
			return fmt.Errorf("创建 Token 提币 Worker 失败：%w", err)
		}
		a.tokenAsset = tokenAsset
		a.tokenScanner = tokenScanner
		a.tokenWithdrawals = tokenWithdrawalService
		a.tokenWithdrawalWorker = tokenWithdrawalWorker
	}
	if cfg.ERC20.SweepEnabled {
		gasStationWorker, err := gasstation.NewWorker(chainClient, tokenContract, store, provider, logger, gasstation.Config{
			Interval:                     cfg.ERC20.SweepInterval,
			Confirmations:                cfg.ERC20.Confirmations,
			ChainID:                      big.NewInt(evm.SepoliaChainID),
			GasSafetyBPS:                 cfg.ERC20.GasSafetyBPS,
			GasTopupMaxWei:               cfg.ERC20.GasTopupMaxWei,
			PlatformMinBalanceWei:        cfg.PlatformWallet.MinETHBalanceWei,
			RebroadcastAfter:             cfg.EVMReplacement.RebroadcastAfter,
			BumpAfter:                    cfg.EVMReplacement.BumpAfter,
			BumpBlockedManualReviewAfter: cfg.EVMReplacement.BumpBlockedManualReviewAfter,
			BumpPercent:                  cfg.EVMReplacement.BumpPercent,
			MaxBumps:                     cfg.EVMReplacement.MaxBumps,
			MaxFeeCapWei:                 cfg.EVMReplacement.MaxFeeCapWei,
		})
		if err != nil {
			return fmt.Errorf("创建 Gas Station Worker 失败：%w", err)
		}
		tokenSweepWorker, err := tokensweep.NewWorker(chainClient, tokenContract, store, provider, logger, tokensweep.Config{
			Interval:                     cfg.ERC20.SweepInterval,
			Confirmations:                cfg.ERC20.Confirmations,
			ChainID:                      big.NewInt(evm.SepoliaChainID),
			Symbol:                       cfg.ERC20.Symbol,
			GasSafetyBPS:                 cfg.ERC20.GasSafetyBPS,
			RebroadcastAfter:             cfg.EVMReplacement.RebroadcastAfter,
			BumpAfter:                    cfg.EVMReplacement.BumpAfter,
			BumpBlockedManualReviewAfter: cfg.EVMReplacement.BumpBlockedManualReviewAfter,
			BumpPercent:                  cfg.EVMReplacement.BumpPercent,
			MaxBumps:                     cfg.EVMReplacement.MaxBumps,
			MaxFeeCapWei:                 cfg.EVMReplacement.MaxFeeCapWei,
		})
		if err != nil {
			return fmt.Errorf("创建 Token Sweep Worker 失败：%w", err)
		}
		a.gasStation = gasStationWorker
		a.tokenSweeper = tokenSweepWorker
	}
	return nil
}

// buildRouter 注册全部 HTTP 健康检查、认证及后台管理路由。
func (a *App) buildRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /livez", a.livez)
	mux.HandleFunc("GET /readyz", a.health)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)

	mux.HandleFunc("GET /api/v1/auth/me", a.requireAdminAuth(a.me))
	mux.HandleFunc("GET /api/v1/users", a.requireAdminAuth(a.listUsers))
	mux.HandleFunc("POST /api/v1/users", a.requireAdminAuth(a.createUser))
	mux.HandleFunc("PATCH /api/v1/users/{user_id}", a.requireAdminAuth(a.updateUser))
	mux.HandleFunc("GET /api/v1/assets", a.requireAdminAuth(a.listAssets))
	mux.HandleFunc("GET /api/v1/users/{user_id}/wallet", a.requireAdminAuth(a.getWallet))
	mux.HandleFunc("GET /api/v1/users/{user_id}/btc-wallet", a.requireAdminAuth(a.getBitcoinWallet))
	mux.HandleFunc("GET /api/v1/users/{user_id}/btc-deposits", a.requireAdminAuth(a.listBitcoinDeposits))
	mux.HandleFunc("GET /api/v1/users/{user_id}/balances", a.requireAdminAuth(a.listUserBalances))
	mux.HandleFunc("GET /api/v1/users/{user_id}/deposits", a.requireAdminAuth(a.listDeposits))
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-deposits", a.requireAdminAuth(a.listTokenDeposits))
	mux.HandleFunc("GET /api/v1/users/{user_id}/withdrawals", a.requireAdminAuth(a.listWithdrawals))
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-withdrawals", a.requireAdminAuth(a.listTokenWithdrawals))
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawal-quote", a.requireAdminAuth(a.quoteWithdrawal))
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawals", a.requireAdminAuth(a.createWithdrawal))
	if a.tokenWithdrawals != nil {
		mux.HandleFunc("POST /api/v1/users/{user_id}/token-withdrawal-quote", a.requireAdminAuth(a.quoteTokenWithdrawal))
		mux.HandleFunc("POST /api/v1/users/{user_id}/token-withdrawals", a.requireAdminAuth(a.createTokenWithdrawal))
	}
	mux.HandleFunc("GET /api/v1/withdrawals/{withdrawal_id}", a.requireAdminAuth(a.getWithdrawal))
	mux.HandleFunc("GET /api/v1/token-withdrawals/{withdrawal_id}", a.requireAdminAuth(a.getTokenWithdrawal))
	mux.HandleFunc("GET /api/v1/transactions", a.requireAdminAuth(a.listTransactions))
	mux.HandleFunc("GET /api/v1/sweeps", a.requireAdminAuth(a.listTokenSweeps))
	mux.HandleFunc("GET /api/v1/btc/sweeps", a.requireAdminAuth(a.listBitcoinSweeps))
	if a.bitcoinWithdrawals != nil {
		mux.HandleFunc("POST /api/v1/users/{user_id}/btc-withdrawals", a.requireAdminAuth(a.createBitcoinWithdrawal))
		mux.HandleFunc("GET /api/v1/users/{user_id}/btc-withdrawals", a.requireAdminAuth(a.listBitcoinWithdrawals))
		mux.HandleFunc("GET /api/v1/btc-withdrawals/{withdrawal_id}", a.requireAdminAuth(a.getBitcoinWithdrawal))
	}
	mux.HandleFunc("GET /api/v1/internal-transfers", a.requireAdminAuth(a.listInternalTransfers))
	mux.HandleFunc("GET /api/v1/system/platform-wallet", a.requireAdminAuth(a.getPlatformWalletStatus))
	mux.HandleFunc("GET /api/v1/system/chains/sepolia", a.requireAdminAuth(a.getChainStatus))
	mux.HandleFunc("GET /api/v1/worker-errors", a.requireAdminAuth(a.listWorkerErrors))
	return mux
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
		// 前面components只是定义了组件，后面go func()中才是真正的启动组件
		go func() { results <- componentResult{name: item.name, err: item.run()} }()
	}
	// 阻塞等待第一个退出的组件或外部取消信号。
	select {
	case result := <-results:
		cancel()
		if result.name != "web" {
			if shutdownErr := a.shutdownServer(); shutdownErr != nil {
				waitRemainingComponents(results, len(components)-1, 15*time.Second)
				return shutdownErr
			}
		}
		if firstErr := waitRemainingComponents(results, len(components)-1, 15*time.Second); result.err == nil && firstErr != nil {
			result.err = firstErr
		}
		if result.err != nil {
			return fmt.Errorf("应用组件 %s 已停止：%w", result.name, result.err)
		}
		return nil
	case <-ctx.Done():
		cancel()
		shutdownErr := a.shutdownServer()
		waitErr := waitRemainingComponents(results, len(components), 15*time.Second)
		if shutdownErr != nil {
			return shutdownErr
		}
		if waitErr != nil {
			return fmt.Errorf("关闭应用组件失败：%w", waitErr)
		}
		return nil
	}
}

// waitRemainingComponents 带超时等待其余后台组件退出，避免过早关闭数据库连接池。
func waitRemainingComponents(results <-chan componentResult, remaining int, timeout time.Duration) error {
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var firstErr error
	for i := 0; i < remaining; i++ {
		select {
		case item := <-results:
			if firstErr == nil && item.err != nil {
				firstErr = item.err
			}
		case <-timer.C:
			if firstErr != nil {
				return firstErr
			}
			return errors.New("等待后台组件退出超时")
		}
	}
	return firstErr
}

// resolveJWTSecret 加载并校验 JWT 签名密钥，未配置时在开发环境自动生成临时密钥。
func resolveJWTSecret(cfg config.Config, logger *slog.Logger) ([]byte, error) {
	if cfg.Auth.JWTSecret != "" {
		return []byte(cfg.Auth.JWTSecret), nil
	}
	secret, err := auth.GenerateRandomSecret()
	if err != nil {
		return nil, err
	}
	logger.Warn("未配置 JWT_SECRET，已为当前开发进程生成临时签名密钥")
	return secret, nil
}

// ensureBootstrapAdmin 根据启动配置幂等初始化首个管理员账号。
func ensureBootstrapAdmin(ctx context.Context, store *postgres.Store, cfg config.Config, logger *slog.Logger) error {
	if strings.TrimSpace(cfg.Auth.BootstrapPassword) == "" {
		logger.Warn("未配置 ADMIN_BOOTSTRAP_PASSWORD，若数据库尚无管理员将无法登录管理后台")
		return nil
	}
	hash, err := auth.HashPassword(cfg.Auth.BootstrapPassword)
	if err != nil {
		return fmt.Errorf("生成初始管理员密码哈希失败：%w", err)
	}
	if err := store.EnsureBootstrapAdmin(ctx, cfg.Auth.BootstrapUsername, hash, cfg.Auth.BootstrapDisplayName); err != nil {
		return fmt.Errorf("初始化管理员账号失败：%w", err)
	}
	return nil
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

// livez 返回进程存活探针状态，不检查外部依赖。
func (a *App) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().In(a.timezoneOrUTC()).Format(time.RFC3339),
	})
}

// health 检查数据库连接与链上组件就绪状态，余额偏低仅作为告警而不返回 503。
func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status := http.StatusOK
	response := map[string]string{
		"status": "ok",
		"time":   time.Now().In(a.timezoneOrUTC()).Format(time.RFC3339),
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
		response["status"] = "unavailable"
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
			response["status"] = "unavailable"
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
		if gasHealth.Status == "DOWN" {
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
	if a.bitcoinClient != nil {
		height, btcErr := a.bitcoinClient.BlockCount(ctx)
		if btcErr != nil {
			response["bitcoin_status"] = "DOWN"
			response["bitcoin_last_error"] = btcErr.Error()
			status = http.StatusServiceUnavailable
			response["status"] = "unavailable"
		} else {
			response["bitcoin_status"] = "HEALTHY"
			response["bitcoin_network_height"] = fmt.Sprintf("%d", height)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
