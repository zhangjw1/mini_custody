package gasstation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const gasStationTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// fakeChain 提供 Gas Station 测试所需的确定性余额、费用、Nonce 和广播结果。
type fakeChain struct {
	header      *types.Header
	tip         *big.Int
	transferGas uint64
	balances    map[common.Address]*big.Int
	nonce       uint64
	latest      uint64
	receipt     *types.Receipt
	receiptErr  error
	sendErrors  []error
	sentRaw     [][]byte
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// HeaderByNumber 返回测试 EIP-1559 区块头。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return f.header, nil
}

// BalanceAt 返回指定测试地址的 ETH 余额。
func (f *fakeChain) BalanceAt(_ context.Context, account common.Address, _ *big.Int) (*big.Int, error) {
	value := f.balances[account]
	if value == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(value), nil
}

// PendingNonceAt 返回平台热钱包测试 Nonce。
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return f.nonce, nil
}

// SuggestGasTipCap 返回测试建议优先费。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return new(big.Int).Set(f.tip), nil
}

// EstimateGas 返回普通 ETH 补气转账的测试 Gas Limit。
func (f *fakeChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return f.transferGas, nil
}

// SendTransaction 记录完整原始交易，并按配置返回广播错误。
func (f *fakeChain) SendTransaction(_ context.Context, transaction *types.Transaction) error {
	raw, _ := transaction.MarshalBinary()
	f.sentRaw = append(f.sentRaw, raw)
	if len(f.sendErrors) == 0 {
		return nil
	}
	err := f.sendErrors[0]
	f.sendErrors = f.sendErrors[1:]
	return err
}

// TransactionReceipt 返回测试 Receipt 或未找到错误。
func (f *fakeChain) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return f.receipt, f.receiptErr
}

// fakeContract 返回固定的 ERC-20 归集 Gas 估算。
type fakeContract struct{ gas uint64 }

// EstimateTransferGas 返回固定的 Token transfer Gas Limit。
func (f fakeContract) EstimateTransferGas(context.Context, common.Address, common.Address, *big.Int) (uint64, error) {
	return f.gas, nil
}

// fakeStore 在内存中模拟归集任务、平台钱包和内部转账状态机。
type fakeStore struct {
	sweep       postgres.TokenSweep
	address     postgres.WalletAddress
	platform    postgres.PlatformWallet
	transfer    *postgres.InternalTransfer
	settlement  *postgres.InternalTransferSettlement
	errors      []postgres.WorkerError
	markedReady bool
}

// TokenSweepByID 返回内存归集任务。
func (f *fakeStore) TokenSweepByID(context.Context, int64) (postgres.TokenSweep, error) {
	return f.sweep, nil
}

// ListGasStationSweeps 返回当前内存归集任务。
func (f *fakeStore) ListGasStationSweeps(context.Context, int) ([]postgres.TokenSweep, error) {
	return []postgres.TokenSweep{f.sweep}, nil
}

// WalletAddressByUser 返回归集来源地址。
func (f *fakeStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return f.address, nil
}

// PlatformWalletByRole 返回平台热钱包。
func (f *fakeStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	return f.platform, nil
}

// InternalTransferByID 返回内存内部转账。
func (f *fakeStore) InternalTransferByID(context.Context, int64) (postgres.InternalTransfer, error) {
	if f.transfer == nil {
		return postgres.InternalTransfer{}, postgres.ErrNotFound
	}
	return *f.transfer, nil
}

// MarkTokenSweepGasReady 标记当前归集地址无需补气。
func (f *fakeStore) MarkTokenSweepGasReady(context.Context, int64) (postgres.TokenSweep, bool, error) {
	f.sweep.Status = postgres.TokenSweepWaitingGas
	f.markedReady = true
	return f.sweep, true, nil
}

// CreateOrGetGasTopup 幂等创建测试内部转账并分配链上 Nonce。
func (f *fakeStore) CreateOrGetGasTopup(_ context.Context, request postgres.GasTopupRequest) (postgres.InternalTransfer, bool, error) {
	if f.transfer != nil {
		return *f.transfer, false, nil
	}
	f.transfer = &postgres.InternalTransfer{
		ID: 9, PlatformWalletID: request.PlatformWalletID, SweepID: request.SweepID,
		TransferType: postgres.InternalTransferGasTopup, FromAddress: request.FromAddress,
		ToAddress: request.ToAddress, AmountWei: new(big.Int).Set(request.AmountWei),
		Nonce: new(big.Int).SetUint64(request.ChainPendingNonce), Status: postgres.InternalTransferSigning,
	}
	f.sweep.Status = postgres.TokenSweepWaitingGas
	f.sweep.GasTopupTransferID = int64Pointer(f.transfer.ID)
	return *f.transfer, true, nil
}

// SaveSignedInternalTransfer 保存测试签名交易并进入已签名状态。
func (f *fakeStore) SaveSignedInternalTransfer(_ context.Context, signed postgres.SignedInternalTransfer) (postgres.InternalTransfer, bool, error) {
	f.transfer.GasLimit = int64Pointer(int64(signed.GasLimit))
	f.transfer.MaxFeePerGasWei = new(big.Int).Set(signed.MaxFeePerGasWei)
	f.transfer.MaxPriorityFeePerGasWei = new(big.Int).Set(signed.MaxPriorityFeePerGasWei)
	f.transfer.RawTx = append([]byte(nil), signed.RawTx...)
	f.transfer.TxHash = signed.TxHash
	f.transfer.Status = postgres.InternalTransferSigned
	return *f.transfer, true, nil
}

// TransitionInternalTransfer 更新测试内部转账状态。
func (f *fakeStore) TransitionInternalTransfer(_ context.Context, _ int64, target string) (postgres.InternalTransfer, error) {
	f.transfer.Status = target
	return *f.transfer, nil
}

// UpdateInternalTransferConfirmations 保存测试确认数并进入确认中状态。
func (f *fakeStore) UpdateInternalTransferConfirmations(_ context.Context, _ int64, confirmations int64) (postgres.InternalTransfer, error) {
	f.transfer.Confirmations = confirmations
	f.transfer.Status = postgres.InternalTransferChecking
	return *f.transfer, nil
}

// FinalizeInternalTransfer 保存测试结算结果并更新最终状态。
func (f *fakeStore) FinalizeInternalTransfer(_ context.Context, settlement postgres.InternalTransferSettlement) (postgres.InternalTransfer, bool, error) {
	f.settlement = &settlement
	if settlement.Success {
		f.transfer.Status = postgres.InternalTransferDone
	} else {
		f.transfer.Status = postgres.InternalTransferFailed
		f.sweep.Status = postgres.TokenSweepFailed
	}
	return *f.transfer, true, nil
}

// RecordWorkerError 保存测试 Worker 错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.errors = append(f.errors, item)
	return int64(len(f.errors)), nil
}

// TestAddSafetyMarginRoundsUp 验证安全余量使用基点并向上取整。
func TestAddSafetyMarginRoundsUp(t *testing.T) {
	if result := addSafetyMargin(big.NewInt(101), 2_000); result.Cmp(big.NewInt(122)) != 0 {
		t.Fatalf("addSafetyMargin() = %s, want 122", result.String())
	}
}

// TestWorkerSkipsTopupWhenUserGasIsEnough 验证用户地址 Gas 足够时不创建内部转账。
func TestWorkerSkipsTopupWhenUserGasIsEnough(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(20_000_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !store.markedReady || store.transfer != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("marked = %v, transfer = %+v, sends = %d", store.markedReady, store.transfer, len(chain.sentRaw))
	}
}

// TestWorkerSendsOnlyMinimumTopup 验证补气金额等于归集最大费用、安全余量与已有余额的差额。
func TestWorkerSendsOnlyMinimumTopup(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(2_000_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if store.transfer == nil || store.transfer.AmountWei.Cmp(big.NewInt(10_120_000)) != 0 {
		t.Fatalf("topup = %+v, want 10120000", store.transfer)
	}
	if store.transfer.Status != postgres.InternalTransferSent || len(chain.sentRaw) != 1 {
		t.Fatalf("status = %s, sends = %d", store.transfer.Status, len(chain.sentRaw))
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(store.transfer.RawTx); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if transaction.Type() != types.DynamicFeeTxType || transaction.Nonce() != 7 || transaction.Value().Cmp(big.NewInt(10_120_000)) != 0 {
		t.Fatalf("signed transaction type = %d, nonce = %d, value = %s", transaction.Type(), transaction.Nonce(), transaction.Value())
	}
}

// TestWorkerRejectsTopupAboveLimit 验证异常 Gas 需求超过安全上限时不会创建或广播交易。
func TestWorkerRejectsTopupAboveLimit(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	worker.config.GasTopupMaxWei = big.NewInt(1_000_000)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	err := worker.Process(context.Background(), store.sweep.ID)
	if !errors.Is(err, ErrTopupLimitExceeded) || store.transfer != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("error = %v, transfer = %+v, sends = %d", err, store.transfer, len(chain.sentRaw))
	}
}

// TestWorkerReplaysSameRawTransactionAfterTimeout 验证广播结果不明确时不更换 Nonce 或签名交易。
func TestWorkerReplaysSameRawTransactionAfterTimeout(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	chain.sendErrors = []error{context.DeadlineExceeded, nil}
	if err := worker.Process(context.Background(), store.sweep.ID); err == nil {
		t.Fatal("first Process() error = nil, want timeout")
	}
	if store.transfer == nil || store.transfer.Status != postgres.InternalTransferSigned {
		t.Fatalf("transfer = %+v, want SIGNED", store.transfer)
	}
	originalRaw := append([]byte(nil), store.transfer.RawTx...)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || string(chain.sentRaw[0]) != string(chain.sentRaw[1]) ||
		string(originalRaw) != string(store.transfer.RawTx) {
		t.Fatal("recovery changed the persisted raw transaction")
	}
}

// TestWorkerFinalizesConfirmedTopup 验证补气达到确认数后记录实际平台 Gas 成本。
func TestWorkerFinalizesConfirmedTopup(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10),
		GasUsed: 21_000, EffectiveGasPrice: big.NewInt(100),
	}
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || !store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(2_100_000)) != 0 {
		t.Fatalf("settlement = %+v", store.settlement)
	}
}

// TestWorkerReportsLowPlatformBalance 验证平台钱包低于阈值时暂停新补气并产生健康告警。
func TestWorkerReportsLowPlatformBalance(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	chain.balances[common.HexToAddress(store.platform.Address)] = big.NewInt(1)
	worker.config.PlatformMinBalanceWei = big.NewInt(10)
	err := worker.Process(context.Background(), store.sweep.ID)
	if !errors.Is(err, ErrPlatformBalanceLow) || store.transfer != nil {
		t.Fatalf("error = %v, transfer = %+v", err, store.transfer)
	}
	if snapshot := worker.Snapshot(); snapshot.Status != "LOW_BALANCE" {
		t.Fatalf("health status = %s, want LOW_BALANCE", snapshot.Status)
	}
}

// newTestWorker 创建具有固定密钥、钱包和费用参数的 Gas Station Worker。
func newTestWorker(t *testing.T) (*fakeChain, *fakeStore, *Worker) {
	t.Helper()
	provider, err := wallet.NewMnemonicKeyProvider(gasStationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	hotAddress, err := provider.Address(context.Background(), wallet.TreasuryPath)
	if err != nil {
		t.Fatalf("hot Address() error = %v", err)
	}
	userPath := wallet.UserPath(1)
	userAddress, err := provider.Address(context.Background(), userPath)
	if err != nil {
		t.Fatalf("user Address() error = %v", err)
	}
	store := &fakeStore{
		address: postgres.WalletAddress{ID: 2, UserID: 3, Address: userAddress.Hex(), DerivationPath: userPath},
		platform: postgres.PlatformWallet{ID: 4, Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot,
			Address: common.HexToAddress(hotAddress.Hex()).Hex(), DerivationPath: wallet.TreasuryPath, NextNonce: new(big.Int)},
		sweep: postgres.TokenSweep{ID: 5, UserID: 3, AddressID: 2, AssetID: 6,
			RecognizedAmountUnits: big.NewInt(1_000_000), Status: postgres.TokenSweepCreated},
	}
	chain := &fakeChain{
		header: &types.Header{BaseFee: big.NewInt(100)}, tip: big.NewInt(2), transferGas: 21_000,
		balances: map[common.Address]*big.Int{hotAddress: big.NewInt(1_000_000_000)},
		nonce:    7, latest: 9, receiptErr: ethereum.NotFound,
	}
	worker, err := NewWorker(chain, fakeContract{gas: 50_000}, store, provider,
		slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
			Interval: time.Second, Confirmations: 3, ChainID: big.NewInt(evm.SepoliaChainID),
			GasSafetyBPS: 2_000, GasTopupMaxWei: big.NewInt(100_000_000), PlatformMinBalanceWei: big.NewInt(1),
		})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return chain, store, worker
}

// int64Pointer 返回测试需要的 int64 指针。
func int64Pointer(value int64) *int64 { return &value }
