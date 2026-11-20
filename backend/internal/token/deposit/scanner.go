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

// batchHeaderChain 定义可选的批量区块头查询能力。
type batchHeaderChain interface {
	HeadersByNumbers(ctx context.Context, numbers []*big.Int) ([]*types.Header, error)
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
	RewindCheckpoint(ctx context.Context, network, scanner string, block int64, hash string) error
	RecordTokenDepositsAndCheckpoint(ctx context.Context, observations []postgres.TokenDepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.TokenDeposit, int, error)
	ListConfirmingTokenDeposits(ctx context.Context, assetID int64, limit int) ([]postgres.TokenDeposit, error)
	ListTokenDepositsAfterBlock(ctx context.Context, assetID, afterBlock int64, limit int) ([]postgres.TokenDeposit, error)
	UpdateTokenDepositConfirmations(ctx context.Context, depositID, confirmations int64) (postgres.TokenDeposit, error)
	CreditTokenDeposit(ctx context.Context, depositID, confirmations int64) (postgres.TokenDeposit, bool, error)
	OrphanTokenDeposit(ctx context.Context, depositID int64) (postgres.TokenDeposit, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// incrementalAddressStore 定义可选的增量钱包地址加载接口。
type incrementalAddressStore interface {
	ListWalletAddressesAfterID(ctx context.Context, afterID int64) ([]postgres.WalletAddress, error)
}

// Config 定义 Token 扫描资产、起点、批次、安全深度、回退块数、确认数和轮询间隔。
type Config struct {
	AssetID           int64
	StartBlock        *uint64
	BatchSize         uint64
	Confirmations     uint64
	SafeDepth         uint64
	ReorgRewindBlocks uint64
	Interval          time.Duration
	ScannerName       string
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
	chain         Chain
	contract      Contract
	store         Store
	logger        *slog.Logger
	config        Config
	addresses     map[common.Address]postgres.WalletAddress
	lastAddressID int64
	healthMu      sync.RWMutex
	health        HealthSnapshot
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
	if cfg.ReorgRewindBlocks == 0 {
		cfg.ReorgRewindBlocks = cfg.Confirmations
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

// Run 周期执行 Token 充值扫描，单条或重组异常不会拖垮应用退出。
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

// runOnce 执行一轮含父哈希校验、重组回退与安全深度扫描的实际步骤。
func (s *Scanner) runOnce(ctx context.Context) error {
	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	latest, err := s.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Token 网络最新区块失败：%w", err)
	}
	s.setNetworkHeight(latest)
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
		parentReorg, toHash, scanErr := s.scanRange(ctx, next, last, expectedParentHash, headerCache)
		if scanErr != nil {
			return scanErr
		}
		if parentReorg {
			prevHeight := uint64(0)
			if next > 0 {
				prevHeight = next - 1
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
		_ = toHash
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

// refreshAddresses 增量或全量同步用户托管地址内存索引。
func (s *Scanner) refreshAddresses(ctx context.Context) error {
	if incStore, ok := s.store.(incrementalAddressStore); ok {
		addresses, err := incStore.ListWalletAddressesAfterID(ctx, s.lastAddressID)
		if err != nil {
			return fmt.Errorf("增量加载 Token 托管地址失败：%w", err)
		}
		for _, address := range addresses {
			if !common.IsHexAddress(address.Address) {
				return fmt.Errorf("Token 托管地址格式无效：address_id=%d", address.ID)
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

// prepareScanCursor 校验当前检查点区块哈希是否仍在主链，发生分叉时自动回退检查点。
func (s *Scanner) prepareScanCursor(ctx context.Context, headerCache map[uint64]*types.Header) (next uint64, expectedParentHash string, scannedHeight uint64, hasReorged bool, rewoundTo uint64, err error) {
	checkpoint, err := s.store.Checkpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName)
	if err == nil {
		if checkpoint.LastScannedBlock < 0 || checkpoint.LastScannedBlock == math.MaxInt64 {
			return 0, "", 0, false, 0, errors.New("数据库中的 Token 扫描检查点无效")
		}
		height := uint64(checkpoint.LastScannedBlock)
		s.setScanHeight(height)
		header, hErr := s.cachedHeaderByNumber(ctx, headerCache, height)
		if hErr != nil {
			return 0, "", 0, false, 0, fmt.Errorf("校验 Token 检查点区块 %d 失败：%w", height, hErr)
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
		return 0, "", 0, false, 0, fmt.Errorf("读取 Token 扫描检查点失败：%w", err)
	}
	if s.config.StartBlock == nil {
		return 0, "", 0, false, 0, ErrStartBlockRequired
	}
	return *s.config.StartBlock, "", 0, false, 0, nil
}

// rewindFromBlock 将 Token 检查点回退配置块数，清空区块头缓存并记录主链目标区块哈希。
func (s *Scanner) rewindFromBlock(ctx context.Context, fromBlock uint64, headerCache map[uint64]*types.Header) (uint64, string, error) {
	clear(headerCache)
	rewindBlock := uint64(0)
	if fromBlock > s.config.ReorgRewindBlocks {
		rewindBlock = fromBlock - s.config.ReorgRewindBlocks
	}
	if rewindBlock > math.MaxInt64 {
		return 0, "", errors.New("Token 回退检查点高度超出范围")
	}
	header, err := s.cachedHeaderByNumber(ctx, headerCache, rewindBlock)
	if err != nil {
		return 0, "", fmt.Errorf("读取 Token 回退目标区块 %d 失败：%w", rewindBlock, err)
	}
	if header == nil {
		return 0, "", fmt.Errorf("Token 回退目标区块 %d 区块头为空", rewindBlock)
	}
	rewindHash := strings.ToLower(header.Hash().Hex())
	if err := s.store.RewindCheckpoint(ctx, postgres.NetworkSepolia, s.config.ScannerName, int64(rewindBlock), rewindHash); err != nil {
		return 0, "", fmt.Errorf("回退 Token 扫描检查点失败：%w", err)
	}
	s.setScanHeight(rewindBlock)
	s.logger.Warn("检测到 Token 链重组，已回退扫描检查点", "from_block", fromBlock, "rewind_block", rewindBlock, "rewind_hash", rewindHash)
	return rewindBlock, rewindHash, nil
}

// reconcileReorgWindow 在重组回退重扫后复核区间内 Token 充值记录，孤立已消失记录并对已入账记录发出告警。
func (s *Scanner) reconcileReorgWindow(ctx context.Context, afterBlock, upToBlock uint64, headerCache map[uint64]*types.Header) error {
	if afterBlock > math.MaxInt64 {
		return errors.New("Token 重组核对高度超出范围")
	}
	deposits, err := s.store.ListTokenDepositsAfterBlock(ctx, s.config.AssetID, int64(afterBlock), confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询重组区间 Token 充值失败：%w", err)
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
			msg := fmt.Sprintf("已入账 Token 充值 %d 原区块 %d (%s) 发生重组，保持用户余额不变请人工核查", deposit.ID, deposit.BlockNumber, deposit.BlockHash)
			s.logger.Error(msg, "deposit_id", deposit.ID)
			s.recordDepositError(ctx, "reorg_reconcile", deposit.ID, "CREDITED_DEPOSIT_REORG", msg)
			continue
		}
		msg := fmt.Sprintf("Token 充值 %d 原区块 %d (%s) 重组后已不在主链，执行孤块冲正", deposit.ID, deposit.BlockNumber, deposit.BlockHash)
		s.recordDepositError(ctx, "reorg_reconcile", deposit.ID, "BLOCK_HASH_MISMATCH", msg)
		if _, orphaned, err := s.store.OrphanTokenDeposit(ctx, deposit.ID); err != nil {
			s.recordDepositError(ctx, "orphan_deposit", deposit.ID, "ORPHAN_TOKEN_DEPOSIT_FAILED", err.Error())
		} else if orphaned {
			s.logger.Warn("Token 孤块充值已冲正", "deposit_id", deposit.ID, "block_number", deposit.BlockNumber)
		}
	}
	return nil
}

// scanRange 查询并提交一个区块闭区间，校验起始块父哈希，范围超限时递归缩小批次。
func (s *Scanner) scanRange(ctx context.Context, fromBlock, toBlock uint64, expectedParentHash string, headerCache map[uint64]*types.Header) (bool, string, error) {
	if expectedParentHash != "" && fromBlock < toBlock {
		s.prefetchHeaders(ctx, headerCache, []uint64{fromBlock, toBlock})
	}
	if expectedParentHash != "" {
		fromHeader, err := s.cachedHeaderByNumber(ctx, headerCache, fromBlock)
		if err != nil {
			return false, "", fmt.Errorf("读取 Token 扫描起始区块 %d 失败：%w", fromBlock, err)
		}
		if fromHeader == nil || fromHeader.Number == nil || fromHeader.Number.Uint64() != fromBlock {
			return false, "", errors.New("Token 扫描起始区块返回无效")
		}
		if !strings.EqualFold(fromHeader.ParentHash.Hex(), expectedParentHash) {
			return true, "", nil
		}
	}
	logs, err := s.contract.FilterTransferLogs(ctx, fromBlock, toBlock)
	if err != nil {
		if errors.Is(err, erc20.ErrRemovedLog) {
			return true, "", nil
		}
		if evm.IsLogRangeTooLarge(err) && fromBlock < toBlock {
			middle := fromBlock + (toBlock-fromBlock)/2
			reorged, middleHash, subErr := s.scanRange(ctx, fromBlock, middle, expectedParentHash, headerCache)
			if subErr != nil || reorged {
				return reorged, middleHash, subErr
			}
			return s.scanRange(ctx, middle+1, toBlock, middleHash, headerCache)
		}
		return false, "", err
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
			if errors.Is(err, erc20.ErrRemovedLog) {
				return true, "", nil
			}
			return false, "", fmt.Errorf("解码 Token Transfer Event 失败：%w", err)
		}
		if event.BlockNumber < fromBlock || event.BlockNumber > toBlock || event.LogIndex > math.MaxInt32 {
			return false, "", errors.New("Token Transfer Event 区块或日志索引无效")
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
	header, err := s.cachedHeaderByNumber(ctx, headerCache, toBlock)
	if err != nil {
		return false, "", fmt.Errorf("读取 Token 扫描批次末区块失败：%w", err)
	}
	if header == nil || header.Number == nil || header.Number.Uint64() != toBlock {
		return false, "", errors.New("Token 扫描批次末区块返回无效")
	}
	toHash := strings.ToLower(header.Hash().Hex())
	_, created, err := s.store.RecordTokenDepositsAndCheckpoint(ctx, observations, postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: s.config.ScannerName,
		LastScannedBlock: int64(toBlock), LastScannedHash: toHash,
	})
	if err != nil {
		return false, "", fmt.Errorf("保存 Token 扫描批次失败：%w", err)
	}
	s.setScanHeight(toBlock)
	if created > 0 {
		s.logger.Info("发现 Token 托管钱包充值", "to_block", toBlock, "deposit_count", created)
	}
	return false, toHash, nil
}

// confirmPending 更新确认数并在复核区块哈希后幂等入账，单条异常隔离跳过而不阻塞后续入账。
func (s *Scanner) confirmPending(ctx context.Context, latest uint64, headerCache map[uint64]*types.Header) error {
	deposits, err := s.store.ListConfirmingTokenDeposits(ctx, s.config.AssetID, confirmBatchSize)
	if err != nil {
		return fmt.Errorf("查询待确认 Token 充值失败：%w", err)
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
			s.recordDepositError(ctx, "confirm_check", deposit.ID, "CONFIRMATIONS_OVERFLOW", "Token 充值确认数超出数据库范围")
			continue
		}
		if confirmations < s.config.Confirmations {
			if _, err := s.store.UpdateTokenDepositConfirmations(ctx, deposit.ID, int64(confirmations)); err != nil {
				s.recordDepositError(ctx, "update_confirmations", deposit.ID, "TOKEN_DEPOSIT_CONFIRMATION_UPDATE_FAILED", err.Error())
			}
			continue
		}
		matched, err := s.verifyBlockHash(ctx, deposit, headerCache)
		if err != nil {
			s.recordDepositError(ctx, "block_hash_check", deposit.ID, "BLOCK_HASH_QUERY_FAILED", err.Error())
			continue
		}
		if !matched {
			if _, orphaned, orphanErr := s.store.OrphanTokenDeposit(ctx, deposit.ID); orphanErr != nil {
				s.recordDepositError(ctx, "orphan_deposit", deposit.ID, "ORPHAN_TOKEN_DEPOSIT_FAILED", orphanErr.Error())
			} else if orphaned {
				s.logger.Warn("确认阶段发现 Token 孤块充值并完成冲正", "deposit_id", deposit.ID, "block_number", deposit.BlockNumber)
			}
			continue
		}
		if _, credited, err := s.store.CreditTokenDeposit(ctx, deposit.ID, int64(confirmations)); err != nil {
			s.recordDepositError(ctx, "credit_deposit", deposit.ID, "TOKEN_DEPOSIT_CREDIT_FAILED", err.Error())
			continue
		} else if credited {
			s.logger.Info("Token 充值已确认入账", "deposit_id", deposit.ID, "confirmations", confirmations)
		}
	}
	return nil
}

// verifyBlockHash 入账前复核 Token Event 原区块哈希是否仍在主链上。
func (s *Scanner) verifyBlockHash(ctx context.Context, deposit postgres.TokenDeposit, headerCache map[uint64]*types.Header) (bool, error) {
	header, err := s.cachedHeaderByNumber(ctx, headerCache, uint64(deposit.BlockNumber))
	if err != nil {
		return false, fmt.Errorf("复核 Token 充值 %d 原区块失败：%w", deposit.ID, err)
	}
	if header != nil && strings.EqualFold(header.Hash().Hex(), deposit.BlockHash) {
		return true, nil
	}
	message := fmt.Sprintf("Token 充值 %d 所在区块 %d 哈希与首次扫描结果不一致", deposit.ID, deposit.BlockNumber)
	s.recordDepositError(ctx, "block_hash_check", deposit.ID, "BLOCK_HASH_MISMATCH", message)
	return false, nil
}

// cachedHeaderByNumber 优先命中本轮 Token 扫描的区块头缓存。
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
	batch, ok := s.chain.(batchHeaderChain)
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

// recordDepositError 写入单笔 Token 充值异常告警，避免单条数据异常中断批处理或扫描器。
func (s *Scanner) recordDepositError(ctx context.Context, stage string, depositID int64, code, message string) {
	if ctx.Err() != nil {
		return
	}
	refID := depositID
	if _, err := s.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "token-deposit-confirmer", Stage: stage, ReferenceType: "TOKEN_DEPOSIT",
		ReferenceID: &refID, ErrorCode: code, ErrorMessage: message,
	}); err != nil {
		s.logger.Warn("写入 Token 充值后台错误失败", "deposit_id", depositID, "error", err)
	}
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
