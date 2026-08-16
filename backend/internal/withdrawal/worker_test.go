package withdrawal

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

const withdrawalTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// fakeChain 提供确定性的 EIP-1559 费用、Nonce、广播和 Receipt 行为。
type fakeChain struct {
	header     *types.Header
	tip        *big.Int
	gas        uint64
	balance    *big.Int
	nonce      uint64
	latest     uint64
	receipt    *types.Receipt
	receiptErr error
	sendErrors []error
	sentHashes []common.Hash
}

// BlockNumber 返回测试最新高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// HeaderByNumber 返回测试区块头。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return f.header, nil
}

// BalanceAt 返回测试链上余额。
func (f *fakeChain) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return new(big.Int).Set(f.balance), nil
}

// PendingNonceAt 返回测试链上 Pending Nonce。
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return f.nonce, nil
}

// SuggestGasTipCap 返回测试建议优先费。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return new(big.Int).Set(f.tip), nil
}

// EstimateGas 返回测试 Gas Limit。
func (f *fakeChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) { return f.gas, nil }

// SendTransaction 记录广播交易哈希并按顺序返回测试错误。
func (f *fakeChain) SendTransaction(_ context.Context, transaction *types.Transaction) error {
	f.sentHashes = append(f.sentHashes, transaction.Hash())
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

// fakeStore 在内存中模拟提币状态机和结算调用。
type fakeStore struct {
	item       postgres.Withdrawal
	address    postgres.WalletAddress
	settlement *postgres.WithdrawalSettlement
	errors     []postgres.WorkerError
}

// WithdrawalByID 返回内存提币记录。
func (f *fakeStore) WithdrawalByID(context.Context, int64) (postgres.Withdrawal, error) {
	return f.item, nil
}

// WalletAddressByUser 返回内存钱包地址。
func (f *fakeStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return f.address, nil
}

// ListProcessableWithdrawals 返回当前内存提币。
func (f *fakeStore) ListProcessableWithdrawals(context.Context, int) ([]postgres.Withdrawal, error) {
	return []postgres.Withdrawal{f.item}, nil
}

// IncreaseWithdrawalFee 更新内存最大预留费用。
func (f *fakeStore) IncreaseWithdrawalFee(_ context.Context, _ int64, value *big.Int) (postgres.Withdrawal, bool, error) {
	changed := value.Cmp(f.item.ReservedFeeWei) > 0
	if changed {
		f.item.ReservedFeeWei = new(big.Int).Set(value)
	}
	return f.item, changed, nil
}

// AllocateWithdrawalNonce 保存测试 Nonce 并进入签名中状态。
func (f *fakeStore) AllocateWithdrawalNonce(_ context.Context, _ int64, nonce uint64) (postgres.Withdrawal, bool, error) {
	if f.item.Nonce != nil {
		return f.item, false, nil
	}
	f.item.Nonce = new(big.Int).SetUint64(nonce)
	f.item.Status = postgres.WithdrawalSigning
	return f.item, true, nil
}

// SaveSignedWithdrawal 保存测试签名交易并进入已签名状态。
func (f *fakeStore) SaveSignedWithdrawal(_ context.Context, signed postgres.SignedWithdrawal) (postgres.Withdrawal, bool, error) {
	f.item.GasLimit = int64Pointer(int64(signed.GasLimit))
	f.item.MaxFeePerGasWei = new(big.Int).Set(signed.MaxFeePerGasWei)
	f.item.MaxPriorityFeePerGasWei = new(big.Int).Set(signed.MaxPriorityFeePerGasWei)
	f.item.RawTx = append([]byte(nil), signed.RawTx...)
	f.item.TxHash = signed.TxHash
	f.item.Status = postgres.WithdrawalSigned
	return f.item, true, nil
}

// TransitionWithdrawal 更新内存提币状态。
func (f *fakeStore) TransitionWithdrawal(_ context.Context, _ int64, target string) (postgres.Withdrawal, error) {
	f.item.Status = target
	return f.item, nil
}

// ReleaseWithdrawal 将内存提币标记失败。
func (f *fakeStore) ReleaseWithdrawal(context.Context, int64, string, string) (postgres.Withdrawal, bool, error) {
	f.item.Status = postgres.WithdrawalFailed
	return f.item, true, nil
}

// UpdateWithdrawalConfirmations 保存测试确认数和确认中状态。
func (f *fakeStore) UpdateWithdrawalConfirmations(_ context.Context, _ int64, confirmations int64) (postgres.Withdrawal, error) {
	f.item.Confirmations = confirmations
	f.item.Status = postgres.WithdrawalConfirming
	return f.item, nil
}

// FinalizeWithdrawal 保存结算参数并更新最终状态。
func (f *fakeStore) FinalizeWithdrawal(_ context.Context, settlement postgres.WithdrawalSettlement) (postgres.Withdrawal, bool, error) {
	f.settlement = &settlement
	if settlement.Success {
		f.item.Status = postgres.WithdrawalCompleted
	} else {
		f.item.Status = postgres.WithdrawalFailed
	}
	return f.item, true, nil
}

// RecordWorkerError 保存测试 Worker 错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.errors = append(f.errors, item)
	return int64(len(f.errors)), nil
}

// TestServiceCreatesWithdrawalWithExactFee 验证创建提币使用严格 ETH 金额和 EIP-1559 最大费用。
func TestServiceCreatesWithdrawalWithExactFee(t *testing.T) {
	chain := defaultFakeChain()
	store := &creationStore{address: testWalletAddress(t)}
	service, err := NewService(chain, store)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	result, err := service.Create(context.Background(), CreateRequest{
		IdempotencyKey: "request-1", UserID: 1,
		ToAddress: "0x2222222222222222222222222222222222222222", AmountETH: "0.002",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !result.Created || store.request.AmountWei.String() != "2000000000000000" {
		t.Fatalf("created = %v, amount = %v", result.Created, store.request.AmountWei)
	}
	if result.Fee.MaxFeePerGasWei.Cmp(big.NewInt(202)) != 0 || result.Fee.ReservedFeeWei.Cmp(big.NewInt(4_242_000)) != 0 {
		t.Fatalf("fee quote = %+v", result.Fee)
	}
}

// TestServiceReturnsCompletedIdempotentRequestWithoutReestimating 验证已完成提币重试直接返回持久化结果。
func TestServiceReturnsCompletedIdempotentRequestWithoutReestimating(t *testing.T) {
	address := testWalletAddress(t)
	gasLimit := int64(21_000)
	existing := postgres.Withdrawal{
		ID: 8, UserID: 1, AddressID: address.ID,
		ToAddress: "0x2222222222222222222222222222222222222222",
		AmountWei: big.NewInt(1_000_000_000_000_000), ReservedFeeWei: big.NewInt(40_000),
		GasLimit: &gasLimit, MaxFeePerGasWei: big.NewInt(200), MaxPriorityFeePerGasWei: big.NewInt(2),
		Status: postgres.WithdrawalCompleted,
	}
	chain := defaultFakeChain()
	chain.header = nil
	store := &creationStore{address: address, existing: &existing}
	service, err := NewService(chain, store)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	result, err := service.Create(context.Background(), CreateRequest{
		IdempotencyKey: "request-existing", UserID: 1,
		ToAddress: existing.ToAddress, AmountETH: "0.001",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.Created || result.Withdrawal.ID != existing.ID || result.Fee.GasLimit != 21_000 {
		t.Fatalf("result = %+v", result)
	}
}

// TestWorkerSignsAndBroadcastsDynamicFeeTransaction 验证 Worker 分配 Nonce、签名并广播 EIP-1559 交易。
func TestWorkerSignsAndBroadcastsDynamicFeeTransaction(t *testing.T) {
	chain := defaultFakeChain()
	store := defaultFakeStore(t, postgres.WithdrawalCreated)
	worker := newTestWorker(t, chain, store)

	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if store.item.Status != postgres.WithdrawalBroadcasted || len(chain.sentHashes) != 1 {
		t.Fatalf("status = %s, sends = %d", store.item.Status, len(chain.sentHashes))
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(store.item.RawTx); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if transaction.Type() != types.DynamicFeeTxType || transaction.Nonce() != 7 || transaction.GasFeeCap().Cmp(big.NewInt(202)) != 0 {
		t.Fatalf("signed transaction = type %d, nonce %d, fee %s", transaction.Type(), transaction.Nonce(), transaction.GasFeeCap())
	}
}

// TestWorkerRebroadcastsSameRawTransactionAfterTimeout 验证广播超时后只重播同一交易。
func TestWorkerRebroadcastsSameRawTransactionAfterTimeout(t *testing.T) {
	chain := defaultFakeChain()
	chain.sendErrors = []error{context.DeadlineExceeded, nil}
	store := defaultFakeStore(t, postgres.WithdrawalCreated)
	worker := newTestWorker(t, chain, store)

	if err := worker.Process(context.Background(), store.item.ID); err == nil {
		t.Fatal("first Process() error = nil, want ambiguous broadcast")
	}
	if store.item.Status != postgres.WithdrawalBroadcastUnknown {
		t.Fatalf("status = %s, want BROADCAST_UNKNOWN", store.item.Status)
	}
	raw := append([]byte(nil), store.item.RawTx...)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if store.item.Status != postgres.WithdrawalBroadcasted || len(chain.sentHashes) != 2 || chain.sentHashes[0] != chain.sentHashes[1] {
		t.Fatalf("status = %s, hashes = %v", store.item.Status, chain.sentHashes)
	}
	if string(raw) != string(store.item.RawTx) {
		t.Fatal("raw transaction changed during recovery")
	}
}

// TestWorkerResumesBroadcastedWithdrawalAfterRestart 验证新 Worker 从数据库恢复已广播提币，只跟踪原交易且不重复广播。
func TestWorkerResumesBroadcastedWithdrawalAfterRestart(t *testing.T) {
	chain := defaultFakeChain()
	store := defaultFakeStore(t, postgres.WithdrawalCreated)
	firstWorker := newTestWorker(t, chain, store)

	if err := firstWorker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	if store.item.Status != postgres.WithdrawalBroadcasted || len(chain.sentHashes) != 1 {
		t.Fatalf("before restart status = %s, sends = %d", store.item.Status, len(chain.sentHashes))
	}
	originalHash := store.item.TxHash
	originalRaw := append([]byte(nil), store.item.RawTx...)

	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10),
		GasUsed: 21_000, EffectiveGasPrice: big.NewInt(100),
	}
	restartedWorker := newTestWorker(t, chain, store)
	if err := restartedWorker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("restarted Process() error = %v", err)
	}
	if store.item.Status != postgres.WithdrawalCompleted || len(chain.sentHashes) != 1 {
		t.Fatalf("after restart status = %s, sends = %d", store.item.Status, len(chain.sentHashes))
	}
	if store.item.TxHash != originalHash || string(store.item.RawTx) != string(originalRaw) {
		t.Fatal("restart recovery changed persisted transaction")
	}
}

// TestWorkerSettlesSuccessfulReceiptWithActualGasFee 验证达到确认数后按 Receipt 实际 Gas 费用结算。
func TestWorkerSettlesSuccessfulReceiptWithActualGasFee(t *testing.T) {
	chain := defaultFakeChain()
	store := defaultFakeStore(t, postgres.WithdrawalCreated)
	worker := newTestWorker(t, chain, store)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10),
		GasUsed: 21_000, EffectiveGasPrice: big.NewInt(100),
	}
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || !store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(2_100_000)) != 0 {
		t.Fatalf("settlement = %+v", store.settlement)
	}
}

// TestWorkerSettlesFailedReceiptAndRefundsTransfer 验证链上执行失败时结算 Gas 并标记失败。
func TestWorkerSettlesFailedReceiptAndRefundsTransfer(t *testing.T) {
	chain := defaultFakeChain()
	store := defaultFakeStore(t, postgres.WithdrawalCreated)
	worker := newTestWorker(t, chain, store)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusFailed, BlockNumber: big.NewInt(10),
		GasUsed: 20_000, EffectiveGasPrice: big.NewInt(100),
	}
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(2_000_000)) != 0 {
		t.Fatalf("settlement = %+v", store.settlement)
	}
	if store.item.Status != postgres.WithdrawalFailed {
		t.Fatalf("status = %s, want FAILED", store.item.Status)
	}
}

// TestAlreadyKnownInspectsWrappedErrors 验证重复广播错误在包装后仍按成功处理。
func TestAlreadyKnownInspectsWrappedErrors(t *testing.T) {
	err := &evm.RPCError{Alias: "primary", Class: evm.FailureRPC, Err: errors.New("节点返回 already known")}
	if !alreadyKnown(err) {
		t.Fatal("alreadyKnown() = false, want true")
	}
}

// creationStore 记录创建服务提交的余额占用请求。
type creationStore struct {
	address  postgres.WalletAddress
	request  postgres.WithdrawalRequest
	existing *postgres.Withdrawal
}

// WalletAddressByUser 返回测试用户地址。
func (c *creationStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return c.address, nil
}

// WithdrawalByIdempotencyKey 返回测试已有提币或未找到错误。
func (c *creationStore) WithdrawalByIdempotencyKey(context.Context, int64, string) (postgres.Withdrawal, error) {
	if c.existing == nil {
		return postgres.Withdrawal{}, postgres.ErrNotFound
	}
	return *c.existing, nil
}

// ReserveWithdrawal 记录创建服务生成的数据库请求。
func (c *creationStore) ReserveWithdrawal(_ context.Context, request postgres.WithdrawalRequest) (postgres.Withdrawal, bool, error) {
	c.request = request
	return postgres.Withdrawal{ID: 1, AmountWei: request.AmountWei, ReservedFeeWei: request.ReservedFeeWei}, true, nil
}

// defaultFakeChain 创建默认未找到 Receipt 的测试链。
func defaultFakeChain() *fakeChain {
	return &fakeChain{
		header: &types.Header{BaseFee: big.NewInt(100)}, tip: big.NewInt(2), gas: 21_000,
		balance: big.NewInt(1_000_000_000), nonce: 7, latest: 9, receiptErr: ethereum.NotFound,
	}
}

// defaultFakeStore 创建指定初始状态的测试提币存储。
func defaultFakeStore(t *testing.T, status string) *fakeStore {
	t.Helper()
	address := testWalletAddress(t)
	return &fakeStore{
		address: address,
		item: postgres.Withdrawal{
			ID: 1, UserID: 1, AddressID: address.ID, ToAddress: "0x2222222222222222222222222222222222222222",
			AmountWei: big.NewInt(1_000_000), ReservedFeeWei: big.NewInt(1), Status: status,
		},
	}
}

// testWalletAddress 从固定助记词派生测试出款地址。
func testWalletAddress(t *testing.T) postgres.WalletAddress {
	t.Helper()
	provider, err := wallet.NewMnemonicKeyProvider(withdrawalTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	path := wallet.UserPath(1)
	address, err := provider.Address(context.Background(), path)
	if err != nil {
		t.Fatalf("Address() error = %v", err)
	}
	return postgres.WalletAddress{ID: 1, UserID: 1, Address: address.Hex(), DerivationPath: path}
}

// newTestWorker 创建使用固定密钥和静默日志的测试 Worker。
func newTestWorker(t *testing.T, chain Chain, store WorkerStore) *Worker {
	t.Helper()
	provider, err := wallet.NewMnemonicKeyProvider(withdrawalTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	worker, err := NewWorker(chain, store, provider, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Interval: time.Second, Confirmations: 3, ChainID: big.NewInt(evm.SepoliaChainID),
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return worker
}

// int64Pointer 返回测试所需的 int64 指针。
func int64Pointer(value int64) *int64 { return &value }
