package btc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
)

type Chain interface {
	BlockCount(context.Context) (int64, error)
	BlockHash(context.Context, int64) (string, error)
	Block(context.Context, string) (bitcoin.Block, error)
}
type ScannerStore interface {
	ListBTCAddresses(context.Context) (map[string]Address, error)
	BTCCheckpoint(context.Context) (int64, string, error)
	RecordBTCDepositsAndCheckpoint(context.Context, []DepositObservation, Checkpoint) (int, error)
	ListConfirmingBTCDeposits(context.Context, int) ([]ConfirmingDeposit, error)
	UpdateBTCDepositConfirmations(context.Context, int64, int64) error
	CreditBTCDeposit(context.Context, int64, int64) error
	RewindBTCCheckpoint(context.Context, int64, string) error
	OrphanBTCDeposit(context.Context, int64) error
	RecordBTCWorkerError(ctx context.Context, stage, referenceType string, referenceID *int64, errorCode, errorMessage string) error
}
type Checkpoint struct {
	Network, Scanner, LastScannedHash string
	LastScannedBlock                  int64
}

// Scanner 按高度扫描 Signet 区块并发现托管地址充值。
type Scanner struct {
	chain         Chain
	store         ScannerStore
	addresses     map[string]Address
	confirmations int64
	startBlock    int64
	batch         int64
	interval      time.Duration
	logger        *slog.Logger
}

// NewScanner 创建 BTC 充值扫描器。
func NewScanner(chain Chain, store ScannerStore, addresses map[string]Address, startBlock, confirmations, batch int64, interval time.Duration, logger *slog.Logger) (*Scanner, error) {
	if chain == nil || store == nil || startBlock < 0 || confirmations <= 0 || batch <= 0 || interval <= 0 {
		return nil, errors.New("BTC 扫描器配置无效")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{chain: chain, store: store, addresses: addresses, startBlock: startBlock, confirmations: confirmations, batch: batch, interval: interval, logger: logger}, nil
}

// Run 持续扫描 BTC 区块，临时错误在下一轮重试。
func (s *Scanner) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Warn("BTC 充值扫描失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 刷新托管地址索引、扫描一批新区块并推进检查点。
func (s *Scanner) RunOnce(ctx context.Context) error {
	if latestAddresses, err := s.store.ListBTCAddresses(ctx); err != nil {
		return err
	} else {
		s.addresses = latestAddresses
	}
	tip, err := s.chain.BlockCount(ctx)
	if err != nil {
		return err
	}
	last, persistedHash, err := s.store.BTCCheckpoint(ctx)
	if err != nil {
		return err
	}
	start := last + 1
	if last < 0 {
		start = s.startBlock
	} else {
		currentHash, hashErr := s.chain.BlockHash(ctx, last)
		if hashErr != nil {
			return hashErr
		}
		if persistedHash != "" && currentHash != persistedHash {
			rewind := last - s.confirmations
			if rewind < 0 {
				rewind = 0
			}
			rewindHash, rewindErr := s.chain.BlockHash(ctx, rewind)
			if rewindErr != nil {
				return rewindErr
			}
			if err = s.store.RewindBTCCheckpoint(ctx, rewind, rewindHash); err != nil {
				return err
			}
			start = rewind + 1
		}
	}
	for n := int64(0); n < s.batch && start+n <= tip; n++ {
		h := start + n
		hash, err := s.chain.BlockHash(ctx, h)
		if err != nil {
			return err
		}
		block, err := s.chain.Block(ctx, hash)
		if err != nil {
			return err
		}
		obs, err := ScanBlock(block, s.addresses)
		if err != nil {
			return err
		}
		if _, err = s.store.RecordBTCDepositsAndCheckpoint(ctx, obs, Checkpoint{Network: Network, Scanner: "btc-deposit", LastScannedBlock: h, LastScannedHash: hash}); err != nil {
			return err
		}
	}
	return s.confirm(ctx, tip)
}

// confirm 更新确认数并在复核原区块哈希后完成入账，单条异常隔离跳过而不阻塞后续入账。
func (s *Scanner) confirm(ctx context.Context, tip int64) error {
	items, err := s.store.ListConfirmingBTCDeposits(ctx, 200)
	if err != nil {
		return err
	}
	for _, item := range items {
		confirmations := Confirmations(tip, item.BlockHeight)
		if confirmations < s.confirmations {
			if err = s.store.UpdateBTCDepositConfirmations(ctx, item.ID, confirmations); err != nil {
				s.recordDepositError(ctx, "update_confirmations", item.ID, "BTC_DEPOSIT_CONFIRMATION_UPDATE_FAILED", err.Error())
				continue
			}
			continue
		}
		hash, hashErr := s.chain.BlockHash(ctx, item.BlockHeight)
		if hashErr != nil {
			s.logger.Warn("复核 BTC 充值区块哈希失败，跳过本条继续后续处理", "deposit_id", item.ID, "block_height", item.BlockHeight, "error", hashErr)
			s.recordDepositError(ctx, "block_hash_check", item.ID, "BTC_BLOCK_HASH_QUERY_FAILED", hashErr.Error())
			continue
		}
		if hash != item.BlockHash {
			msg := fmt.Sprintf("BTC 充值 %d 所在区块 %d 哈希从 %s 变更为 %s，触发孤块冲正", item.ID, item.BlockHeight, item.BlockHash, hash)
			s.logger.Warn("检测到 BTC 孤块充值", "deposit_id", item.ID, "block_height", item.BlockHeight)
			s.recordDepositError(ctx, "block_hash_check", item.ID, "BLOCK_HASH_MISMATCH", msg)
			rewind := item.BlockHeight - s.confirmations
			if rewind < 0 {
				rewind = 0
			}
			if last, _, cpErr := s.store.BTCCheckpoint(ctx); cpErr == nil && last >= item.BlockHeight {
				if rewindHash, rHashErr := s.chain.BlockHash(ctx, rewind); rHashErr == nil {
					if rErr := s.store.RewindBTCCheckpoint(ctx, rewind, rewindHash); rErr != nil {
						s.recordDepositError(ctx, "rewind_checkpoint", item.ID, "BTC_REORG_REWIND_FAILED", rErr.Error())
					}
				}
			}
			if orphanErr := s.store.OrphanBTCDeposit(ctx, item.ID); orphanErr != nil {
				s.recordDepositError(ctx, "orphan_deposit", item.ID, "BTC_ORPHAN_FAILED", orphanErr.Error())
			}
			continue
		}
		if err = s.store.CreditBTCDeposit(ctx, item.ID, confirmations); err != nil {
			s.logger.Warn("BTC 充值入账失败，记录错误并继续后续充值", "deposit_id", item.ID, "error", err)
			s.recordDepositError(ctx, "credit_deposit", item.ID, "BTC_CREDIT_FAILED", err.Error())
			continue
		}
	}
	return nil
}

// recordDepositError 写入 BTC 单条充值异常告警，避免单笔问题阻塞整批确认。
func (s *Scanner) recordDepositError(ctx context.Context, stage string, depositID int64, code, message string) {
	if ctx.Err() != nil {
		return
	}
	refID := depositID
	if err := s.store.RecordBTCWorkerError(ctx, stage, "BTC_DEPOSIT", &refID, code, message); err != nil {
		s.logger.Warn("写入 BTC 后台任务错误失败", "deposit_id", depositID, "error", err)
	}
}

// Confirmations 返回区块在当前 tip 下的确认数。
func Confirmations(tip, height int64) int64 {
	if tip < height {
		return 0
	}
	return tip - height + 1
}
