package deposit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

// fakeChain 为扫描器单元测试提供确定性的区块和区块头。
type fakeChain struct {
	latest           uint64
	blocks           map[uint64]*types.Block
	blockErrors      map[uint64]error
	headers          map[uint64]*types.Header
	blockCalls       []uint64
	batchBlockCalls  [][]uint64
	headerCalls      []uint64
	batchHeaderCalls [][]uint64
	scanHeights      []uint64
}

// BlockNumber 返回测试配置的最新高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) {
	return f.latest, nil
}

// BlockByNumber 返回指定高度的测试区块并记录调用。
func (f *fakeChain) BlockByNumber(_ context.Context, number *big.Int) (*types.Block, error) {
	height := number.Uint64()
	f.blockCalls = append(f.blockCalls, height)
	if err := f.blockErrors[height]; err != nil {
		return nil, err
	}
	block, ok := f.blocks[height]
	if !ok {
		return nil, errors.New("测试区块不存在")
	}
	return block, nil
}

// BlocksByNumbers 批量返回多个高度的测试区块并记录一次批量调用。
func (f *fakeChain) BlocksByNumbers(_ context.Context, numbers []*big.Int) ([]*types.Block, error) {
	batch := make([]uint64, len(numbers))
	blocks := make([]*types.Block, len(numbers))
	for i, number := range numbers {
		height := number.Uint64()
		batch[i] = height
		if err := f.blockErrors[height]; err != nil {
			f.batchBlockCalls = append(f.batchBlockCalls, batch)
			return nil, err
		}
		block, ok := f.blocks[height]
		if !ok {
			f.batchBlockCalls = append(f.batchBlockCalls, batch)
			return nil, errors.New("测试区块不存在")
		}
		blocks[i] = block
	}
	f.batchBlockCalls = append(f.batchBlockCalls, batch)
	return blocks, nil
}

// HeaderByNumber 返回指定高度的测试区块头并记录调用。
func (f *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	height := number.Uint64()
	f.headerCalls = append(f.headerCalls, height)
	header, ok := f.headers[height]
	if !ok {
		return nil, errors.New("测试区块头不存在")
	}
	return header, nil
}

// HeadersByNumbers 批量返回多个高度的测试区块头并记录一次批量调用。
func (f *fakeChain) HeadersByNumbers(_ context.Context, numbers []*big.Int) ([]*types.Header, error) {
	batch := make([]uint64, len(numbers))
	headers := make([]*types.Header, len(numbers))
	for i, number := range numbers {
		height := number.Uint64()
		batch[i] = height
		header, ok := f.headers[height]
		if !ok {
			f.batchHeaderCalls = append(f.batchHeaderCalls, batch)
			return nil, errors.New("测试区块头不存在")
		}
		headers[i] = header
	}
	f.batchHeaderCalls = append(f.batchHeaderCalls, batch)
	return headers, nil
}

// SetScanHeight 记录扫描器已完成的高度。
func (f *fakeChain) SetScanHeight(height uint64) {
	f.scanHeights = append(f.scanHeights, height)
}

// fakeStore 为扫描器单元测试保存内存态检查点和充值记录。
type fakeStore struct {
	addresses           []postgres.WalletAddress
	checkpoint          *postgres.ChainCheckpoint
	rewinds             []postgres.ChainCheckpoint
	deposits            []postgres.Deposit
	observations        []postgres.DepositObservation
	confirmations       map[int64]int64
	credited            map[int64]int64
	orphaned            map[int64]bool
	workerErrors        []postgres.WorkerError
	internalTxs         map[string]bool
	filterInternalCalls int
	recordCalls         int
	addressQueries      []int64
	nextDepositID       int64
}

// ListWalletAddresses 返回测试托管地址。
func (f *fakeStore) ListWalletAddresses(ctx context.Context) ([]postgres.WalletAddress, error) {
	return f.ListWalletAddressesAfterID(ctx, 0)
}

// ListWalletAddressesAfterID 按游标增量返回测试托管地址。
func (f *fakeStore) ListWalletAddressesAfterID(_ context.Context, afterID int64) ([]postgres.WalletAddress, error) {
	f.addressQueries = append(f.addressQueries, afterID)
	result := make([]postgres.WalletAddress, 0, len(f.addresses))
	for _, item := range f.addresses {
		if item.ID > afterID {
			result = append(result, item)
		}
	}
	return result, nil
}

// Checkpoint 返回内存检查点或未找到错误。
func (f *fakeStore) Checkpoint(context.Context, string, string) (postgres.ChainCheckpoint, error) {
	if f.checkpoint == nil {
		return postgres.ChainCheckpoint{}, postgres.ErrNotFound
	}
	return *f.checkpoint, nil
}

// RewindCheckpoint 回退内存中的扫描检查点。
func (f *fakeStore) RewindCheckpoint(_ context.Context, network, scanner string, block int64, hash string) error {
	cp := postgres.ChainCheckpoint{Network: network, Scanner: scanner, LastScannedBlock: block, LastScannedHash: hash}
	f.checkpoint = &cp
	f.rewinds = append(f.rewinds, cp)
	return nil
}

// FilterInternalTransferTxs 批量判断测试交易哈希是否属于平台内部转账。
func (f *fakeStore) FilterInternalTransferTxs(_ context.Context, txHashes []string) (map[string]bool, error) {
	f.filterInternalCalls++
	matched := make(map[string]bool, len(txHashes))
	for _, hash := range txHashes {
		if f.internalTxs[hash] {
			matched[hash] = true
		}
	}
	return matched, nil
}

// RecordDepositsBatchAndCheckpoint 批量保存多区块观察结果并推进内存检查点。
func (f *fakeStore) RecordDepositsBatchAndCheckpoint(ctx context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error) {
	return f.RecordDepositsAndCheckpoint(ctx, observations, checkpoint)
}

// RecordDepositsAndCheckpoint 保存整区块观察结果并推进内存检查点。
func (f *fakeStore) RecordDepositsAndCheckpoint(_ context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error) {
	f.recordCalls++
	f.checkpoint = &checkpoint
	f.observations = append(f.observations, observations...)
	items := make([]postgres.Deposit, 0, len(observations))
	for _, observation := range observations {
		matched := false
		for i := range f.deposits {
			if f.deposits[i].TxHash == observation.TxHash && f.deposits[i].AddressID == observation.AddressID && f.deposits[i].Status != postgres.DepositOrphaned {
				if f.deposits[i].Status != postgres.DepositCredited {
					f.deposits[i].BlockNumber = observation.BlockNumber
					f.deposits[i].BlockHash = observation.BlockHash
					f.deposits[i].TxIndex = observation.TxIndex
				}
				items = append(items, f.deposits[i])
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		f.nextDepositID++
		item := postgres.Deposit{
			ID:          f.nextDepositID,
			UserID:      observation.UserID,
			AddressID:   observation.AddressID,
			TxHash:      observation.TxHash,
			TxIndex:     observation.TxIndex,
			BlockNumber: observation.BlockNumber,
			BlockHash:   observation.BlockHash,
			AmountWei:   observation.AmountWei,
			Status:      postgres.DepositConfirming,
		}
		f.deposits = append(f.deposits, item)
		items = append(items, item)
	}
	return items, len(items), nil
}

// ListConfirmingDeposits 返回内存中尚未入账且未被孤立的充值。
func (f *fakeStore) ListConfirmingDeposits(context.Context, int) ([]postgres.Deposit, error) {
	result := make([]postgres.Deposit, 0, len(f.deposits))
	for _, item := range f.deposits {
		if item.Status == postgres.DepositConfirming || item.Status == postgres.DepositConfirmed || item.Status == postgres.DepositDetected {
			result = append(result, item)
		}
	}
	return result, nil
}

// ListDepositsAfterBlock 返回指定区块之后的非孤块测试充值记录并严格遵守批次大小。
func (f *fakeStore) ListDepositsAfterBlock(ctx context.Context, afterBlock int64, limit int) ([]postgres.Deposit, error) {
	return f.ListDepositsAfterBlockCursor(ctx, afterBlock+1, afterBlock, 0, limit)
}

// ListDepositsAfterBlockCursor 按键集游标分页返回测试非孤块充值记录。
func (f *fakeStore) ListDepositsAfterBlockCursor(_ context.Context, minBlock, afterBlock, afterID int64, limit int) ([]postgres.Deposit, error) {
	result := make([]postgres.Deposit, 0)
	for _, item := range f.deposits {
		if item.BlockNumber >= minBlock && item.Status != postgres.DepositOrphaned &&
			(item.BlockNumber > afterBlock || (item.BlockNumber == afterBlock && item.ID > afterID)) {
			result = append(result, item)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// ListCreditedDepositsAfterCursor 按键集游标分页返回测试已入账充值记录。
func (f *fakeStore) ListCreditedDepositsAfterCursor(_ context.Context, minBlock, afterBlock, afterID int64, limit int) ([]postgres.Deposit, error) {
	result := make([]postgres.Deposit, 0)
	for _, item := range f.deposits {
		if item.BlockNumber >= minBlock && item.Status == postgres.DepositCredited &&
			(item.BlockNumber > afterBlock || (item.BlockNumber == afterBlock && item.ID > afterID)) {
			result = append(result, item)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// UpdateDepositConfirmations 记录充值最新确认数。
func (f *fakeStore) UpdateDepositConfirmations(_ context.Context, depositID, confirmations int64) (postgres.Deposit, error) {
	if f.confirmations == nil {
		f.confirmations = make(map[int64]int64)
	}
	f.confirmations[depositID] = confirmations
	return postgres.Deposit{ID: depositID, Confirmations: confirmations}, nil
}

// CreditDeposit 记录充值已经完成入账。
func (f *fakeStore) CreditDeposit(_ context.Context, depositID, confirmations int64) (postgres.Deposit, bool, error) {
	if f.credited == nil {
		f.credited = make(map[int64]int64)
	}
	f.credited[depositID] = confirmations
	for i := range f.deposits {
		if f.deposits[i].ID == depositID {
			f.deposits[i].Status = postgres.DepositCredited
			break
		}
	}
	return postgres.Deposit{ID: depositID, Status: postgres.DepositCredited}, true, nil
}

// OrphanDeposit 标记测试充值进入孤块状态。
func (f *fakeStore) OrphanDeposit(_ context.Context, depositID int64) (postgres.Deposit, bool, error) {
	if f.orphaned == nil {
		f.orphaned = make(map[int64]bool)
	}
	for i := range f.deposits {
		if f.deposits[i].ID == depositID {
			if f.deposits[i].Status == postgres.DepositOrphaned {
				return f.deposits[i], false, nil
			}
			f.deposits[i].Status = postgres.DepositOrphaned
			f.orphaned[depositID] = true
			return f.deposits[i], true, nil
		}
	}
	return postgres.Deposit{}, false, postgres.ErrNotFound
}

// RecordWorkerError 保存扫描器写入的安全错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.workerErrors = append(f.workerErrors, item)
	return int64(len(f.workerErrors)), nil
}

// TestScannerFindsOnlyTopLevelMonitoredTransfers 验证扫描器只记录发往托管地址的正金额顶层转账。
func TestScannerFindsOnlyTopLevelMonitoredTransfers(t *testing.T) {
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	other := common.HexToAddress("0x2222222222222222222222222222222222222222")
	block10 := testBlock(10,
		testTransaction(1, &target, 100),
		testTransaction(2, &other, 200),
		testTransaction(3, &target, 0),
		testTransaction(4, nil, 300),
	)
	block11 := testBlockWithParent(11, block10.Hash())
	chain := &fakeChain{latest: 11, blocks: map[uint64]*types.Block{10: block10, 11: block11}, headers: map[uint64]*types.Header{}}
	store := &fakeStore{addresses: []postgres.WalletAddress{{ID: 7, UserID: 5, Address: target.Hex()}}}
	start := uint64(10)
	scanner := newTestScanner(t, chain, store, Config{StartBlock: &start, Confirmations: 3, BatchSize: 2, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.observations) != 1 || store.observations[0].AmountWei.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("observations = %+v, want one 100 wei deposit", store.observations)
	}
	if store.checkpoint == nil || store.checkpoint.LastScannedBlock != 11 {
		t.Fatalf("checkpoint = %+v, want block 11", store.checkpoint)
	}
	if got := store.confirmations[1]; got != 2 {
		t.Fatalf("confirmations = %d, want 2", got)
	}
}

// TestScannerSkipsInternalGasTopup 验证平台补气交易只推进检查点且不创建用户 ETH 充值。
func TestScannerSkipsInternalGasTopup(t *testing.T) {
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	internal := testTransaction(1, &target, 100)
	external := testTransaction(2, &target, 200)
	block := testBlock(10, internal, external)
	chain := &fakeChain{latest: 10, blocks: map[uint64]*types.Block{10: block}, headers: map[uint64]*types.Header{}}
	store := &fakeStore{
		addresses:   []postgres.WalletAddress{{ID: 7, UserID: 5, Address: target.Hex()}},
		internalTxs: map[string]bool{strings.ToLower(internal.Hash().Hex()): true},
	}
	start := uint64(10)
	scanner := newTestScanner(t, chain, store, Config{StartBlock: &start, Confirmations: 3, BatchSize: 1, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.observations) != 1 || store.observations[0].TxHash != strings.ToLower(external.Hash().Hex()) {
		t.Fatalf("observations = %+v, want only external transfer", store.observations)
	}
	if store.checkpoint == nil || store.checkpoint.LastScannedBlock != 10 {
		t.Fatalf("checkpoint = %+v, want block 10", store.checkpoint)
	}
}

// TestScannerResumesAfterCheckpoint 验证重启后从持久化检查点的下一高度继续扫描。
func TestScannerResumesAfterCheckpoint(t *testing.T) {
	block10 := testBlock(10)
	block11 := testBlockWithParent(11, block10.Hash())
	chain := &fakeChain{
		latest:  11,
		blocks:  map[uint64]*types.Block{10: block10, 11: block11},
		headers: map[uint64]*types.Header{10: block10.Header(), 11: block11.Header()},
	}
	store := &fakeStore{checkpoint: &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 10, LastScannedHash: strings.ToLower(block10.Hash().Hex()),
	}}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	if err := scanner.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(chain.blockCalls) != 1 || chain.blockCalls[0] != 11 {
		t.Fatalf("block calls = %v, want [11]", chain.blockCalls)
	}
}

// TestScannerDoesNotAdvanceCheckpointAfterTemporaryRPCFailure 验证 RPC 临时失败不会跳过区块，恢复后仍从原高度重试。
func TestScannerDoesNotAdvanceCheckpointAfterTemporaryRPCFailure(t *testing.T) {
	block10 := testBlock(10)
	block11 := testBlockWithParent(11, block10.Hash())
	chain := &fakeChain{
		latest:      11,
		blocks:      map[uint64]*types.Block{10: block10, 11: block11},
		blockErrors: map[uint64]error{11: errors.New("测试 RPC 限流")},
		headers:     map[uint64]*types.Header{10: block10.Header(), 11: block11.Header()},
	}
	store := &fakeStore{checkpoint: &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: defaultScannerName,
		LastScannedBlock: 10, LastScannedHash: strings.ToLower(block10.Hash().Hex()),
	}}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want temporary RPC failure")
	}
	if store.checkpoint.LastScannedBlock != 10 || len(store.observations) != 0 || len(store.credited) != 0 {
		t.Fatalf("failed scan changed state: checkpoint = %d, observations = %d, credited = %d",
			store.checkpoint.LastScannedBlock, len(store.observations), len(store.credited))
	}

	delete(chain.blockErrors, 11)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("recovery RunOnce() error = %v", err)
	}
	if store.checkpoint.LastScannedBlock != 11 {
		t.Fatalf("recovery checkpoint = %d, want 11", store.checkpoint.LastScannedBlock)
	}
	if len(chain.blockCalls) != 2 || chain.blockCalls[0] != 11 || chain.blockCalls[1] != 11 {
		t.Fatalf("block calls = %v, want [11 11]", chain.blockCalls)
	}
}

// TestScannerCreditsAfterMatchingBlockHash 验证达到确认数且区块哈希一致时执行入账。
func TestScannerCreditsAfterMatchingBlockHash(t *testing.T) {
	header10 := &types.Header{Number: big.NewInt(10), Time: 10}
	header12 := &types.Header{Number: big.NewInt(12), Time: 12}
	chain := &fakeChain{latest: 12, blocks: map[uint64]*types.Block{}, headers: map[uint64]*types.Header{10: header10, 12: header12}}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 12, LastScannedHash: strings.ToLower(header12.Hash().Hex())},
		deposits:   []postgres.Deposit{{ID: 9, BlockNumber: 10, BlockHash: strings.ToLower(header10.Hash().Hex()), Status: postgres.DepositConfirming}},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got := store.credited[9]; got != 3 {
		t.Fatalf("credited confirmations = %d, want 3", got)
	}
}

// TestScannerIsolatesAndOrphansBlockHashMismatch 验证孤块充值被冲正且不阻塞同批次其他合法充值入账。
func TestScannerIsolatesAndOrphansBlockHashMismatch(t *testing.T) {
	header10 := &types.Header{Number: big.NewInt(10), Time: 10}
	header12 := &types.Header{Number: big.NewInt(12), Time: 12}
	chain := &fakeChain{latest: 12, blocks: map[uint64]*types.Block{}, headers: map[uint64]*types.Header{10: header10, 12: header12}}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 12, LastScannedHash: strings.ToLower(header12.Hash().Hex())},
		deposits: []postgres.Deposit{
			{ID: 9, BlockNumber: 10, BlockHash: common.HexToHash("0x9999").Hex(), Status: postgres.DepositConfirming},
			{ID: 10, BlockNumber: 10, BlockHash: strings.ToLower(header10.Hash().Hex()), Status: postgres.DepositConfirming},
		},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v, want isolated handling without error", err)
	}
	if len(store.workerErrors) != 1 || !store.orphaned[9] {
		t.Fatalf("worker errors = %d, orphaned = %v", len(store.workerErrors), store.orphaned)
	}
	if got := store.credited[10]; got != 3 {
		t.Fatalf("valid deposit credited = %d, want 3", got)
	}
}

// TestScannerSafeDepthAndReorgRewind 验证扫描安全深度限制、父哈希不一致回退重扫及已入账充值告警保护。
func TestScannerSafeDepthAndReorgRewind(t *testing.T) {
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	relocatedTx := testTransaction(1, &target, 150)

	block7 := testBlock(7)
	block8 := testBlockWithParent(8, block7.Hash())
	block9 := testBlockWithParent(9, block8.Hash())
	block10New := testBlockWithParent(10, block9.Hash(), relocatedTx)
	block11New := testBlockWithParent(11, block10New.Hash())
	block12New := testBlockWithParent(12, block11New.Hash())

	chain := &fakeChain{
		latest: 14,
		blocks: map[uint64]*types.Block{
			7: block7, 8: block8, 9: block9, 10: block10New, 11: block11New, 12: block12New,
		},
		headers: map[uint64]*types.Header{
			7: block7.Header(), 8: block8.Header(), 9: block9.Header(),
			10: block10New.Header(), 11: block11New.Header(), 12: block12New.Header(),
		},
	}
	store := &fakeStore{
		addresses: []postgres.WalletAddress{{ID: 7, UserID: 5, Address: target.Hex()}},
		checkpoint: &postgres.ChainCheckpoint{
			Network: postgres.NetworkSepolia, Scanner: defaultScannerName,
			LastScannedBlock: 10, LastScannedHash: common.HexToHash("0xdeadbeef").Hex(),
		},
		deposits: []postgres.Deposit{
			{ID: 1, UserID: 5, AddressID: 7, TxHash: strings.ToLower(relocatedTx.Hash().Hex()), BlockNumber: 9, BlockHash: common.HexToHash("0xdead09").Hex(), AmountWei: big.NewInt(150), Status: postgres.DepositConfirming},
			{ID: 2, UserID: 5, AddressID: 7, TxHash: common.HexToHash("0xaaaa").Hex(), BlockNumber: 9, BlockHash: common.HexToHash("0xdead09").Hex(), AmountWei: big.NewInt(200), Status: postgres.DepositConfirming},
			{ID: 3, UserID: 5, AddressID: 7, TxHash: common.HexToHash("0xbbbb").Hex(), BlockNumber: 8, BlockHash: common.HexToHash("0xdead08").Hex(), AmountWei: big.NewInt(300), Status: postgres.DepositCredited},
		},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, SafeDepth: 2, ReorgRewindBlocks: 3, BatchSize: 10, Interval: time.Second})
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.rewinds) != 1 || store.rewinds[0].LastScannedBlock != 7 {
		t.Fatalf("rewinds = %+v, want rewind to block 7", store.rewinds)
	}
	// SafeDepth=2 且 latest=14，扫描上限应停在 12（不会扫描 13/14）。
	if store.checkpoint.LastScannedBlock != 12 {
		t.Fatalf("checkpoint = %d, want 12 (latest 14 - safeDepth 2)", store.checkpoint.LastScannedBlock)
	}
	// 重定位后的交易应更新为 block 10 且不被孤立，随后因 confirmations=14-10+1=5>=3 直接入账。
	if store.orphaned[1] || store.deposits[0].BlockNumber != 10 || store.credited[1] != 5 {
		t.Fatalf("relocated deposit = %+v, orphaned = %v, credited = %v", store.deposits[0], store.orphaned, store.credited)
	}
	// 消失的 pending 充值应被孤立。
	if !store.orphaned[2] {
		t.Fatalf("vanished deposit #2 should be orphaned, orphaned = %v", store.orphaned)
	}
	// 已入账的充值 #3 发生重组时绝不孤立，但应写入告警 worker_errors。
	if store.orphaned[3] || store.deposits[2].Status != postgres.DepositCredited {
		t.Fatalf("credited deposit #3 must remain credited, got status=%s orphaned=%v", store.deposits[2].Status, store.orphaned[3])
	}
	hasCreditedAlert := false
	for _, we := range store.workerErrors {
		if we.ErrorCode == "CREDITED_DEPOSIT_REORG" {
			hasCreditedAlert = true
		}
	}
	if !hasCreditedAlert {
		t.Fatalf("expected CREDITED_DEPOSIT_REORG alert in workerErrors: %+v", store.workerErrors)
	}
}

// TestScannerRequiresInitialBlockWithoutCheckpoint 验证全新数据库必须明确配置首次扫描高度。
func TestScannerRequiresInitialBlockWithoutCheckpoint(t *testing.T) {
	chain := &fakeChain{blocks: map[uint64]*types.Block{}, headers: map[uint64]*types.Header{}}
	store := &fakeStore{}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})
	if err := scanner.Initialize(context.Background()); !errors.Is(err, ErrStartBlockRequired) {
		t.Fatalf("Initialize() error = %v, want ErrStartBlockRequired", err)
	}
}

// newTestScanner 创建使用静默日志的测试扫描器。
func newTestScanner(t *testing.T, chain ChainClient, store Store, cfg Config) *Scanner {
	t.Helper()
	scanner, err := NewScanner(chain, store, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatalf("NewScanner() error = %v", err)
	}
	return scanner
}

// testBlockWithParent 构造带指定父哈希和交易的确定性测试区块。
func testBlockWithParent(height uint64, parentHash common.Hash, transactions ...*types.Transaction) *types.Block {
	header := &types.Header{Number: new(big.Int).SetUint64(height), Time: height, ParentHash: parentHash}
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: transactions})
}

// testBlock 构造包含指定交易的确定性测试区块。
func testBlock(height uint64, transactions ...*types.Transaction) *types.Block {
	return testBlockWithParent(height, common.Hash{}, transactions...)
}

// testTransaction 构造普通交易或合约创建交易。
func testTransaction(nonce uint64, to *common.Address, value int64) *types.Transaction {
	return types.NewTx(&types.LegacyTx{Nonce: nonce, To: to, Value: big.NewInt(value), Gas: 21_000, GasPrice: big.NewInt(1)})
}

// TestScannerBatchRPCAndIncrementalAddresses 验证批量 RPC 拉块、批量内部交易过滤、单次检查点提交及增量地址加载。
func TestScannerBatchRPCAndIncrementalAddresses(t *testing.T) {
	target1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	target2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tx1 := testTransaction(1, &target1, 100)
	txInternal := testTransaction(2, &target1, 200)
	tx3 := testTransaction(3, &target1, 300)

	b10 := testBlock(10, tx1)
	b11 := testBlockWithParent(11, b10.Hash(), txInternal)
	b12 := testBlockWithParent(12, b11.Hash())
	b13 := testBlockWithParent(13, b12.Hash(), tx3)
	b14 := testBlockWithParent(14, b13.Hash())

	chain := &fakeChain{
		latest: 14,
		blocks: map[uint64]*types.Block{10: b10, 11: b11, 12: b12, 13: b13, 14: b14},
		headers: map[uint64]*types.Header{
			10: b10.Header(), 11: b11.Header(), 12: b12.Header(), 13: b13.Header(), 14: b14.Header(),
		},
	}
	store := &fakeStore{
		addresses:   []postgres.WalletAddress{{ID: 7, UserID: 5, Address: target1.Hex()}},
		internalTxs: map[string]bool{strings.ToLower(txInternal.Hash().Hex()): true},
	}
	start := uint64(10)
	scanner := newTestScanner(t, chain, store, Config{StartBlock: &start, Confirmations: 3, BatchSize: 5, Interval: time.Second})
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	if len(chain.batchBlockCalls) != 1 || len(chain.batchBlockCalls[0]) != 5 || len(chain.blockCalls) != 0 {
		t.Fatalf("expected 1 batchBlockCalls of 5 blocks and 0 single blockCalls, got batch=%v single=%v", chain.batchBlockCalls, chain.blockCalls)
	}
	if len(chain.headerCalls) != 0 {
		t.Fatalf("confirmPending should hit cached headers from fetched blocks without extra HeaderByNumber calls, got %v", chain.headerCalls)
	}
	if store.filterInternalCalls != 1 || store.recordCalls != 1 {
		t.Fatalf("filterInternalCalls = %d, recordCalls = %d, want 1 and 1", store.filterInternalCalls, store.recordCalls)
	}
	if len(store.observations) != 2 || store.checkpoint.LastScannedBlock != 14 {
		t.Fatalf("observations = %d, checkpoint = %+v", len(store.observations), store.checkpoint)
	}

	// 新增地址后，第二轮使用 1000 回看窗口（max(7-1000,0)=0）避免漏掉并发乱序提交的小 ID 地址，并扫描新增区块 15。
	tx5 := testTransaction(4, &target2, 500)
	b15 := testBlockWithParent(15, b14.Hash(), tx5)
	chain.latest = 15
	chain.blocks[15] = b15
	chain.headers[15] = b15.Header()
	store.addresses = append(store.addresses, postgres.WalletAddress{ID: 8, UserID: 6, Address: target2.Hex()})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if len(store.addressQueries) != 2 || store.addressQueries[0] != 0 || store.addressQueries[1] != 0 {
		t.Fatalf("addressQueries = %v, want [0 0] with lookback window", store.addressQueries)
	}
	if len(store.observations) != 3 || store.observations[2].UserID != 6 {
		t.Fatalf("expected incremental address deposit observed, got %+v", store.observations)
	}
}

// TestScannerLookbackAndPeriodicFullRefreshRecoverOutOfOrderAddress 验证并发乱序提交的小 ID 地址可通过回看窗口与周期全量刷新补扫，不会永久漏账。
func TestScannerLookbackAndPeriodicFullRefreshRecoverOutOfOrderAddress(t *testing.T) {
	firstAddr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	lateInWindowAddr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	lateOldAddr := common.HexToAddress("0x3333333333333333333333333333333333333333")

	b10 := testBlock(10)
	b11 := testBlockWithParent(11, b10.Hash(), testTransaction(1, &lateInWindowAddr, 500))
	chain := &fakeChain{
		latest:  11,
		blocks:  map[uint64]*types.Block{10: b10, 11: b11},
		headers: map[uint64]*types.Header{10: b10.Header(), 11: b11.Header()},
	}
	store := &fakeStore{
		addresses: []postgres.WalletAddress{{ID: 1500, UserID: 1, Address: firstAddr.Hex()}},
	}
	start := uint64(10)
	scanner := newTestScanner(t, chain, store, Config{StartBlock: &start, Confirmations: 3, BatchSize: 10, Interval: time.Second})
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}

	// ID=1400 比 lastAddressID=1500 小，模拟并发事务晚提交但仍在 1000 回看窗口内；应在扫区块 11 时被捕获。
	store.addresses = append(store.addresses, postgres.WalletAddress{ID: 1400, UserID: 2, Address: lateInWindowAddr.Hex()})
	store.checkpoint.LastScannedBlock = 10
	store.checkpoint.LastScannedHash = strings.ToLower(b10.Hash().Hex())
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("lookback RunOnce() error = %v", err)
	}
	if len(store.observations) != 1 || store.observations[0].UserID != 2 {
		t.Fatalf("expected lookback window to recover address 1400, observations = %+v", store.observations)
	}

	// ID=100 超出 1000 回看窗口，到达周期全量刷新轮次后可找回该地址并捕获后续区块 12 的充值。
	b12 := testBlockWithParent(12, b11.Hash(), testTransaction(2, &lateOldAddr, 800))
	store.addresses = append(store.addresses, postgres.WalletAddress{ID: 100, UserID: 3, Address: lateOldAddr.Hex()})
	for i := 0; i < 49; i++ {
		if err := scanner.RunOnce(context.Background()); err != nil {
			t.Fatalf("periodic RunOnce(%d) error = %v", i, err)
		}
	}
	chain.latest = 12
	chain.blocks[12] = b12
	chain.headers[12] = b12.Header()
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("periodic full refresh RunOnce() error = %v", err)
	}
	if len(store.observations) < 2 || store.observations[1].UserID != 3 {
		t.Fatalf("expected periodic full refresh to recover old out-of-order address 100, observations = %+v", store.observations)
	}
}

// TestScannerStillConfirmsAndCreditsWhenScanFails 验证扫块阶段失败时不阻塞待确认充值继续确认入账。
func TestScannerStillConfirmsAndCreditsWhenScanFails(t *testing.T) {
	b10 := testBlock(10)
	b12 := testBlockWithParent(12, b10.Hash())
	chain := &fakeChain{
		latest:      12,
		blocks:      map[uint64]*types.Block{10: b10, 12: b12},
		blockErrors: map[uint64]error{11: errors.New("RPC 拉块临时超时")},
		headers:     map[uint64]*types.Header{10: b10.Header(), 12: b12.Header()},
	}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{
			Network: postgres.NetworkSepolia, Scanner: defaultScannerName,
			LastScannedBlock: 10, LastScannedHash: strings.ToLower(b10.Hash().Hex()),
		},
		deposits: []postgres.Deposit{
			{ID: 1, UserID: 7, BlockNumber: 10, BlockHash: strings.ToLower(b10.Hash().Hex()), Status: postgres.DepositConfirming},
		},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})
	if err := scanner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want scan error propagated")
	}
	if store.credited[1] != 3 {
		t.Fatalf("expected deposit 1 credited despite scan failure, got credited = %v", store.credited)
	}
}

// TestScannerAuditsCreditedDepositsIndependentlyBeyondBatchLimit 验证已入账充值重组核对独立执行、分批突破 confirmBatchSize 限制且不依赖扫块。
func TestScannerAuditsCreditedDepositsIndependentlyBeyondBatchLimit(t *testing.T) {
	b10 := testBlock(10)
	b11 := testBlockWithParent(11, b10.Hash())
	b12 := testBlockWithParent(12, b11.Hash())
	b13 := testBlockWithParent(13, b12.Hash())
	chain := &fakeChain{
		latest: 13,
		headers: map[uint64]*types.Header{
			10: b10.Header(), 11: b11.Header(), 12: b12.Header(), 13: b13.Header(),
		},
	}
	deposits := make([]postgres.Deposit, 0, 120)
	for i := 1; i <= 119; i++ {
		deposits = append(deposits, postgres.Deposit{
			ID: int64(i), BlockNumber: 10, BlockHash: strings.ToLower(b10.Hash().Hex()), Status: postgres.DepositCredited,
		})
	}
	// 第 120 条超出 confirmBatchSize(100) 且位于发生重组的区块 12（检查点已停在 13，本轮不触发扫块重扫）。
	deposits = append(deposits, postgres.Deposit{
		ID: 120, BlockNumber: 12, BlockHash: common.HexToHash("0xdead12").Hex(), Status: postgres.DepositCredited,
	})
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{
			Network: postgres.NetworkSepolia, Scanner: defaultScannerName,
			LastScannedBlock: 13, LastScannedHash: strings.ToLower(b13.Hash().Hex()),
		},
		deposits: deposits,
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, SafeDepth: 2, BatchSize: 10, Interval: time.Second})
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.workerErrors) != 1 || store.workerErrors[0].ErrorCode != "CREDITED_DEPOSIT_REORG" || *store.workerErrors[0].ReferenceID != 120 {
		t.Fatalf("expected CREDITED_DEPOSIT_REORG for deposit 120 beyond confirmBatchSize, got %+v", store.workerErrors)
	}
}
