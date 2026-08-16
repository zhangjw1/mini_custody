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
	header     *types.Header
	tip        *big.Int
	ethBalance *big.Int
	nonce      uint64
	latest     uint64
	receipt    *types.Receipt
	receiptErr error
	sendErrors []error
	sentRaw    [][]byte
}

// HeaderByNumber 返回固定 EIP-1559 区块头。
func (f *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return f.header, nil
}

// SuggestGasTipCap 返回固定优先费。
func (f *fakeChain) SuggestGasTipCap(context.Context) (*big.Int, error) {
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

// AllocateTokenWithdrawalNonce 保存测试 Nonce 并进入签名状态。
func (f *fakeStore) AllocateTokenWithdrawalNonce(_ context.Context, _ int64, nonce uint64) (postgres.TokenWithdrawal, bool, error) {
	if f.item.Nonce != nil {
		return f.item, false, nil
	}
	f.item.Nonce = new(big.Int).SetUint64(nonce)
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
	f.item.Status = postgres.WithdrawalSigned
	return f.item, true, nil
}

// TransitionTokenWithdrawal 更新测试 Token 提币状态。
func (f *fakeStore) TransitionTokenWithdrawal(_ context.Context, _ int64, target string) (postgres.TokenWithdrawal, error) {
	f.item.Status = target
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
	return f.item, true, nil
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
