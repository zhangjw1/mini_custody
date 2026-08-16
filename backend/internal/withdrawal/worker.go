package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = 100

// WorkerStore 定义提币 Worker 所需的持久化事务和恢复查询。
type WorkerStore interface {
	WithdrawalByID(ctx context.Context, id int64) (postgres.Withdrawal, error)
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	ListProcessableWithdrawals(ctx context.Context, limit int) ([]postgres.Withdrawal, error)
	IncreaseWithdrawalFee(ctx context.Context, withdrawalID int64, newReservedFeeWei *big.Int) (postgres.Withdrawal, bool, error)
	AllocateWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64) (postgres.Withdrawal, bool, error)
	SaveSignedWithdrawal(ctx context.Context, signed postgres.SignedWithdrawal) (postgres.Withdrawal, bool, error)
	TransitionWithdrawal(ctx context.Context, withdrawalID int64, target string) (postgres.Withdrawal, error)
	ReleaseWithdrawal(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (postgres.Withdrawal, bool, error)
	UpdateWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (postgres.Withdrawal, error)
	FinalizeWithdrawal(ctx context.Context, settlement postgres.WithdrawalSettlement) (postgres.Withdrawal, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// WorkerConfig 定义提币轮询间隔、确认数和 Sepolia Chain ID。
type WorkerConfig struct {
	Interval      time.Duration
	Confirmations uint64
	ChainID       *big.Int
}

// Worker 负责提币签名、幂等广播、崩溃恢复和费用结算。
type Worker struct {
	chain  Chain
	store  WorkerStore
	keys   wallet.KeyProvider
	logger *slog.Logger
	config WorkerConfig
}

// NewWorker 创建并校验提币 Worker。
func NewWorker(chain Chain, store WorkerStore, keys wallet.KeyProvider, logger *slog.Logger, cfg WorkerConfig) (*Worker, error) {
	if chain == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("提币 Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil || cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 {
		return nil, errors.New("提币 Worker 配置无效")
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{chain: chain, store: store, keys: keys, logger: logger, config: cfg}, nil
}

// Run 周期处理可恢复提币，单条失败不会阻止其他提币继续推进。
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("Sepolia 提币 Worker 本轮存在失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 查询全部可恢复提币并逐条推进状态。
func (w *Worker) RunOnce(ctx context.Context) error {
	items, err := w.store.ListProcessableWithdrawals(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询待处理提币失败：%w", err)
	}
	var firstErr error
	for _, item := range items {
		if err := w.Process(ctx, item.ID); err != nil {
			w.recordError(ctx, item.ID, "process", "WITHDRAWAL_PROCESS_FAILED", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据持久化状态恢复并推进一笔提币。
func (w *Worker) Process(ctx context.Context, withdrawalID int64) error {
	item, err := w.store.WithdrawalByID(ctx, withdrawalID)
	if err != nil {
		return fmt.Errorf("查询提币 %d 失败：%w", withdrawalID, err)
	}
	switch item.Status {
	case postgres.WithdrawalCreated:
		return w.prepare(ctx, item)
	case postgres.WithdrawalSigning:
		return w.sign(ctx, item)
	case postgres.WithdrawalSigned, postgres.WithdrawalBroadcasting, postgres.WithdrawalBroadcastUnknown:
		return w.broadcast(ctx, item)
	case postgres.WithdrawalBroadcasted, postgres.WithdrawalConfirming:
		return w.confirm(ctx, item)
	default:
		return nil
	}
}

// prepare 重新估算费用、校验链上余额并原子分配 Nonce。
func (w *Worker) prepare(ctx context.Context, item postgres.Withdrawal) error {
	walletAddress, quote, err := w.quoteFor(ctx, item)
	if err != nil {
		return err
	}
	item, _, err = w.store.IncreaseWithdrawalFee(ctx, item.ID, quote.ReservedFeeWei)
	if errors.Is(err, postgres.ErrInsufficientBalance) {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "FEE_RESERVE_INSUFFICIENT", "可用余额不足以补充最新最大网络费")
		return releaseErr
	}
	if err != nil {
		return fmt.Errorf("调整提币 %d 最大网络费失败：%w", item.ID, err)
	}
	required := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	chainBalance, err := w.chain.BalanceAt(ctx, common.HexToAddress(walletAddress.Address), nil)
	if err != nil {
		return fmt.Errorf("查询提币地址链上余额失败：%w", err)
	}
	if chainBalance == nil || chainBalance.Cmp(required) < 0 {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "CHAIN_BALANCE_INSUFFICIENT", "托管地址链上余额不足以支付提币金额和最大网络费")
		return releaseErr
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, common.HexToAddress(walletAddress.Address))
	if err != nil {
		return fmt.Errorf("查询提币地址 Pending Nonce 失败：%w", err)
	}
	item, _, err = w.store.AllocateWithdrawalNonce(ctx, item.ID, chainNonce)
	if err != nil {
		return fmt.Errorf("分配提币 %d Nonce 失败：%w", item.ID, err)
	}
	return w.sign(ctx, item)
}

// sign 在签名前再次估算费用并持久化确定的 EIP-1559 原始交易。
func (w *Worker) sign(ctx context.Context, item postgres.Withdrawal) error {
	walletAddress, quote, err := w.quoteFor(ctx, item)
	if err != nil {
		return err
	}
	item, _, err = w.store.IncreaseWithdrawalFee(ctx, item.ID, quote.ReservedFeeWei)
	if errors.Is(err, postgres.ErrInsufficientBalance) {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "FEE_RESERVE_INSUFFICIENT", "签名前可用余额不足以补充最新最大网络费")
		return releaseErr
	}
	if err != nil {
		return fmt.Errorf("签名前调整提币费用失败：%w", err)
	}
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return errors.New("提币 Nonce 无效")
	}
	required := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	chainBalance, err := w.chain.BalanceAt(ctx, common.HexToAddress(walletAddress.Address), nil)
	if err != nil {
		return fmt.Errorf("签名前查询链上余额失败：%w", err)
	}
	if chainBalance == nil || chainBalance.Cmp(required) < 0 {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "CHAIN_BALANCE_INSUFFICIENT", "签名前托管地址链上余额不足")
		return releaseErr
	}
	to := common.HexToAddress(item.ToAddress)
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(w.config.ChainID),
		Nonce:     item.Nonce.Uint64(),
		GasTipCap: quote.MaxPriorityFeePerGasWei,
		GasFeeCap: quote.MaxFeePerGasWei,
		Gas:       quote.GasLimit,
		To:        &to,
		Value:     new(big.Int).Set(item.AmountWei),
	})
	signed, err := w.keys.SignTx(ctx, walletAddress.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署提币交易失败：%w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码已签名提币交易失败：%w", err)
	}
	item, _, err = w.store.SaveSignedWithdrawal(ctx, postgres.SignedWithdrawal{
		WithdrawalID:            item.ID,
		GasLimit:                quote.GasLimit,
		MaxFeePerGasWei:         quote.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: quote.MaxPriorityFeePerGasWei,
		RawTx:                   raw,
		TxHash:                  signed.Hash().Hex(),
	})
	if err != nil {
		return fmt.Errorf("持久化已签名提币失败：%w", err)
	}
	return w.broadcast(ctx, item)
}

// broadcast 优先查询原交易 Receipt，并始终重播数据库中相同的 raw_tx。
func (w *Worker) broadcast(ctx context.Context, item postgres.Withdrawal) error {
	var transaction types.Transaction
	if len(item.RawTx) == 0 || item.TxHash == "" {
		return errors.New("待广播提币缺少已签名原始交易")
	}
	if err := transaction.UnmarshalBinary(item.RawTx); err != nil {
		return errors.New("数据库中的已签名提币交易无效")
	}
	if transaction.Hash() != common.HexToHash(item.TxHash) {
		return errors.New("数据库中的提币交易哈希与原始交易不一致")
	}
	if receipt, err := w.chain.TransactionReceipt(ctx, transaction.Hash()); err == nil && receipt != nil {
		item, err = w.ensureBroadcasted(ctx, item)
		if err != nil {
			return err
		}
		return w.confirmReceipt(ctx, item, receipt)
	} else if err != nil && !errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("查询待广播提币 Receipt 失败：%w", err)
	}
	if item.Status == postgres.WithdrawalSigned {
		var err error
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasting)
		if err != nil {
			return fmt.Errorf("提币进入广播中状态失败：%w", err)
		}
	}
	if err := w.chain.SendTransaction(ctx, &transaction); err != nil && !alreadyKnown(err) {
		if item.Status == postgres.WithdrawalBroadcasting {
			_, _ = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcastUnknown)
		}
		return fmt.Errorf("广播提币交易结果不明确：%w", err)
	}
	item, err := w.ensureBroadcasted(ctx, item)
	if err != nil {
		return err
	}
	w.logger.Info("Sepolia 提币交易已广播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// ensureBroadcasted 将不同恢复状态安全推进到已广播状态。
func (w *Worker) ensureBroadcasted(ctx context.Context, item postgres.Withdrawal) (postgres.Withdrawal, error) {
	var err error
	if item.Status == postgres.WithdrawalSigned {
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasting)
		if err != nil {
			return postgres.Withdrawal{}, fmt.Errorf("提币进入广播中状态失败：%w", err)
		}
	}
	if item.Status == postgres.WithdrawalBroadcasting || item.Status == postgres.WithdrawalBroadcastUnknown {
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
		if err != nil {
			return postgres.Withdrawal{}, fmt.Errorf("提币进入已广播状态失败：%w", err)
		}
	}
	return item, nil
}

// confirm 查询 Receipt 并更新确认数或完成实际费用结算。
func (w *Worker) confirm(ctx context.Context, item postgres.Withdrawal) error {
	if item.TxHash == "" {
		return errors.New("待确认提币缺少交易哈希")
	}
	receipt, err := w.chain.TransactionReceipt(ctx, common.HexToHash(item.TxHash))
	if errors.Is(err, ethereum.NotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询提币 Receipt 失败：%w", err)
	}
	return w.confirmReceipt(ctx, item, receipt)
}

// confirmReceipt 根据 Receipt 和最新高度更新确认数并结算余额。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.Withdrawal, receipt *types.Receipt) error {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.EffectiveGasPrice == nil {
		return errors.New("提币 Receipt 内容无效")
	}
	latest, err := w.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询提币确认高度失败：%w", err)
	}
	blockNumber := receipt.BlockNumber.Uint64()
	if blockNumber > latest || blockNumber > math.MaxInt64 {
		return errors.New("提币 Receipt 区块高度无效")
	}
	confirmations := latest - blockNumber + 1
	if confirmations > math.MaxInt64 {
		return errors.New("提币确认数超出数据库范围")
	}
	if confirmations < w.config.Confirmations {
		_, err := w.store.UpdateWithdrawalConfirmations(ctx, item.ID, int64(confirmations))
		return err
	}
	actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	result, changed, err := w.store.FinalizeWithdrawal(ctx, postgres.WithdrawalSettlement{
		WithdrawalID:  item.ID,
		ActualFeeWei:  actualFee,
		Success:       receipt.Status == types.ReceiptStatusSuccessful,
		BlockNumber:   int64(blockNumber),
		Confirmations: int64(confirmations),
	})
	if err != nil {
		return fmt.Errorf("结算提币 %d 失败：%w", item.ID, err)
	}
	if changed {
		w.logger.Info("Sepolia 提币已完成费用结算", "withdrawal_id", result.ID, "status", result.Status, "confirmations", confirmations)
	}
	return nil
}

// quoteFor 查询提币出款地址并生成最新费用估算。
func (w *Worker) quoteFor(ctx context.Context, item postgres.Withdrawal) (postgres.WalletAddress, FeeQuote, error) {
	walletAddress, err := w.store.WalletAddressByUser(ctx, item.UserID)
	if err != nil {
		return postgres.WalletAddress{}, FeeQuote{}, fmt.Errorf("查询提币托管地址失败：%w", err)
	}
	if walletAddress.ID != item.AddressID {
		return postgres.WalletAddress{}, FeeQuote{}, errors.New("提币记录与托管地址不匹配")
	}
	quote, err := estimateFee(ctx, w.chain, common.HexToAddress(walletAddress.Address), common.HexToAddress(item.ToAddress), item.AmountWei)
	return walletAddress, quote, err
}

// recordError 保存经过清洗的提币 Worker 错误，记录失败不覆盖原业务错误。
func (w *Worker) recordError(ctx context.Context, withdrawalID int64, stage, code string, workerErr error) {
	referenceID := withdrawalID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker:        "withdrawal-worker",
		Stage:         stage,
		ReferenceType: "WITHDRAWAL",
		ReferenceID:   &referenceID,
		ErrorCode:     code,
		ErrorMessage:  workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录提币 Worker 错误失败", "withdrawal_id", withdrawalID, "error", err)
	}
}

// alreadyKnown 判断 RPC 错误链中是否表示节点已经接收相同交易。
func alreadyKnown(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		if strings.Contains(message, "already known") || strings.Contains(message, "known transaction") {
			return true
		}
	}
	return false
}
