package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

const (
	defaultScannerName = "eth-deposit"
	confirmBatchSize   = 200
)

var (
	ErrStartBlockRequired = errors.New("数据库中没有充值扫描检查点，必须配置 SEPOLIA_SCAN_START_BLOCK")
	ErrBlockHashMismatch  = errors.New("充值所在区块哈希发生变化，已暂停自动入账")
)

// ChainClient 定义充值扫描所需的最小 EVM 链访问能力。
type ChainClient interface {
	BlockNumber(ctx context.Context) (uint64, error)
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SetScanHeight(height uint64)
}

// Store 定义充值扫描所需的持久化和余额事务能力。
type Store interface {
	ListWalletAddresses(ctx context.Context) ([]postgres.WalletAddress, error)
	Checkpoint(ctx context.Context, network, scanner string) (postgres.ChainCheckpoint, error)
	RecordDepositsAndCheckpoint(ctx context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error)
	ListConfirmingDeposits(ctx context.Context, limit int) ([]postgres.Deposit, error)
	UpdateDepositConfirmations(ctx context.Context, depositID, confirmations int64) (postgres.Deposit, error)
	CreditDeposit(ctx context.Context, depositID, confirmations int64) (postgres.Deposit, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// Config 定义 Sepolia 充值扫描起点、确认数和轮询策略。
type Config struct {
	StartBlock    *uint64
	Confirmations uint64
	BatchSize     uint64
	Interval      time.Duration
	ScannerName   string
}

// Scanner 扫描 Sepolia 区块并将普通 ETH 充值安全记入平台账本。
type Scanner struct {
	chain     ChainClient
	store     Store
	logger    *slog.Logger
	config    Config
	addresses map[common.Address]postgres.WalletAddress
}

// NewScanner 创建并校验充值扫描器依赖和配置。
func NewScanner(chain ChainClient, store Store, logger *slog.Logger, cfg Config) (*Scanner, error) {
	if chain == nil || store == nil || logger == nil {
		return nil, errors.New("充值扫描器依赖不能为空")
	}
	if cfg.Confirmations == 0 || cfg.BatchSize == 0 || cfg.BatchSize > 100 || cfg.Interval <= 0 {
		return nil, errors.New("充值扫描器配置无效")
	}
	if strings.TrimSpace(cfg.ScannerName) == "" {
		cfg.ScannerName = defaultScannerName
	}
	return &Scanner{
		chain:     chain,
		store:     store,
		logger:    logger,
		config:    cfg,
		addresses: make(map[common.Address]postgres.WalletAddress),
	}, nil
}

// Initialize 加载托管地址和持久化检查点，并在首次启动时校验扫描起点。
func (s *Scanner) Initialize(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 {
			return errors.New("数据库中的充值扫描检查点无效")
		}
		s.chain.SetScanHeight(uint64(checkpoint.LastScannedBlock))
		return nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return fmt.Errorf("读取充值扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return ErrStartBlockRequired
	}
	return nil
}

// Run 按配置间隔持续执行扫描，临时错误等待下轮重试，安全错误立即停止。
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
			if errors.Is(err, ErrBlockHashMismatch) || errors.Is(err, ErrStartBlockRequired) {
				return err
			}
			s.logger.Warn("Sepolia 充值扫描本轮失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 执行一轮区块发现和充值确认，便于测试和人工触发。
func (s *Scanner) RunOnce(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	latest, err := s.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Sepolia 最新区块失败：%w", err)
	}
	next, err := s.nextBlock(ctx)
	if err != nil {
		return err
	}
	if next <= latest {
		last := latest
		if remaining := s.config.BatchSize - 1; remaining < latest-next {
			last = next + remaining
		}
		for height := next; ; height++ {
			if err := s.scanBlock(ctx, height); err != nil {
				return err
			}
			if height == last {
				break
			}
		}
	}
	return s.confirmPending(ctx, latest)
}

// refreshAddresses 从数据库重建托管地址索引，使新增地址无需重启即可被扫描。
func (s *Scanner) refreshAddresses(ctx context.Context) error {
	addresses, err := s.store.ListWalletAddresses(ctx)
	if err != nil {
		return fmt.Errorf("加载托管钱包地址失败：%w", err)
	}
	index := make(map[common.Address]postgres.WalletAddress, len(addresses))
	for _, address := range addresses {
		if !common.IsHexAddress(address.Address) {
			return fmt.Errorf("托管钱包地址格式无效：address_id=%d", address.ID)
		}
		index[common.HexToAddress(address.Address)] = address
	}
	s.addresses = index
	return nil
}

// nextBlock 根据数据库检查点或首次启动配置计算下一待扫描高度。
func (s *Scanner) nextBlock(ctx context.Context) (uint64, error) {
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 || checkpoint.LastScannedBlock == math.MaxInt64 {
			return 0, errors.New("数据库中的充值扫描检查点无效")
		}
		height := uint64(checkpoint.LastScannedBlock)
		s.chain.SetScanHeight(height)
		return height + 1, nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return 0, fmt.Errorf("读取充值扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return 0, ErrStartBlockRequired
	}
	return *s.config.StartBlock, nil
}

// scanBlock 识别一个完整区块中的托管地址普通 ETH 转账并原子推进检查点。
func (s *Scanner) scanBlock(ctx context.Context, height uint64) error {
	if height > math.MaxInt64 {
		return errors.New("Sepolia 区块高度超出数据库范围")
	}
	block, err := s.chain.BlockByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		return fmt.Errorf("读取 Sepolia 区块 %d 失败：%w", height, err)
	}
	if block == nil || block.NumberU64() != height {
		return fmt.Errorf("Sepolia 区块 %d 返回内容无效", height)
	}
	blockHash := strings.ToLower(block.Hash().Hex())
	observations := make([]postgres.DepositObservation, 0)
	for index, transaction := range block.Transactions() {
		to := transaction.To()
		if to == nil || transaction.Value().Sign() <= 0 {
			continue
		}
		address, monitored := s.addresses[*to]
		if !monitored {
			continue
		}
		observations = append(observations, postgres.DepositObservation{
			UserID:      address.UserID,
			AddressID:   address.ID,
			TxHash:      strings.ToLower(transaction.Hash().Hex()),
			TxIndex:     int32(index),
			BlockNumber: int64(height),
			BlockHash:   blockHash,
			AmountWei:   new(big.Int).Set(transaction.Value()),
		})
	}
	_, created, err := s.store.RecordDepositsAndCheckpoint(ctx, observations, postgres.ChainCheckpoint{
		Network:          postgres.NetworkSepolia,
		Scanner:          s.config.ScannerName,
		LastScannedBlock: int64(height),
		LastScannedHash:  blockHash,
	})
	if err != nil {
		return fmt.Errorf("保存 Sepolia 区块 %d 充值结果失败：%w", height, err)
	}
	s.chain.SetScanHeight(height)
	if created > 0 {
		s.logger.Info("发现 Sepolia 托管钱包充值", "block_number", height, "deposit_count", created)
	}
	return nil
}

// confirmPending 更新确认数，并在复核原区块哈希后完成充值入账。
func (s *Scanner) confirmPending(ctx context.Context, latest uint64) error {
	deposits, err := s.store.ListConfirmingDeposits(ctx, confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询待确认充值失败：%w", err)
	}
	for _, deposit := range deposits {
		if deposit.BlockNumber < 0 || uint64(deposit.BlockNumber) > latest {
			continue
		}
		confirmations := latest - uint64(deposit.BlockNumber) + 1
		if confirmations > math.MaxInt64 {
			return errors.New("充值确认数超出数据库范围")
		}
		if confirmations < s.config.Confirmations {
			if _, err := s.store.UpdateDepositConfirmations(ctx, deposit.ID, int64(confirmations)); err != nil {
				return fmt.Errorf("更新充值 %d 确认数失败：%w", deposit.ID, err)
			}
			continue
		}
		if err := s.verifyBlockHash(ctx, deposit); err != nil {
			return err
		}
		if _, credited, err := s.store.CreditDeposit(ctx, deposit.ID, int64(confirmations)); err != nil {
			return fmt.Errorf("充值 %d 入账失败：%w", deposit.ID, err)
		} else if credited {
			s.logger.Info("Sepolia 充值已确认入账", "deposit_id", deposit.ID, "confirmations", confirmations)
		}
	}
	return nil
}

// verifyBlockHash 入账前复核充值原区块哈希，防止链重组导致错误入账。
func (s *Scanner) verifyBlockHash(ctx context.Context, deposit postgres.Deposit) error {
	header, err := s.chain.HeaderByNumber(ctx, big.NewInt(deposit.BlockNumber))
	if err != nil {
		return fmt.Errorf("复核充值 %d 原区块失败：%w", deposit.ID, err)
	}
	if header != nil && strings.EqualFold(header.Hash().Hex(), deposit.BlockHash) {
		return nil
	}
	message := fmt.Sprintf("充值 %d 所在区块 %d 的哈希与首次扫描结果不一致", deposit.ID, deposit.BlockNumber)
	referenceID := deposit.ID
	if _, err := s.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker:        "deposit-confirmer",
		Stage:         "block_hash_check",
		ReferenceType: "DEPOSIT",
		ReferenceID:   &referenceID,
		ErrorCode:     "BLOCK_HASH_MISMATCH",
		ErrorMessage:  message,
	}); err != nil {
		return fmt.Errorf("%s，且记录后台任务错误失败：%w", message, err)
	}
	return fmt.Errorf("充值入账暂停：%w：%s", ErrBlockHashMismatch, message)
}
