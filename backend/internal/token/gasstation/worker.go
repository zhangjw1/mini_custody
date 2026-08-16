package gasstation

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
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = 100

var (
	ErrTopupLimitExceeded = errors.New("Gas 补充金额超过单次安全上限")
	ErrPlatformBalanceLow = errors.New("平台热钱包 ETH 余额低于安全阈值")
)

// Chain 定义 Gas 补充估算、广播和确认所需的 EVM 能力。
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, transaction *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// TokenContract 定义归集前估算 ERC-20 transfer Gas 所需的合约能力。
type TokenContract interface {
	EstimateTransferGas(ctx context.Context, from, to common.Address, amountUnits *big.Int) (uint64, error)
}

// Store 定义 Gas Station 的持久化状态机和恢复查询。
type Store interface {
	TokenSweepByID(ctx context.Context, id int64) (postgres.TokenSweep, error)
	ListGasStationSweeps(ctx context.Context, limit int) ([]postgres.TokenSweep, error)
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	InternalTransferByID(ctx context.Context, id int64) (postgres.InternalTransfer, error)
	MarkTokenSweepGasReady(ctx context.Context, sweepID int64) (postgres.TokenSweep, bool, error)
	CreateOrGetGasTopup(ctx context.Context, request postgres.GasTopupRequest) (postgres.InternalTransfer, bool, error)
	SaveSignedInternalTransfer(ctx context.Context, signed postgres.SignedInternalTransfer) (postgres.InternalTransfer, bool, error)
	TransitionInternalTransfer(ctx context.Context, transferID int64, target string) (postgres.InternalTransfer, error)
	UpdateInternalTransferConfirmations(ctx context.Context, transferID, confirmations int64) (postgres.InternalTransfer, error)
	FinalizeInternalTransfer(ctx context.Context, settlement postgres.InternalTransferSettlement) (postgres.InternalTransfer, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// Config 定义 Gas Station 的轮询、确认和风险控制参数。
type Config struct {
	Interval              time.Duration
	Confirmations         uint64
	ChainID               *big.Int
	GasSafetyBPS          uint64
	GasTopupMaxWei        *big.Int
	PlatformMinBalanceWei *big.Int
}

// HealthSnapshot 描述平台 Gas 钱包的余额和告警状态。
type HealthSnapshot struct {
	Status        string
	Address       string
	BalanceWei    *big.Int
	MinBalanceWei *big.Int
	LastError     string
	CheckedAt     time.Time
}

// feeQuote 保存一笔 EIP-1559 交易的费用上限。
type feeQuote struct {
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	ReservedFeeWei          *big.Int
}

// Worker 负责最小必要 Gas 计算、内部 ETH 转账和崩溃恢复。
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

// NewWorker 创建并校验 Gas Station Worker。
func NewWorker(chain Chain, contract TokenContract, store Store, keys wallet.KeyProvider, logger *slog.Logger, cfg Config) (*Worker, error) {
	if chain == nil || contract == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("Gas Station Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil ||
		cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 || cfg.GasSafetyBPS == 0 || cfg.GasSafetyBPS > 10_000 ||
		cfg.GasTopupMaxWei == nil || cfg.GasTopupMaxWei.Sign() <= 0 ||
		cfg.PlatformMinBalanceWei == nil || cfg.PlatformMinBalanceWei.Sign() <= 0 {
		return nil, errors.New("Gas Station Worker 配置无效")
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	cfg.GasTopupMaxWei = new(big.Int).Set(cfg.GasTopupMaxWei)
	cfg.PlatformMinBalanceWei = new(big.Int).Set(cfg.PlatformMinBalanceWei)
	return &Worker{
		chain: chain, contract: contract, store: store, keys: keys, logger: logger, config: cfg,
		health: HealthSnapshot{Status: "CHECKING", BalanceWei: new(big.Int), MinBalanceWei: new(big.Int).Set(cfg.PlatformMinBalanceWei)},
	}, nil
}

// Run 周期推进全部可恢复 Gas 补充任务。
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("Gas Station Worker 本轮存在失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 刷新平台余额并逐条推进 Gas 补充任务，单条失败不阻塞其他任务。
func (w *Worker) RunOnce(ctx context.Context) error {
	var firstErr error
	if err := w.refreshHealth(ctx); err != nil {
		firstErr = err
	}
	items, err := w.store.ListGasStationSweeps(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询 Gas Station 任务失败：%w", err)
	}
	for _, item := range items {
		if err := w.Process(ctx, item.ID); err != nil {
			w.recordError(ctx, item.ID, "process", "GAS_TOPUP_PROCESS_FAILED", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据归集和内部转账的持久化状态恢复一笔 Gas 补充。
func (w *Worker) Process(ctx context.Context, sweepID int64) error {
	sweep, err := w.store.TokenSweepByID(ctx, sweepID)
	if err != nil {
		return fmt.Errorf("查询归集任务 %d 失败：%w", sweepID, err)
	}
	if sweep.Status == postgres.TokenSweepCreated {
		return w.prepare(ctx, sweep)
	}
	if sweep.Status != postgres.TokenSweepWaitingGas || sweep.GasTopupTransferID == nil {
		return nil
	}
	transfer, err := w.store.InternalTransferByID(ctx, *sweep.GasTopupTransferID)
	if err != nil {
		return fmt.Errorf("查询 Gas 补充内部转账失败：%w", err)
	}
	return w.processTransfer(ctx, transfer)
}

// prepare 估算归集 Gas、计算最小补充金额并原子分配平台 Nonce。
func (w *Worker) prepare(ctx context.Context, sweep postgres.TokenSweep) error {
	address, platform, err := w.walletsFor(ctx, sweep)
	if err != nil {
		return err
	}
	from := common.HexToAddress(address.Address)
	to := common.HexToAddress(platform.Address)
	gasLimit, err := w.contract.EstimateTransferGas(ctx, from, to, sweep.RecognizedAmountUnits)
	if err != nil {
		return fmt.Errorf("估算归集 Token Gas 失败：%w", err)
	}
	quote, err := w.quoteWithGasLimit(ctx, gasLimit)
	if err != nil {
		return err
	}
	userBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("查询用户充值地址 ETH 余额失败：%w", err)
	}
	if userBalance == nil || userBalance.Sign() < 0 {
		return errors.New("用户充值地址 ETH 余额无效")
	}
	targetBalance := addSafetyMargin(quote.ReservedFeeWei, w.config.GasSafetyBPS)
	topup := new(big.Int).Sub(targetBalance, userBalance)
	if topup.Sign() <= 0 {
		_, changed, err := w.store.MarkTokenSweepGasReady(ctx, sweep.ID)
		if err != nil {
			return fmt.Errorf("标记归集 Gas 已就绪失败：%w", err)
		}
		if changed {
			w.logger.Info("归集地址 Gas 已充足，无需平台补气", "sweep_id", sweep.ID, "address", address.Address)
		}
		return nil
	}
	if topup.Cmp(w.config.GasTopupMaxWei) > 0 {
		return fmt.Errorf("%w：需要 %s Wei，上限 %s Wei", ErrTopupLimitExceeded, topup.String(), w.config.GasTopupMaxWei.String())
	}
	platformBalance, err := w.chain.BalanceAt(ctx, to, nil)
	if err != nil {
		return fmt.Errorf("查询平台热钱包 ETH 余额失败：%w", err)
	}
	w.updateHealth(platform, platformBalance, err)
	if platformBalance == nil || platformBalance.Cmp(w.config.PlatformMinBalanceWei) < 0 {
		return ErrPlatformBalanceLow
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, to)
	if err != nil {
		return fmt.Errorf("查询平台热钱包 Pending Nonce 失败：%w", err)
	}
	transfer, _, err := w.store.CreateOrGetGasTopup(ctx, postgres.GasTopupRequest{
		SweepID: sweep.ID, PlatformWalletID: platform.ID, FromAddress: platform.Address,
		ToAddress: address.Address, AmountWei: topup, ChainPendingNonce: chainNonce,
	})
	if err != nil {
		return fmt.Errorf("创建 Gas 补充内部转账失败：%w", err)
	}
	return w.processTransfer(ctx, transfer)
}

// processTransfer 根据内部转账状态继续签名、广播或确认。
func (w *Worker) processTransfer(ctx context.Context, transfer postgres.InternalTransfer) error {
	switch transfer.Status {
	case postgres.InternalTransferSigning:
		return w.sign(ctx, transfer)
	case postgres.InternalTransferSigned:
		return w.broadcast(ctx, transfer)
	case postgres.InternalTransferSent, postgres.InternalTransferChecking:
		return w.confirm(ctx, transfer)
	case postgres.InternalTransferDone, postgres.InternalTransferFailed:
		return nil
	default:
		return errors.New("Gas 补充内部转账状态无效")
	}
}

// sign 构建 EIP-1559 ETH 转账，并在广播前保存相同 raw_tx 和 tx_hash。
func (w *Worker) sign(ctx context.Context, transfer postgres.InternalTransfer) error {
	if transfer.Nonce == nil || !transfer.Nonce.IsUint64() {
		return errors.New("Gas 补充 Nonce 无效")
	}
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return fmt.Errorf("查询平台热钱包失败：%w", err)
	}
	if platform.ID != transfer.PlatformWalletID || platform.Address != transfer.FromAddress {
		return postgres.ErrWalletKeyMismatch
	}
	from := common.HexToAddress(transfer.FromAddress)
	to := common.HexToAddress(transfer.ToAddress)
	quote, err := w.quoteTransfer(ctx, from, to, transfer.AmountWei)
	if err != nil {
		return err
	}
	balance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("签名前查询平台热钱包 ETH 余额失败：%w", err)
	}
	required := new(big.Int).Add(transfer.AmountWei, quote.ReservedFeeWei)
	if balance == nil || balance.Cmp(required) < 0 {
		return errors.New("平台热钱包 ETH 余额不足以支付补气金额和最大网络费")
	}
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID: new(big.Int).Set(w.config.ChainID), Nonce: transfer.Nonce.Uint64(),
		GasTipCap: quote.MaxPriorityFeePerGasWei, GasFeeCap: quote.MaxFeePerGasWei,
		Gas: quote.GasLimit, To: &to, Value: new(big.Int).Set(transfer.AmountWei),
	})
	signed, err := w.keys.SignTx(ctx, platform.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署 Gas 补充交易失败：%w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码已签名 Gas 补充交易失败：%w", err)
	}
	transfer, _, err = w.store.SaveSignedInternalTransfer(ctx, postgres.SignedInternalTransfer{
		TransferID: transfer.ID, GasLimit: quote.GasLimit, MaxFeePerGasWei: quote.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: quote.MaxPriorityFeePerGasWei, RawTx: raw,
		TxHash: strings.ToLower(signed.Hash().Hex()),
	})
	if err != nil {
		return fmt.Errorf("持久化已签名 Gas 补充交易失败：%w", err)
	}
	return w.broadcast(ctx, transfer)
}

// broadcast 优先查询原交易 Receipt，并始终重播数据库中的同一份 raw_tx。
func (w *Worker) broadcast(ctx context.Context, transfer postgres.InternalTransfer) error {
	if len(transfer.RawTx) == 0 || transfer.TxHash == "" {
		return errors.New("待广播 Gas 补充缺少已签名原始交易")
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(transfer.RawTx); err != nil {
		return errors.New("数据库中的 Gas 补充原始交易无效")
	}
	if transaction.Hash() != common.HexToHash(transfer.TxHash) {
		return errors.New("Gas 补充交易哈希与原始交易不一致")
	}
	if receipt, err := w.chain.TransactionReceipt(ctx, transaction.Hash()); err == nil && receipt != nil {
		transfer, err = w.store.TransitionInternalTransfer(ctx, transfer.ID, postgres.InternalTransferSent)
		if err != nil {
			return fmt.Errorf("恢复 Gas 补充已广播状态失败：%w", err)
		}
		return w.confirmReceipt(ctx, transfer, receipt)
	} else if err != nil && !errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("查询待广播 Gas 补充 Receipt 失败：%w", err)
	}
	if err := w.chain.SendTransaction(ctx, &transaction); err != nil && !alreadyKnown(err) {
		return fmt.Errorf("广播 Gas 补充交易结果不明确：%w", err)
	}
	transfer, err := w.store.TransitionInternalTransfer(ctx, transfer.ID, postgres.InternalTransferSent)
	if err != nil {
		return fmt.Errorf("更新 Gas 补充已广播状态失败：%w", err)
	}
	w.logger.Info("Gas 补充交易已广播", "transfer_id", transfer.ID, "sweep_id", transfer.SweepID, "tx_hash", transfer.TxHash)
	return nil
}

// confirm 查询补气交易 Receipt 并更新确认数或完成结算。
func (w *Worker) confirm(ctx context.Context, transfer postgres.InternalTransfer) error {
	if transfer.TxHash == "" {
		return errors.New("待确认 Gas 补充缺少交易哈希")
	}
	receipt, err := w.chain.TransactionReceipt(ctx, common.HexToHash(transfer.TxHash))
	if errors.Is(err, ethereum.NotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询 Gas 补充 Receipt 失败：%w", err)
	}
	return w.confirmReceipt(ctx, transfer, receipt)
}

// confirmReceipt 根据最新高度计算确认数并保存实际平台 Gas 成本。
func (w *Worker) confirmReceipt(ctx context.Context, transfer postgres.InternalTransfer, receipt *types.Receipt) error {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.EffectiveGasPrice == nil {
		return errors.New("Gas 补充 Receipt 内容无效")
	}
	latest, err := w.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Gas 补充确认高度失败：%w", err)
	}
	blockNumber := receipt.BlockNumber.Uint64()
	if blockNumber > latest || blockNumber > math.MaxInt64 {
		return errors.New("Gas 补充 Receipt 区块高度无效")
	}
	confirmations := latest - blockNumber + 1
	if confirmations > math.MaxInt64 {
		return errors.New("Gas 补充确认数超出数据库范围")
	}
	if confirmations < w.config.Confirmations {
		_, err := w.store.UpdateInternalTransferConfirmations(ctx, transfer.ID, int64(confirmations))
		return err
	}
	actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	result, changed, err := w.store.FinalizeInternalTransfer(ctx, postgres.InternalTransferSettlement{
		TransferID: transfer.ID, ActualFeeWei: actualFee, Success: receipt.Status == types.ReceiptStatusSuccessful,
		BlockNumber: int64(blockNumber), Confirmations: int64(confirmations),
	})
	if err != nil {
		return fmt.Errorf("结算 Gas 补充内部转账失败：%w", err)
	}
	if changed {
		w.logger.Info("Gas 补充交易已完成", "transfer_id", result.ID, "status", result.Status, "confirmations", confirmations)
	}
	return nil
}

// walletsFor 查询并校验归集来源地址和平台热钱包。
func (w *Worker) walletsFor(ctx context.Context, sweep postgres.TokenSweep) (postgres.WalletAddress, postgres.PlatformWallet, error) {
	address, err := w.store.WalletAddressByUser(ctx, sweep.UserID)
	if err != nil {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, fmt.Errorf("查询归集来源地址失败：%w", err)
	}
	if address.ID != sweep.AddressID {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, errors.New("归集任务与用户充值地址不匹配")
	}
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return postgres.WalletAddress{}, postgres.PlatformWallet{}, fmt.Errorf("查询平台热钱包失败：%w", err)
	}
	return address, platform, nil
}

// quoteTransfer 估算普通 ETH 补气转账的 EIP-1559 最大费用。
func (w *Worker) quoteTransfer(ctx context.Context, from, to common.Address, value *big.Int) (feeQuote, error) {
	gasLimit, err := w.chain.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: value})
	if err != nil {
		return feeQuote{}, fmt.Errorf("估算 Gas 补充交易费用失败：%w", err)
	}
	return w.quoteWithGasLimit(ctx, gasLimit)
}

// quoteWithGasLimit 根据最新 Base Fee 和建议 Tip 计算费用上限。
func (w *Worker) quoteWithGasLimit(ctx context.Context, gasLimit uint64) (feeQuote, error) {
	if gasLimit == 0 {
		return feeQuote{}, errors.New("RPC 返回的 Gas Limit 无效")
	}
	header, err := w.chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return feeQuote{}, fmt.Errorf("查询最新区块费用失败：%w", err)
	}
	if header == nil || header.BaseFee == nil || header.BaseFee.Sign() < 0 {
		return feeQuote{}, errors.New("最新区块缺少有效 Base Fee")
	}
	tip, err := w.chain.SuggestGasTipCap(ctx)
	if err != nil {
		return feeQuote{}, fmt.Errorf("查询建议优先费失败：%w", err)
	}
	if tip == nil || tip.Sign() < 0 {
		return feeQuote{}, errors.New("RPC 返回的建议优先费无效")
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tip)
	reserved := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), maxFee)
	return feeQuote{
		GasLimit: gasLimit, MaxFeePerGasWei: maxFee,
		MaxPriorityFeePerGasWei: new(big.Int).Set(tip), ReservedFeeWei: reserved,
	}, nil
}

// refreshHealth 查询平台热钱包当前 ETH 余额并更新健康快照。
func (w *Worker) refreshHealth(ctx context.Context) error {
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		w.updateHealth(postgres.PlatformWallet{}, nil, err)
		return fmt.Errorf("查询 Gas Station 平台钱包失败：%w", err)
	}
	balance, err := w.chain.BalanceAt(ctx, common.HexToAddress(platform.Address), nil)
	w.updateHealth(platform, balance, err)
	if err != nil {
		return fmt.Errorf("查询 Gas Station ETH 余额失败：%w", err)
	}
	if balance == nil || balance.Cmp(w.config.PlatformMinBalanceWei) < 0 {
		w.logger.Warn("平台热钱包 ETH 余额不足，暂停新的 Gas 补充任务", "address", platform.Address,
			"balance_wei", valueOrZero(balance).String(), "minimum_wei", w.config.PlatformMinBalanceWei.String())
		return ErrPlatformBalanceLow
	}
	return nil
}

// updateHealth 原子保存不会泄露敏感配置的 Gas Station 健康状态。
func (w *Worker) updateHealth(platform postgres.PlatformWallet, balance *big.Int, healthErr error) {
	snapshot := HealthSnapshot{
		Status: "HEALTHY", Address: platform.Address, BalanceWei: valueOrZero(balance),
		MinBalanceWei: new(big.Int).Set(w.config.PlatformMinBalanceWei), CheckedAt: time.Now(),
	}
	if healthErr != nil {
		snapshot.Status = "DOWN"
		snapshot.LastError = healthErr.Error()
	} else if balance == nil || balance.Cmp(w.config.PlatformMinBalanceWei) < 0 {
		snapshot.Status = "LOW_BALANCE"
		snapshot.LastError = ErrPlatformBalanceLow.Error()
	}
	w.healthMu.Lock()
	w.health = snapshot
	w.healthMu.Unlock()
}

// Snapshot 返回 Gas Station 最近一次余额检查结果的深拷贝。
func (w *Worker) Snapshot() HealthSnapshot {
	w.healthMu.RLock()
	defer w.healthMu.RUnlock()
	result := w.health
	result.BalanceWei = valueOrZero(w.health.BalanceWei)
	result.MinBalanceWei = valueOrZero(w.health.MinBalanceWei)
	return result
}

// recordError 保存经过清洗的 Gas Station Worker 错误。
func (w *Worker) recordError(ctx context.Context, sweepID int64, stage, code string, workerErr error) {
	referenceID := sweepID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "gas-station-worker", Stage: stage, ReferenceType: "TOKEN_SWEEP",
		ReferenceID: &referenceID, ErrorCode: code, ErrorMessage: workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录 Gas Station Worker 错误失败", "sweep_id", sweepID, "error", err)
	}
}

// addSafetyMargin 按基点向上取整计算 Gas 安全余量后的目标余额。
func addSafetyMargin(required *big.Int, basisPoints uint64) *big.Int {
	margin := new(big.Int).Mul(new(big.Int).Set(required), new(big.Int).SetUint64(basisPoints))
	margin.Add(margin, big.NewInt(9_999))
	margin.Div(margin, big.NewInt(10_000))
	return new(big.Int).Add(new(big.Int).Set(required), margin)
}

// valueOrZero 返回数值深拷贝，空值转换为零。
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
