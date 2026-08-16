package sweep

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const sweepTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// fakeChain 提供 Token Sweep 测试所需的费用、ETH 余额、Nonce、广播和 Receipt。
type fakeChain struct {
	header     *types.Header
	tip        *big.Int
	balances   map[common.Address]*big.Int
	nonce      uint64
	latest     uint64
	receipt    *types.Receipt
	receiptErr error
	sendErrors []error
	sentRaw    [][]byte
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// HeaderByNumber 返回测试 EIP-1559 区块头。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return f.header, nil
}

// BalanceAt 返回指定地址的测试 ETH 余额。
func (f *fakeChain) BalanceAt(_ context.Context, account common.Address, _ *big.Int) (*big.Int, error) {
	value := f.balances[account]
	if value == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(value), nil
}

// PendingNonceAt 返回用户归集地址测试 Nonce。
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return f.nonce, nil
}

// SuggestGasTipCap 返回测试建议优先费。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return new(big.Int).Set(f.tip), nil
}

// SendTransaction 记录完整原始交易并按配置返回广播错误。
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

// fakeContractChain 为真实 ERC-20 适配器提供测试 balanceOf 和 Gas 估算。
type fakeContractChain struct {
	contract common.Address
	tokenABI abi.ABI
	balances map[common.Address]*big.Int
	gas      uint64
}

// CodeAt 返回非空测试合约字节码。
func (f *fakeContractChain) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return []byte{0x01}, nil
}

// CallContract 解析 balanceOf owner 并返回测试 Token 余额。
func (f *fakeContractChain) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	method := f.tokenABI.Methods["balanceOf"]
	if call.To == nil || *call.To != f.contract || len(call.Data) < 4 || !bytes.Equal(call.Data[:4], method.ID) {
		return nil, errors.New("测试合约调用无效")
	}
	values, err := method.Inputs.Unpack(call.Data[4:])
	if err != nil || len(values) != 1 {
		return nil, errors.New("测试 balanceOf 参数无效")
	}
	owner := values[0].(common.Address)
	balance := f.balances[owner]
	if balance == nil {
		balance = new(big.Int)
	}
	return method.Outputs.Pack(balance)
}

// FilterLogs 返回空测试日志列表。
func (f *fakeContractChain) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, nil
}

// EstimateGas 返回固定 Token transfer Gas Limit。
func (f *fakeContractChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return f.gas, nil
}

// fakeStore 在内存中模拟 Token 归集持久化状态机。
type fakeStore struct {
	sweep      postgres.TokenSweep
	address    postgres.WalletAddress
	platform   postgres.PlatformWallet
	settlement *postgres.TokenSweepSettlement
	errors     []postgres.WorkerError
	failed     bool
}

// TokenSweepByID 返回内存归集任务。
func (f *fakeStore) TokenSweepByID(context.Context, int64) (postgres.TokenSweep, error) {
	return f.sweep, nil
}

// ListProcessableTokenSweeps 返回当前内存归集任务。
func (f *fakeStore) ListProcessableTokenSweeps(context.Context, int) ([]postgres.TokenSweep, error) {
	return []postgres.TokenSweep{f.sweep}, nil
}

// WalletAddressByUser 返回归集来源用户地址。
func (f *fakeStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return f.address, nil
}

// PlatformWalletByRole 返回平台热钱包。
func (f *fakeStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	return f.platform, nil
}

// AllocateTokenSweepNonce 固定测试归集金额和 Nonce 并进入签名状态。
func (f *fakeStore) AllocateTokenSweepNonce(_ context.Context, _ int64, amountUnits *big.Int, nonce uint64) (postgres.TokenSweep, bool, error) {
	if f.sweep.Nonce != nil {
		return f.sweep, false, nil
	}
	f.sweep.SweepAmountUnits = new(big.Int).Set(amountUnits)
	f.sweep.Nonce = new(big.Int).SetUint64(nonce)
	f.sweep.Status = postgres.TokenSweepSigning
	return f.sweep, true, nil
}

// SaveSignedTokenSweep 保存测试签名交易并进入已签名状态。
func (f *fakeStore) SaveSignedTokenSweep(_ context.Context, signed postgres.SignedTokenSweep) (postgres.TokenSweep, bool, error) {
	f.sweep.GasLimit = int64Pointer(int64(signed.GasLimit))
	f.sweep.MaxFeePerGasWei = new(big.Int).Set(signed.MaxFeePerGasWei)
	f.sweep.MaxPriorityFeePerGasWei = new(big.Int).Set(signed.MaxPriorityFeePerGasWei)
	f.sweep.RawTx = append([]byte(nil), signed.RawTx...)
	f.sweep.TxHash = signed.TxHash
	f.sweep.Status = postgres.TokenSweepSigned
	return f.sweep, true, nil
}

// TransitionTokenSweep 更新测试归集状态。
func (f *fakeStore) TransitionTokenSweep(_ context.Context, _ int64, target string) (postgres.TokenSweep, error) {
	f.sweep.Status = target
	return f.sweep, nil
}

// UpdateTokenSweepConfirmations 保存测试确认数并进入确认中状态。
func (f *fakeStore) UpdateTokenSweepConfirmations(_ context.Context, _ int64, confirmations int64) (postgres.TokenSweep, error) {
	f.sweep.Confirmations = confirmations
	f.sweep.Status = postgres.TokenSweepConfirming
	return f.sweep, nil
}

// FinalizeTokenSweep 保存测试结算并更新最终状态。
func (f *fakeStore) FinalizeTokenSweep(_ context.Context, settlement postgres.TokenSweepSettlement) (postgres.TokenSweep, bool, error) {
	f.settlement = &settlement
	if settlement.Success {
		f.sweep.Status = postgres.TokenSweepCompleted
	} else {
		f.sweep.Status = postgres.TokenSweepFailed
	}
	return f.sweep, true, nil
}

// FailWaitingTokenSweep 将等待中的测试归集标记失败。
func (f *fakeStore) FailWaitingTokenSweep(_ context.Context, _ int64, _, _ string) (postgres.TokenSweep, bool, error) {
	f.sweep.Status = postgres.TokenSweepFailed
	f.failed = true
	return f.sweep, true, nil
}

// RecordWorkerError 保存测试 Worker 错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.errors = append(f.errors, item)
	return int64(len(f.errors)), nil
}

// TestWorkerSignsMinimumRecognizedBalanceAndBroadcasts 验证归集金额取链上余额与已识别金额的较小值。
func TestWorkerSignsMinimumRecognizedBalanceAndBroadcasts(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	contractChain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(800)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if store.sweep.Status != postgres.TokenSweepBroadcasted || store.sweep.SweepAmountUnits.Cmp(big.NewInt(800)) != 0 || len(chain.sentRaw) != 1 {
		t.Fatalf("status = %s, amount = %v, sends = %d", store.sweep.Status, store.sweep.SweepAmountUnits, len(chain.sentRaw))
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(store.sweep.RawTx); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if transaction.To() == nil || *transaction.To() != contractChain.contract || transaction.Nonce() != 7 || transaction.Value().Sign() != 0 {
		t.Fatalf("transaction to = %v, nonce = %d, value = %s", transaction.To(), transaction.Nonce(), transaction.Value())
	}
	to, amountUnits, err := worker.contract.DecodeTransferCalldata(transaction.Data())
	if err != nil || to != common.HexToAddress(store.platform.Address) || amountUnits.Cmp(big.NewInt(800)) != 0 {
		t.Fatalf("calldata to = %s, amount = %v, error = %v", to.Hex(), amountUnits, err)
	}
}

// TestWorkerReplaysSameRawTransactionAfterTimeout 验证广播超时后只重播同一份归集 raw_tx。
func TestWorkerReplaysSameRawTransactionAfterTimeout(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	contractChain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(1_000)
	chain.sendErrors = []error{context.DeadlineExceeded, nil}
	if err := worker.Process(context.Background(), store.sweep.ID); err == nil {
		t.Fatal("first Process() error = nil, want timeout")
	}
	if store.sweep.Status != postgres.TokenSweepSigned {
		t.Fatalf("status = %s, want SIGNED", store.sweep.Status)
	}
	originalRaw := append([]byte(nil), store.sweep.RawTx...)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || !bytes.Equal(chain.sentRaw[0], chain.sentRaw[1]) || !bytes.Equal(originalRaw, store.sweep.RawTx) {
		t.Fatal("recovery changed Token sweep raw transaction")
	}
}

// TestWorkerFinalizesExpectedTransferAndRefreshesInventory 验证预期到账 Event 完成归集并刷新热钱包库存。
func TestWorkerFinalizesExpectedTransferAndRefreshesInventory(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	from := common.HexToAddress(store.address.Address)
	to := common.HexToAddress(store.platform.Address)
	contractChain.balances[from] = big.NewInt(1_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	contractChain.balances[to] = big.NewInt(5_000)
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10), GasUsed: 50_000,
		EffectiveGasPrice: big.NewInt(100), Logs: []*types.Log{transferLog(t, contractChain, from, to, big.NewInt(1_000))},
	}
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || !store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(5_000_000)) != 0 {
		t.Fatalf("settlement = %+v", store.settlement)
	}
	if snapshot := worker.Snapshot(); snapshot.Status != "HEALTHY" || snapshot.BalanceUnits.Cmp(big.NewInt(5_000)) != 0 {
		t.Fatalf("inventory = %+v", snapshot)
	}
}

// TestWorkerFailsWhenExpectedHotWalletEventIsMissing 验证成功 Receipt 缺少预期到账 Event 时产生异常告警。
func TestWorkerFailsWhenExpectedHotWalletEventIsMissing(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	contractChain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(1_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10),
		GasUsed: 50_000, EffectiveGasPrice: big.NewInt(100), Logs: nil,
	}
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.Success || store.settlement.ErrorCode != "HOT_WALLET_CREDIT_MISSING" {
		t.Fatalf("settlement = %+v", store.settlement)
	}
	if len(store.errors) != 1 || store.errors[0].ErrorCode != "HOT_WALLET_CREDIT_MISSING" {
		t.Fatalf("worker errors = %+v", store.errors)
	}
}

// TestWorkerFailsBeforeNonceWhenTokenBalanceIsEmpty 验证链上 Token 为零时在分配 Nonce 前终止任务。
func TestWorkerFailsBeforeNonceWhenTokenBalanceIsEmpty(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !store.failed || store.sweep.Nonce != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("failed = %v, nonce = %v, sends = %d", store.failed, store.sweep.Nonce, len(chain.sentRaw))
	}
}

// transferLog 构造可被真实 ERC-20 适配器严格解析的 Transfer Event。
func transferLog(t *testing.T, chain *fakeContractChain, from, to common.Address, amountUnits *big.Int) *types.Log {
	t.Helper()
	event := chain.tokenABI.Events["Transfer"]
	data, err := event.Inputs.NonIndexed().Pack(amountUnits)
	if err != nil {
		t.Fatalf("pack Transfer data error = %v", err)
	}
	return &types.Log{
		Address: chain.contract, Topics: []common.Hash{event.ID, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())},
		Data: data, TxHash: common.HexToHash("0x01"), BlockHash: common.HexToHash("0x02"), BlockNumber: 10,
	}
}

// newTestWorker 创建使用真实 ERC-20 ABI 和固定托管密钥的归集 Worker。
func newTestWorker(t *testing.T) (*fakeChain, *fakeContractChain, *fakeStore, *Worker) {
	t.Helper()
	provider, err := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	userPath := wallet.UserPath(1)
	userAddress, err := provider.Address(context.Background(), userPath)
	if err != nil {
		t.Fatalf("user Address() error = %v", err)
	}
	hotAddress, err := provider.Address(context.Background(), wallet.TreasuryPath)
	if err != nil {
		t.Fatalf("hot Address() error = %v", err)
	}
	contractAddress := common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238")
	tokenABI, err := erc20.StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	contractChain := &fakeContractChain{
		contract: contractAddress, tokenABI: tokenABI, balances: make(map[common.Address]*big.Int), gas: 50_000,
	}
	contract, err := erc20.NewContract(contractChain, contractAddress)
	if err != nil {
		t.Fatalf("NewContract() error = %v", err)
	}
	store := &fakeStore{
		address: postgres.WalletAddress{ID: 2, UserID: 3, Address: userAddress.Hex(), DerivationPath: userPath},
		platform: postgres.PlatformWallet{ID: 4, Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot,
			Address: hotAddress.Hex(), DerivationPath: wallet.TreasuryPath, NextNonce: new(big.Int)},
		sweep: postgres.TokenSweep{ID: 5, UserID: 3, AddressID: 2, AssetID: 6,
			RecognizedAmountUnits: big.NewInt(1_000), Status: postgres.TokenSweepWaitingGas},
	}
	chain := &fakeChain{
		header: &types.Header{BaseFee: big.NewInt(100)}, tip: big.NewInt(2),
		balances: map[common.Address]*big.Int{userAddress: big.NewInt(20_000_000)},
		nonce:    7, latest: 9, receiptErr: ethereum.NotFound,
	}
	worker, err := NewWorker(chain, contract, store, provider, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Interval: time.Second, Confirmations: 3, ChainID: big.NewInt(evm.SepoliaChainID), Symbol: "USDC",
	})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return chain, contractChain, store, worker
}

// int64Pointer 返回测试需要的 int64 指针。
func int64Pointer(value int64) *int64 { return &value }
