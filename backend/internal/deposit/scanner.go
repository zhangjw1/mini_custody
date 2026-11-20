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
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

const (
	defaultScannerName = "eth-deposit"
	confirmBatchSize   = evm.DefaultConfirmBatchSize
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

// batchChainClient 定义可选的批量 RPC 查询接口，用于在单轮扫块和确认中合并往返。
type batchChainClient interface {
	BlocksByNumbers(ctx context.Context, numbers []*big.Int) ([]*types.Block, error)
	HeadersByNumbers(ctx context.Context, numbers []*big.Int) ([]*types.Header, error)
}

// Store 定义充值扫描所需的持久化和余额事务能力。
type Store interface {
	ListWalletAddresses(ctx context.Context) ([]postgres.WalletAddress, error)
	Checkpoint(ctx context.Context, network, scanner string) (postgres.ChainCheckpoint, error)
	RewindCheckpoint(ctx context.Context, network, scanner string, block int64, hash string) error
	FilterInternalTransferTxs(ctx context.Context, txHashes []string) (map[string]bool, error)
	RecordDepositsAndCheckpoint(ctx context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error)
	ListConfirmingDeposits(ctx context.Context, limit int) ([]postgres.Deposit, error)
	ListDepositsAfterBlock(ctx context.Context, afterBlock int64, limit int) ([]postgres.Deposit, error)
	UpdateDepositConfirmations(ctx context.Context, depositID, confirmations int64) (postgres.Deposit, error)
	CreditDeposit(ctx context.Context, depositID, confirmations int64) (postgres.Deposit, bool, error)
	OrphanDeposit(ctx context.Context, depositID int64) (postgres.Deposit, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// incrementalAddressStore 定义可选的增量钱包地址加载接口。
type incrementalAddressStore interface {
	ListWalletAddressesAfterID(ctx context.Context, afterID int64) ([]postgres.WalletAddress, error)
}

// batchDepositStore 定义可选的多区块批量充值与检查点提交接口。
type batchDepositStore interface {
	RecordDepositsBatchAndCheckpoint(ctx context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error)
}

// Config 定义 Sepolia 充值扫描起点、确认数、安全深度和重组回退策略。
type Config struct {
	StartBlock        *uint64
	Confirmations     uint64
	SafeDepth         uint64
	ReorgRewindBlocks uint64
	BatchSize         uint64
	Interval          time.Duration
	ScannerName       string
}

// Scanner 扫描 Sepolia 区块并将普通 ETH 充值安全记入平台账本。
type Scanner struct {
	chain         ChainClient
	store         Store
	logger        *slog.Logger
	config        Config
	addresses     map[common.Address]postgres.WalletAddress
	lastAddressID int64
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
	if cfg.ReorgRewindBlocks == 0 {
		cfg.ReorgRewindBlocks = cfg.Confirmations
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

// Run 按配置间隔持续执行扫描，单条或临时异常不退出，仅缺少首次起点配置时停止。
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
			if errors.Is(err, ErrStartBlockRequired) {
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

// RunOnce 执行一轮重组校验、安全深度区块发现和隔离式充值确认。
func (s *Scanner) RunOnce(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	latest, err := s.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Sepolia 最新区块失败：%w", err)
	}
	headerCache := make(map[uint64]*types.Header)
	next, expectedParentHash, scannedHeight, hasReorged, rewoundTo, err := s.prepareScanCursor(ctx, headerCache)
	if err != nil {
		return err
	}
	canScan := latest >= s.config.SafeDepth
	scanLimit := uint64(0)
	if canScan {
		scanLimit = latest - s.config.SafeDepth
	}
	for attempt := 0; attempt < 2 && canScan && next <= scanLimit; attempt++ {
		last := scanLimit
		if remaining := s.config.BatchSize - 1; remaining < scanLimit-next {
			last = next + remaining
		}
		lastHash, parentMismatch, mismatchHeight, scanErr := s.scanBatch(ctx, next, last, expectedParentHash, headerCache)
		if scanErr != nil {
			return scanErr
		}
		if parentMismatch {
			prevHeight := uint64(0)
			if mismatchHeight > 0 {
				prevHeight = mismatchHeight - 1
			}
			newRewindTo, newRewindHash, rewindErr := s.rewindFromBlock(ctx, prevHeight, headerCache)
			if rewindErr != nil {
				return rewindErr
			}
			hasReorged = true
			rewoundTo = newRewindTo
			scannedHeight = newRewindTo
			expectedParentHash = newRewindHash
			next = newRewindTo + 1
			continue
		}
		expectedParentHash = lastHash
		scannedHeight = last
		break
	}
	if hasReorged {
		if err := s.reconcileReorgWindow(ctx, rewoundTo, scannedHeight, headerCache); err != nil {
			return err
		}
	}
	return s.confirmPending(ctx, latest, headerCache)
}

// refreshAddresses 从数据库增量或全量同步托管地址索引，使新增地址无需重启即可被扫描。
func (s *Scanner) refreshAddresses(ctx context.Context) error {
	if incStore, ok := s.store.(incrementalAddressStore); ok {
		addresses, err := incStore.ListWalletAddressesAfterID(ctx, s.lastAddressID)
		if err != nil {
			return fmt.Errorf("增量加载托管钱包地址失败：%w", err)
		}
		for _, address := range addresses {
			if !common.IsHexAddress(address.Address) {
				return fmt.Errorf("托管钱包地址格式无效：address_id=%d", address.ID)
			}
			s.addresses[common.HexToAddress(address.Address)] = address
			if address.ID > s.lastAddressID {
				s.lastAddressID = address.ID
			}
		}
		return nil
	}
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

// prepareScanCursor 检查当前检查点区块哈希是否仍在主链上，发生分叉时自动回退检查点。
func (s *Scanner) prepareScanCursor(ctx context.Context, headerCache map[uint64]*types.Header) (next uint64, expectedParentHash string, scannedHeight uint64, hasReorged bool, rewoundTo uint64, err error) {
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 || checkpoint.LastScannedBlock == math.MaxInt64 {
			return 0, "", 0, false, 0, errors.New("数据库中的充值扫描检查点无效")
		}
		height := uint64(checkpoint.LastScannedBlock)
		s.chain.SetScanHeight(height)
		header, hErr := s.cachedHeaderByNumber(ctx, headerCache, height)
		if hErr != nil {
			return 0, "", 0, false, 0, fmt.Errorf("校验 Sepolia 检查点区块 %d 失败：%w", height, hErr)
		}
		if header != nil && strings.EqualFold(header.Hash().Hex(), checkpoint.LastScannedHash) {
			return height + 1, strings.ToLower(header.Hash().Hex()), height, false, 0, nil
		}
		rewindBlock, rewindHash, rewindErr := s.rewindFromBlock(ctx, height, headerCache)
		if rewindErr != nil {
			return 0, "", 0, false, 0, rewindErr
		}
		return rewindBlock + 1, rewindHash, rewindBlock, true, rewindBlock, nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return 0, "", 0, false, 0, fmt.Errorf("读取充值扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return 0, "", 0, false, 0, ErrStartBlockRequired
	}
	return *s.config.StartBlock, "", 0, false, 0, nil
}

// rewindFromBlock 将检查点回退配置块数，清空区块头缓存并写入主链目标高度的新区块哈希。
func (s *Scanner) rewindFromBlock(ctx context.Context, fromBlock uint64, headerCache map[uint64]*types.Header) (uint64, string, error) {
	clear(headerCache)
	rewindBlock := uint64(0)
	if fromBlock > s.config.ReorgRewindBlocks {
		rewindBlock = fromBlock - s.config.ReorgRewindBlocks
	}
	if rewindBlock > math.MaxInt64 {
		return 0, "", errors.New("Sepolia 回退检查点高度超出范围")
	}
	header, err := s.cachedHeaderByNumber(ctx, headerCache, rewindBlock)
	if err != nil {
		return 0, "", fmt.Errorf("读取回退目标区块 %d 失败：%w", rewindBlock, err)
	}
	if header == nil {
		return 0, "", fmt.Errorf("回退目标区块 %d 区块头为空", rewindBlock)
	}
	rewindHash := strings.ToLower(header.Hash().Hex())
	if err := s.store.RewindCheckpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName, int64(rewindBlock), rewindHash); err != nil {
		return 0, "", fmt.Errorf("回退 Sepolia 扫描检查点失败：%w", err)
	}
	s.chain.SetScanHeight(rewindBlock)
	s.logger.Warn("检测到 Sepolia 链重组，已回退扫描检查点", "from_block", fromBlock, "rewind_block", rewindBlock, "rewind_hash", rewindHash)
	return rewindBlock, rewindHash, nil
}

// reconcileReorgWindow 在重组回退重扫后复核区间内充值记录，孤立已消失的待确认充值并对已入账充值发出告警。
func (s *Scanner) reconcileReorgWindow(ctx context.Context, afterBlock, upToBlock uint64, headerCache map[uint64]*types.Header) error {
	if afterBlock > math.MaxInt64 {
		return errors.New("Sepolia 重组核对高度超出范围")
	}
	deposits, err := s.store.ListDepositsAfterBlock(ctx, int64(afterBlock), confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询重组区间 Sepolia 充值失败：%w", err)
	}
	heights := make([]uint64, 0, len(deposits))
	for _, deposit := range deposits {
		if deposit.BlockNumber >= 0 && uint64(deposit.BlockNumber) <= upToBlock {
			heights = append(heights, uint64(deposit.BlockNumber))
		}
	}
	s.prefetchHeaders(ctx, headerCache, heights)
	for _, deposit := range deposits {
		if deposit.BlockNumber < 0 || uint64(deposit.BlockNumber) > upToBlock {
			continue
		}
		header, err := s.cachedHeaderByNumber(ctx, headerCache, uint64(deposit.BlockNumber))
		if err != nil {
			s.recordDepositError(ctx, "reorg_reconcile", deposit.ID, "REORG_HEADER_QUERY_FAILED", err.Error())
			continue
		}
		if header != nil && strings.EqualFold(header.Hash().Hex(), deposit.BlockHash) {
			continue
		}
		if deposit.Status == postgres.DepositCredited {
			msg := fmt.Sprintf("已入账 Sepolia 充值 %d 原区块 %d (%s) 发生重组，保持用户余额不变请人工核查", deposit.ID, deposit.BlockNumber, deposit.BlockHash)
			s.logger.Error(msg, "deposit_id", deposit.ID)
			s.recordDepositError(ctx, "reorg_reconcile", deposit.ID, "CREDITED_DEPOSIT_REORG", msg)
			continue
		}
		msg := fmt.Sprintf("Sepolia 充值 %d 原区块 %d (%s) 重组后已不在主链，执行孤块冲正", deposit.ID, deposit.BlockNumber, deposit.BlockHash)
		s.recordDepositError(ctx, "reorg_reconcile", deposit.ID, "BLOCK_HASH_MISMATCH", msg)
		if _, orphaned, err := s.store.OrphanDeposit(ctx, deposit.ID); err != nil {
			s.recordDepositError(ctx, "orphan_deposit", deposit.ID, "ORPHAN_DEPOSIT_FAILED", err.Error())
		} else if orphaned {
			s.logger.Warn("Sepolia 孤块充值已冲正", "deposit_id", deposit.ID, "block_number", deposit.BlockNumber)
		}
	}
	return nil
}

// depositCandidate 描述等待过滤内部转账哈希的候选 ETH 充值交易。
type depositCandidate struct {
	address     postgres.WalletAddress
	txHash      string
	txIndex     int32
	blockNumber int64
	blockHash   string
	amountWei   *big.Int
}

// scanBatch 批量拉取连续区块区间、校验父哈希并批量提交充值与检查点。
func (s *Scanner) scanBatch(ctx context.Context, fromBlock, toBlock uint64, expectedParentHash string, headerCache map[uint64]*types.Header) (string, bool, uint64, error) {
	if toBlock > math.MaxInt64 {
		return "", false, 0, errors.New("Sepolia 区块高度超出数据库范围")
	}
	blocks, err := s.fetchBlocks(ctx, fromBlock, toBlock)
	if err != nil {
		return "", false, 0, err
	}
	candidates := make([]depositCandidate, 0)
	candidateHashes := make([]string, 0)
	blockHashes := make([]string, len(blocks))
	for i, block := range blocks {
		height := fromBlock + uint64(i)
		if block == nil || block.NumberU64() != height {
			return "", false, 0, fmt.Errorf("Sepolia 区块 %d 返回内容无效", height)
		}
		headerCache[height] = block.Header()
		if expectedParentHash != "" && !strings.EqualFold(block.ParentHash().Hex(), expectedParentHash) {
			return "", true, height, nil
		}
		blockHash := strings.ToLower(block.Hash().Hex())
		blockHashes[i] = blockHash
		expectedParentHash = blockHash
		for index, transaction := range block.Transactions() {
			to := transaction.To()
			if to == nil || transaction.Value().Sign() <= 0 {
				continue
			}
			address, monitored := s.addresses[*to]
			if !monitored {
				continue
			}
			txHash := strings.ToLower(transaction.Hash().Hex())
			candidateHashes = append(candidateHashes, txHash)
			candidates = append(candidates, depositCandidate{
				address:     address,
				txHash:      txHash,
				txIndex:     int32(index),
				blockNumber: int64(height),
				blockHash:   blockHash,
				amountWei:   new(big.Int).Set(transaction.Value()),
			})
		}
	}

	var internalSet map[string]bool
	if len(candidateHashes) > 0 {
		internalSet, err = s.store.FilterInternalTransferTxs(ctx, candidateHashes)
		if err != nil {
			return "", false, 0, fmt.Errorf("识别 Sepolia 内部转账失败：%w", err)
		}
	}
	observations := make([]postgres.DepositObservation, 0, len(candidates))
	for _, candidate := range candidates {
		if internalSet[candidate.txHash] {
			s.logger.Info("跳过平台内部 Gas 补充交易", "block_number", candidate.blockNumber, "tx_hash", candidate.txHash)
			continue
		}
		observations = append(observations, postgres.DepositObservation{
			UserID:      candidate.address.UserID,
			AddressID:   candidate.address.ID,
			TxHash:      candidate.txHash,
			TxIndex:     candidate.txIndex,
			BlockNumber: candidate.blockNumber,
			BlockHash:   candidate.blockHash,
			AmountWei:   candidate.amountWei,
		})
	}

	lastHash := blockHashes[len(blockHashes)-1]
	checkpoint := postgres.ChainCheckpoint{
		Network:          postgres.NetworkSepolia,
		Scanner:          s.config.ScannerName,
		LastScannedBlock: int64(toBlock),
		LastScannedHash:  lastHash,
	}
	var created int
	if batchStore, ok := s.store.(batchDepositStore); ok && toBlock > fromBlock {
		_, created, err = batchStore.RecordDepositsBatchAndCheckpoint(ctx, observations, checkpoint)
	} else if toBlock == fromBlock {
		_, created, err = s.store.RecordDepositsAndCheckpoint(ctx, observations, checkpoint)
	} else {
		created, err = s.recordByBlockFallback(ctx, fromBlock, blockHashes, observations)
	}
	if err != nil {
		return "", false, 0, fmt.Errorf("保存 Sepolia 区块 %d-%d 充值结果失败：%w", fromBlock, toBlock, err)
	}
	s.chain.SetScanHeight(toBlock)
	if created > 0 {
		s.logger.Info("发现 Sepolia 托管钱包充值", "from_block", fromBlock, "to_block", toBlock, "deposit_count", created)
	}
	return lastHash, false, 0, nil
}

// fetchBlocks 拉取闭区间内的完整区块，优先使用批量 JSON-RPC 接口。
func (s *Scanner) fetchBlocks(ctx context.Context, fromBlock, toBlock uint64) ([]*types.Block, error) {
	count := int(toBlock - fromBlock + 1)
	if count > 1 {
		if batch, ok := s.chain.(batchChainClient); ok {
			numbers := make([]*big.Int, count)
			for i := 0; i < count; i++ {
				numbers[i] = new(big.Int).SetUint64(fromBlock + uint64(i))
			}
			blocks, err := batch.BlocksByNumbers(ctx, numbers)
			if err != nil {
				return nil, fmt.Errorf("批量读取 Sepolia 区块 %d-%d 失败：%w", fromBlock, toBlock, err)
			}
			if len(blocks) != count {
				return nil, fmt.Errorf("批量读取 Sepolia 区块 %d-%d 返回数量不匹配", fromBlock, toBlock)
			}
			return blocks, nil
		}
	}
	blocks := make([]*types.Block, count)
	for i := 0; i < count; i++ {
		height := fromBlock + uint64(i)
		block, err := s.chain.BlockByNumber(ctx, new(big.Int).SetUint64(height))
		if err != nil {
			return nil, fmt.Errorf("读取 Sepolia 区块 %d 失败：%w", height, err)
		}
		blocks[i] = block
	}
	return blocks, nil
}

// recordByBlockFallback 在存储层未提供批量检查点接口时按区块逐块提交观测。
func (s *Scanner) recordByBlockFallback(ctx context.Context, fromBlock uint64, blockHashes []string, observations []postgres.DepositObservation) (int, error) {
	byBlock := make(map[int64][]postgres.DepositObservation)
	for _, obs := range observations {
		byBlock[obs.BlockNumber] = append(byBlock[obs.BlockNumber], obs)
	}
	totalCreated := 0
	for i, hash := range blockHashes {
		height := int64(fromBlock) + int64(i)
		_, created, err := s.store.RecordDepositsAndCheckpoint(ctx, byBlock[height], postgres.ChainCheckpoint{
			Network:          postgres.NetworkSepolia,
			Scanner:          s.config.ScannerName,
			LastScannedBlock: height,
			LastScannedHash:  hash,
		})
		if err != nil {
			return 0, err
		}
		totalCreated += created
	}
	return totalCreated, nil
}

// cachedHeaderByNumber 优先命中本轮区块头缓存，未命中时再发起单个 HeaderByNumber RPC 请求。
func (s *Scanner) cachedHeaderByNumber(ctx context.Context, headerCache map[uint64]*types.Header, height uint64) (*types.Header, error) {
	if cached, ok := headerCache[height]; ok {
		return cached, nil
	}
	header, err := s.chain.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		return nil, err
	}
	if header != nil {
		headerCache[height] = header
	}
	return header, nil
}

// prefetchHeaders 在链客户端支持批量查询时预取多个未缓存高度的区块头。
func (s *Scanner) prefetchHeaders(ctx context.Context, headerCache map[uint64]*types.Header, heights []uint64) {
	batch, ok := s.chain.(batchChainClient)
	if !ok || len(heights) <= 1 {
		return
	}
	seen := make(map[uint64]struct{}, len(heights))
	missing := make([]uint64, 0, len(heights))
	numbers := make([]*big.Int, 0, len(heights))
	for _, h := range heights {
		if _, cached := headerCache[h]; cached {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		missing = append(missing, h)
		numbers = append(numbers, new(big.Int).SetUint64(h))
	}
	if len(numbers) <= 1 {
		return
	}
	headers, err := batch.HeadersByNumbers(ctx, numbers)
	if err != nil || len(headers) != len(missing) {
		return
	}
	for i, h := range missing {
		if headers[i] != nil {
			headerCache[h] = headers[i]
		}
	}
}

// confirmPending 更新确认数并在复核原区块哈希后完成充值入账，单条异常隔离跳过而不阻塞后续入账。
func (s *Scanner) confirmPending(ctx context.Context, latest uint64, headerCache map[uint64]*types.Header) error {
	deposits, err := s.store.ListConfirmingDeposits(ctx, confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询待确认充值失败：%w", err)
	}
	heights := make([]uint64, 0, len(deposits))
	for _, deposit := range deposits {
		if deposit.BlockNumber >= 0 && uint64(deposit.BlockNumber) <= latest {
			confirmations := latest - uint64(deposit.BlockNumber) + 1
			if confirmations >= s.config.Confirmations && confirmations <= math.MaxInt64 {
				heights = append(heights, uint64(deposit.BlockNumber))
			}
		}
	}
	s.prefetchHeaders(ctx, headerCache, heights)
	for _, deposit := range deposits {
		if deposit.BlockNumber < 0 || uint64(deposit.BlockNumber) > latest {
			continue
		}
		confirmations := latest - uint64(deposit.BlockNumber) + 1
		if confirmations > math.MaxInt64 {
			s.recordDepositError(ctx, "confirm_check", deposit.ID, "CONFIRMATIONS_OVERFLOW", "充值确认数超出数据库范围")
			continue
		}
		if confirmations < s.config.Confirmations {
			if _, err := s.store.UpdateDepositConfirmations(ctx, deposit.ID, int64(confirmations)); err != nil {
				s.recordDepositError(ctx, "update_confirmations", deposit.ID, "DEPOSIT_CONFIRMATION_UPDATE_FAILED", err.Error())
			}
			continue
		}
		matched, err := s.verifyBlockHash(ctx, deposit, headerCache)
		if err != nil {
			s.recordDepositError(ctx, "block_hash_check", deposit.ID, "BLOCK_HASH_QUERY_FAILED", err.Error())
			continue
		}
		if !matched {
			if _, orphaned, orphanErr := s.store.OrphanDeposit(ctx, deposit.ID); orphanErr != nil {
				s.recordDepositError(ctx, "orphan_deposit", deposit.ID, "ORPHAN_DEPOSIT_FAILED", orphanErr.Error())
			} else if orphaned {
				s.logger.Warn("确认阶段发现 Sepolia 孤块充值并完成冲正", "deposit_id", deposit.ID, "block_number", deposit.BlockNumber)
			}
			continue
		}
		if _, credited, err := s.store.CreditDeposit(ctx, deposit.ID, int64(confirmations)); err != nil {
			s.recordDepositError(ctx, "credit_deposit", deposit.ID, "DEPOSIT_CREDIT_FAILED", err.Error())
			continue
		} else if credited {
			s.logger.Info("Sepolia 充值已确认入账", "deposit_id", deposit.ID, "confirmations", confirmations)
		}
	}
	return nil
}

// verifyBlockHash 入账前复核充值原区块哈希是否仍在主链上，不一致时记录错误日志。
func (s *Scanner) verifyBlockHash(ctx context.Context, deposit postgres.Deposit, headerCache map[uint64]*types.Header) (bool, error) {
	header, err := s.cachedHeaderByNumber(ctx, headerCache, uint64(deposit.BlockNumber))
	if err != nil {
		return false, fmt.Errorf("复核充值 %d 原区块失败：%w", deposit.ID, err)
	}
	if header != nil && strings.EqualFold(header.Hash().Hex(), deposit.BlockHash) {
		return true, nil
	}
	message := fmt.Sprintf("充值 %d 所在区块 %d 的哈希与首次扫描结果不一致", deposit.ID, deposit.BlockNumber)
	s.recordDepositError(ctx, "block_hash_check", deposit.ID, "BLOCK_HASH_MISMATCH", message)
	return false, nil
}

// recordDepositError 写入单笔 Sepolia 充值异常告警，防止单条数据异常拖垮扫描器。
func (s *Scanner) recordDepositError(ctx context.Context, stage string, depositID int64, code, message string) {
	if ctx.Err() != nil {
		return
	}
	refID := depositID
	if _, err := s.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker:        "deposit-confirmer",
		Stage:         stage,
		ReferenceType: "DEPOSIT",
		ReferenceID:   &refID,
		ErrorCode:     code,
		ErrorMessage:  message,
	}); err != nil {
		s.logger.Warn("写入 Sepolia 充值后台错误失败", "deposit_id", depositID, "error", err)
	}
}
