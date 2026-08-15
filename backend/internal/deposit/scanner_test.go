package deposit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
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
	deposits      []postgres.Deposit
	observations  []postgres.DepositObservation
	confirmations map[int64]int64
	credited      map[int64]int64
	workerErrors  []postgres.WorkerError
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

// RecordDepositsAndCheckpoint 保存整区块观察结果并推进内存检查点。
func (f *fakeStore) RecordDepositsAndCheckpoint(_ context.Context, observations []postgres.DepositObservation, checkpoint postgres.ChainCheckpoint) ([]postgres.Deposit, int, error) {
	f.checkpoint = &checkpoint
	f.observations = append(f.observations, observations...)
	items := make([]postgres.Deposit, 0, len(observations))
	for _, observation := range observations {
		f.nextDepositID++
		item := postgres.Deposit{
			ID:          f.nextDepositID,
			UserID:      observation.UserID,
			AddressID:   observation.AddressID,
			TxHash:      observation.TxHash,
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

// ListConfirmingDeposits 返回内存中尚未入账的充值。
func (f *fakeStore) ListConfirmingDeposits(context.Context, int) ([]postgres.Deposit, error) {
	return f.deposits, nil
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
	return postgres.Deposit{ID: depositID, Status: postgres.DepositCredited}, true, nil
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
	block11 := testBlock(11)
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

// TestScannerResumesAfterCheckpoint 验证重启后从持久化检查点的下一高度继续扫描。
func TestScannerResumesAfterCheckpoint(t *testing.T) {
	block11 := testBlock(11)
	chain := &fakeChain{latest: 11, blocks: map[uint64]*types.Block{11: block11}, headers: map[uint64]*types.Header{}}
	store := &fakeStore{checkpoint: &postgres.ChainCheckpoint{
		Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 10, LastScannedHash: common.HexToHash("0x10").Hex(),
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

// TestScannerCreditsAfterMatchingBlockHash 验证达到确认数且区块哈希一致时执行入账。
func TestScannerCreditsAfterMatchingBlockHash(t *testing.T) {
	header := &types.Header{Number: big.NewInt(10), Time: 10}
	chain := &fakeChain{latest: 12, blocks: map[uint64]*types.Block{}, headers: map[uint64]*types.Header{10: header}}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 12, LastScannedHash: common.HexToHash("0x12").Hex()},
		deposits:   []postgres.Deposit{{ID: 9, BlockNumber: 10, BlockHash: header.Hash().Hex(), Status: postgres.DepositConfirming}},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got := store.credited[9]; got != 3 {
		t.Fatalf("credited confirmations = %d, want 3", got)
	}
}

// TestScannerPausesOnBlockHashMismatch 验证链重组时记录错误并暂停自动入账。
func TestScannerPausesOnBlockHashMismatch(t *testing.T) {
	header := &types.Header{Number: big.NewInt(10), Time: 10}
	chain := &fakeChain{latest: 12, blocks: map[uint64]*types.Block{}, headers: map[uint64]*types.Header{10: header}}
	store := &fakeStore{
		checkpoint: &postgres.ChainCheckpoint{Network: postgres.NetworkSepolia, Scanner: defaultScannerName, LastScannedBlock: 12, LastScannedHash: common.HexToHash("0x12").Hex()},
		deposits:   []postgres.Deposit{{ID: 9, BlockNumber: 10, BlockHash: common.HexToHash("0x9999").Hex(), Status: postgres.DepositConfirming}},
	}
	scanner := newTestScanner(t, chain, store, Config{Confirmations: 3, BatchSize: 10, Interval: time.Second})

	err := scanner.RunOnce(context.Background())
	if !errors.Is(err, ErrBlockHashMismatch) {
		t.Fatalf("RunOnce() error = %v, want ErrBlockHashMismatch", err)
	}
	if len(store.workerErrors) != 1 || len(store.credited) != 0 {
		t.Fatalf("worker errors = %d, credited = %v", len(store.workerErrors), store.credited)
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

// testBlock 构造包含指定交易的确定性测试区块。
func testBlock(height uint64, transactions ...*types.Transaction) *types.Block {
	header := &types.Header{Number: new(big.Int).SetUint64(height), Time: height}
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: transactions})
}

// testTransaction 构造普通交易或合约创建交易。
func testTransaction(nonce uint64, to *common.Address, value int64) *types.Transaction {
	return types.NewTx(&types.LegacyTx{Nonce: nonce, To: to, Value: big.NewInt(value), Gas: 21_000, GasPrice: big.NewInt(1)})
}
