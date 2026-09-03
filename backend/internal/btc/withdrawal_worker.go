package btc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// WithdrawalChain 定义 BTC 提币 worker 的链访问能力。
type WithdrawalChain interface {
	SendRawTransaction(context.Context, string) (string, error)
	TestMempoolAccept(context.Context, string) (bitcoin.MempoolAcceptResult, error)
	RawTransaction(context.Context, string) (bitcoin.RawTransaction, error)
	Block(context.Context, string) (bitcoin.Block, error)
}

// WithdrawalWorkerStore 定义 BTC 提币状态持久化边界。
type WithdrawalWorkerStore interface {
	ListProcessableBTCWithdrawals(context.Context, int) ([]Withdrawal, error)
	LockWithdrawalInputs(context.Context, int64, int64, string) ([]UTXO, []Address, []byte, []byte, error)
	SaveBTCWithdrawalSigned(context.Context, int64, []byte, int64, int64) (Withdrawal, error)
	MarkBTCWithdrawalBroadcastUnknown(context.Context, int64, string) error
	MarkBTCWithdrawalBroadcasted(context.Context, int64, string) error
	CompleteBTCWithdrawal(context.Context, int64, int64, int64) error
	FailBTCWithdrawal(context.Context, int64, string, string) error
}

// WithdrawalWorker 负责 BTC 提币签名、广播和确认恢复。
type WithdrawalWorker struct {
	chain         WithdrawalChain
	store         WithdrawalWorkerStore
	provider      *wallet.MnemonicKeyProvider
	interval      time.Duration
	confirmations int64
	logger        *slog.Logger
}

// NewWithdrawalWorker 创建 BTC 提币 worker。
func NewWithdrawalWorker(chain WithdrawalChain, store WithdrawalWorkerStore, provider *wallet.MnemonicKeyProvider, interval time.Duration, confirmations int64, logger *slog.Logger) (*WithdrawalWorker, error) {
	if chain == nil || store == nil || provider == nil || interval <= 0 || confirmations <= 0 {
		return nil, errors.New("BTC 提币 worker 配置无效")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WithdrawalWorker{chain: chain, store: store, provider: provider, interval: interval, confirmations: confirmations, logger: logger}, nil
}

// Run 周期处理全部可恢复 BTC 提币任务。
func (w *WithdrawalWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("BTC 提币 worker 失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 逐条推进 BTC 提币任务。
func (w *WithdrawalWorker) RunOnce(ctx context.Context) error {
	items, err := w.store.ListProcessableBTCWithdrawals(ctx, 100)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := w.Process(ctx, item); err != nil {
			w.logger.Warn("BTC 提币处理失败", "withdrawal_id", item.ID, "error", err)
		}
	}
	return nil
}

// Process 恢复并推进一笔 BTC 提币任务。
func (w *WithdrawalWorker) Process(ctx context.Context, item Withdrawal) error {
	switch item.Status {
	case "CREATED":
		inputs, addresses, targetScript, changeScript, err := w.store.LockWithdrawalInputs(ctx, item.ID, item.AmountSats, "withdrawal-"+fmt.Sprint(item.ID))
		if err != nil {
			return err
		}
		raw, fee, changeAmount, buildErr := BuildWithdrawal(w.provider, inputs, addresses, targetScript, changeScript, item.AmountSats, item.FeeRateSatVB)
		if buildErr != nil {
			return w.store.FailBTCWithdrawal(ctx, item.ID, "BUILD_FAILED", buildErr.Error())
		}
		item, err = w.store.SaveBTCWithdrawalSigned(ctx, item.ID, raw, fee, changeAmount)
		if err != nil {
			return err
		}
		fallthrough
	case "SIGNED", "BROADCAST_UNKNOWN":
		return w.broadcast(ctx, item)
	case "BROADCASTED", "CONFIRMING":
		if item.TxID == "" {
			return errors.New("BTC 提币缺少 txid")
		}
		tx, err := w.chain.RawTransaction(ctx, item.TxID)
		if err != nil {
			return err
		}
		if tx.Confirmations < int64(w.confirmations) {
			return nil
		}
		block, err := w.chain.Block(ctx, tx.BlockHash)
		if err != nil {
			return err
		}
		return w.store.CompleteBTCWithdrawal(ctx, item.ID, tx.Confirmations, block.Height)
	case "COMPLETED":
		return nil
	default:
		return errors.New("BTC 提币状态不可恢复")
	}
}

// broadcast 对已签名提币执行 mempool 检查和广播。
func (w *WithdrawalWorker) broadcast(ctx context.Context, item Withdrawal) error {
	if len(item.RawTx) == 0 {
		return errors.New("BTC 提币缺少已签名 raw_tx")
	}
	rawHex := hex.EncodeToString(item.RawTx)
	if item.Status == "SIGNED" {
		check, err := w.chain.TestMempoolAccept(ctx, rawHex)
		if err != nil {
			return err
		}
		if !check.Allowed {
			return w.store.FailBTCWithdrawal(ctx, item.ID, "MEMPOOL_REJECTED", check.RejectReason)
		}
	}
	txid, err := w.chain.SendRawTransaction(ctx, rawHex)
	if err != nil {
		_ = w.store.MarkBTCWithdrawalBroadcastUnknown(ctx, item.ID, err.Error())
		return err
	}
	return w.store.MarkBTCWithdrawalBroadcasted(ctx, item.ID, txid)
}
