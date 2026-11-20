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
	latest      uint64
	blocks      map[uint64]*types.Block
	blockErrors map[uint64]error
	headers     map[uint64]*types.Header
	blockCalls  []uint64
	scanHeights []uint64
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

// HeaderByNumber 返回指定高度的测试区块头。
func (f *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	header, ok := f.headers[number.Uint64()]
	if !ok {
		return nil, errors.New("测试区块头不存在")
	}
	return header, nil
}

// SetScanHeight 记录扫描器已完成的高度。
func (f *fakeChain) SetScanHeight(height uint64) {
	f.scanHeights = append(f.scanHeights, height)
}

// fakeStore 为扫描器单元测试保存内存态检查点和充值记录。
type fakeStore struct {
	addresses     []postgres.WalletAddress
	checkpoint    *postgres.ChainCheckpoint
	rewinds       []postgres.ChainCheckpoint
	deposits      []postgres.Deposit
	observations  []postgres.DepositObservation
	confirmations map[int64]int64
	credited      map[int64]int64
	orphaned      map[int64]bool
	workerErrors  []postgres.WorkerError
	internalTxs   map[string]bool
	nextDepositID int64
}

// ListWalletAddresses 返回测试托管地址。
func (f *fakeStore) ListWalletAddresses(context.Context) ([]postgres.WalletAddress, error) {
	return f.addresses, nil
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

// IsInternalTransferTx 判断测试交易是否属于平台内部转账。
func (f *fakeStore) IsInternalTransferTx(_ context.Context, txHash string) (bool, error) {
	return f.internalTxs[txHash], nil
}

// RecordDepositsAndCheckpoint 保存整区块观察结果并推进内存检查点。
func (f *fakeStore) RecordDepositsAndCheckpoint(_ context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error) {
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

// ListDepositsAfterBlock 返回指定区块之后的非孤块测试充值记录。
func (f *fakeStore) ListDepositsAfterBlock(_ context.Context, afterBlock int64, _ int) ([]postgres.Deposit, error) {
	result := make([]postgres.Deposit, 0)
	for _, item := range f.deposits {
		if item.BlockNumber > afterBlock && item.Status != postgres.DepositOrphaned {
			result = append(result, item)
		}
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
