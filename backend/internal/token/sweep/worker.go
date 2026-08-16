package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = 100

// Chain 定义 Token 归集费用、Nonce、广播和确认所需的 EVM 能力。
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
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
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	AllocateTokenSweepNonce(ctx context.Context, sweepID int64, sweepAmountUnits *big.Int, chainPendingNonce uint64) (postgres.TokenSweep, bool, error)
	SaveSignedTokenSweep(ctx context.Context, signed postgres.SignedTokenSweep) (postgres.TokenSweep, bool, error)
	TransitionTokenSweep(ctx context.Context, sweepID int64, target string) (postgres.TokenSweep, error)
	UpdateTokenSweepConfirmations(ctx context.Context, sweepID, confirmations int64) (postgres.TokenSweep, error)
	FinalizeTokenSweep(ctx context.Context, settlement postgres.TokenSweepSettlement) (postgres.TokenSweep, bool, error)
	FailWaitingTokenSweep(ctx context.Context, sweepID int64, errorCode, errorMessage string) (postgres.TokenSweep, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// Config 定义 Token 归集轮询、确认数、Chain ID 和展示资产信息。
type Config struct {
	Interval      time.Duration
	Confirmations uint64
	ChainID       *big.Int
	Symbol        string
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

// feeQuote 保存归集交易的 EIP-1559 费用上限。
type feeQuote struct {
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	ReservedFeeWei          *big.Int
}

// Worker 负责用户地址 Token 自动归集、幂等广播和库存快照。
type Worker struct {
	chain    Chain
	contract TokenContract
	store    Store
	keys     wallet.KeyProvider
	logger   *slog.Logger
	config   Config
	healthMu sync.RWMutex
	health   HealthSnapshot
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
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{
		chain: chain, contract: contract, store: store, keys: keys, logger: logger, config: cfg,
		health: HealthSnapshot{Status: "CHECKING", Symbol: cfg.Symbol, BalanceUnits: new(big.Int)},
	}, nil
}

// Run 周期推进全部可恢复 Token 归集任务。
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("Token Sweep Worker 本轮存在失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 刷新热钱包库存并逐条推进归集，单条失败不阻塞其他任务。
func (w *Worker) RunOnce(ctx context.Context) error {
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
	item, _, err = w.store.AllocateTokenSweepNonce(ctx, item.ID, sweepAmount, chainNonce)
	if err != nil {
		return fmt.Errorf("分配 Token 归集 Nonce 失败：%w", err)
	}
	return w.sign(ctx, item)
}

// sign 复核余额和 calldata 后签署归集交易，并在广播前持久化原始交易。
func (w *Worker) sign(ctx context.Context, item postgres.TokenSweep) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.SweepAmountUnits == nil || item.SweepAmountUnits.Sign() <= 0 {
		return errors.New("Token 归集 Nonce 或金额无效")
	}
	address, platform, err := w.walletsFor(ctx, item)
	if err != nil {
		return err
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	tokenBalance, err := w.contract.BalanceOf(ctx, from)
	if err != nil {
		return fmt.Errorf("签名前查询归集来源 Token 余额失败：%w", err)
	}
	if tokenBalance == nil || tokenBalance.Cmp(item.SweepAmountUnits) < 0 {
		return errors.New("签名前归集来源 Token 余额不足")
	}
	gasLimit, err := w.contract.EstimateTransferGas(ctx, from, to, item.SweepAmountUnits)
	if err != nil {
		return fmt.Errorf("签名前估算 Token 归集 Gas 失败：%w", err)
	}
	quote, err := w.quoteWithGasLimit(ctx, gasLimit)
	if err != nil {
		return err
	}
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("签名前查询归集来源 ETH 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(quote.ReservedFeeWei) < 0 {
		return errors.New("签名前归集来源 ETH 余额不足")
	}
	calldata, err := w.contract.EncodeTransfer(to, item.SweepAmountUnits)
	if err != nil {
		return fmt.Errorf("编码 Token 归集 calldata 失败：%w", err)
	}
	contractAddress := w.contract.Address()
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID: new(big.Int).Set(w.config.ChainID), Nonce: item.Nonce.Uint64(),
		GasTipCap: quote.MaxPriorityFeePerGasWei, GasFeeCap: quote.MaxFeePerGasWei,
		Gas: quote.GasLimit, To: &contractAddress, Value: new(big.Int), Data: calldata,
	})
	if err := w.validateTransaction(transaction, to, item.SweepAmountUnits); err != nil {
		return err
	}
	signed, err := w.keys.SignTx(ctx, address.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署 Token 归集交易失败：%w", err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != from {
		return errors.New("已签名 Token 归集交易发送地址与托管地址不一致")
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码已签名 Token 归集交易失败：%w", err)
	}
	item, _, err = w.store.SaveSignedTokenSweep(ctx, postgres.SignedTokenSweep{
		SweepID: item.ID, GasLimit: quote.GasLimit, MaxFeePerGasWei: quote.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: quote.MaxPriorityFeePerGasWei, RawTx: raw,
		TxHash: strings.ToLower(signed.Hash().Hex()),
	})
	if err != nil {
		return fmt.Errorf("持久化已签名 Token 归集失败：%w", err)
	}
	return w.broadcast(ctx, item)
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
	if len(item.RawTx) == 0 || item.TxHash == "" {
		return errors.New("待广播 Token 归集缺少已签名原始交易")
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(item.RawTx); err != nil {
		return errors.New("数据库中的 Token 归集原始交易无效")
	}
	if transaction.Hash() != common.HexToHash(item.TxHash) {
		return errors.New("Token 归集交易哈希与原始交易不一致")
	}
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return fmt.Errorf("广播前查询平台热钱包失败：%w", err)
	}
	if err := w.validateTransaction(&transaction, common.HexToAddress(platform.Address), item.SweepAmountUnits); err != nil {
		return err
	}
	if receipt, err := w.chain.TransactionReceipt(ctx, transaction.Hash()); err == nil && receipt != nil {
		item, err = w.store.TransitionTokenSweep(ctx, item.ID, postgres.TokenSweepBroadcasted)
		if err != nil {
			return fmt.Errorf("恢复 Token 归集已广播状态失败：%w", err)
		}
		return w.confirmReceipt(ctx, item, receipt)
	} else if err != nil && !errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("查询待广播 Token 归集 Receipt 失败：%w", err)
	}
	if err := w.chain.SendTransaction(ctx, &transaction); err != nil && !alreadyKnown(err) {
		return fmt.Errorf("广播 Token 归集交易结果不明确：%w", err)
	}
	item, err = w.store.TransitionTokenSweep(ctx, item.ID, postgres.TokenSweepBroadcasted)
	if err != nil {
		return fmt.Errorf("更新 Token 归集已广播状态失败：%w", err)
	}
	w.logger.Info("Token 归集交易已广播", "sweep_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// confirm 查询归集 Receipt 并更新确认数或完成结算。
func (w *Worker) confirm(ctx context.Context, item postgres.TokenSweep) error {
	if item.TxHash == "" {
		return errors.New("待确认 Token 归集缺少交易哈希")
	}
	receipt, err := w.chain.TransactionReceipt(ctx, common.HexToHash(item.TxHash))
	if errors.Is(err, ethereum.NotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询 Token 归集 Receipt 失败：%w", err)
	}
	return w.confirmReceipt(ctx, item, receipt)
}

// confirmReceipt 校验 Transfer Event、更新热钱包库存并保存实际 Gas 成本。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.TokenSweep, receipt *types.Receipt) error {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.EffectiveGasPrice == nil {
		return errors.New("Token 归集 Receipt 内容无效")
	}
	latest, err := w.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Token 归集确认高度失败：%w", err)
	}
	blockNumber := receipt.BlockNumber.Uint64()
	if blockNumber > latest || blockNumber > math.MaxInt64 {
		return errors.New("Token 归集 Receipt 区块高度无效")
	}
	confirmations := latest - blockNumber + 1
	if confirmations > math.MaxInt64 {
		return errors.New("Token 归集确认数超出数据库范围")
	}
	if confirmations < w.config.Confirmations {
		_, err := w.store.UpdateTokenSweepConfirmations(ctx, item.ID, int64(confirmations))
		return err
	}
	actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	settlement := postgres.TokenSweepSettlement{
		SweepID: item.ID, ActualFeeWei: actualFee, BlockNumber: int64(blockNumber), Confirmations: int64(confirmations),
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
		w.logger.Info("Token 归集交易已完成", "sweep_id", result.ID, "status", result.Status, "confirmations", confirmations)
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
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, fmt.Errorf("查询平台热钱包失败：%w", err)
	}
	return address, platform, nil
}

// quoteWithGasLimit 根据最新 Base Fee、建议 Tip 和 Gas Limit 计算最大网络费。
func (w *Worker) quoteWithGasLimit(ctx context.Context, gasLimit uint64) (feeQuote, error) {
	if gasLimit == 0 {
		return feeQuote{}, errors.New("RPC 返回的 Token 归集 Gas Limit 无效")
	}
	header, err := w.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return feeQuote{}, fmt.Errorf("查询 Token 归集最新区块费用失败：%w", err)
	}
	if header == nil || header.BaseFee == nil || header.BaseFee.Sign() < 0 {
		return feeQuote{}, errors.New("Token 归集最新区块缺少有效 Base Fee")
	}
	tip, err := w.chain.SuggestGasTipCap(ctx)
	if err != nil {
		return feeQuote{}, fmt.Errorf("查询 Token 归集建议优先费失败：%w", err)
	}
	if tip == nil || tip.Sign() < 0 {
		return feeQuote{}, errors.New("RPC 返回的 Token 归集建议优先费无效")
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tip)
	reserved := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), maxFee)
	return feeQuote{
		GasLimit: gasLimit, MaxFeePerGasWei: maxFee,
		MaxPriorityFeePerGasWei: new(big.Int).Set(tip), ReservedFeeWei: reserved,
	}, nil
}

// refreshInventory 查询平台热钱包 Token 链上库存并更新健康快照。
func (w *Worker) refreshInventory(ctx context.Context) error {
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
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

// alreadyKnown 判断 RPC 错误链是否表示节点已经接收相同交易。
func alreadyKnown(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		if strings.Contains(message, "already known") || strings.Contains(message, "known transaction") {
			return true
		}
	}
	return false
}
