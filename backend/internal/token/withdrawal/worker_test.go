package withdrawal

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

const tokenWithdrawalTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// fakeChain 提供 Token 提币测试所需的费用、ETH、Nonce、广播和 Receipt。
type fakeChain struct {
	header         *types.Header
	tip            *big.Int
	ethBalance     *big.Int
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

// HeaderByNumber 返回固定 EIP-1559 区块头并累计调用次数。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	f.headerCalls++
	return f.header, nil
}

// SuggestGasTipCap 返回固定优先费并累计调用次数。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
	f.tipCalls++
	return new(big.Int).Set(f.tip), nil
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) { return f.latest, nil }

// BalanceAt 返回平台热钱包测试 ETH 余额。
func (f *fakeChain) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return new(big.Int).Set(f.ethBalance), nil
}

// PendingNonceAt 返回平台钱包测试 Pending Nonce。
func (f *fakeChain) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return f.nonce, nil
}

// NonceAt 返回平台钱包测试最新已确认 Nonce。
func (f *fakeChain) NonceAt(context.Context, common.Address, *big.Int) (uint64, error) {
	return f.confirmedNonce, nil
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

// fakeContract 使用标准 ABI 模拟 Token 库存、calldata 和 Transfer Event。
type fakeContract struct {
	address  common.Address
	tokenABI abi.ABI
	balance  *big.Int
	gas      uint64
}

// Address 返回测试 Token 合约地址。
func (f *fakeContract) Address() common.Address { return f.address }

// BalanceOf 返回平台测试 Token 库存。
func (f *fakeContract) BalanceOf(context.Context, common.Address) (*big.Int, error) {
	return new(big.Int).Set(f.balance), nil
}

// EncodeTransfer 使用标准 ABI 编码测试 transfer。
func (f *fakeContract) EncodeTransfer(to common.Address, value *big.Int) ([]byte, error) {
	return f.tokenABI.Pack("transfer", to, value)
}

// DecodeTransferCalldata 使用标准 ABI 解码测试 transfer。
func (f *fakeContract) DecodeTransferCalldata(data []byte) (common.Address, *big.Int, error) {
	method := f.tokenABI.Methods["transfer"]
	if len(data) < 4 || !bytes.Equal(data[:4], method.ID) {
		return common.Address{}, nil, errors.New("测试 transfer calldata 无效")
	}
	values, err := method.Inputs.Unpack(data[4:])
	if err != nil || len(values) != 2 {
		return common.Address{}, nil, errors.New("测试 transfer calldata 解析失败")
	}
	return values[0].(common.Address), new(big.Int).Set(values[1].(*big.Int)), nil
}

// EstimateTransferGas 返回固定 Token transfer Gas Limit。
func (f *fakeContract) EstimateTransferGas(context.Context, common.Address, common.Address, *big.Int) (uint64, error) {
	return f.gas, nil
}

// DecodeTransferLog 使用标准 ABI 严格解码测试 Transfer Event。
func (f *fakeContract) DecodeTransferLog(log types.Log) (erc20.TransferEvent, error) {
	event := f.tokenABI.Events["Transfer"]
	if log.Address != f.address || len(log.Topics) != 3 || log.Topics[0] != event.ID {
		return erc20.TransferEvent{}, erc20.ErrInvalidTransferLog
	}
	values, err := event.Inputs.NonIndexed().Unpack(log.Data)
	if err != nil || len(values) != 1 {
		return erc20.TransferEvent{}, erc20.ErrInvalidTransferLog
	}
	return erc20.TransferEvent{
		From: common.BytesToAddress(log.Topics[1].Bytes()[12:]), To: common.BytesToAddress(log.Topics[2].Bytes()[12:]),
		AmountUnits: new(big.Int).Set(values[0].(*big.Int)),
	}, nil
}

// fakeStore 在内存中模拟 Token 提币持久化状态机。
type fakeStore struct {
	item       postgres.TokenWithdrawal
	platform   postgres.PlatformWallet
	attempts   []postgres.EVMTxAttempt
	settlement *postgres.TokenWithdrawalSettlement
	errors     []postgres.WorkerError
}

// TokenWithdrawalByID 返回内存 Token 提币。
func (f *fakeStore) TokenWithdrawalByID(context.Context, int64) (postgres.TokenWithdrawal, error) {
	return f.item, nil
}

// ListProcessableTokenWithdrawals 返回当前内存 Token 提币。
func (f *fakeStore) ListProcessableTokenWithdrawals(context.Context, int) ([]postgres.TokenWithdrawal, error) {
	return []postgres.TokenWithdrawal{f.item}, nil
}

// PlatformWalletByRole 返回测试平台热钱包。
func (f *fakeStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	return f.platform, nil
}

// AllocateTokenWithdrawalNonce 保存测试 Nonce 与 Gas 参数并进入签名状态。
func (f *fakeStore) AllocateTokenWithdrawalNonce(_ context.Context, _ int64, nonce uint64, gas postgres.PreparedGasParams) (postgres.TokenWithdrawal, bool, error) {
	if f.item.Nonce != nil {
		return f.item, false, nil
	}
	gasLimit := int64(gas.GasLimit)
	f.item.Nonce = new(big.Int).SetUint64(nonce)
	f.item.GasLimit = &gasLimit
	f.item.MaxFeePerGasWei = new(big.Int).Set(gas.MaxFeePerGasWei)
	f.item.MaxPriorityFeePerGasWei = new(big.Int).Set(gas.MaxPriorityFeePerGasWei)
	f.item.Status = postgres.WithdrawalSigning
	return f.item, true, nil
}

// SaveSignedTokenWithdrawal 保存测试签名交易并进入已签名状态。
func (f *fakeStore) SaveSignedTokenWithdrawal(_ context.Context, signed postgres.SignedTokenWithdrawal) (postgres.TokenWithdrawal, bool, error) {
	gasLimit := int64(signed.GasLimit)
	f.item.GasLimit = &gasLimit
	f.item.MaxFeePerGasWei = new(big.Int).Set(signed.MaxFeePerGasWei)
	f.item.MaxPriorityFeePerGasWei = new(big.Int).Set(signed.MaxPriorityFeePerGasWei)
	f.item.RawTx = append([]byte(nil), signed.RawTx...)
	f.item.TxHash = signed.TxHash
	f.item.SigningRetryCount = 0
	f.item.Status = postgres.WithdrawalSigned
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessTokenWithdrawal,
		BusinessID:              f.item.ID,
		Nonce:                   f.item.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  signed.TxHash,
		RawTx:                   append([]byte(nil), signed.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(signed.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(signed.MaxPriorityFeePerGasWei),
		GasLimit:                gasLimit,
		CreatedAt:               time.Now(),
	}}, f.attempts...)
	return f.item, true, nil
}

// ListEVMTxAttempts 返回按尝试序号倒序排列的测试候选交易记录。
func (f *fakeStore) ListEVMTxAttempts(context.Context, string, int64) ([]postgres.EVMTxAttempt, error) {
	return append([]postgres.EVMTxAttempt(nil), f.attempts...), nil
}

// ReplaceTokenWithdrawalTx 保存测试 Token 提币加价替换交易并推进替换次数。
func (f *fakeStore) ReplaceTokenWithdrawalTx(_ context.Context, replacement postgres.SignedTokenWithdrawal) (postgres.TokenWithdrawal, bool, error) {
	gasLimit := int64(replacement.GasLimit)
	f.item.GasLimit = &gasLimit
	f.item.MaxFeePerGasWei = new(big.Int).Set(replacement.MaxFeePerGasWei)
	f.item.MaxPriorityFeePerGasWei = new(big.Int).Set(replacement.MaxPriorityFeePerGasWei)
	f.item.RawTx = append([]byte(nil), replacement.RawTx...)
	f.item.TxHash = replacement.TxHash
	f.item.BumpCount++
	f.item.BumpBlockedAt = nil
	f.item.BumpBlockedReason = ""
	now := time.Now()
	f.item.LastBroadcastAt = &now
	f.attempts = append([]postgres.EVMTxAttempt{{
		BusinessType:            postgres.EVMBusinessTokenWithdrawal,
		BusinessID:              f.item.ID,
		Nonce:                   f.item.Nonce,
		AttemptNo:               int32(len(f.attempts) + 1),
		TxHash:                  replacement.TxHash,
		RawTx:                   append([]byte(nil), replacement.RawTx...),
		MaxFeePerGasWei:         new(big.Int).Set(replacement.MaxFeePerGasWei),
		MaxPriorityFeePerGasWei: new(big.Int).Set(replacement.MaxPriorityFeePerGasWei),
		GasLimit:                gasLimit,
		CreatedAt:               now,
	}}, f.attempts...)
	return f.item, true, nil
}

// TouchTokenWithdrawalBroadcast 更新测试 Token 提币最近广播时间。
func (f *fakeStore) TouchTokenWithdrawalBroadcast(context.Context, int64) (postgres.TokenWithdrawal, error) {
	now := time.Now()
	if f.item.BroadcastedAt == nil {
		f.item.BroadcastedAt = &now
	}
	f.item.LastBroadcastAt = &now
	return f.item, nil
}

// MarkTokenWithdrawalManualReview 将测试 Token 提币转入人工核查状态。
func (f *fakeStore) MarkTokenWithdrawalManualReview(_ context.Context, _ int64, errorCode, errorMessage string) (postgres.TokenWithdrawal, bool, error) {
	f.item.Status = postgres.WithdrawalManualReview
	f.item.ErrorCode = errorCode
	f.item.ErrorMessage = errorMessage
	return f.item, true, nil
}

// TransitionTokenWithdrawal 更新测试 Token 提币状态。
func (f *fakeStore) TransitionTokenWithdrawal(_ context.Context, _ int64, target string) (postgres.TokenWithdrawal, error) {
	f.item.Status = target
	if target == postgres.WithdrawalBroadcasted {
		now := time.Now()
		if f.item.BroadcastedAt == nil {
			f.item.BroadcastedAt = &now
		}
		f.item.LastBroadcastAt = &now
	}
	return f.item, nil
}

// UpdateTokenWithdrawalConfirmations 保存测试确认数。
func (f *fakeStore) UpdateTokenWithdrawalConfirmations(_ context.Context, _ int64, confirmations int64) (postgres.TokenWithdrawal, error) {
	f.item.Confirmations = confirmations
	f.item.Status = postgres.WithdrawalConfirming
	return f.item, nil
}

// FinalizeTokenWithdrawal 保存测试结算并更新最终状态。
func (f *fakeStore) FinalizeTokenWithdrawal(_ context.Context, settlement postgres.TokenWithdrawalSettlement) (postgres.TokenWithdrawal, bool, error) {
	f.settlement = &settlement
	if settlement.Success {
		f.item.Status = postgres.WithdrawalCompleted
	} else {
		f.item.Status = postgres.WithdrawalFailed
	}
	if settlement.TxHash != "" {
		f.item.TxHash = settlement.TxHash
	}
	return f.item, true, nil
}

// RecordEVMSigningFailure 记录 Token 提币签名失败并在达到上限后转入人工核查且仅告警一次。
func (f *fakeStore) RecordEVMSigningFailure(_ context.Context, _ string, _ int64, maxRetries int, worker, refType string, refID int64, errorCode, errorMessage string) (bool, error) {
	f.item.SigningRetryCount++
	if int(f.item.SigningRetryCount) >= maxRetries && f.item.Status == postgres.WithdrawalSigning {
		f.item.Status = postgres.WithdrawalManualReview
		f.item.ErrorCode = errorCode
		f.item.ErrorMessage = errorMessage
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
	if f.item.BumpBlockedAt == nil {
		now := time.Now()
		f.item.BumpBlockedAt = &now
		f.item.BumpBlockedReason = reasonMessage
		f.errors = append(f.errors, postgres.WorkerError{
			Worker: worker, Stage: "bump", ReferenceType: refType, ReferenceID: &refID,
			ErrorCode: reasonCode, ErrorMessage: reasonMessage,
		})
		return now, true, nil
	}
	return *f.item.BumpBlockedAt, false, nil
}

// RecordWorkerError 保存测试 Worker 错误。
func (f *fakeStore) RecordWorkerError(_ context.Context, item postgres.WorkerError) (int64, error) {
	f.errors = append(f.errors, item)
	return int64(len(f.errors)), nil
}

// TestWorkerKeepsCreatedWhenHotWalletTokenIsInsufficient 验证库存不足时不会分配 Nonce 或签名。
func TestWorkerKeepsCreatedWhenHotWalletTokenIsInsufficient(t *testing.T) {
	chain, contract, store, worker := newTestWorker(t)
	contract.balance.SetInt64(999)
	err := worker.Process(context.Background(), store.item.ID)
	if !errors.Is(err, ErrHotWalletTokenInsufficient) || store.item.Status != postgres.WithdrawalCreated || store.item.Nonce != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("error = %v, status = %s, nonce = %v, sends = %d", err, store.item.Status, store.item.Nonce, len(chain.sentRaw))
	}
}

// TestWorkerKeepsCreatedWhenHotWalletGasIsInsufficient 验证 Gas 不足时不会分配 Nonce 或签名。
func TestWorkerKeepsCreatedWhenHotWalletGasIsInsufficient(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	chain.ethBalance.SetInt64(1)
	err := worker.Process(context.Background(), store.item.ID)
	if !errors.Is(err, ErrHotWalletGasInsufficient) || store.item.Status != postgres.WithdrawalCreated || store.item.Nonce != nil || len(chain.sentRaw) != 0 {
		t.Fatalf("error = %v, status = %s, nonce = %v, sends = %d", err, store.item.Status, store.item.Nonce, len(chain.sentRaw))
	}
}

// TestWorkerReplaysSameRawTransactionAfterTimeout 验证广播超时后只重播同一份 Token 提币原始交易。
func TestWorkerReplaysSameRawTransactionAfterTimeout(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	chain.sendErrors = []error{context.DeadlineExceeded, nil}
	if err := worker.Process(context.Background(), store.item.ID); err == nil {
		t.Fatal("first Process() error = nil")
	}
	if store.item.Status != postgres.WithdrawalSigned {
		t.Fatalf("status = %s, want SIGNED", store.item.Status)
	}
	originalRaw := append([]byte(nil), store.item.RawTx...)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("second Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || !bytes.Equal(chain.sentRaw[0], chain.sentRaw[1]) || !bytes.Equal(originalRaw, store.item.RawTx) {
		t.Fatal("Token 提币恢复时改变了原始交易")
	}
}

// TestWorkerSettlesSuccessfulTransferAndActualPlatformGas 验证成功 Receipt 结算 Token 并记录平台实际 Gas。
func TestWorkerSettlesSuccessfulTransferAndActualPlatformGas(t *testing.T) {
	chain, contract, store, worker := newTestWorker(t)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10), GasUsed: 50_000,
		EffectiveGasPrice: big.NewInt(100), Logs: []*types.Log{tokenWithdrawalTransferLog(t, contract, store)},
	}
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || !store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(5_000_000)) != 0 {
		t.Fatalf("settlement = %+v", store.settlement)
	}
}

// TestWorkerSettlesFailedReceiptAndRetainsPlatformGas 验证执行失败时释放用户 Token 且保留平台 Gas 记录。
func TestWorkerSettlesFailedReceiptAndRetainsPlatformGas(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("broadcast Process() error = %v", err)
	}
	chain.receiptErr = nil
	chain.latest = 12
	chain.receipt = &types.Receipt{Status: types.ReceiptStatusFailed, BlockNumber: big.NewInt(10), GasUsed: 40_000, EffectiveGasPrice: big.NewInt(100)}
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.Success || store.settlement.ActualFeeWei.Cmp(big.NewInt(4_000_000)) != 0 || store.settlement.ErrorCode == "" {
		t.Fatalf("settlement = %+v", store.settlement)
	}
}

// tokenWithdrawalTransferLog 构造平台热钱包到目标地址的标准 Transfer Event。
func tokenWithdrawalTransferLog(t *testing.T, contract *fakeContract, store *fakeStore) *types.Log {
	t.Helper()
	event := contract.tokenABI.Events["Transfer"]
	data, err := event.Inputs.NonIndexed().Pack(store.item.AmountUnits)
	if err != nil {
		t.Fatalf("Pack() error = %v", err)
	}
	from := common.HexToAddress(store.platform.Address)
	to := common.HexToAddress(store.item.ToAddress)
	return &types.Log{Address: contract.address, Topics: []common.Hash{event.ID, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())}, Data: data}
}

// newTestWorker 创建使用标准 ABI 和确定性平台密钥的 Token 提币 Worker。
func newTestWorker(t *testing.T) (*fakeChain, *fakeContract, *fakeStore, *Worker) {
	t.Helper()
	provider, err := wallet.NewMnemonicKeyProvider(tokenWithdrawalTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	hotAddress, err := provider.Address(context.Background(), wallet.TreasuryPath)
	if err != nil {
		t.Fatalf("Address() error = %v", err)
	}
	tokenABI, err := erc20.StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	contract := &fakeContract{address: common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"), tokenABI: tokenABI, balance: big.NewInt(10_000), gas: 50_000}
	store := &fakeStore{
		platform: postgres.PlatformWallet{ID: 7, Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot, Address: hotAddress.Hex(), DerivationPath: wallet.TreasuryPath, NextNonce: new(big.Int)},
		item:     postgres.TokenWithdrawal{ID: 8, UserID: 2, AssetID: 3, PlatformWalletID: 7, ToAddress: "0x2222222222222222222222222222222222222222", AmountUnits: big.NewInt(1_000), Status: postgres.WithdrawalCreated},
	}
	chain := &fakeChain{header: &types.Header{BaseFee: big.NewInt(100)}, tip: big.NewInt(2), ethBalance: big.NewInt(20_000_000), nonce: 9, latest: 9, receiptErr: ethereum.NotFound}
	worker, err := NewWorker(chain, contract, store, provider, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{Interval: time.Second, Confirmations: 3, ChainID: big.NewInt(evm.SepoliaChainID)})
	if err != nil {
		t.Fatalf("NewWorker() error = %v", err)
	}
	return chain, contract, store, worker
}

// TestWorkerRebroadcastsBumpsAndSettlesMatchedTokenAttempt 验证 Token 提币重播、加价替换、多候选结算和人工核查。
func TestWorkerRebroadcastsBumpsAndSettlesMatchedTokenAttempt(t *testing.T) {
	chain, contract, store, worker := newTestWorker(t)
	worker.config.RebroadcastAfter = 10 * time.Second
	worker.config.BumpAfter = 30 * time.Second
	worker.config.BumpPercent = 15
	worker.config.MaxBumps = 3

	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	firstHash := common.HexToHash(store.item.TxHash)
	stale := time.Now().Add(-15 * time.Second)
	store.item.BroadcastedAt = &stale
	store.item.LastBroadcastAt = &stale
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("rebroadcast Process() error = %v", err)
	}
	if len(chain.sentRaw) != 2 || !bytes.Equal(chain.sentRaw[0], chain.sentRaw[1]) || store.item.BumpCount != 0 {
		t.Fatalf("sends = %d, bump_count = %d", len(chain.sentRaw), store.item.BumpCount)
	}

	veryStale := time.Now().Add(-time.Minute)
	store.item.BroadcastedAt = &veryStale
	store.item.LastBroadcastAt = &veryStale
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("bump Process() error = %v", err)
	}
	if store.item.BumpCount != 1 || len(store.attempts) != 2 || len(chain.sentRaw) != 3 {
		t.Fatalf("bump_count = %d, attempts = %d, sends = %d", store.item.BumpCount, len(store.attempts), len(chain.sentRaw))
	}

	chain.latest = 12
	chain.receiptsByHash = map[common.Hash]*types.Receipt{
		firstHash: {
			Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(10), GasUsed: 50_000,
			EffectiveGasPrice: big.NewInt(100), Logs: []*types.Log{tokenWithdrawalTransferLog(t, contract, store)},
		},
	}
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("confirm Process() error = %v", err)
	}
	if store.settlement == nil || store.settlement.TxHash != firstHash.Hex() || store.item.Status != postgres.WithdrawalCompleted {
		t.Fatalf("settlement = %+v, status = %s", store.settlement, store.item.Status)
	}
}

// TestWorkerMovesNonceGapTokenWithdrawalToManualReview 验证越过 Nonce 且无 Receipt 时 Token 提币转入人工核查。
func TestWorkerMovesNonceGapTokenWithdrawalToManualReview(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	chain.confirmedNonce = store.item.Nonce.Uint64() + 1
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("manual review Process() error = %v", err)
	}
	if store.item.Status != postgres.WithdrawalManualReview || len(store.errors) == 0 || store.errors[0].ErrorCode != "EVM_TX_MANUAL_REVIEW" {
		t.Fatalf("status = %s, errors = %+v", store.item.Status, store.errors)
	}
}

// TestWorkerSignsTokenWithdrawalWithoutExtraFeeRPC 验证 SIGNING 阶段只读取持久化 Gas 参数而不调用费率 RPC。
func TestWorkerSignsTokenWithdrawalWithoutExtraFeeRPC(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	gasLimit := int64(50_000)
	store.item.Status = postgres.WithdrawalSigning
	store.item.Nonce = new(big.Int).SetUint64(9)
	store.item.GasLimit = &gasLimit
	store.item.MaxFeePerGasWei = big.NewInt(202)
	store.item.MaxPriorityFeePerGasWei = big.NewInt(2)
	chain.header = nil

	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if chain.headerCalls != 0 || chain.tipCalls != 0 {
		t.Fatalf("unexpected fee RPC calls during sign: header=%d tip=%d", chain.headerCalls, chain.tipCalls)
	}
	if store.item.Status != postgres.WithdrawalBroadcasted || len(chain.sentRaw) != 1 {
		t.Fatalf("status = %s, sends = %d", store.item.Status, len(chain.sentRaw))
	}
}

// TestWorkerHandlesTokenWithdrawalBumpBlockedOnceAndRebroadcasts 验证热钱包 Gas 不足阻塞加价时仅告警一次并按重播间隔继续重播。
func TestWorkerHandlesTokenWithdrawalBumpBlockedAndRebroadcasts(t *testing.T) {
	chain, _, store, worker := newTestWorker(t)
	worker.config.RebroadcastAfter = 10 * time.Second
	worker.config.BumpAfter = 30 * time.Second
	worker.config.BumpPercent = 15
	worker.config.MaxBumps = 3

	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("initial Process() error = %v", err)
	}
	chain.ethBalance = big.NewInt(1)
	stale := time.Now().Add(-time.Minute)
	store.item.BroadcastedAt = &stale
	store.item.LastBroadcastAt = &stale

	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("first blocked bump Process() error = %v", err)
	}
	if store.item.BumpCount != 0 || len(store.errors) != 1 || store.errors[0].ErrorCode != "HOT_WALLET_GAS_INSUFFICIENT" || len(chain.sentRaw) != 2 {
		t.Fatalf("bump_count = %d, errors = %+v, sends = %d", store.item.BumpCount, store.errors, len(chain.sentRaw))
	}

	store.item.LastBroadcastAt = &stale
	if err := worker.Process(context.Background(), store.item.ID); err != nil {
		t.Fatalf("second blocked bump Process() error = %v", err)
	}
	if len(store.errors) != 1 || len(chain.sentRaw) != 3 {
		t.Fatalf("errors should stay 1 (%d) and sends should advance to 3 (%d)", len(store.errors), len(chain.sentRaw))
	}
}
