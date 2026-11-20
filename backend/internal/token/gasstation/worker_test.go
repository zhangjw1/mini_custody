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
	header         *types.Header
	tip            *big.Int
	transferGas    uint64
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
	estimateCalls  int
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// HeaderByNumber 返回测试 EIP-1559 区块头并累计调用次数。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	f.headerCalls++
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

// NonceAt 返回平台热钱包测试最新已确认 Nonce。
func (f *fakeChain) NonceAt(context.Context, common.Address, *big.Int) (uint64, error) {
	return f.confirmedNonce, nil
}

// SuggestGasTipCap 返回测试建议优先费并累计调用次数。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	f.tipCalls++
	return new(big.Int).Set(f.tip), nil
}

// EstimateGas 返回普通 ETH 补气转账的测试 Gas Limit 并累计调用次数。
func (f *fakeChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	f.estimateCalls++
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
func (f *fakeChain) TransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	if f.receiptsByHash != nil {
		if receipt, ok := f.receiptsByHash[hash]; ok && receipt != nil {
			return receipt, nil
		}
		return nil, ethereum.NotFound
	}
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
	attempts    []postgres.EVMTxAttempt
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

// CreateOrGetGasTopup 幂等创建测试内部转账并保存 Nonce 与 Gas 参数。
func (f *fakeStore) CreateOrGetGasTopup(_ context.Context, request postgres.GasTopupRequest) (postgres.InternalTransfer, bool, error) {
	if f.transfer != nil {
		return *f.transfer, false, nil
	}
	gasLimit := int64(request.GasLimit)
	f.transfer = &postgres.InternalTransfer{
		ID: 9, PlatformWalletID: request.PlatformWalletID, SweepID: request.SweepID,
		TransferType: postgres.InternalTransferGasTopup, FromAddress: request.FromAddress,
		ToAddress: request.ToAddress, AmountWei: new(big.Int).Set(request.AmountWei),
		Nonce: new(big.Int).SetUint64(request.ChainPendingNonce), GasLimit: &gasLimit,
		MaxFeePerGasWei:         new(big.Int).Set(request.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(request.MaxPriorityFeePerGasWei),
		Status:                  postgres.InternalTransferSigning,
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
	f.transfer.SigningRetryCount = 0
	f.transfer.Status = postgres.InternalTransferSigned
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessInternalTransfer,
		BusinessID:              f.transfer.ID,
		Nonce:                   f.transfer.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  signed.TxHash,
		RawTx:                   append([]byte(nil), signed.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(signed.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(signed.MaxPriorityFeePerGasWei),
		GasLimit:                int64(signed.GasLimit),
		CreatedAt:               time.Now(),
	}}, f.attempts...)
	return *f.transfer, true, nil
}

// ListEVMTxAttempts 返回按尝试序号倒序排列的测试候选交易记录。
func (f *fakeStore) ListEVMTxAttempts(context.Context, string, int64) ([]postgres.EVMTxAttempt, error) {
	return append([]postgres.EVMTxAttempt(nil), f.attempts...), nil
}

// ReplaceInternalTransferTx 保存测试内部转账加价替换交易并推进替换次数。
func (f *fakeStore) ReplaceInternalTransferTx(_ context.Context, replacement postgres.SignedInternalTransfer) (postgres.InternalTransfer, bool, error) {
	f.transfer.GasLimit = int64Pointer(int64(replacement.GasLimit))
	f.transfer.MaxFeePerGasWei = new(big.Int).Set(replacement.MaxFeePerGasWei)
	f.transfer.MaxPriorityFeePerGasWei = new(big.Int).Set(replacement.MaxPriorityFeePerGasWei)
	f.transfer.RawTx = append([]byte(nil), replacement.RawTx...)
	f.transfer.TxHash = replacement.TxHash
	f.transfer.BumpCount++
	f.transfer.BumpBlockedAt = nil
	f.transfer.BumpBlockedReason = ""
	now := time.Now()
	f.transfer.LastBroadcastAt = &now
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessInternalTransfer,
		BusinessID:              f.transfer.ID,
		Nonce:                   f.transfer.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  replacement.TxHash,
		RawTx:                   append([]byte(nil), replacement.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(replacement.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(replacement.MaxPriorityFeePerGasWei),
		GasLimit:                int64(replacement.GasLimit),
		CreatedAt:               now,
	}}, f.attempts...)
	return *f.transfer, true, nil
}

// TouchInternalTransferBroadcast 更新测试内部转账最近广播时间戳。
func (f *fakeStore) TouchInternalTransferBroadcast(context.Context, int64) (postgres.InternalTransfer, error) {
	now := time.Now()
	if f.transfer.BroadcastedAt == nil {
		f.transfer.BroadcastedAt = &now
	}
	f.transfer.LastBroadcastAt = &now
	return *f.transfer, nil
}

// MarkInternalTransferManualReview 将测试内部转账转入人工核查状态。
func (f *fakeStore) MarkInternalTransferManualReview(_ context.Context, _ int64, errorCode, errorMessage string) (postgres.InternalTransfer, bool, error) {
	f.transfer.Status = postgres.InternalTransferManualReview
	f.transfer.ErrorCode = errorCode
	f.transfer.ErrorMessage = errorMessage
	return *f.transfer, true, nil
}

// TransitionInternalTransfer 更新测试内部转账状态。
func (f *fakeStore) TransitionInternalTransfer(_ context.Context, _ int64, target string) (postgres.InternalTransfer, error) {
	f.transfer.Status = target
	if target == postgres.InternalTransferSent {
		now := time.Now()
		if f.transfer.BroadcastedAt == nil {
			f.transfer.BroadcastedAt = &now
		}
		f.transfer.LastBroadcastAt = &now
	}
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
	if settlement.TxHash != "" {
		f.transfer.TxHash = settlement.TxHash
	}
	return *f.transfer, true, nil
}

// RecordEVMSigningFailure 记录补气交易签名失败并在达到上限后转入人工核查且仅告警一次。
func (f *fakeStore) RecordEVMSigningFailure(_ context.Context, _ string, _ int64, maxRetries int, worker, refType string, refID int64, errorCode, errorMessage string) (bool, error) {
	f.transfer.SigningRetryCount++
	if int(f.transfer.SigningRetryCount) >= maxRetries && f.transfer.Status == postgres.InternalTransferSigning {
		f.transfer.Status = postgres.InternalTransferManualReview
		f.transfer.ErrorCode = errorCode
		f.transfer.ErrorMessage = errorMessage
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
	if f.transfer.BumpBlockedAt == nil {
		now := time.Now()
		f.transfer.BumpBlockedAt = &now
		f.transfer.BumpBlockedReason = reasonMessage
		f.errors = append(f.errors, postgres.WorkerError{
			Worker: worker, Stage: "bump", ReferenceType: refType, ReferenceID: &refID,
			ErrorCode: reasonCode, ErrorMessage: reasonMessage,
		})
		return now, true, nil
	}
	return *f.transfer.BumpBlockedAt, false, nil
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

// TestWorkerRebroadcastsBumpsAndSettlesMatchedTopupAttempt 验证补气交易重播、加价替换和早期尝试结算。
func TestWorkerRebroadcastsBumpsAndSettlesMatchedTopupAttempt(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	worker.config.RebroadcastAfter = 10 * time.Second
	worker.config.BumpAfter = 30 * time.Second
	worker.config.BumpPercent = 15
	worker.config.MaxBumps = 3

	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	firstHash := common.HexToHash(store.transfer.TxHash)
	stale := time.Now().Add(-15 * time.Second)
	store.transfer.BroadcastedAt = &stale
	store.transfer.LastBroadcastAt = &stale
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("rebroadcast Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || string(chain.sentRaw[0]) != string(chain.sentRaw[1]) || store.transfer.BumpCount != 0 {
		t.Fatalf("sends = %d, bump_count = %d", len(chain.sentRaw), store.transfer.BumpCount)
	}

	veryStale := time.Now().Add(-time.Minute)
	store.transfer.BroadcastedAt = &veryStale
	store.transfer.LastBroadcastAt = &veryStale
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("bump Process() error = %v", err)
	}
	if store.transfer.BumpCount != 1 || len(store.attempts) != 2 || len(chain.sentRaw) != 3 {
		t.Fatalf("bump_count = %d, attempts = %d, sends = %d", store.transfer.BumpCount, len(store.attempts), len(chain.sentRaw))
	}

	chain.latest = 12
	chain.receiptsByHash = map[common.Hash]*types.Receipt{
		firstHash: {Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10), GasUsed: 21_000, EffectiveGasPrice: big.NewInt(100)},
	}
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.TxHash != firstHash.Hex() || store.transfer.Status != postgres.InternalTransferDone {
		t.Fatalf("settlement = %+v, status = %s", store.settlement, store.transfer.Status)
	}
}

// TestWorkerMovesNonceGapTopupToManualReview 验证越过 Nonce 且无 Receipt 时补气交易转入人工核查。
func TestWorkerMovesNonceGapTopupToManualReview(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	chain.confirmedNonce = store.transfer.Nonce.Uint64() + 1
	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("manual review Process() error = %v", err)
	}
	if store.transfer.Status != postgres.InternalTransferManualReview || len(store.errors) == 0 || store.errors[0].ErrorCode != "EVM_TX_MANUAL_REVIEW" {
		t.Fatalf("status = %s, errors = %+v", store.transfer.Status, store.errors)
	}
}

// TestWorkerRejectsTopupWhenPlatformBalanceCannotCoverTopupAndFee 验证平台热钱包余额不足以覆盖补气、手续费与最低保留金时不分配 Nonce。
func TestWorkerRejectsTopupWhenPlatformBalanceCannotCoverTopupAndFee(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	chain.balances[common.HexToAddress(store.address.Address)] = new(big.Int)
	// 需补气 12,120,000 + 手续费 4,242,000 + 最低保留 1 = 16,362,001；设置 16,000,000 应触发 H6 保护。
	chain.balances[common.HexToAddress(store.platform.Address)] = big.NewInt(16_000_000)

	err := worker.Process(context.Background(), store.sweep.ID)
	if !errors.Is(err, ErrPlatformBalanceLow) || store.transfer != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("error = %v, transfer = %+v, sends = %d", err, store.transfer, len(chain.sentRaw))
	}
}

// TestWorkerSignsTopupWithoutExtraFeeRPC 验证补气 SIGNING 阶段只读取持久化 Gas 参数而不调用费率 RPC。
func TestWorkerSignsTopupWithoutExtraFeeRPC(t *testing.T) {
	chain, store, worker := newTestWorker(t)
	gasLimit := int64(21_000)
	transferID := int64(9)
	store.sweep.Status = postgres.TokenSweepWaitingGas
	store.sweep.GasTopupTransferID = &transferID
	store.transfer = &postgres.InternalTransfer{
		ID: transferID, PlatformWalletID: store.platform.ID, SweepID: store.sweep.ID,
		TransferType: postgres.InternalTransferGasTopup, FromAddress: store.platform.Address,
		ToAddress: store.address.Address, AmountWei: big.NewInt(10_000_000),
		Nonce: new(big.Int).SetUint64(7), GasLimit: &gasLimit,
		MaxFeePerGasWei: big.NewInt(202), MaxPriorityFeePerGasWei: big.NewInt(2),
		Status: postgres.InternalTransferSigning,
	}
	chain.header = nil

	if err := worker.Process(context.Background(), store.sweep.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if chain.headerCalls != 0 || chain.tipCalls != 0 || chain.estimateCalls != 0 {
		t.Fatalf("unexpected fee RPC calls during sign: header=%d tip=%d estimate=%d", chain.headerCalls, chain.tipCalls, chain.estimateCalls)
	}
	if store.transfer.Status != postgres.InternalTransferSent || len(chain.sentRaw) != 1 {
		t.Fatalf("status = %s, sends = %d", store.transfer.Status, len(chain.sentRaw))
	}
}
