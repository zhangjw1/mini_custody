package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

const confirmBatchSize = 200

var (
	ErrStartBlockRequired = errors.New("数据库中没有 Token 扫描检查点，必须配置 ERC20_SCAN_START_BLOCK")
	ErrBlockHashMismatch  = errors.New("Token 充值所在区块哈希发生变化，已暂停自动入账")
)

// Chain 定义 Token 充值扫描所需的区块链查询能力。
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
}

// Contract 定义 Token Event 扫描和解码能力。
type Contract interface {
	Address() common.Address
	FilterTransferLogs(ctx context.Context, fromBlock, toBlock uint64) ([]types.Log, error)
	DecodeTransferLog(log types.Log) (erc20.TransferEvent, error)
}

// Store 定义 Token 充值扫描所需的持久化事务能力。
type Store interface {
	ListWalletAddresses(ctx context.Context) ([]postgres.WalletAddress, error)
	Checkpoint(ctx context.Context, network, scanner string) (postgres.ChainCheckpoint, error)
	RecordTokenDepositsAndCheckpoint(ctx context.Context, observations []postgres.TokenDepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.TokenDeposit, int, error)
	ListConfirmingTokenDeposits(ctx context.Context, assetID int64, limit int) ([]postgres.TokenDeposit, error)
	UpdateTokenDepositConfirmations(ctx context.Context, depositID, confirmations int64) (postgres.TokenDeposit, error)
	CreditTokenDeposit(ctx context.Context, depositID, confirmations int64) (postgres.TokenDeposit, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// Config 定义 Token 扫描资产、起点、批次、确认数和轮询间隔。
type Config struct {
	AssetID       int64
	StartBlock    *uint64
	BatchSize     uint64
	Confirmations uint64
	Interval      time.Duration
	ScannerName   string
}

// HealthSnapshot 描述 Token 扫描器网络高度、扫描进度和最近错误。
type HealthSnapshot struct {
	Status        string
	NetworkHeight uint64
	ScanHeight    uint64
	LagBlocks     uint64
	LastError     string
	CheckedAt     time.Time
}

// Scanner 扫描配置合约 Transfer Event 并完成 Token 充值确认入账。
type Scanner struct {
	chain     Chain
	contract  Contract
	store     Store
	logger    *slog.Logger
	config    Config
	addresses map[common.Address]postgres.WalletAddress
	healthMu  sync.RWMutex
	health    HealthSnapshot
}

// NewScanner 创建并校验 Token 充值扫描器。
func NewScanner(chain Chain, contract Contract, store Store, logger *slog.Logger, cfg Config) (*Scanner, error) {
	if chain == nil || contract == nil || store == nil || logger == nil {
		return nil, errors.New("Token 充值扫描器依赖不能为空")
	}
	if cfg.AssetID <= 0 || cfg.BatchSize == 0 || cfg.BatchSize > 1000 || cfg.Confirmations == 0 || cfg.Interval <= 0 {
		return nil, errors.New("Token 充值扫描器配置无效")
	}
	if strings.TrimSpace(cfg.ScannerName) == "" {
		cfg.ScannerName = "erc20:" + strings.ToLower(contract.Address().Hex())
	}
	return &Scanner{
		chain: chain, contract: contract, store: store, logger: logger, config: cfg,
		addresses: make(map[common.Address]postgres.WalletAddress),
		health:    HealthSnapshot{Status: "DEGRADED"},
	}, nil
}

// Initialize 加载地址索引和 Token 持久化检查点。
func (s *Scanner) Initialize(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 {
			return errors.New("数据库中的 Token 扫描检查点无效")
		}
		s.setScanHeight(uint64(checkpoint.LastScannedBlock))
		return nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return fmt.Errorf("读取 Token 扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return ErrStartBlockRequired
	}
	return nil
}

// Run 周期执行 Token 充值扫描，安全错误会停止 Worker。
func (s *Scanner) Run(ctx context.Context) error {
	if err := s.Initialize(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if errors.Is(err, ErrBlockHashMismatch) || errors.Is(err, ErrStartBlockRequired) || errors.Is(err, erc20.ErrRemovedLog) {
				return err
			}
			s.logger.Warn("Token 充值扫描本轮失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 执行一轮 Token Event 扫描和待确认充值入账。
func (s *Scanner) RunOnce(ctx context.Context) error {
	err := s.runOnce(ctx)
	if err != nil {
		s.markFailure(err)
		return err
	}
	s.markSuccess()
	return nil
}

// runOnce 执行一轮扫描的实际步骤。
func (s *Scanner) runOnce(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	latest, err := s.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Token 网络最新区块失败：%w", err)
	}
	s.setNetworkHeight(latest)
	next, err := s.nextBlock(ctx)
	if err != nil {
		return err
	}
	if next <= latest {
		last := latest
		if remaining := s.config.BatchSize - 1; remaining < latest-next {
			last = next + remaining
		}
		if err := s.scanRange(ctx, next, last); err != nil {
			return err
		}
	}
	return s.confirmPending(ctx, latest)
}

// refreshAddresses 重建用户托管地址内存索引。
func (s *Scanner) refreshAddresses(ctx context.Context) error {
	addresses, err := s.store.ListWalletAddresses(ctx)
	if err != nil {
		return fmt.Errorf("加载 Token 托管地址失败：%w", err)
	}
	index := make(map[common.Address]postgres.WalletAddress, len(addresses))
	for _, address := range addresses {
		if !common.IsHexAddress(address.Address) {
			return fmt.Errorf("Token 托管地址格式无效：address_id=%d", address.ID)
		}
		index[common.HexToAddress(address.Address)] = address
	}
	s.addresses = index
	return nil
}

// nextBlock 从数据库检查点下一高度恢复，首次启动使用显式配置起点。
func (s *Scanner) nextBlock(ctx context.Context) (uint64, error) {
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 || checkpoint.LastScannedBlock == math.MaxInt64 {
			return 0, errors.New("数据库中的 Token 扫描检查点无效")
		}
		height := uint64(checkpoint.LastScannedBlock)
		s.setScanHeight(height)
		return height + 1, nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return 0, fmt.Errorf("读取 Token 扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return 0, ErrStartBlockRequired
	}
	return *s.config.StartBlock, nil
}

// scanRange 查询并提交一个区块闭区间，范围超限时递归缩小批次。
func (s *Scanner) scanRange(ctx context.Context, fromBlock, toBlock uint64) error {
	logs, err := s.contract.FilterTransferLogs(ctx, fromBlock, toBlock)
	if err != nil {
		if evm.IsLogRangeTooLarge(err) && fromBlock < toBlock {
			middle := fromBlock + (toBlock-fromBlock)/2
			if err := s.scanRange(ctx, fromBlock, middle); err != nil {
				return err
			}
			return s.scanRange(ctx, middle+1, toBlock)
		}
		return err
	}
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].BlockNumber != logs[j].BlockNumber {
			return logs[i].BlockNumber < logs[j].BlockNumber
		}
		if logs[i].TxIndex != logs[j].TxIndex {
			return logs[i].TxIndex < logs[j].TxIndex
		}
		return logs[i].Index < logs[j].Index
	})
	observations := make([]postgres.TokenDepositObservation, 0)
	for _, log := range logs {
		event, err := s.contract.DecodeTransferLog(log)
		if err != nil {
			return fmt.Errorf("解码 Token Transfer Event 失败：%w", err)
		}
		if event.BlockNumber < fromBlock || event.BlockNumber > toBlock || event.LogIndex > math.MaxInt32 {
			return errors.New("Token Transfer Event 区块或日志索引无效")
		}
		address, monitored := s.addresses[event.To]
		if !monitored {
			continue
		}
		observations = append(observations, postgres.TokenDepositObservation{
			UserID: address.UserID, AddressID: address.ID, AssetID: s.config.AssetID,
			TxHash: strings.ToLower(event.TxHash.Hex()), LogIndex: int32(event.LogIndex),
			BlockNumber: int64(event.BlockNumber), BlockHash: strings.ToLower(event.BlockHash.Hex()),
			FromAddress: strings.ToLower(event.From.Hex()), ToAddress: strings.ToLower(event.To.Hex()),
			AmountUnits: new(big.Int).Set(event.AmountUnits),
		})
	}
	header, err := s.chain.HeaderByNumber(ctx, new(big.Int).SetUint64(toBlock))
	if err != nil {
		return fmt.Errorf("读取 Token 扫描批次末区块失败：%w", err)
	}
	if header == nil || header.Number == nil || header.Number.Uint64() != toBlock {
		return errors.New("Token 扫描批次末区块返回无效")
	}
	_, created, err := s.store.RecordTokenDepositsAndCheckpoint(ctx, observations, postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: s.config.ScannerName,
		LastScannedBlock: int64(toBlock), LastScannedHash: strings.ToLower(header.Hash().Hex()),
	})
	if err != nil {
		return fmt.Errorf("保存 Token 扫描批次失败：%w", err)
	}
	s.setScanHeight(toBlock)
	if created > 0 {
		s.logger.Info("发现 Token 托管钱包充值", "to_block", toBlock, "deposit_count", created)
	}
	return nil
}

// confirmPending 更新确认数，复核区块哈希后幂等完成 Token 入账。
func (s *Scanner) confirmPending(ctx context.Context, latest uint64) error {
	deposits, err := s.store.ListConfirmingTokenDeposits(ctx, s.config.AssetID, confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询待确认 Token 充值失败：%w", err)
	}
	for _, deposit := range deposits {
		if deposit.BlockNumber < 0 || uint64(deposit.BlockNumber) > latest {
			continue
		}
		confirmations := latest - uint64(deposit.BlockNumber) + 1
		if confirmations > math.MaxInt64 {
			return errors.New("Token 充值确认数超出数据库范围")
		}
		if confirmations < s.config.Confirmations {
			if _, err := s.store.UpdateTokenDepositConfirmations(ctx, deposit.ID, int64(confirmations)); err != nil {
				return fmt.Errorf("更新 Token 充值 %d 确认数失败：%w", deposit.ID, err)
			}
			continue
		}
		if err := s.verifyBlockHash(ctx, deposit); err != nil {
			return err
		}
		if _, credited, err := s.store.CreditTokenDeposit(ctx, deposit.ID, int64(confirmations)); err != nil {
			return fmt.Errorf("Token 充值 %d 入账失败：%w", deposit.ID, err)
		} else if credited {
			s.logger.Info("Token 充值已确认入账", "deposit_id", deposit.ID, "confirmations", confirmations)
		}
	}
	return nil
}

// verifyBlockHash 入账前复核 Token Event 原区块哈希。
func (s *Scanner) verifyBlockHash(ctx context.Context, deposit postgres.TokenDeposit) error {
	header, err := s.chain.HeaderByNumber(ctx, big.NewInt(deposit.BlockNumber))
	if err != nil {
		return fmt.Errorf("复核 Token 充值 %d 原区块失败：%w", deposit.ID, err)
	}
	if header != nil && strings.EqualFold(header.Hash().Hex(), deposit.BlockHash) {
		return nil
	}
	message := fmt.Sprintf("Token 充值 %d 所在区块 %d 哈希与首次扫描结果不一致", deposit.ID, deposit.BlockNumber)
	referenceID := deposit.ID
	if _, err := s.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "token-deposit-confirmer", Stage: "block_hash_check", ReferenceType: "TOKEN_DEPOSIT",
		ReferenceID: &referenceID, ErrorCode: "BLOCK_HASH_MISMATCH", ErrorMessage: message,
	}); err != nil {
		return fmt.Errorf("%s，且记录后台错误失败：%w", message, err)
	}
	return fmt.Errorf("Token 充值入账暂停：%w：%s", ErrBlockHashMismatch, message)
}

// Snapshot 返回不发起网络请求的 Token 扫描健康快照。
func (s *Scanner) Snapshot() HealthSnapshot {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health
}

// setNetworkHeight 更新 Token 扫描器观察到的最新网络高度。
func (s *Scanner) setNetworkHeight(height uint64) {
	s.healthMu.Lock()
	s.health.NetworkHeight = height
	s.updateLagLocked()
	s.health.CheckedAt = time.Now()
	s.healthMu.Unlock()
}

// setScanHeight 更新 Token 扫描器已经完整提交的检查点高度。
func (s *Scanner) setScanHeight(height uint64) {
	s.healthMu.Lock()
	s.health.ScanHeight = height
	s.updateLagLocked()
	s.health.CheckedAt = time.Now()
	s.healthMu.Unlock()
}

// markFailure 记录不会泄露 RPC URL 的最近扫描错误。
func (s *Scanner) markFailure(err error) {
	s.healthMu.Lock()
	s.health.Status = "DOWN"
	s.health.LastError = err.Error()
	s.health.CheckedAt = time.Now()
	s.healthMu.Unlock()
}

// markSuccess 清理最近错误并标记 Token 扫描健康。
func (s *Scanner) markSuccess() {
	s.healthMu.Lock()
	s.health.Status = "HEALTHY"
	s.health.LastError = ""
	s.health.CheckedAt = time.Now()
	s.healthMu.Unlock()
}

// updateLagLocked 在持有健康状态写锁时重新计算落后区块数。
func (s *Scanner) updateLagLocked() {
	if s.health.NetworkHeight > s.health.ScanHeight {
		s.health.LagBlocks = s.health.NetworkHeight - s.health.ScanHeight
	} else {
		s.health.LagBlocks = 0
	}
}
