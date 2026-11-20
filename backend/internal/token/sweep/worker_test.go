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
	header         *types.Header
	tip            *big.Int
	balances       map[common.Address]*big.Int
	nonce          uint64
	confirmedNonce uint64
	latest         uint64
	receipt        *types.Receipt
	receiptsByHash map[common.Hash]*types.Receipt
	receiptErr     error
	sendErrors     []error
	sentRaw        [][]byte
	headerCalls    int
	tipCalls       int
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// HeaderByNumber 返回测试 EIP-1559 区块头并累计调用次数。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	f.headerCalls++
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

// NonceAt 返回用户归集地址测试最新已确认 Nonce。
func (f *fakeChain) NonceAt(context.Context, common.Address, *big.Int) (uint64, error) {
	return f.confirmedNonce, nil
}

// SuggestGasTipCap 返回测试建议优先费并累计调用次数。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	f.tipCalls++
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
func (f *fakeChain) TransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	if f.receiptsByHash != nil {
		if receipt, ok := f.receiptsByHash[hash]; ok && receipt != nil {
			return receipt, nil
		}
		return nil, ethereum.NotFound
	}
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
	attempts   []postgres.EVMTxAttempt
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

// AllocateTokenSweepNonce 固定测试归集金额、Nonce 和 Gas 参数并进入签名状态。
func (f *fakeStore) AllocateTokenSweepNonce(_ context.Context, _ int64, amountUnits *big.Int, nonce uint64, gas postgres.PreparedGasParams) (postgres.TokenSweep, bool, error) {
	if f.sweep.Nonce != nil {
		return f.sweep, false, nil
	}
	f.sweep.SweepAmountUnits = new(big.Int).Set(amountUnits)
	f.sweep.Nonce = new(big.Int).SetUint64(nonce)
	f.sweep.GasLimit = int64Pointer(int64(gas.GasLimit))
	f.sweep.MaxFeePerGasWei = new(big.Int).Set(gas.MaxFeePerGasWei)
	f.sweep.MaxPriorityFeePerGasWei = new(big.Int).Set(gas.MaxPriorityFeePerGasWei)
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
	f.sweep.SigningRetryCount = 0
	f.sweep.Status = postgres.TokenSweepSigned
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessTokenSweep,
		BusinessID:              f.sweep.ID,
		Nonce:                   f.sweep.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  signed.TxHash,
		RawTx:                   append([]byte(nil), signed.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(signed.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(signed.MaxPriorityFeePerGasWei),
		GasLimit:                int64(signed.GasLimit),
		CreatedAt:               time.Now(),
	}}, f.attempts...)
	return f.sweep, true, nil
}

// ListEVMTxAttempts 返回按尝试序号倒序排列的测试候选交易记录。
func (f *fakeStore) ListEVMTxAttempts(context.Context, string, int64) ([]postgres.EVMTxAttempt, error) {
	return append([]postgres.EVMTxAttempt(nil), f.attempts...), nil
}

// ReplaceTokenSweepTx 保存测试 Token 归集加价替换交易并推进替换次数。
func (f *fakeStore) ReplaceTokenSweepTx(_ context.Context, replacement postgres.SignedTokenSweep) (postgres.TokenSweep, bool, error) {
	f.sweep.GasLimit = int64Pointer(int64(replacement.GasLimit))
	f.sweep.MaxFeePerGasWei = new(big.Int).Set(replacement.MaxFeePerGasWei)
	f.sweep.MaxPriorityFeePerGasWei = new(big.Int).Set(replacement.MaxPriorityFeePerGasWei)
	f.sweep.RawTx = append([]byte(nil), replacement.RawTx...)
	f.sweep.TxHash = replacement.TxHash
	f.sweep.BumpCount++
	f.sweep.BumpBlockedAt = nil
	f.sweep.BumpBlockedReason = ""
	now := time.Now()
	f.sweep.LastBroadcastAt = &now
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessTokenSweep,
		BusinessID:              f.sweep.ID,
		Nonce:                   f.sweep.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  replacement.TxHash,
		RawTx:                   append([]byte(nil), replacement.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(replacement.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(replacement.MaxPriorityFeePerGasWei),
		GasLimit:                int64(replacement.GasLimit),
		CreatedAt:               now,
	}}, f.attempts...)
	return f.sweep, true, nil
}

// TouchTokenSweepBroadcast 更新测试 Token 归集最近广播时间戳。
func (f *fakeStore) TouchTokenSweepBroadcast(context.Context, int64) (postgres.TokenSweep, error) {
	now := time.Now()
	if f.sweep.BroadcastedAt == nil {
		f.sweep.BroadcastedAt = &now
	}
	f.sweep.LastBroadcastAt = &now
	return f.sweep, nil
}

// MarkTokenSweepManualReview 将测试 Token 归集转入人工核查状态。
func (f *fakeStore) MarkTokenSweepManualReview(_ context.Context, _ int64, errorCode, errorMessage string) (postgres.TokenSweep, bool, error) {
	f.sweep.Status = postgres.TokenSweepManualReview
	f.sweep.ErrorCode = errorCode
	f.sweep.ErrorMessage = errorMessage
	return f.sweep, true, nil
}

// TransitionTokenSweep 更新测试归集状态。
func (f *fakeStore) TransitionTokenSweep(_ context.Context, _ int64, target string) (postgres.TokenSweep, error) {
	f.sweep.Status = target
	if target == postgres.TokenSweepBroadcasted {
		now := time.Now()
		if f.sweep.BroadcastedAt == nil {
			f.sweep.BroadcastedAt = &now
		}
		f.sweep.LastBroadcastAt = &now
	}
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
	if settlement.TxHash != "" {
		f.sweep.TxHash = settlement.TxHash
	}
	return f.sweep, true, nil
}

// FailWaitingTokenSweep 将等待中的测试归集标记失败。
func (f *fakeStore) FailWaitingTokenSweep(_ context.Context, _ int64, _, _ string) (postgres.TokenSweep, bool, error) {
	f.sweep.Status = postgres.TokenSweepFailed
	f.failed = true
	return f.sweep, true, nil
}

// RecordEVMSigningFailure 记录 Token 归集签名失败并在达到上限后转入人工核查且仅告警一次。
func (f *fakeStore) RecordEVMSigningFailure(_ context.Context, _ string, _ int64, maxRetries int, worker, refType string, refID int64, errorCode, errorMessage string) (bool, error) {
	f.sweep.SigningRetryCount++
	if int(f.sweep.SigningRetryCount) >= maxRetries && f.sweep.Status == postgres.TokenSweepSigning {
		f.sweep.Status = postgres.TokenSweepManualReview
		f.sweep.ErrorCode = errorCode
		f.sweep.ErrorMessage = errorMessage
		f.errors = append(f.errors, postgres.WorkerError{
			Worker: worker, Stage: "sign", ReferenceType: refType, ReferenceID: &refID,
			ErrorCode: errorCode, ErrorMessage: errorMessage,
		})
		return true, nil
	}
	return false, nil
}

// RecordEVMBumpBlocked 持久化首次加价阻塞原因并去重写入告警。
func (f *fakeStore) RecordEVMBumpBlocked(_ context.Context, _ string, _ int64, worker, refType string, refID int64, reasonCode, reasonMessage string) (time.Time, bool, error) {
	if f.sweep.BumpBlockedAt == nil {
		now := time.Now()
		f.sweep.BumpBlockedAt = &now
		f.sweep.BumpBlockedReason = reasonMessage
		f.errors = append(f.errors, postgres.WorkerError{
			Worker: worker, Stage: "bump", ReferenceType: refType, ReferenceID: &refID,
			ErrorCode: reasonCode, ErrorMessage: reasonMessage,
		})
		return now, true, nil
	}
	return *f.sweep.BumpBlockedAt, false, nil
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

// TestWorkerRebroadcastsBumpsAndSettlesMatchedSweepAttempt 验证 Token 归集重播、加价替换和早期尝试结算。
func TestWorkerRebroadcastsBumpsAndSettlesMatchedSweepAttempt(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	worker.config.RebroadcastAfter = 10 * time.Second
	worker.config.BumpAfter = 30 * time.Second
	worker.config.BumpPercent = 15
	worker.config.MaxBumps = 3

	from := common.HexToAddress(store.address.Address)
	to := common.HexToAddress(store.platform.Address)
	contractChain.balances[from] = big.NewInt(1_000)

	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	firstHash := common.HexToHash(store.sweep.TxHash)
	stale := time.Now().Add(-15 * time.Second)
	store.sweep.BroadcastedAt = &stale
	store.sweep.LastBroadcastAt = &stale
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("rebroadcast Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || !bytes.Equal(chain.sentRaw[0], chain.sentRaw[1]) || store.sweep.BumpCount != 0 {
		t.Fatalf("sends = %d, bump_count = %d", len(chain.sentRaw), store.sweep.BumpCount)
	}

	veryStale := time.Now().Add(-time.Minute)
	store.sweep.BroadcastedAt = &veryStale
	store.sweep.LastBroadcastAt = &veryStale
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("bump Process() error = %v", err)
	}
	if store.sweep.BumpCount != 1 || len(store.attempts) != 2 || len(chain.sentRaw) != 3 {
		t.Fatalf("bump_count = %d, attempts = %d, sends = %d", store.sweep.BumpCount, len(store.attempts), len(chain.sentRaw))
	}

	chain.latest = 12
	contractChain.balances[to] = big.NewInt(1_000)
	chain.receiptsByHash = map[common.Hash]*types.Receipt{
		firstHash: {
			Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10), GasUsed: 50_000,
			EffectiveGasPrice: big.NewInt(100), Logs: []*types.Log{transferLog(t, contractChain, from, to, big.NewInt(1_000))},
		},
	}
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.TxHash != firstHash.Hex() || store.sweep.Status != postgres.TokenSweepCompleted {
		t.Fatalf("settlement = %+v, status = %s", store.settlement, store.sweep.Status)
	}
}

// TestWorkerMovesNonceGapSweepToManualReview 验证越过 Nonce 且无 Receipt 时 Token 归集转入人工核查。
func TestWorkerMovesNonceGapSweepToManualReview(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	contractChain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(1_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	chain.confirmedNonce = store.sweep.Nonce.Uint64() + 1
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("manual review Process() error = %v", err)
	}
	if store.sweep.Status != postgres.TokenSweepManualReview || len(store.errors) == 0 || store.errors[0].ErrorCode != "EVM_TX_MANUAL_REVIEW" {
		t.Fatalf("status = %s, errors = %+v", store.sweep.Status, store.errors)
	}
}

// TestWorkerSignsSweepWithoutExtraFeeRPC 验证归集 SIGNING 阶段仅使用持久化 Gas 参数而不调用费率 RPC。
func TestWorkerSignsSweepWithoutExtraFeeRPC(t *testing.T) {
	chain, contractChain, store, worker := newTestWorker(t)
	contractChain.balances[common.HexToAddress(store.address.Address)] = big.NewInt(1_000)
	store.sweep.Status = postgres.TokenSweepSigning
	store.sweep.Nonce = new(big.Int).SetUint64(7)
	store.sweep.SweepAmountUnits = big.NewInt(1_000)
	store.sweep.GasLimit = int64Pointer(50_000)
	store.sweep.MaxFeePerGasWei = big.NewInt(202)
	store.sweep.MaxPriorityFeePerGasWei = big.NewInt(2)
	chain.header = nil

	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if chain.headerCalls != 0 || chain.tipCalls != 0 {
		t.Fatalf("unexpected fee RPC calls during sign: header=%d tip=%d", chain.headerCalls, chain.tipCalls)
	}
	if store.sweep.Status != postgres.TokenSweepBroadcasted || len(chain.sentRaw) != 1 {
		t.Fatalf("status = %s, sends = %d", store.sweep.Status, len(chain.sentRaw))
	}
}

// TestWorkerCapsSweepEffectiveMaxBumpsByGasSafetyAndHandlesBlockedBump 验证归集按安全余量限制有效加价次数并在余额不足时去重告警与重播。
func TestWorkerCapsSweepEffectiveMaxBumpsByGasSafetyAndHandlesBlockedBump(t *testing.T) {
	if got := evm.MaxCoveredSweepBumps(2_000, 15); got != 1 {
		t.Fatalf("MaxCoveredSweepBumps(2000, 15) = %d, want 1", got)
	}
	chain, contractChain, store, worker := newTestWorker(t)
	worker.config.RebroadcastAfter = 10 * time.Second
	worker.config.BumpAfter = 30 * time.Second
	worker.config.BumpPercent = 15
	worker.config.MaxBumps = 5
	worker.config.GasSafetyBPS = 2_000

	from := common.HexToAddress(store.address.Address)
	contractChain.balances[from] = big.NewInt(1_000)
	chain.balances[from] = big.NewInt(12_120_000)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}

	stale := time.Now().Add(-time.Minute)
	store.sweep.BroadcastedAt = &stale
	store.sweep.LastBroadcastAt = &stale
	// 余额仅够覆盖 202->233 第一次 15% 加价，且受 GasSafetyBPS=2000 限制最多只允许加价 1 次。
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("first bump Process() error = %v", err)
	}
	if store.sweep.BumpCount != 1 {
		t.Fatalf("bump_count = %d, want 1", store.sweep.BumpCount)
	}

	// 即使 MaxBumps=5，后续超阈值也只能重播而不再发起第二次加价。
	store.sweep.LastBroadcastAt = &stale
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("capped rebroadcast Process() error = %v", err)
	}
	if store.sweep.BumpCount != 1 || len(chain.sentRaw) != 3 {
		t.Fatalf("bump_count = %d, sends = %d", store.sweep.BumpCount, len(chain.sentRaw))
	}

	// 验证来源地址 ETH 不足触发 H1 阻塞时仅告警一次并重播。
	chain2, contractChain2, store2, worker2 := newTestWorker(t)
	worker2.config.RebroadcastAfter = 10 * time.Second
	worker2.config.BumpAfter = 30 * time.Second
	worker2.config.BumpPercent = 15
	worker2.config.MaxBumps = 3
	worker2.config.GasSafetyBPS = 2_000
	from2 := common.HexToAddress(store2.address.Address)
	contractChain2.balances[from2] = big.NewInt(1_000)
	chain2.balances[from2] = big.NewInt(10_500_000)
	if err := worker2.Process(context.Background(), store2.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	store2.sweep.BroadcastedAt = &stale
	store2.sweep.LastBroadcastAt = &stale
	if err := worker2.Process(context.Background(), store2.sweep.ID); err != nil {
		t.Fatalf("first blocked bump Process() error = %v", err)
	}
	store2.sweep.LastBroadcastAt = &stale
	if err := worker2.Process(context.Background(), store2.sweep.ID); err != nil {
		t.Fatalf("second blocked bump Process() error = %v", err)
	}
	if len(store2.errors) != 1 || store2.errors[0].ErrorCode != "CHAIN_BALANCE_INSUFFICIENT" || len(chain2.sentRaw) != 3 {
		t.Fatalf("errors = %+v, sends = %d", store2.errors, len(chain2.sentRaw))
	}
}
