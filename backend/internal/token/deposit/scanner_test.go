package deposit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

type fakeChain struct {
	latest           uint64
	headers          map[uint64]*types.Header
	headerCalls      []uint64
	batchHeaderCalls [][]uint64
	err              error
}

// BlockNumber 返回测试网络高度或错误。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) {
	return f.latest, f.err
}

// HeaderByNumber 返回测试区块头并记录调用。
func (f *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	f.headerCalls = append(f.headerCalls, number.Uint64())
	if f.err != nil {
		return nil, f.err
	}
	return f.headers[number.Uint64()], nil
}

// HeadersByNumbers 批量返回测试区块头并记录批量调用。
func (f *fakeChain) HeadersByNumbers(_ context.Context, numbers []*big.Int) ([]*types.Header, error) {
	batch := make([]uint64, len(numbers))
	headers := make([]*types.Header, len(numbers))
	if f.err != nil {
		f.batchHeaderCalls = append(f.batchHeaderCalls, batch)
		return nil, f.err
	}
	for i, n := range numbers {
		h := n.Uint64()
		batch[i] = h
		headers[i] = f.headers[h]
	}
	f.batchHeaderCalls = append(f.batchHeaderCalls, batch)
	return headers, nil
}

type blockRange struct {
	from uint64
	to   uint64
}

type fakeContract struct {
	address  common.Address
	logs     []types.Log
	events   map[uint]erc20.TransferEvent
	err      error
	calls    []blockRange
	onFilter func()
}

// Address 返回测试 Token 合约地址。
func (f *fakeContract) Address() common.Address { return f.address }

// FilterTransferLogs 记录区块范围并返回测试日志。
func (f *fakeContract) FilterTransferLogs(_ context.Context, fromBlock, toBlock uint64) ([]types.Log, error) {
	f.calls = append(f.calls, blockRange{from: fromBlock, to: toBlock})
	if f.onFilter != nil {
		f.onFilter()
	}
	return f.logs, f.err
}

// DecodeTransferLog 根据日志索引返回预设 Event。
func (f *fakeContract) DecodeTransferLog(log types.Log) (erc20.TransferEvent, error) {
	event, ok := f.events[log.Index]
	if !ok {
		return erc20.TransferEvent{}, erc20.ErrInvalidTransferLog
	}
	return event, nil
}

type fakeStore struct {
	addresses     []postgres.WalletAddress
	checkpoint    *postgres.ChainCheckpoint
	rewinds       []postgres.ChainCheckpoint
	recorded      []postgres.TokenDepositObservation
	recordCalls   int
	confirming    []postgres.TokenDeposit
	updated       map[int64]int64
	credited      map[int64]int
	orphaned      map[int64]bool
	workerErrors  []postgres.WorkerError
	nextDepositID int64
}

// ListWalletAddresses 返回测试托管地址。
func (f *fakeStore) ListWalletAddresses(context.Context) ([]postgres.WalletAddress, error) {
	return f.addresses, nil
}

// Checkpoint 返回当前测试检查点。
func (f *fakeStore) Checkpoint(context.Context, string, string) (postgres.ChainCheckpoint, error) {
	if f.checkpoint == nil {
		return postgres.ChainCheckpoint{}, postgres.ErrNotFound
	}
	return *f.checkpoint, nil
}

// RewindCheckpoint 回退测试检查点。
func (f *fakeStore) RewindCheckpoint(_ context.Context, network, scanner string, block int64, hash string) error {
	cp := postgres.ChainCheckpoint{Network: network, Scanner: scanner, LastScannedBlock: block, LastScannedHash: hash}
	f.checkpoint = &cp
	f.rewinds = append(f.rewinds, cp)
	return nil
}

// RecordTokenDepositsAndCheckpoint 记录扫描提交并推进内存检查点。
func (f *fakeStore) RecordTokenDepositsAndCheckpoint(_ context.Context, observations []postgres.TokenDepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.TokenDeposit, int, error) {
	f.recordCalls++
	f.recorded = append(f.recorded, observations...)
	f.checkpoint = &checkpoint
	items := make([]postgres.TokenDeposit, 0, len(observations))
	for _, obs := range observations {
		matched := false
		for i := range f.confirming {
			if f.confirming[i].AssetID == obs.AssetID && f.confirming[i].TxHash == obs.TxHash && f.confirming[i].LogIndex == obs.LogIndex && f.confirming[i].Status != postgres.DepositOrphaned {
				if f.confirming[i].Status != postgres.DepositCredited {
					f.confirming[i].BlockNumber = obs.BlockNumber
					f.confirming[i].BlockHash = obs.BlockHash
				}
				items = append(items, f.confirming[i])
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		f.nextDepositID++
		item := postgres.TokenDeposit{
			ID: f.nextDepositID, UserID: obs.UserID, AddressID: obs.AddressID, AssetID: obs.AssetID,
			TxHash: obs.TxHash, LogIndex: obs.LogIndex, BlockNumber: obs.BlockNumber, BlockHash: obs.BlockHash,
			FromAddress: obs.FromAddress, ToAddress: obs.ToAddress, AmountUnits: obs.AmountUnits, Status: postgres.DepositConfirming,
		}
		f.confirming = append(f.confirming, item)
		items = append(items, item)
	}
	return items, len(items), nil
}

// ListConfirmingTokenDeposits 返回测试待确认充值。
func (f *fakeStore) ListConfirmingTokenDeposits(context.Context, int64, int) ([]postgres.TokenDeposit, error) {
	result := make([]postgres.TokenDeposit, 0, len(f.confirming))
	for _, item := range f.confirming {
		if item.Status == postgres.DepositConfirming || item.Status == postgres.DepositConfirmed || item.Status == postgres.DepositDetected {
			result = append(result, item)
		}
	}
	return result, nil
}

// ListTokenDepositsAfterBlock 返回指定高度之后的非孤块测试 Token 充值记录。
func (f *fakeStore) ListTokenDepositsAfterBlock(ctx context.Context, assetID, afterBlock int64, limit int) ([]postgres.TokenDeposit, error) {
	return f.ListTokenDepositsAfterBlockCursor(ctx, assetID, afterBlock+1, afterBlock, 0, limit)
}

// ListTokenDepositsAfterBlockCursor 按键集游标分页返回测试非孤块 Token 充值记录。
func (f *fakeStore) ListTokenDepositsAfterBlockCursor(_ context.Context, assetID, minBlock, afterBlock, afterID int64, limit int) ([]postgres.TokenDeposit, error) {
	result := make([]postgres.TokenDeposit, 0)
	for _, item := range f.confirming {
		if item.AssetID == assetID && item.BlockNumber >= minBlock && item.Status != postgres.DepositOrphaned &&
			(item.BlockNumber > afterBlock || (item.BlockNumber == afterBlock && item.ID > afterID)) {
			result = append(result, item)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// ListCreditedTokenDepositsAfterCursor 按键集游标分页返回测试已入账 Token 充值记录。
func (f *fakeStore) ListCreditedTokenDepositsAfterCursor(_ context.Context, assetID, minBlock, afterBlock, afterID int64, limit int) ([]postgres.TokenDeposit, error) {
	result := make([]postgres.TokenDeposit, 0)
	for _, item := range f.confirming {
		if item.AssetID == assetID && item.BlockNumber >= minBlock && item.Status == postgres.DepositCredited &&
			(item.BlockNumber > afterBlock || (item.BlockNumber == afterBlock && item.ID > afterID)) {
			result = append(result, item)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// UpdateTokenDepositConfirmations 记录确认数更新。
func (f *fakeStore) UpdateTokenDepositConfirmations(_ context.Context, depositID, confirmations int64) (postgres.TokenDeposit, error) {
	f.updated[depositID] = confirmations
	return postgres.TokenDeposit{ID: depositID, Confirmations: confirmations}, nil
}

// CreditTokenDeposit 记录 Token 入账调用。
func (f *fakeStore) CreditTokenDeposit(_ context.Context, depositID, confirmations int64) (postgres.TokenDeposit, bool, error) {
	f.credited[depositID]++
	for i := range f.confirming {
		if f.confirming[i].ID == depositID {
			f.confirming[i].Status = postgres.DepositCredited
			break
		}
	}
	return postgres.TokenDeposit{ID: depositID, Confirmations: confirmations, Status: postgres.DepositCredited}, true, nil
}

// OrphanTokenDeposit 标记测试 Token 充值进入孤块状态。
func (f *fakeStore) OrphanTokenDeposit(_ context.Context, depositID int64) (postgres.TokenDeposit, bool, error) {
	if f.orphaned == nil {
		f.orphaned = make(map[int64]bool)
	}
	for i := range f.confirming {
		if f.confirming[i].ID == depositID {
			if f.confirming[i].Status == postgres.DepositOrphaned {
				return f.confirming[i], false, nil
			}
			f.confirming[i].Status = postgres.DepositOrphaned
			f.orphaned[depositID] = true
			return f.confirming[i], true, nil
		}
	}
	return postgres.TokenDeposit{}, false, postgres.ErrNotFound
}

// RecordWorkerError 保存测试后台错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.workerErrors = append(f.workerErrors, item)
	return int64(len(f.workerErrors)), nil
}

// TestScannerSortsFiltersAndResumesFromCheckpoint 验证地址过滤、稳定排序和重启后从检查点下一高度续扫。
func TestScannerSortsFiltersAndResumesFromCheckpoint(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	unmonitored := common.HexToAddress("0x2222222222222222222222222222222222222222")
	headers := headersForRange(10, 14)
	chain := &fakeChain{latest: 12, headers: headers}
	ev1 := testEvent(monitored, 10, 1)
	ev1.BlockHash = headers[10].Hash()
	ev2 := testEvent(unmonitored, 10, 2)
	ev2.BlockHash = headers[10].Hash()
	ev3 := testEvent(monitored, 12, 3)
	ev3.BlockHash = headers[12].Hash()
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		logs: []types.Log{
			{BlockNumber: 12, BlockHash: headers[12].Hash(), TxIndex: 1, Index: 3},
			{BlockNumber: 10, BlockHash: headers[10].Hash(), TxIndex: 2, Index: 2},
			{BlockNumber: 10, BlockHash: headers[10].Hash(), TxIndex: 1, Index: 1},
		},
		events: map[uint]erc20.TransferEvent{
			1: ev1, 2: ev2, 3: ev3,
		},
	}
	store := newFakeStore(monitored)
	start := uint64(10)
	scanner := newTestScanner(t, chain, contract, store, &start)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.recorded) != 2 || store.recorded[0].LogIndex != 1 || store.recorded[1].LogIndex != 3 {
		t.Fatalf("recorded observations = %+v", store.recorded)
	}
	if store.checkpoint == nil || store.checkpoint.LastScannedBlock != 12 {
		t.Fatalf("checkpoint = %+v", store.checkpoint)
	}

	contract.logs = nil
	chain.latest = 14
	restarted := newTestScanner(t, chain, contract, store, &start)
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restarted RunOnce() error = %v", err)
	}
	lastCall := contract.calls[len(contract.calls)-1]
	if lastCall.from != 13 || lastCall.to != 14 {
		t.Fatalf("restart range = %+v, want 13..14", lastCall)
	}
	health := restarted.Snapshot()
	if health.Status != "HEALTHY" || health.NetworkHeight != 14 || health.ScanHeight != 14 || health.LagBlocks != 0 {
		t.Fatalf("health = %+v", health)
	}
}

// TestScannerRPCFailureDoesNotAdvanceCheckpoint 验证 RPC 失败时检查点和充值提交都不变化。
func TestScannerRPCFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	chain := &fakeChain{latest: 10, headers: headersForRange(10, 10)}
	contract := &fakeContract{address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"), err: errors.New("RPC 临时失败")}
	store := newFakeStore(monitored)
	start := uint64(10)
	scanner := newTestScanner(t, chain, contract, store, &start)
	if err := scanner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want RPC failure")
	}
	if store.recordCalls != 0 || store.checkpoint != nil {
		t.Fatalf("record calls = %d, checkpoint = %+v", store.recordCalls, store.checkpoint)
	}
	if health := scanner.Snapshot(); health.Status != "DOWN" || health.LastError == "" {
		t.Fatalf("health = %+v", health)
	}
}

// TestScannerConfirmsOnlyAfterHashVerification 验证达到确认数并复核区块哈希后入账，孤块被冲正且不阻塞后续合法充值。
func TestScannerConfirmsOnlyAfterHashVerification(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(10, 12)
	chain := &fakeChain{latest: 12, headers: headers}
	contract := &fakeContract{address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"), events: make(map[uint]erc20.TransferEvent)}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: "erc20:test", LastScannedBlock: 12, LastScannedHash: stringsLower(headers[12].Hash().Hex())}
	store.confirming = []postgres.TokenDeposit{
		{ID: 7, AssetID: 1, BlockNumber: 10, BlockHash: stringsLower(headers[10].Hash().Hex()), Status: postgres.DepositConfirming},
	}
	scanner := newTestScanner(t, chain, contract, store, nil)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if store.credited[7] != 1 {
		t.Fatalf("credit calls = %d, want 1", store.credited[7])
	}
	store.confirming = []postgres.TokenDeposit{
		{ID: 8, AssetID: 1, BlockNumber: 10, BlockHash: common.HexToHash("0x9999").Hex(), Status: postgres.DepositConfirming},
		{ID: 9, AssetID: 1, BlockNumber: 10, BlockHash: stringsLower(headers[10].Hash().Hex()), Status: postgres.DepositConfirming},
	}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v, want isolated handling without error", err)
	}
	if len(store.workerErrors) != 1 || !store.orphaned[8] {
		t.Fatalf("worker errors = %d, orphaned = %v", len(store.workerErrors), store.orphaned)
	}
	if store.credited[9] != 1 {
		t.Fatalf("valid token deposit #9 credit calls = %d, want 1", store.credited[9])
	}
}

// TestScannerSafeDepthAndReorgRewind 验证 Token 扫描安全深度上限与重组回退重扫。
func TestScannerSafeDepthAndReorgRewind(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(7, 14)
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		events:  make(map[uint]erc20.TransferEvent),
	}
	chain := &fakeChain{latest: 14, headers: headers}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: "erc20:test",
		LastScannedBlock: 10, LastScannedHash: common.HexToHash("0xdead10").Hex(),
	}
	store.confirming = []postgres.TokenDeposit{
		{ID: 11, AssetID: 1, BlockNumber: 9, BlockHash: common.HexToHash("0xdead09").Hex(), Status: postgres.DepositConfirming},
		{ID: 12, AssetID: 1, BlockNumber: 8, BlockHash: common.HexToHash("0xdead08").Hex(), Status: postgres.DepositCredited},
	}
	scanner, err := NewScanner(chain, contract, store, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		AssetID: 1, BatchSize: 20, Confirmations: 3, SafeDepth: 2, ReorgRewindBlocks: 3, Interval: time.Second, ScannerName: "erc20:test",
	})
	if err != nil {
		t.Fatalf("NewScanner() error = %v", err)
	}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.rewinds) != 1 || store.rewinds[0].LastScannedBlock != 7 {
		t.Fatalf("rewinds = %+v, want block 7", store.rewinds)
	}
	if store.checkpoint.LastScannedBlock != 12 {
		t.Fatalf("checkpoint = %d, want 12 (latest 14 - safeDepth 2)", store.checkpoint.LastScannedBlock)
	}
	if !store.orphaned[11] || store.orphaned[12] {
		t.Fatalf("orphaned = %v, want deposit 11 orphaned and credited deposit 12 intact", store.orphaned)
	}
	if len(store.workerErrors) < 2 {
		t.Fatalf("expected worker error alerts, got %+v", store.workerErrors)
	}
}

// newFakeStore 创建包含一个托管地址的测试 Store。
func newFakeStore(monitored common.Address) *fakeStore {
	return &fakeStore{
		addresses: []postgres.WalletAddress{{ID: 11, UserID: 7, Network: postgres.NetworkSepolia, Address: stringsLower(monitored.Hex())}},
		updated:   make(map[int64]int64), credited: make(map[int64]int),
	}
}

// newTestScanner 创建无等待的 Token 扫描器。
func newTestScanner(t *testing.T, chain *fakeChain, contract *fakeContract, store *fakeStore, start *uint64) *Scanner {
	t.Helper()
	scanner, err := NewScanner(chain, contract, store, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		AssetID: 1, StartBlock: start, BatchSize: 20, Confirmations: 3, Interval: time.Second, ScannerName: "erc20:test",
	})
	if err != nil {
		t.Fatalf("NewScanner() error = %v", err)
	}
	return scanner
}

// headersForRange 构造连续高度与链式父哈希的确定性区块头。
func headersForRange(from, to uint64) map[uint64]*types.Header {
	result := make(map[uint64]*types.Header)
	for height := from; height <= to; height++ {
		var parentHash common.Hash
		if prev, ok := result[height-1]; ok {
			parentHash = prev.Hash()
		}
		result[height] = &types.Header{
			Number:     new(big.Int).SetUint64(height),
			ParentHash: parentHash,
			Extra:      []byte(fmt.Sprintf("block-%d", height)),
		}
	}
	return result
}

// testEvent 构造测试 Transfer Event。
func testEvent(to common.Address, block uint64, index uint) erc20.TransferEvent {
	return erc20.TransferEvent{
		Contract: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		From:     common.HexToAddress("0x3333333333333333333333333333333333333333"), To: to,
		AmountUnits: big.NewInt(int64(index) * 100), TxHash: common.BigToHash(big.NewInt(int64(index))),
		LogIndex: index, BlockNumber: block, BlockHash: common.BigToHash(big.NewInt(int64(block))),
	}
}

// stringsLower 将测试地址规范化为小写。
func stringsLower(value string) string {
	result := []byte(value)
	for index, character := range result {
		if character >= 'A' && character <= 'F' {
			result[index] = character + ('a' - 'A')
		}
	}
	return string(result)
}

// TestScannerCachesHeadersWithinTick 验证同一区块内的多笔 Token 待确认充值复用本轮区块头缓存，不重复发起 RPC 查询。
func TestScannerCachesHeadersWithinTick(t *testing.T) {
	headers := headersForRange(10, 14)
	chain := &fakeChain{latest: 14, headers: headers}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{
			Network: postgres.NetworkSepolia, Scanner: "erc20:test",
			LastScannedBlock: 14, LastScannedHash: stringsLower(headers[14].Hash().Hex()),
		},
		confirming: []postgres.TokenDeposit{
			{ID: 1, BlockNumber: 11, BlockHash: stringsLower(headers[11].Hash().Hex()), Status: postgres.DepositConfirming},
			{ID: 2, BlockNumber: 11, BlockHash: stringsLower(headers[11].Hash().Hex()), Status: postgres.DepositConfirming},
			{ID: 3, BlockNumber: 11, BlockHash: stringsLower(headers[11].Hash().Hex()), Status: postgres.DepositConfirming},
		},
		updated: make(map[int64]int64), credited: make(map[int64]int),
	}
	scanner := newTestScanner(t, chain, &fakeContract{}, store, nil)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	// 仅应校验 checkpoint 14 一次 + confirmPending 对区块 11 一次，而非 3 次重复查询区块 11。
	if len(chain.headerCalls) != 2 || len(chain.batchHeaderCalls) != 0 {
		t.Fatalf("headerCalls = %v, batchHeaderCalls = %v, want [14 11] and 0 batches", chain.headerCalls, chain.batchHeaderCalls)
	}
	if len(store.credited) != 3 {
		t.Fatalf("expected all 3 deposits credited, got %v", store.credited)
	}
}

// TestTokenScannerLookbackRecoversOutOfOrderAddress 验证并发乱序提交的小 ID 托管地址可被回看窗口和全量刷新捕获。
func TestTokenScannerLookbackRecoversOutOfOrderAddress(t *testing.T) {
	first := common.HexToAddress("0x1111111111111111111111111111111111111111")
	late := common.HexToAddress("0x2222222222222222222222222222222222222222")
	headers := headersForRange(10, 12)
	chain := &fakeChain{latest: 12, headers: headers}
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		events:  make(map[uint]erc20.TransferEvent),
	}
	store := &fakeStore{
		addresses: []postgres.WalletAddress{{ID: 1500, UserID: 1, Network: postgres.NetworkSepolia, Address: stringsLower(first.Hex())}},
		updated:   make(map[int64]int64), credited: make(map[int64]int),
	}
	start := uint64(10)
	scanner := newTestScanner(t, chain, contract, store, &start)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce() error = %v", err)
	}
	ev := testEvent(late, 12, 9)
	ev.BlockHash = headers[12].Hash()
	contract.logs = []types.Log{{BlockNumber: 12, BlockHash: headers[12].Hash(), TxIndex: 1, Index: 9}}
	contract.events[9] = ev
	store.addresses = append(store.addresses, postgres.WalletAddress{ID: 1200, UserID: 2, Network: postgres.NetworkSepolia, Address: stringsLower(late.Hex())})
	store.checkpoint.LastScannedBlock = 11
	store.checkpoint.LastScannedHash = stringsLower(headers[11].Hash().Hex())
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("lookback RunOnce() error = %v", err)
	}
	if len(store.recorded) != 1 || store.recorded[0].UserID != 2 {
		t.Fatalf("expected lookback address 1200 observed, recorded = %+v", store.recorded)
	}
}

// TestTokenScannerRewindsWhenLogBlockHashMismatchesHeader 验证日志 BlockHash 与区块头不一致时按重组回退且不落库。
func TestTokenScannerRewindsWhenLogBlockHashMismatchesHeader(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(7, 12)
	chain := &fakeChain{latest: 12, headers: headers}
	staleHash := common.HexToHash("0xdead11")
	ev := testEvent(monitored, 11, 1)
	ev.BlockHash = staleHash
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		logs:    []types.Log{{BlockNumber: 11, BlockHash: staleHash, TxIndex: 0, Index: 1}},
		events:  map[uint]erc20.TransferEvent{1: ev},
	}
	// 回退后第二轮扫不到假日志。
	var round int
	contract.onFilter = func() {
		round++
		if round > 1 {
			contract.logs = nil
		}
	}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: "erc20:test",
		LastScannedBlock: 10, LastScannedHash: stringsLower(headers[10].Hash().Hex()),
	}
	scanner := newTestScanner(t, chain, contract, store, nil)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(store.rewinds) == 0 || len(store.recorded) != 0 {
		t.Fatalf("expected reorg rewind without recording mismatched log, rewinds=%+v recorded=%+v", store.rewinds, store.recorded)
	}
}

// TestTokenScannerRevalidatesToBlockHeaderAfterGetLogs 验证 getLogs 期间末块发生重组时不会复用旧缓存提交过期哈希。
func TestTokenScannerRevalidatesToBlockHeaderAfterGetLogs(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(9, 12)
	oldToHash := stringsLower(headers[12].Hash().Hex())
	chain := &fakeChain{latest: 12, headers: headers}
	newToHeader := &types.Header{
		Number:     big.NewInt(12),
		ParentHash: headers[11].Hash(),
		Extra:      []byte("reorged-block-12"),
	}
	newToHash := stringsLower(newToHeader.Hash().Hex())
	var mutated bool
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		onFilter: func() {
			if !mutated {
				mutated = true
				chain.headers[12] = newToHeader
			}
		},
	}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: "erc20:test",
		LastScannedBlock: 9, LastScannedHash: stringsLower(headers[9].Hash().Hex()),
	}
	scanner := newTestScanner(t, chain, contract, store, nil)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if store.checkpoint == nil || store.checkpoint.LastScannedHash == oldToHash || store.checkpoint.LastScannedHash != newToHash {
		t.Fatalf("checkpoint hash = %s, want revalidated post-getLogs hash %s (not stale %s)", store.checkpoint.LastScannedHash, newToHash, oldToHash)
	}
}

// TestTokenScannerStillConfirmsAndCreditsWhenScanFails 验证 Token 扫块失败时不阻塞已有待确认充值确认与入账。
func TestTokenScannerStillConfirmsAndCreditsWhenScanFails(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(10, 12)
	chain := &fakeChain{latest: 12, headers: headers}
	contract := &fakeContract{address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"), err: errors.New("getLogs 超时")}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: "erc20:test",
		LastScannedBlock: 10, LastScannedHash: stringsLower(headers[10].Hash().Hex()),
	}
	store.confirming = []postgres.TokenDeposit{
		{ID: 5, AssetID: 1, BlockNumber: 10, BlockHash: stringsLower(headers[10].Hash().Hex()), Status: postgres.DepositConfirming},
	}
	scanner := newTestScanner(t, chain, contract, store, nil)
	if err := scanner.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want scan failure")
	}
	if store.credited[5] != 1 {
		t.Fatalf("expected token deposit 5 credited despite getLogs error, got %v", store.credited)
	}
}
