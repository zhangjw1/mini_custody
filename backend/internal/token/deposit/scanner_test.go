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
	latest  uint64
	headers map[uint64]*types.Header
	err     error
}

// BlockNumber 返回测试网络高度或错误。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) {
	return f.latest, f.err
}

// HeaderByNumber 返回测试区块头。
func (f *fakeChain) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.headers[number.Uint64()], nil
}

type blockRange struct {
	from uint64
	to   uint64
}

type fakeContract struct {
	address common.Address
	logs    []types.Log
	events  map[uint]erc20.TransferEvent
	err     error
	calls   []blockRange
}

// Address 返回测试 Token 合约地址。
func (f *fakeContract) Address() common.Address { return f.address }

// FilterTransferLogs 记录区块范围并返回测试日志。
func (f *fakeContract) FilterTransferLogs(_ context.Context, fromBlock, toBlock uint64) ([]types.Log, error) {
	f.calls = append(f.calls, blockRange{from: fromBlock, to: toBlock})
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
	addresses    []postgres.WalletAddress
	checkpoint   *postgres.ChainCheckpoint
	recorded     []postgres.TokenDepositObservation
	recordCalls  int
	confirming   []postgres.TokenDeposit
	updated      map[int64]int64
	credited     map[int64]int
	workerErrors []postgres.WorkerError
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

// RecordTokenDepositsAndCheckpoint 记录扫描提交并推进内存检查点。
func (f *fakeStore) RecordTokenDepositsAndCheckpoint(_ context.Context, observations []postgres.TokenDepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.TokenDeposit, int, error) {
	f.recordCalls++
	f.recorded = append(f.recorded, observations...)
	f.checkpoint = &checkpoint
	return make([]postgres.TokenDeposit, len(observations)), len(observations), nil
}

// ListConfirmingTokenDeposits 返回测试待确认充值。
func (f *fakeStore) ListConfirmingTokenDeposits(context.Context, int64, int) ([]postgres.TokenDeposit, error) {
	return f.confirming, nil
}

// UpdateTokenDepositConfirmations 记录确认数更新。
func (f *fakeStore) UpdateTokenDepositConfirmations(_ context.Context, depositID, confirmations int64) (postgres.TokenDeposit, error) {
	f.updated[depositID] = confirmations
	return postgres.TokenDeposit{ID: depositID, Confirmations: confirmations}, nil
}

// CreditTokenDeposit 记录 Token 入账调用。
func (f *fakeStore) CreditTokenDeposit(_ context.Context, depositID, confirmations int64) (postgres.TokenDeposit, bool, error) {
	f.credited[depositID]++
	return postgres.TokenDeposit{ID: depositID, Confirmations: confirmations, Status: postgres.DepositCredited}, true, nil
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
	chain := &fakeChain{latest: 12, headers: headersForRange(10, 14)}
	contract := &fakeContract{
		address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"),
		logs:    []types.Log{{BlockNumber: 12, TxIndex: 1, Index: 3}, {BlockNumber: 10, TxIndex: 2, Index: 2}, {BlockNumber: 10, TxIndex: 1, Index: 1}},
		events: map[uint]erc20.TransferEvent{
			1: testEvent(monitored, 10, 1), 2: testEvent(unmonitored, 10, 2), 3: testEvent(monitored, 12, 3),
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

// TestScannerConfirmsOnlyAfterHashVerification 验证达到确认数并复核区块哈希后才执行入账。
func TestScannerConfirmsOnlyAfterHashVerification(t *testing.T) {
	monitored := common.HexToAddress("0x1111111111111111111111111111111111111111")
	headers := headersForRange(10, 12)
	chain := &fakeChain{latest: 12, headers: headers}
	contract := &fakeContract{address: common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"), events: make(map[uint]erc20.TransferEvent)}
	store := newFakeStore(monitored)
	store.checkpoint = &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: "erc20:test", LastScannedBlock: 12, LastScannedHash: headers[12].Hash().Hex()}
	store.confirming = []postgres.TokenDeposit{{ID: 7, AssetID: 1, BlockNumber: 10, BlockHash: headers[10].Hash().Hex(), Status: postgres.DepositConfirming}}
	scanner := newTestScanner(t, chain, contract, store, nil)
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if store.credited[7] != 1 {
		t.Fatalf("credit calls = %d, want 1", store.credited[7])
	}
	store.confirming[0].BlockHash = common.HexToHash("0x9999").Hex()
	if err := scanner.RunOnce(context.Background()); !errors.Is(err, ErrBlockHashMismatch) {
		t.Fatalf("RunOnce() error = %v, want hash mismatch", err)
	}
	if len(store.workerErrors) != 1 {
		t.Fatalf("worker errors = %d, want 1", len(store.workerErrors))
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

// headersForRange 构造连续高度的确定性区块头。
func headersForRange(from, to uint64) map[uint64]*types.Header {
	result := make(map[uint64]*types.Header)
	for height := from; height <= to; height++ {
		result[height] = &types.Header{Number: new(big.Int).SetUint64(height), Extra: []byte(fmt.Sprintf("block-%d", height))}
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
