package btc

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// SweepChain 定义归集 worker 所需的最小链访问能力。
type SweepChain interface {
	SendRawTransaction(context.Context, string) (string, error)
	RawTransaction(context.Context, string) (bitcoin.RawTransaction, error)
	Block(context.Context, string) (bitcoin.Block, error)
	TestMempoolAccept(context.Context, string) (bitcoin.MempoolAcceptResult, error)
}

// SweepStore 定义归集 worker 的持久化状态边界。
type SweepStore interface {
	ListProcessableBTCSweeps(context.Context, int) ([]Sweep, error)
	SaveBTCSweepSigned(context.Context, int64, []byte, string, int64, int64, int64) (Sweep, error)
	MarkBTCSweepBroadcasted(context.Context, int64, string) (Sweep, error)
	CompleteBTCSweep(context.Context, int64, int64, int64) error
	MarkBTCSweepBroadcastUnknown(context.Context, int64, string) error
	FailBTCSweep(context.Context, int64, string, string) error
}

// SweepWorker 负责签名、持久化和广播 BTC 归集任务。
type SweepWorker struct {
	chain         SweepChain
	store         SweepStore
	provider      *wallet.MnemonicKeyProvider
	interval      time.Duration
	confirmations int64
	feeRate       int64
	logger        *slog.Logger
}

// NewSweepWorker 创建 BTC 归集 worker。
func NewSweepWorker(chain SweepChain, store SweepStore, provider *wallet.MnemonicKeyProvider, interval time.Duration, feeRate, confirmations int64, logger *slog.Logger) (*SweepWorker, error) {
	if chain == nil || store == nil || provider == nil || interval <= 0 || feeRate <= 0 || confirmations <= 0 {
		return nil, errors.New("BTC 归集 worker 配置无效")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SweepWorker{chain: chain, store: store, provider: provider, interval: interval, feeRate: feeRate, confirmations: confirmations, logger: logger}, nil
}

// Run 周期处理全部可恢复归集任务。
func (w *SweepWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("BTC 归集 worker 失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 逐条推进当前可处理归集任务。
func (w *SweepWorker) RunOnce(ctx context.Context) error {
	items, err := w.store.ListProcessableBTCSweeps(ctx, 100)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := w.Process(ctx, item); err != nil {
			w.logger.Warn("BTC 归集任务处理失败", "sweep_id", item.ID, "error", err)
		}
	}
	return nil
}

// Process 恢复并推进一笔 BTC 归集任务。
func (w *SweepWorker) Process(ctx context.Context, item Sweep) error {
	switch item.Status {
	case SweepCreated:
		built, err := BuildSweep(w.provider, item.From.Path, item.UTXO, item.To, w.feeRate)
		if err != nil {
			return err
		}
		persisted, err := w.store.SaveBTCSweepSigned(ctx, item.ID, built.RawTx, built.TxID, built.OutputValueSats, built.FeeSats, built.FeeRateSatVB)
		if err != nil {
			return err
		}
		item = persisted
		fallthrough
	case SweepSigned, "BROADCAST_UNKNOWN":
		if len(item.RawTx) == 0 {
			return errors.New("已签名 BTC 归集缺少原始交易")
		}
		rawHex := hex.EncodeToString(item.RawTx)
		if item.Status == SweepSigned {
			check, checkErr := w.chain.TestMempoolAccept(ctx, rawHex)
			if checkErr != nil {
				return checkErr
			}
			if !check.Allowed {
				return w.store.FailBTCSweep(ctx, item.ID, "MEMPOOL_REJECTED", check.RejectReason)
			}
		}
		txid, err := w.chain.SendRawTransaction(ctx, rawHex)
		if err != nil {
			if markErr := w.store.MarkBTCSweepBroadcastUnknown(ctx, item.ID, err.Error()); markErr != nil {
				return markErr
			}
			return err
		}
		_, err = w.store.MarkBTCSweepBroadcasted(ctx, item.ID, txid)
		return err
	case SweepBroadcasted, SweepConfirming, SweepCompleted:
		if item.Status == SweepCompleted {
			return nil
		}
		if item.TxID == "" {
			return errors.New("BTC 归集缺少 txid")
		}
		transaction, err := w.chain.RawTransaction(ctx, item.TxID)
		if err != nil {
			return err
		}
		if transaction.Confirmations < w.confirmations {
			return nil
		}
		block, err := w.chain.Block(ctx, transaction.BlockHash)
		if err != nil {
			return err
		}
		return w.store.CompleteBTCSweep(ctx, item.ID, transaction.Confirmations, block.Height)
	default:
		return errors.New("BTC 归集状态不可恢复")
	}
}
