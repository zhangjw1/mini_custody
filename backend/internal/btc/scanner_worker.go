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
	ListBTCDepositsAfterBlock(context.Context, int64) ([]ConfirmingDeposit, error)
	UpdateBTCDepositConfirmations(context.Context, int64, int64) error
	CreditBTCDeposit(context.Context, int64, int64) error
	RewindBTCCheckpointWithDeposits(context.Context, int64, string, []int64) error
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
			if rewindErr := s.rewindAndOrphanChanged(ctx, rewind); rewindErr != nil {
				return rewindErr
			}
			start = rewind + 1
		}
	}
	var scanErr error
	for n := int64(0); n < s.batch && start+n <= tip; n++ {
		h := start + n
		hash, err := s.chain.BlockHash(ctx, h)
		if err != nil {
			scanErr = err
			break
		}
		block, err := s.chain.Block(ctx, hash)
		if err != nil {
			scanErr = err
			break
		}
		obs, err := ScanBlock(block, s.addresses)
		if err != nil {
			scanErr = err
			break
		}
		if _, err = s.store.RecordBTCDepositsAndCheckpoint(ctx, obs, Checkpoint{Network: Network, Scanner: "btc-deposit", LastScannedBlock: h, LastScannedHash: hash}); err != nil {
			scanErr = err
			break
		}
	}
	confirmErr := s.confirm(ctx, tip)
	return errors.Join(scanErr, confirmErr)
}

// rewindAndOrphanChanged 先核查回退目标块与各充值所在块的最新主链哈希，仅对哈希实际发生变化的充值执行冲正。
func (s *Scanner) rewindAndOrphanChanged(ctx context.Context, rewind int64) error {
	rewindHash, err := s.chain.BlockHash(ctx, rewind)
	if err != nil {
		return fmt.Errorf("查询 BTC 回退区块 %d 哈希失败：%w", rewind, err)
	}
	deposits, err := s.store.ListBTCDepositsAfterBlock(ctx, rewind)
	if err != nil {
		return fmt.Errorf("查询回退区间 BTC 充值失败：%w", err)
	}
	chainHashes := make(map[int64]string, len(deposits))
	changedIDs := make([]int64, 0)
	for _, dep := range deposits {
		chainHash, ok := chainHashes[dep.BlockHeight]
		if !ok {
			chainHash, err = s.chain.BlockHash(ctx, dep.BlockHeight)
			if err != nil {
				return fmt.Errorf("复核 BTC 充值区块 %d 哈希失败：%w", dep.BlockHeight, err)
			}
			chainHashes[dep.BlockHeight] = chainHash
		}
		if chainHash != dep.BlockHash {
			changedIDs = append(changedIDs, dep.ID)
		}
	}
	return s.store.RewindBTCCheckpointWithDeposits(ctx, rewind, rewindHash, changedIDs)
}

// confirm 更新确认数并在复核原区块哈希后完成入账，回退块哈希查询失败时直接中止而不误孤立充值。
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
			last, _, cpErr := s.store.BTCCheckpoint(ctx)
			if cpErr != nil {
				s.recordDepositError(ctx, "rewind_checkpoint", item.ID, "BTC_REORG_REWIND_FAILED", cpErr.Error())
				return fmt.Errorf("查询 BTC 检查点失败，停止孤立充值 %d：%w", item.ID, cpErr)
			}
			if last >= item.BlockHeight {
				if rewindErr := s.rewindAndOrphanChanged(ctx, rewind); rewindErr != nil {
					s.recordDepositError(ctx, "rewind_checkpoint", item.ID, "BTC_REORG_REWIND_FAILED", rewindErr.Error())
					return rewindErr
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
