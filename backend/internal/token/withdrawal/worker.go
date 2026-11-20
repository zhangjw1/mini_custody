package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = evm.DefaultWorkerBatchSize

var (
	ErrHotWalletTokenInsufficient = errors.New("平台热钱包 Token 库存不足，任务保持等待并将在下轮重试")
	ErrHotWalletGasInsufficient   = errors.New("平台热钱包 ETH 不足以支付最大网络费，任务保持等待并将在下轮重试")
)

// Chain 定义 Token 提币库存、费用、Nonce、广播和确认所需的 EVM 能力。
type Chain interface {
	FeeChain
	BlockNumber(ctx context.Context) (uint64, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
	SendTransaction(ctx context.Context, transaction *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// TokenContract 定义 Token 提币所需的合约余额、transfer 编解码和 Event 能力。
type TokenContract interface {
	QuoteContract
	BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error)
	EncodeTransfer(to common.Address, amountUnits *big.Int) ([]byte, error)
	DecodeTransferCalldata(calldata []byte) (common.Address, *big.Int, error)
	DecodeTransferLog(log types.Log) (erc20.TransferEvent, error)
}

// WorkerStore 定义 Token 提币持久化状态机和恢复查询。
type WorkerStore interface {
	TokenWithdrawalByID(ctx context.Context, id int64) (postgres.TokenWithdrawal, error)
	ListProcessableTokenWithdrawals(ctx context.Context, limit int) ([]postgres.TokenWithdrawal, error)
	ListEVMTxAttempts(ctx context.Context, businessType string, businessID int64) ([]postgres.EVMTxAttempt, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	AllocateTokenWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64) (postgres.TokenWithdrawal, bool, error)
	SaveSignedTokenWithdrawal(ctx context.Context, signed postgres.SignedTokenWithdrawal) (postgres.TokenWithdrawal, bool, error)
	ReplaceTokenWithdrawalTx(ctx context.Context, replacement postgres.SignedTokenWithdrawal) (postgres.TokenWithdrawal, bool, error)
	TouchTokenWithdrawalBroadcast(ctx context.Context, withdrawalID int64) (postgres.TokenWithdrawal, error)
	MarkTokenWithdrawalManualReview(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (postgres.TokenWithdrawal, bool, error)
	TransitionTokenWithdrawal(ctx context.Context, withdrawalID int64, target string) (postgres.TokenWithdrawal, error)
	UpdateTokenWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (postgres.TokenWithdrawal, error)
	FinalizeTokenWithdrawal(ctx context.Context, settlement postgres.TokenWithdrawalSettlement) (postgres.TokenWithdrawal, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// WorkerConfig 定义 Token 提币轮询间隔、确认数、卡单重播加价策略和 Sepolia Chain ID。
type WorkerConfig struct {
	Interval         time.Duration
	Confirmations    uint64
	ChainID          *big.Int
	RebroadcastAfter time.Duration
	BumpAfter        time.Duration
	BumpPercent      uint64
	MaxBumps         int
	MaxFeeCapWei     *big.Int
}

// Worker 负责平台热钱包 Token 提币签名、幂等广播、恢复和账本结算。
type Worker struct {
	chain       Chain
	contract    TokenContract
	store       WorkerStore
	keys        wallet.KeyProvider
	logger      *slog.Logger
	config      WorkerConfig
	fees        evm.FeeEstimator
	broadcaster evm.Broadcaster
	confirmer   evm.Confirmer
	signing     *evm.SigningFailureTracker
}

// NewWorker 创建并校验 Token 提币 Worker。
func NewWorker(chain Chain, contract TokenContract, store WorkerStore, keys wallet.KeyProvider, logger *slog.Logger, cfg WorkerConfig) (*Worker, error) {
	if chain == nil || contract == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("Token 提币 Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil || cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 {
		return nil, errors.New("Token 提币 Worker 配置无效")
	}
	if err := normalizeReplacementConfig(&cfg); err != nil {
		return nil, err
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{
		chain: chain, contract: contract, store: store, keys: keys, logger: logger, config: cfg,
		fees:        evm.NewFeeEstimator(chain),
		broadcaster: evm.NewBroadcaster(chain),
		confirmer:   evm.NewConfirmer(chain),
		signing:     evm.NewSigningFailureTracker(evm.DefaultMaxSigningRetryCount),
	}, nil
}

// Run 周期推进全部可恢复 Token 提币任务。
func (w *Worker) Run(ctx context.Context) error {
	return evm.RunPollLoop(ctx, w.config.Interval, w.RunOnce, func(err error) {
		w.logger.Warn("Token 提币 Worker 本轮存在失败，将在下轮重试", "error", err)
	})
}

// RunOnce 查询全部可恢复 Token 提币并逐条推进，单条失败不阻塞其他任务。
func (w *Worker) RunOnce(ctx context.Context) error {
	ctx = evm.WithTickCache(ctx)
	items, err := w.store.ListProcessableTokenWithdrawals(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询待处理 Token 提币失败：%w", err)
	}
	var firstErr error
	for _, item := range items {
		if w.signing.IsPaused(item.ID) {
			continue
		}
		if err := w.Process(ctx, item.ID); err != nil {
			if !w.signing.IsPaused(item.ID) {
				w.recordError(ctx, item.ID, "process", "TOKEN_WITHDRAWAL_PROCESS_FAILED", err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据持久化状态恢复并推进一笔 Token 提币。
func (w *Worker) Process(ctx context.Context, withdrawalID int64) error {
	item, err := w.store.TokenWithdrawalByID(ctx, withdrawalID)
	if err != nil {
		return fmt.Errorf("查询 Token 提币 %d 失败：%w", withdrawalID, err)
	}
	switch item.Status {
	case postgres.WithdrawalCreated:
		return w.prepare(ctx, item)
	case postgres.WithdrawalSigning:
		return w.sign(ctx, item)
	case postgres.WithdrawalSigned:
		return w.broadcast(ctx, item)
	case postgres.WithdrawalBroadcasted, postgres.WithdrawalConfirming:
		return w.confirm(ctx, item)
	default:
		return nil
	}
}

// prepare 校验平台 Token 库存和 ETH Gas 后原子分配共享平台 Nonce。
func (w *Worker) prepare(ctx context.Context, item postgres.TokenWithdrawal) error {
	platform, _, err := w.checkBalancesAndQuote(ctx, item)
	if err != nil {
		return err
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, common.HexToAddress(platform.Address))
	if err != nil {
		return fmt.Errorf("查询 Token 提币平台钱包 Pending Nonce 失败：%w", err)
	}
	item, _, err = w.store.AllocateTokenWithdrawalNonce(ctx, item.ID, chainNonce)
	if err != nil {
		return fmt.Errorf("分配 Token 提币平台 Nonce 失败：%w", err)
	}
	return w.sign(ctx, item)
}

// sign 在已分配平台 Nonce 后直接构建并持久化交易，不再因库存/余额波动无限阻断。
func (w *Worker) sign(ctx context.Context, item postgres.TokenWithdrawal) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.AmountUnits == nil || item.AmountUnits.Sign() <= 0 {
		return w.handleSigningFailure(ctx, item.ID, errors.New("Token 提币 Nonce 或金额无效"))
	}
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	from := common.HexToAddress(platform.Address)
	to := common.HexToAddress(item.ToAddress)
	fee, err := estimateFee(ctx, w.chain, w.contract, from, to, item.AmountUnits)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	calldata, err := w.contract.EncodeTransfer(to, item.AmountUnits)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("编码 Token 提币 calldata 失败：%w", err))
	}
	contractAddress := w.contract.Address()
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID: w.config.ChainID, Nonce: item.Nonce.Uint64(), GasTipCap: fee.MaxPriorityFeePerGasWei,
		GasFeeCap: fee.MaxFeePerGasWei, Gas: fee.GasLimit, To: &contractAddress,
		Value: new(big.Int), Data: calldata,
	})
	if err := w.validateTransaction(transaction, item); err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	signed, err := w.keys.SignTx(ctx, platform.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("签署 Token 提币交易失败：%w", err))
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != from {
		return w.handleSigningFailure(ctx, item.ID, errors.New("Token 提币签名地址与平台热钱包不一致"))
	}
	if err := w.validateTransaction(signed, item); err != nil {
		return w.handleSigningFailure(ctx, item.ID, err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("编码已签名 Token 提币交易失败：%w", err))
	}
	item, _, err = w.store.SaveSignedTokenWithdrawal(ctx, postgres.SignedTokenWithdrawal{
		WithdrawalID: item.ID, GasLimit: fee.GasLimit, MaxFeePerGasWei: fee.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: fee.MaxPriorityFeePerGasWei, RawTx: raw, TxHash: signed.Hash().Hex(),
	})
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("持久化已签名 Token 提币失败：%w", err))
	}
	w.signing.Clear(item.ID)
	return w.broadcast(ctx, item)
}

// handleSigningFailure 统计分配 Nonce 后的连续签名失败次数，超限后写人工介入错误并暂停自动重复写入。
func (w *Worker) handleSigningFailure(ctx context.Context, id int64, err error) error {
	w.signing.RecordFailure(id, func() {
		w.recordError(ctx, id, "signing", "TOKEN_SIGNING_MANUAL_INTERVENTION", err)
	})
	return err
}

// broadcast 复核数据库原始交易并始终重播同一 tx_hash，避免重启后重复出款。
func (w *Worker) broadcast(ctx context.Context, item postgres.TokenWithdrawal) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	if err := w.validateTransaction(transaction, item); err != nil {
		return err
	}
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return err
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), transaction)
	if err != nil || sender != common.HexToAddress(platform.Address) {
		return errors.New("待广播 Token 提币发送方与平台热钱包不一致")
	}
	if receipt, err := w.broadcaster.CheckExistingReceipt(ctx, transaction.Hash()); err != nil {
		return fmt.Errorf("查询待广播 Token 提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		item, err = w.store.TransitionTokenWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
		if err != nil {
			return fmt.Errorf("恢复 Token 提币已广播状态失败：%w", err)
		}
		return w.confirmReceipt(ctx, item, receipt, item.TxHash)
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		return fmt.Errorf("广播 Token 提币交易结果不明确：%w", err)
	}
	if handled {
		return nil
	}
	item, err = w.store.TransitionTokenWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
	if err != nil {
		return fmt.Errorf("更新 Token 提币已广播状态失败：%w", err)
	}
	w.logger.Info("Token 提币交易已广播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// confirm 查询 Token 提币全部候选交易 Receipt，并在卡单超时后执行重播或 Gas 加价替换。
func (w *Worker) confirm(ctx context.Context, item postgres.TokenWithdrawal) error {
	attempts, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return err
	}
	receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates)
	if err != nil {
		return fmt.Errorf("查询 Token 提币 Receipt 失败：%w", err)
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
	if item.Status != postgres.WithdrawalBroadcasted {
		return nil
	}
	tierStart := replacementTierStart(item.BumpCount, item.BroadcastedAt, item.LastBroadcastAt, attempts)
	if tierStart != nil && time.Since(*tierStart) >= w.config.BumpAfter && int(item.BumpCount) < w.config.MaxBumps {
		return w.bumpAndBroadcast(ctx, item, attempts)
	}
	if item.LastBroadcastAt != nil && time.Since(*item.LastBroadcastAt) >= w.config.RebroadcastAfter {
		return w.rebroadcast(ctx, item)
	}
	return nil
}

// confirmReceipt 校验确认数和标准 Transfer Event，并分离结算用户 Token 与平台 Gas。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.TokenWithdrawal, receipt *types.Receipt, matchedTxHash string) error {
	blockNumber, confirmations, err := w.confirmer.EvaluateConfirmations(ctx, receipt)
	if err != nil {
		return err
	}
	if uint64(confirmations) < w.config.Confirmations {
		_, err := w.store.UpdateTokenWithdrawalConfirmations(ctx, item.ID, confirmations)
		return err
	}
	settlement := postgres.TokenWithdrawalSettlement{
		WithdrawalID:  item.ID,
		ActualFeeWei:  new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice),
		BlockNumber:   blockNumber,
		Confirmations: confirmations,
		TxHash:        matchedTxHash,
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		settlement.ErrorCode = "TOKEN_WITHDRAWAL_REVERTED"
		settlement.ErrorMessage = "Token 提币交易链上执行失败"
	} else {
		matched, err := w.hasExpectedTransfer(ctx, item, receipt.Logs)
		if err != nil {
			return err
		}
		if !matched {
			return errors.New("Token 提币 Receipt 缺少预期到账 Event，任务保留等待人工核对")
		}
		settlement.Success = true
	}
	result, changed, err := w.store.FinalizeTokenWithdrawal(ctx, settlement)
	if err != nil {
		return fmt.Errorf("结算 Token 提币失败：%w", err)
	}
	if changed {
		w.logger.Info("Token 提币已完成账本结算", "withdrawal_id", result.ID, "status", result.Status, "confirmations", confirmations, "tx_hash", result.TxHash)
	}
	return nil
}

// checkBalancesAndQuote 校验记录归属、热钱包 Token 库存及 ETH Gas 余额。
func (w *Worker) checkBalancesAndQuote(ctx context.Context, item postgres.TokenWithdrawal) (postgres.PlatformWallet, FeeQuote, error) {
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, err
	}
	from := common.HexToAddress(platform.Address)
	tokenBalance, err := w.contract.BalanceOf(ctx, from)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, fmt.Errorf("查询平台热钱包 Token 库存失败：%w", err)
	}
	if tokenBalance == nil || tokenBalance.Cmp(item.AmountUnits) < 0 {
		return postgres.PlatformWallet{}, FeeQuote{}, ErrHotWalletTokenInsufficient
	}
	fee, err := estimateFee(ctx, w.chain, w.contract, from, common.HexToAddress(item.ToAddress), item.AmountUnits)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, err
	}
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, fmt.Errorf("查询平台热钱包 ETH Gas 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(fee.ReservedFeeWei) < 0 {
		return postgres.PlatformWallet{}, FeeQuote{}, ErrHotWalletGasInsufficient
	}
	return platform, fee, nil
}

// platformFor 查询并校验 Token 提币使用的是配置的平台热钱包。
func (w *Worker) platformFor(ctx context.Context, item postgres.TokenWithdrawal) (postgres.PlatformWallet, error) {
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		return postgres.PlatformWallet{}, fmt.Errorf("查询 Token 提币平台热钱包失败：%w", err)
	}
	if platform.ID != item.PlatformWalletID {
		return postgres.PlatformWallet{}, errors.New("Token 提币记录与平台热钱包不匹配")
	}
	return platform, nil
}

// validateTransaction 严格复核链、Nonce、合约、金额和 transfer calldata。
func (w *Worker) validateTransaction(transaction *types.Transaction, item postgres.TokenWithdrawal) error {
	if transaction == nil || transaction.ChainId().Cmp(w.config.ChainID) != 0 || item.Nonce == nil ||
		!item.Nonce.IsUint64() || transaction.Nonce() != item.Nonce.Uint64() || transaction.To() == nil ||
		*transaction.To() != w.contract.Address() || transaction.Value().Sign() != 0 || transaction.Gas() == 0 ||
		transaction.GasFeeCap().Sign() <= 0 || transaction.GasTipCap().Sign() < 0 {
		return errors.New("Token 提币交易基础参数复核失败")
	}
	to, amountUnits, err := w.contract.DecodeTransferCalldata(transaction.Data())
	if err != nil {
		return fmt.Errorf("复核 Token 提币 calldata 失败：%w", err)
	}
	if to != common.HexToAddress(item.ToAddress) || amountUnits.Cmp(item.AmountUnits) != 0 {
		return errors.New("Token 提币 calldata 与数据库请求不一致")
	}
	return nil
}

// hasExpectedTransfer 校验 Receipt 存在平台热钱包到目标地址的精确 Transfer Event。
func (w *Worker) hasExpectedTransfer(ctx context.Context, item postgres.TokenWithdrawal, logs []*types.Log) (bool, error) {
	platform, err := evm.CachedValue(ctx, postgres.NetworkSepolia+":"+postgres.PlatformRoleHot, func(c context.Context) (postgres.PlatformWallet, error) {
		return w.store.PlatformWalletByRole(c, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	})
	if err != nil {
		return false, fmt.Errorf("校验 Token 提币 Event 时查询平台热钱包失败：%w", err)
	}
	from := common.HexToAddress(platform.Address)
	to := common.HexToAddress(item.ToAddress)
	for _, log := range logs {
		if log == nil || log.Address != w.contract.Address() {
			continue
		}
		event, err := w.contract.DecodeTransferLog(*log)
		if err != nil {
			if errors.Is(err, erc20.ErrInvalidTransferLog) {
				continue
			}
			return false, fmt.Errorf("解析 Token 提币 Transfer Event 失败：%w", err)
		}
		if event.From == from && event.To == to && event.AmountUnits.Cmp(item.AmountUnits) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// recordError 保存经过清洗的 Token 提币 Worker 错误。
func (w *Worker) recordError(ctx context.Context, withdrawalID int64, stage, code string, workerErr error) {
	referenceID := withdrawalID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "token-withdrawal-worker", Stage: stage, ReferenceType: "TOKEN_WITHDRAWAL",
		ReferenceID: &referenceID, ErrorCode: code, ErrorMessage: workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录 Token 提币 Worker 错误失败", "withdrawal_id", withdrawalID, "error", err)
	}
}

// normalizeReplacementConfig 校验并填充 Token 提币重播与 Gas 加价配置默认值。
func normalizeReplacementConfig(cfg *WorkerConfig) error {
	settings := evm.ReplacementSettings{
		RebroadcastAfter: cfg.RebroadcastAfter,
		BumpAfter:        cfg.BumpAfter,
		BumpPercent:      cfg.BumpPercent,
		MaxBumps:         cfg.MaxBumps,
		MaxFeeCapWei:     cfg.MaxFeeCapWei,
	}
	if err := evm.NormalizeReplacementSettings(&settings); err != nil {
		return err
	}
	cfg.RebroadcastAfter = settings.RebroadcastAfter
	cfg.BumpAfter = settings.BumpAfter
	cfg.BumpPercent = settings.BumpPercent
	cfg.MaxBumps = settings.MaxBumps
	cfg.MaxFeeCapWei = settings.MaxFeeCapWei
	return nil
}

// candidateHashes 查询同一笔 Token 提币按尝试序号倒序排列的所有候选交易哈希。
func (w *Worker) candidateHashes(ctx context.Context, item postgres.TokenWithdrawal) ([]postgres.EVMTxAttempt, []string, error) {
	attempts, err := w.store.ListEVMTxAttempts(ctx, postgres.EVMBusinessTokenWithdrawal, item.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("查询 Token 提币候选交易记录失败：%w", err)
	}
	candidates, err := evm.CollectCandidateHashes(attempts, func(a postgres.EVMTxAttempt) string { return a.TxHash }, item.TxHash)
	if err != nil {
		return nil, nil, errors.New("待确认 Token 提币缺少交易哈希")
	}
	return attempts, candidates, nil
}

// checkNonceConsumed 在所有候选交易均无 Receipt 时校验平台钱包已确认 Nonce 是否已越过本单 Nonce。
func (w *Worker) checkNonceConsumed(ctx context.Context, item postgres.TokenWithdrawal, candidates []string) (bool, error) {
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return false, nil
	}
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return false, err
	}
	confirmedNonce, err := w.chain.NonceAt(ctx, common.HexToAddress(platform.Address), nil)
	if err != nil {
		return false, fmt.Errorf("查询 Token 提币平台已确认 Nonce 失败：%w", err)
	}
	if confirmedNonce <= item.Nonce.Uint64() {
		return false, nil
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("二次复核 Token 提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		return true, w.confirmReceipt(ctx, item, receipt, matchedHash)
	}
	reason := fmt.Sprintf("平台热钱包最新确认 Nonce(%d) 已超过 Token 提币 Nonce(%d)，但所有候选交易均未上链，需人工核查", confirmedNonce, item.Nonce.Uint64())
	_, changed, err := w.store.MarkTokenWithdrawalManualReview(ctx, item.ID, "EVM_TX_MANUAL_REVIEW", reason)
	if err != nil {
		return false, fmt.Errorf("转入 Token 提币人工核查状态失败：%w", err)
	}
	if changed {
		w.recordError(ctx, item.ID, "confirm", "EVM_TX_MANUAL_REVIEW", errors.New(reason))
		w.logger.Warn("Token 提币转入人工核查状态", "withdrawal_id", item.ID, "nonce", item.Nonce.String(), "confirmed_nonce", confirmedNonce)
	}
	return true, nil
}

// handleNonceTooLow 在广播遇到 nonce too low 时优先复核所有候选 Receipt 与已确认 Nonce。
func (w *Worker) handleNonceTooLow(ctx context.Context, item postgres.TokenWithdrawal) (bool, error) {
	_, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return false, err
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("复核 Token 提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		ensured, transitionErr := w.store.TransitionTokenWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
		if transitionErr != nil {
			return false, fmt.Errorf("恢复 Token 提币已广播状态失败：%w", transitionErr)
		}
		return true, w.confirmReceipt(ctx, ensured, receipt, matchedHash)
	}
	return w.checkNonceConsumed(ctx, item, candidates)
}

// bumpAndBroadcast 按同一 Nonce 提升 Token 提币 Gas 费用重新签名、落库并广播替换交易。
func (w *Worker) bumpAndBroadcast(ctx context.Context, item postgres.TokenWithdrawal, attempts []postgres.EVMTxAttempt) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.AmountUnits == nil || item.AmountUnits.Sign() <= 0 {
		return errors.New("待加价 Token 提币缺少有效 Nonce 或金额")
	}
	prevFeeCap := item.MaxFeePerGasWei
	prevTip := item.MaxPriorityFeePerGasWei
	if (prevFeeCap == nil || prevTip == nil) && len(attempts) > 0 {
		prevFeeCap = attempts[0].MaxFeePerGasWei
		prevTip = attempts[0].MaxPriorityFeePerGasWei
	}
	if prevFeeCap == nil || prevFeeCap.Sign() <= 0 || prevTip == nil || prevTip.Sign() < 0 {
		return errors.New("待加价 Token 提币缺少有效历史 Gas 参数")
	}
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return err
	}
	from := common.HexToAddress(platform.Address)
	to := common.HexToAddress(item.ToAddress)
	fee, err := estimateFee(ctx, w.chain, w.contract, from, to, item.AmountUnits)
	if err != nil {
		return err
	}
	gasLimit := fee.GasLimit
	if item.GasLimit != nil && *item.GasLimit > 0 && uint64(*item.GasLimit) > gasLimit {
		gasLimit = uint64(*item.GasLimit)
	}
	baseFee, err := w.fees.LatestBaseFee(ctx)
	if err != nil {
		return fmt.Errorf("Token 提币加价查询 Base Fee 失败：%w", err)
	}
	bumpedFeeCap, bumpedTip, ok := evm.ComputeBumpedFees(prevFeeCap, prevTip, baseFee, fee.MaxPriorityFeePerGasWei, w.config.BumpPercent, w.config.MaxFeeCapWei)
	if !ok {
		w.recordError(ctx, item.ID, "bump", "EVM_BUMP_FEE_CAP_EXCEEDED", errors.New("Token 提币加价后手续费上限超过配置最大值，停止继续加价"))
		if item.LastBroadcastAt != nil && time.Since(*item.LastBroadcastAt) >= w.config.RebroadcastAfter {
			return w.rebroadcast(ctx, item)
		}
		return nil
	}
	requiredFee := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), bumpedFeeCap)
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("Token 提币加价查询平台热钱包 ETH 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(requiredFee) < 0 {
		w.recordError(ctx, item.ID, "bump", "HOT_WALLET_GAS_INSUFFICIENT", ErrHotWalletGasInsufficient)
		return nil
	}
	calldata, err := w.contract.EncodeTransfer(to, item.AmountUnits)
	if err != nil {
		return fmt.Errorf("编码 Token 提币加价 calldata 失败：%w", err)
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
	if err := w.validateTransaction(transaction, item); err != nil {
		return err
	}
	signed, err := w.keys.SignTx(ctx, platform.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署 Token 提币加价替换交易失败：%w", err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != from {
		return errors.New("Token 提币加价替换签名地址与平台热钱包不一致")
	}
	if err := w.validateTransaction(signed, item); err != nil {
		return err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码 Token 提币加价替换交易失败：%w", err)
	}
	replaced, changed, err := w.store.ReplaceTokenWithdrawalTx(ctx, postgres.SignedTokenWithdrawal{
		WithdrawalID:            item.ID,
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         bumpedFeeCap,
		MaxPriorityFeePerGasWei: bumpedTip,
		RawTx:                   raw,
		TxHash:                  strings.ToLower(signed.Hash().Hex()),
	})
	if err != nil {
		return fmt.Errorf("保存 Token 提币加价替换交易失败：%w", err)
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, signed, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, replaced)
	})
	if err != nil {
		return fmt.Errorf("广播 Token 提币加价替换交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if changed {
		w.logger.Info("Token 提币已完成 Gas 加价替换并广播", "withdrawal_id", replaced.ID, "bump_count", replaced.BumpCount, "tx_hash", replaced.TxHash)
	}
	return nil
}

// rebroadcast 使用相同 raw_tx 原样重播卡在内存池的 Token 提币交易。
func (w *Worker) rebroadcast(ctx context.Context, item postgres.TokenWithdrawal) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	if err := w.validateTransaction(transaction, item); err != nil {
		return err
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		return fmt.Errorf("重播 Token 提币交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if _, err := w.store.TouchTokenWithdrawalBroadcast(ctx, item.ID); err != nil {
		return fmt.Errorf("更新 Token 提币重播时间失败：%w", err)
	}
	w.logger.Info("Token 提币交易已重播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
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
