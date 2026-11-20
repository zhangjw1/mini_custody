package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = evm.DefaultWorkerBatchSize

// Chain 定义 Token 归集费用、Nonce、广播和确认所需的 EVM 能力。
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	SendTransaction(ctx context.Context, transaction *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// TokenContract 定义 Token 余额、transfer 编解码、Gas 估算和 Event 校验能力。
type TokenContract interface {
	Address() common.Address
	BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error)
	EncodeTransfer(to common.Address, amountUnits *big.Int) ([]byte, error)
	DecodeTransferCalldata(calldata []byte) (common.Address, *big.Int, error)
	EstimateTransferGas(ctx context.Context, from, to common.Address, amountUnits *big.Int) (uint64, error)
	DecodeTransferLog(log types.Log) (erc20.TransferEvent, error)
}

// Store 定义 Token 归集持久化状态机和恢复查询。
type Store interface {
	TokenSweepByID(ctx context.Context, id int64) (postgres.TokenSweep, error)
	ListProcessableTokenSweeps(ctx context.Context, limit int) ([]postgres.TokenSweep, error)
	ListEVMTxAttempts(ctx context.Context, businessType string, businessID int64) ([]postgres.EVMTxAttempt, error)
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	AllocateTokenSweepNonce(ctx context.Context, sweepID int64, sweepAmountUnits *big.Int, chainPendingNonce uint64, gas postgres.PreparedGasParams) (postgres.TokenSweep, bool, error)
	SaveSignedTokenSweep(ctx context.Context, signed postgres.SignedTokenSweep) (postgres.TokenSweep, bool, error)
	ReplaceTokenSweepTx(ctx context.Context, replacement postgres.SignedTokenSweep) (postgres.TokenSweep, bool, error)
	TouchTokenSweepBroadcast(ctx context.Context, sweepID int64) (postgres.TokenSweep, error)
	MarkTokenSweepManualReview(ctx context.Context, sweepID int64, errorCode, errorMessage string) (postgres.TokenSweep, bool, error)
	TransitionTokenSweep(ctx context.Context, sweepID int64, target string) (postgres.TokenSweep, error)
	UpdateTokenSweepConfirmations(ctx context.Context, sweepID, confirmations int64) (postgres.TokenSweep, error)
	FinalizeTokenSweep(ctx context.Context, settlement postgres.TokenSweepSettlement) (postgres.TokenSweep, bool, error)
	FailWaitingTokenSweep(ctx context.Context, sweepID int64, errorCode, errorMessage string) (postgres.TokenSweep, bool, error)
	RecordEVMSigningFailure(ctx context.Context, businessType string, businessID int64, maxRetries int, worker, refType string, refID int64, errorCode, errorMessage string) (bool, error)
	RecordEVMBumpBlocked(ctx context.Context, businessType string, businessID int64, worker, refType string, refID int64, reasonCode, reasonMessage string) (time.Time, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// Config 定义 Token 归集轮询、确认数、卡单重播加价策略、Chain ID 和展示资产信息。
type Config struct {
	Interval                     time.Duration
	Confirmations                uint64
	ChainID                      *big.Int
	Symbol                       string
	GasSafetyBPS                 uint64
	RebroadcastAfter             time.Duration
	BumpAfter                    time.Duration
	BumpBlockedManualReviewAfter time.Duration
	BumpPercent                  uint64
	MaxBumps                     int
	MaxFeeCapWei                 *big.Int
}

// HealthSnapshot 描述热钱包 Token 链上库存的最近快照。
type HealthSnapshot struct {
	Status       string
	Symbol       string
	Address      string
	BalanceUnits *big.Int
	LastError    string
	CheckedAt    time.Time
}

// Worker 负责用户地址 Token 自动归集、幂等广播和库存快照。
type Worker struct {
	chain       Chain
	contract    TokenContract
	store       Store
	keys        wallet.KeyProvider
	logger      *slog.Logger
	config      Config
	healthMu    sync.RWMutex
	health      HealthSnapshot
	fees        evm.FeeEstimator
	broadcaster evm.Broadcaster
	confirmer   evm.Confirmer
}

// NewWorker 创建并校验 Token Sweep Worker。
func NewWorker(chain Chain, contract TokenContract, store Store, keys wallet.KeyProvider, logger *slog.Logger, cfg Config) (*Worker, error) {
	cfg.Symbol = strings.TrimSpace(cfg.Symbol)
	if chain == nil || contract == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("Token Sweep Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil ||
		cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 || cfg.Symbol == "" {
		return nil, errors.New("Token Sweep Worker 配置无效")
	}
	if err := normalizeReplacementConfig(&cfg); err != nil {
		return nil, err
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{
		chain: chain, contract: contract, store: store, keys: keys, logger: logger, config: cfg,
		health:      HealthSnapshot{Status: "CHECKING", Symbol: cfg.Symbol, BalanceUnits: new(big.Int)},
		fees:        evm.NewFeeEstimator(chain),
		broadcaster: evm.NewBroadcaster(chain),
		confirmer:   evm.NewConfirmer(chain),
	}, nil
}

// Run 周期推进全部可恢复 Token 归集任务。
func (w *Worker) Run(ctx context.Context) error {
	return evm.RunPollLoop(ctx, w.config.Interval, w.RunOnce, func(err error) {
		w.logger.Warn("Token Sweep Worker 本轮存在失败，将在下轮重试", "error", err)
	})
}

// RunOnce 刷新热钱包库存并逐条推进归集，单条失败不阻塞其他任务。
func (w *Worker) RunOnce(ctx context.Context) error {
	ctx = evm.WithTickCache(ctx)
	var firstErr error
	if err := w.refreshInventory(ctx); err != nil {
		firstErr = err
	}
	items, err := w.store.ListProcessableTokenSweeps(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询待处理 Token 归集失败：%w", err)
	}
	for _, item := range items {
		if err := w.Process(ctx, item.ID); err != nil {
			w.recordError(ctx, item.ID, "process", "TOKEN_SWEEP_PROCESS_FAILED", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据持久化状态恢复并推进一笔 Token 归集。
func (w *Worker) Process(ctx context.Context, sweepID int64) error {
	item, err := w.store.TokenSweepByID(ctx, sweepID)
	if err != nil {
		return fmt.Errorf("查询 Token 归集 %d 失败：%w", sweepID, err)
	}
	switch item.Status {
	case postgres.TokenSweepWaitingGas:
		return w.prepare(ctx, item)
	case postgres.TokenSweepSigning:
		return w.sign(ctx, item)
	case postgres.TokenSweepSigned:
		return w.broadcast(ctx, item)
	case postgres.TokenSweepBroadcasted, postgres.TokenSweepConfirming:
		return w.confirm(ctx, item)
	default:
		return nil
	}
}

// prepare 读取最新链上 Token 和 ETH 余额，并在全部校验通过后分配用户地址 Nonce。
func (w *Worker) prepare(ctx context.Context, item postgres.TokenSweep) error {
	address, platform, err := w.walletsFor(ctx, item)
	if err != nil {
		return err
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	tokenBalance, err := w.contract.BalanceOf(ctx, from)
	if err != nil {
		return fmt.Errorf("查询归集来源 Token 余额失败：%w", err)
	}
	if tokenBalance == nil || tokenBalance.Sign() <= 0 {
		_, changed, failErr := w.store.FailWaitingTokenSweep(ctx, item.ID, "TOKEN_BALANCE_EMPTY", "归集来源地址链上 Token 余额为零")
		if failErr != nil {
			return failErr
		}
		if changed {
			w.recordError(ctx, item.ID, "prepare", "TOKEN_BALANCE_EMPTY", errors.New("归集来源地址链上 Token 余额为零"))
		}
		return nil
	}
	sweepAmount := minimum(tokenBalance, item.RecognizedAmountUnits)
	if sweepAmount.Sign() <= 0 {
		return errors.New("Token 归集金额无效")
	}
	gasLimit, err := w.contract.EstimateTransferGas(ctx, from, to, sweepAmount)
	if err != nil {
		return fmt.Errorf("估算 Token 归集 Gas 失败：%w", err)
	}
	quote, err := w.quoteWithGasLimit(ctx, gasLimit)
	if err != nil {
		return err
	}
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("查询归集来源 ETH 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(quote.ReservedFeeWei) < 0 {
		return errors.New("归集来源地址 ETH 余额不足以支付最大网络费，任务保持等待并将在下轮重试")
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, from)
	if err != nil {
		return fmt.Errorf("查询归集来源地址 Pending Nonce 失败：%w", err)
	}
	item, _, err = w.store.AllocateTokenSweepNonce(ctx, item.ID, sweepAmount, chainNonce, postgres.PreparedGasParams{
		GasLimit:                quote.GasLimit,
		MaxFeePerGasWei:         quote.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: quote.MaxPriorityFeePerGasWei,
	})
	if err != nil {
		return fmt.Errorf("分配 Token 归集 Nonce 失败：%w", err)
	}
	return w.sign(ctx, item)
}

// sign 在已分配用户地址 Nonce 后直接读取已落库 Gas 参数、签名并持久化，不再调用会失败的链上 RPC。
func (w *Worker) sign(ctx context.Context, item postgres.TokenSweep) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.SweepAmountUnits == nil || item.SweepAmountUnits.Sign() <= 0 {
		return w.handleSigningFailure(ctx, item.ID, errors.New("Token 归集 Nonce 或金额无效"))
	}
	if item.GasLimit == nil || *item.GasLimit <= 0 || item.MaxFeePerGasWei == nil ||
		item.MaxFeePerGasWei.Sign() <= 0 || item.MaxPriorityFeePerGasWei == nil || item.MaxPriorityFeePerGasWei.Sign() < 0 {
		return w.handleSigningFailure(ctx, item.ID, errors.New("已分配 Nonce 的 Token 归集缺少有效 Gas 参数"))
	}
	address, platform, err := w.walletsFor(ctx, item)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	gasLimit := uint64(*item.GasLimit)
	maxFee := new(big.Int).Set(item.MaxFeePerGasWei)
	maxTip := new(big.Int).Set(item.MaxPriorityFeePerGasWei)
	calldata, err := w.contract.EncodeTransfer(to, item.SweepAmountUnits)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("编码 Token 归集 calldata 失败：%w", err))
	}
	contractAddress := w.contract.Address()
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID: new(big.Int).Set(w.config.ChainID), Nonce: item.Nonce.Uint64(),
		GasTipCap: maxTip, GasFeeCap: maxFee,
		Gas: gasLimit, To: &contractAddress, Value: new(big.Int), Data: calldata,
	})
	if err := w.validateTransaction(transaction, to, item.SweepAmountUnits); err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	signed, err := w.keys.SignTx(ctx, address.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("签署 Token 归集交易失败：%w", err))
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != from {
		return w.handleSigningFailure(ctx, item.ID, errors.New("已签名 Token 归集交易发送地址与托管地址不一致"))
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("编码已签名 Token 归集交易失败：%w", err))
	}
	item, _, err = w.store.SaveSignedTokenSweep(ctx, postgres.SignedTokenSweep{
		SweepID: item.ID, GasLimit: gasLimit, MaxFeePerGasWei: maxFee,
		MaxPriorityFeePerGasWei: maxTip, RawTx: raw,
		TxHash: strings.ToLower(signed.Hash().Hex()),
	})
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("持久化已签名 Token 归集失败：%w", err))
	}
	return w.broadcast(ctx, item)
}

// handleSigningFailure 持久化累计分配 Nonce 后的连续签名失败次数，超限后转入 MANUAL_REVIEW 并写一次告警。
func (w *Worker) handleSigningFailure(ctx context.Context, id int64, err error) error {
	escalated, recErr := w.store.RecordEVMSigningFailure(
		ctx, postgres.EVMBusinessTokenSweep, id, evm.DefaultMaxSigningRetryCount,
		"token-sweep-worker", "TOKEN_SWEEP", id,
		"TOKEN_SWEEP_SIGNING_MANUAL_INTERVENTION", err.Error(),
	)
	if recErr != nil {
		w.logger.Error("记录 Token 归集签名失败计数异常", "sweep_id", id, "error", recErr)
	} else if escalated {
		w.logger.Warn("Token 归集连续签名失败超限，已转入人工核查状态", "sweep_id", id)
	}
	return err
}

// validateTransaction 解码并复核合约地址、热钱包目标、归集金额和零 ETH Value。
func (w *Worker) validateTransaction(transaction *types.Transaction, expectedTo common.Address, expectedAmount *big.Int) error {
	if transaction == nil || transaction.To() == nil || *transaction.To() != w.contract.Address() || transaction.Value().Sign() != 0 {
		return errors.New("Token 归集交易合约地址或 ETH Value 无效")
	}
	to, amountUnits, err := w.contract.DecodeTransferCalldata(transaction.Data())
	if err != nil {
		return fmt.Errorf("复核 Token 归集 calldata 失败：%w", err)
	}
	if to != expectedTo || amountUnits.Cmp(expectedAmount) != 0 {
		return errors.New("Token 归集 calldata 的热钱包目标或金额不匹配")
	}
	return nil
}

// broadcast 优先查询原交易 Receipt，并始终重播数据库中的同一份 raw_tx。
func (w *Worker) broadcast(ctx context.Context, item postgres.TokenSweep) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		return fmt.Errorf("广播前查询平台热钱包失败：%w", err)
	}
	if err := w.validateTransaction(transaction, common.HexToAddress(platform.Address), item.SweepAmountUnits); err != nil {
		return err
	}
	if receipt, err := w.broadcaster.CheckExistingReceipt(ctx, transaction.Hash()); err != nil {
		return fmt.Errorf("查询待广播 Token 归集 Receipt 失败：%w", err)
	} else if receipt != nil {
		item, err = w.store.TransitionTokenSweep(ctx, item.ID, postgres.TokenSweepBroadcasted)
		if err != nil {
			return fmt.Errorf("恢复 Token 归集已广播状态失败：%w", err)
		}
		return w.confirmReceipt(ctx, item, receipt, item.TxHash)
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		return fmt.Errorf("广播 Token 归集交易结果不明确：%w", err)
	}
	if handled {
		return nil
	}
	item, err = w.store.TransitionTokenSweep(ctx, item.ID, postgres.TokenSweepBroadcasted)
	if err != nil {
		return fmt.Errorf("更新 Token 归集已广播状态失败：%w", err)
	}
	w.logger.Info("Token 归集交易已广播", "sweep_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// confirm 查询归集全部候选交易 Receipt，并在卡单超时后执行重播或 Gas 加价替换。
func (w *Worker) confirm(ctx context.Context, item postgres.TokenSweep) error {
	attempts, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return err
	}
	receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates)
	if err != nil {
		return fmt.Errorf("查询 Token 归集 Receipt 失败：%w", err)
	}
	if receipt != nil {
		return w.confirmReceipt(ctx, item, receipt, matchedHash)
	}
	consumed, err := w.checkNonceConsumed(ctx, item, candidates)
	if err != nil {
		return err
	}
	if consumed {
		return nil
	}
	if item.Status != postgres.TokenSweepBroadcasted {
		return nil
	}
	tierStart := replacementTierStart(item.BumpCount, item.BroadcastedAt, item.LastBroadcastAt, attempts)
	if tierStart != nil && time.Since(*tierStart) >= w.config.BumpAfter && int(item.BumpCount) < w.effectiveMaxBumps() {
		return w.bumpAndBroadcast(ctx, item, attempts)
	}
	if item.LastBroadcastAt != nil && time.Since(*item.LastBroadcastAt) >= w.config.RebroadcastAfter {
		return w.rebroadcast(ctx, item)
	}
	return nil
}

// confirmReceipt 校验 Transfer Event、更新热钱包库存并保存实际 Gas 成本。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.TokenSweep, receipt *types.Receipt, matchedTxHash string) error {
	blockNumber, confirmations, err := w.confirmer.EvaluateConfirmations(ctx, receipt)
	if err != nil {
		return err
	}
	if uint64(confirmations) < w.config.Confirmations {
		_, err := w.store.UpdateTokenSweepConfirmations(ctx, item.ID, confirmations)
		return err
	}
	actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	settlement := postgres.TokenSweepSettlement{
		SweepID:       item.ID,
		ActualFeeWei:  actualFee,
		BlockNumber:   blockNumber,
		Confirmations: confirmations,
		TxHash:        matchedTxHash,
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		settlement.ErrorCode = "TOKEN_SWEEP_REVERTED"
		settlement.ErrorMessage = "Token 归集交易链上执行失败"
	} else {
		matched, matchErr := w.hasExpectedTransfer(ctx, item, receipt.Logs)
		if matchErr != nil || !matched {
			settlement.ErrorCode = "HOT_WALLET_CREDIT_MISSING"
			settlement.ErrorMessage = "Token 归集 Receipt 缺少预期热钱包到账 Event"
		} else {
			settlement.Success = true
			if err := w.refreshInventory(ctx); err != nil {
				return err
			}
		}
	}
	result, changed, err := w.store.FinalizeTokenSweep(ctx, settlement)
	if err != nil {
		return fmt.Errorf("结算 Token 归集失败：%w", err)
	}
	if changed {
		w.logger.Info("Token 归集交易已完成", "sweep_id", result.ID, "status", result.Status, "confirmations", confirmations, "tx_hash", result.TxHash)
	}
	if !settlement.Success {
		w.recordError(ctx, item.ID, "receipt", settlement.ErrorCode, errors.New(settlement.ErrorMessage))
	}
	return nil
}

// hasExpectedTransfer 校验 Receipt 中存在从用户地址到平台热钱包的精确 Transfer Event。
func (w *Worker) hasExpectedTransfer(ctx context.Context, item postgres.TokenSweep, logs []*types.Log) (bool, error) {
	address, platform, err := w.walletsFor(ctx, item)
	if err != nil {
		return false, err
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	for _, log := range logs {
		if log == nil || log.Address != w.contract.Address() {
			continue
		}
		event, err := w.contract.DecodeTransferLog(*log)
		if err != nil {
			if errors.Is(err, erc20.ErrInvalidTransferLog) {
				continue
			}
			return false, fmt.Errorf("解析归集 Transfer Event 失败：%w", err)
		}
		if event.From == from && event.To == to && event.AmountUnits.Cmp(item.SweepAmountUnits) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// walletsFor 查询并校验归集来源用户地址和平台热钱包。
func (w *Worker) walletsFor(ctx context.Context, item postgres.TokenSweep) (postgres.WalletAddress, postgres.PlatformWallet, error) {
	address, err := w.store.WalletAddressByUser(ctx, item.UserID)
	if err != nil {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, fmt.Errorf("查询归集来源地址失败：%w", err)
	}
	if address.ID != item.AddressID {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, errors.New("Token 归集与用户充值地址不匹配")
	}
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, fmt.Errorf("查询平台热钱包失败：%w", err)
	}
	return address, platform, nil
}

// quoteWithGasLimit 根据最新 Base Fee、建议 Tip 和 Gas Limit 计算最大网络费。
func (w *Worker) quoteWithGasLimit(ctx context.Context, gasLimit uint64) (evm.EIP1559FeeQuote, error) {
	return w.fees.QuoteWithGasLimit(ctx, gasLimit)
}

// refreshInventory 查询平台热钱包 Token 链上库存并更新健康快照。
func (w *Worker) refreshInventory(ctx context.Context) error {
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		w.updateHealth(postgres.PlatformWallet{}, nil, err)
		return fmt.Errorf("查询 Token 库存热钱包失败：%w", err)
	}
	balance, err := w.contract.BalanceOf(ctx, common.HexToAddress(platform.Address))
	w.updateHealth(platform, balance, err)
	if err != nil {
		return fmt.Errorf("查询热钱包 Token 链上库存失败：%w", err)
	}
	return nil
}

// updateHealth 原子保存 Token 库存健康快照。
func (w *Worker) updateHealth(platform postgres.PlatformWallet, balance *big.Int, healthErr error) {
	snapshot := HealthSnapshot{
		Status: "HEALTHY", Symbol: w.config.Symbol, Address: platform.Address,
		BalanceUnits: valueOrZero(balance), CheckedAt: time.Now(),
	}
	if healthErr != nil {
		snapshot.Status = "DOWN"
		snapshot.LastError = healthErr.Error()
	}
	w.healthMu.Lock()
	w.health = snapshot
	w.healthMu.Unlock()
}

// Snapshot 返回热钱包 Token 库存最近快照的深拷贝。
func (w *Worker) Snapshot() HealthSnapshot {
	w.healthMu.RLock()
	defer w.healthMu.RUnlock()
	result := w.health
	result.BalanceUnits = valueOrZero(w.health.BalanceUnits)
	return result
}

// recordError 保存经过清洗的 Token Sweep Worker 错误。
func (w *Worker) recordError(ctx context.Context, sweepID int64, stage, code string, workerErr error) {
	referenceID := sweepID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "token-sweep-worker", Stage: stage, ReferenceType: "TOKEN_SWEEP",
		ReferenceID: &referenceID, ErrorCode: code, ErrorMessage: workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录 Token Sweep Worker 错误失败", "sweep_id", sweepID, "error", err)
	}
}

// minimum 返回两个非负大整数中较小值的深拷贝。
func minimum(left, right *big.Int) *big.Int {
	if left == nil || right == nil {
		return new(big.Int)
	}
	if left.Cmp(right) <= 0 {
		return new(big.Int).Set(left)
	}
	return new(big.Int).Set(right)
}

// valueOrZero 返回大整数深拷贝，空值转换为零。
func valueOrZero(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(value)
}

// normalizeReplacementConfig 校验并填充 Token 归集重播与 Gas 加价配置默认值。
func normalizeReplacementConfig(cfg *Config) error {
	settings := evm.ReplacementSettings{
		RebroadcastAfter:             cfg.RebroadcastAfter,
		BumpAfter:                    cfg.BumpAfter,
		BumpBlockedManualReviewAfter: cfg.BumpBlockedManualReviewAfter,
		BumpPercent:                  cfg.BumpPercent,
		MaxBumps:                     cfg.MaxBumps,
		MaxFeeCapWei:                 cfg.MaxFeeCapWei,
	}
	if err := evm.NormalizeReplacementSettings(&settings); err != nil {
		return err
	}
	cfg.RebroadcastAfter = settings.RebroadcastAfter
	cfg.BumpAfter = settings.BumpAfter
	cfg.BumpBlockedManualReviewAfter = settings.BumpBlockedManualReviewAfter
	cfg.BumpPercent = settings.BumpPercent
	cfg.MaxBumps = settings.MaxBumps
	cfg.MaxFeeCapWei = settings.MaxFeeCapWei
	return nil
}

// effectiveMaxBumps 根据补气安全余量覆盖能力限制归集允许的最大加价次数（H2）。
func (w *Worker) effectiveMaxBumps() int {
	if w.config.GasSafetyBPS == 0 {
		return w.config.MaxBumps
	}
	covered := evm.MaxCoveredSweepBumps(w.config.GasSafetyBPS, w.config.BumpPercent)
	if covered < w.config.MaxBumps {
		return covered
	}
	return w.config.MaxBumps
}

// candidateHashes 查询同一笔 Token 归集按尝试序号倒序排列的所有候选交易哈希。
func (w *Worker) candidateHashes(ctx context.Context, item postgres.TokenSweep) ([]postgres.EVMTxAttempt, []string, error) {
	attempts, err := w.store.ListEVMTxAttempts(ctx, postgres.EVMBusinessTokenSweep, item.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("查询 Token 归集候选交易记录失败：%w", err)
	}
	candidates, err := evm.CollectCandidateHashes(attempts, func(a postgres.EVMTxAttempt) string { return a.TxHash }, item.TxHash)
	if err != nil {
		return nil, nil, errors.New("待确认 Token 归集缺少交易哈希")
	}
	return attempts, candidates, nil
}

// checkNonceConsumed 在所有候选交易均无 Receipt 时校验用户地址已确认 Nonce 是否已越过本单 Nonce。
func (w *Worker) checkNonceConsumed(ctx context.Context, item postgres.TokenSweep, candidates []string) (bool, error) {
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return false, nil
	}
	address, _, err := w.walletsFor(ctx, item)
	if err != nil {
		return false, err
	}
	confirmedNonce, err := w.chain.NonceAt(ctx, common.HexToAddress(address.Address), nil)
	if err != nil {
		return false, fmt.Errorf("查询 Token 归集地址已确认 Nonce 失败：%w", err)
	}
	if confirmedNonce <= item.Nonce.Uint64() {
		return false, nil
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("二次复核 Token 归集 Receipt 失败：%w", err)
	} else if receipt != nil {
		return true, w.confirmReceipt(ctx, item, receipt, matchedHash)
	}
	reason := fmt.Sprintf("归集来源地址最新确认 Nonce(%d) 已超过归集 Nonce(%d)，但所有候选交易均未上链，需人工核查", confirmedNonce, item.Nonce.Uint64())
	_, changed, err := w.store.MarkTokenSweepManualReview(ctx, item.ID, "EVM_TX_MANUAL_REVIEW", reason)
	if err != nil {
		return false, fmt.Errorf("转入 Token 归集人工核查状态失败：%w", err)
	}
	if changed {
		w.recordError(ctx, item.ID, "confirm", "EVM_TX_MANUAL_REVIEW", errors.New(reason))
		w.logger.Warn("Token 归集转入人工核查状态", "sweep_id", item.ID, "nonce", item.Nonce.String(), "confirmed_nonce", confirmedNonce)
	}
	return true, nil
}

// handleNonceTooLow 在广播遇到 nonce too low 时优先复核所有候选 Receipt 与已确认 Nonce。
func (w *Worker) handleNonceTooLow(ctx context.Context, item postgres.TokenSweep) (bool, error) {
	_, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return false, err
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("复核 Token 归集 Receipt 失败：%w", err)
	} else if receipt != nil {
		ensured, transitionErr := w.store.TransitionTokenSweep(ctx, item.ID, postgres.TokenSweepBroadcasted)
		if transitionErr != nil {
			return false, fmt.Errorf("恢复 Token 归集已广播状态失败：%w", transitionErr)
		}
		return true, w.confirmReceipt(ctx, ensured, receipt, matchedHash)
	}
	return w.checkNonceConsumed(ctx, item, candidates)
}

// bumpAndBroadcast 按同一 Nonce 提升 Token 归集 Gas 费用重新签名、落库并广播替换交易。
func (w *Worker) bumpAndBroadcast(ctx context.Context, item postgres.TokenSweep, attempts []postgres.EVMTxAttempt) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.SweepAmountUnits == nil || item.SweepAmountUnits.Sign() <= 0 {
		return errors.New("待加价 Token 归集缺少有效 Nonce 或归集金额")
	}
	prevFeeCap := item.MaxFeePerGasWei
	prevTip := item.MaxPriorityFeePerGasWei
	if (prevFeeCap == nil || prevTip == nil) && len(attempts) > 0 {
		prevFeeCap = attempts[0].MaxFeePerGasWei
		prevTip = attempts[0].MaxPriorityFeePerGasWei
	}
	if prevFeeCap == nil || prevFeeCap.Sign() <= 0 || prevTip == nil || prevTip.Sign() < 0 {
		return errors.New("待加价 Token 归集缺少有效历史 Gas 参数")
	}
	address, platform, err := w.walletsFor(ctx, item)
	if err != nil {
		return err
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	gasLimit, err := w.contract.EstimateTransferGas(ctx, from, to, item.SweepAmountUnits)
	if err != nil {
		return fmt.Errorf("Token 归集加价估算 Gas 失败：%w", err)
	}
	if item.GasLimit != nil && *item.GasLimit > 0 && uint64(*item.GasLimit) > gasLimit {
		gasLimit = uint64(*item.GasLimit)
	}
	quote, err := w.quoteWithGasLimit(ctx, gasLimit)
	if err != nil {
		return err
	}
	baseFee, err := w.fees.LatestBaseFee(ctx)
	if err != nil {
		return err
	}
	bumpedFeeCap, bumpedTip, ok := evm.ComputeBumpedFees(prevFeeCap, prevTip, baseFee, quote.MaxPriorityFeePerGasWei, w.config.BumpPercent, w.config.MaxFeeCapWei)
	if !ok {
		return w.handleBumpBlocked(ctx, item, "EVM_BUMP_FEE_CAP_EXCEEDED", "Token 归集加价后手续费上限超过配置最大值，停止继续加价")
	}
	requiredFee := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), bumpedFeeCap)
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("Token 归集加价查询来源 ETH 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(requiredFee) < 0 {
		return w.handleBumpBlocked(ctx, item, "CHAIN_BALANCE_INSUFFICIENT", "归集来源地址 ETH 余额不足以支持归集加价替换")
	}
	calldata, err := w.contract.EncodeTransfer(to, item.SweepAmountUnits)
	if err != nil {
		return fmt.Errorf("编码 Token 归集加价 calldata 失败：%w", err)
	}
	contractAddress := w.contract.Address()
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(w.config.ChainID),
		Nonce:     item.Nonce.Uint64(),
		GasTipCap: bumpedTip,
		GasFeeCap: bumpedFeeCap,
		Gas:       gasLimit,
		To:        &contractAddress,
		Value:     new(big.Int),
		Data:      calldata,
	})
	if err := w.validateTransaction(transaction, to, item.SweepAmountUnits); err != nil {
		return err
	}
	signed, err := w.keys.SignTx(ctx, address.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署 Token 归集加价替换交易失败：%w", err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != from {
		return errors.New("Token 归集加价替换签名地址与托管地址不一致")
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码 Token 归集加价替换交易失败：%w", err)
	}
	replaced, changed, err := w.store.ReplaceTokenSweepTx(ctx, postgres.SignedTokenSweep{
		SweepID:                 item.ID,
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         bumpedFeeCap,
		MaxPriorityFeePerGasWei: bumpedTip,
		RawTx:                   raw,
		TxHash:                  strings.ToLower(signed.Hash().Hex()),
	})
	if err != nil {
		return fmt.Errorf("保存 Token 归集加价替换交易失败：%w", err)
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, signed, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, replaced)
	})
	if err != nil {
		return fmt.Errorf("广播 Token 归集加价替换交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if changed {
		w.logger.Info("Token 归集已完成 Gas 加价替换并广播", "sweep_id", replaced.ID, "bump_count", replaced.BumpCount, "tx_hash", replaced.TxHash)
	}
	return nil
}

// rebroadcast 使用相同 raw_tx 原样重播卡在内存池的 Token 归集交易。
func (w *Worker) rebroadcast(ctx context.Context, item postgres.TokenSweep) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		return fmt.Errorf("重播归集前查询平台热钱包失败：%w", err)
	}
	if err := w.validateTransaction(transaction, common.HexToAddress(platform.Address), item.SweepAmountUnits); err != nil {
		return err
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		return fmt.Errorf("重播 Token 归集交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if _, err := w.store.TouchTokenSweepBroadcast(ctx, item.ID); err != nil {
		return fmt.Errorf("更新 Token 归集重播时间失败：%w", err)
	}
	w.logger.Info("Token 归集交易已重播", "sweep_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// replacementTierStart 返回当前费率阶梯的起始时间，用于判断是否满足加价等待阈值。
func replacementTierStart(bumpCount int32, broadcastedAt, lastBroadcastAt *time.Time, attempts []postgres.EVMTxAttempt) *time.Time {
	var latestAttemptAt *time.Time
	if len(attempts) > 0 {
		latestAttemptAt = &attempts[0].CreatedAt
	}
	return evm.ReplacementTierStart(bumpCount, broadcastedAt, lastBroadcastAt, latestAttemptAt)
}

// handleBumpBlocked 统一持久化 Token 归集加价阻塞原因、按间隔重播原交易并在超时后转人工核查。
func (w *Worker) handleBumpBlocked(ctx context.Context, item postgres.TokenSweep, code, message string) error {
	return evm.HandleBumpBlocked(ctx, evm.BumpBlockedAction{
		RecordBlocked: func(c context.Context, cCode, cMsg string) (time.Time, error) {
			at, _, err := w.store.RecordEVMBumpBlocked(c, postgres.EVMBusinessTokenSweep, item.ID, "token-sweep-worker", "TOKEN_SWEEP", item.ID, cCode, cMsg)
			return at, err
		},
		MarkManualReview: func(c context.Context, cCode, cMsg string) error {
			_, changed, err := w.store.MarkTokenSweepManualReview(c, item.ID, cCode, cMsg)
			if err != nil {
				return err
			}
			if changed {
				w.recordError(c, item.ID, "bump", cCode, errors.New(cMsg))
				w.logger.Warn("Token 归集加价长期受阻，已转入人工核查", "sweep_id", item.ID, "reason", code)
			}
			return nil
		},
		Rebroadcast:       func(c context.Context) error { return w.rebroadcast(c, item) },
		LastBroadcastAt:   item.LastBroadcastAt,
		RebroadcastAfter:  w.config.RebroadcastAfter,
		ManualReviewAfter: w.config.BumpBlockedManualReviewAfter,
		ReasonCode:        code,
		ReasonMessage:     message,
	})
}
