package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = 100

var (
	ErrHotWalletTokenInsufficient = errors.New("平台热钱包 Token 库存不足，任务保持等待并将在下轮重试")
	ErrHotWalletGasInsufficient   = errors.New("平台热钱包 ETH 不足以支付最大网络费，任务保持等待并将在下轮重试")
)

// Chain 定义 Token 提币库存、费用、Nonce、广播和确认所需的 EVM 能力。
type Chain interface {
	FeeChain
	BlockNumber(ctx context.Context) (uint64, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SendTransaction(ctx context.Context, transaction *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// TokenContract 定义 Token 提币所需的合约余额、transfer 编解码和 Event 能力。
type TokenContract interface {
	QuoteContract
	BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error)
	EncodeTransfer(to common.Address, amountUnits *big.Int) ([]byte, error)
	DecodeTransferCalldata(calldata []byte) (common.Address, *big.Int, error)
	DecodeTransferLog(log types.Log) (erc20.TransferEvent, error)
}

// WorkerStore 定义 Token 提币持久化状态机和恢复查询。
type WorkerStore interface {
	TokenWithdrawalByID(ctx context.Context, id int64) (postgres.TokenWithdrawal, error)
	ListProcessableTokenWithdrawals(ctx context.Context, limit int) ([]postgres.TokenWithdrawal, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	AllocateTokenWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64) (postgres.TokenWithdrawal, bool, error)
	SaveSignedTokenWithdrawal(ctx context.Context, signed postgres.SignedTokenWithdrawal) (postgres.TokenWithdrawal, bool, error)
	TransitionTokenWithdrawal(ctx context.Context, withdrawalID int64, target string) (postgres.TokenWithdrawal, error)
	UpdateTokenWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (postgres.TokenWithdrawal, error)
	FinalizeTokenWithdrawal(ctx context.Context, settlement postgres.TokenWithdrawalSettlement) (postgres.TokenWithdrawal, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// WorkerConfig 定义 Token 提币轮询间隔、确认数和 Sepolia Chain ID。
type WorkerConfig struct {
	Interval      time.Duration
	Confirmations uint64
	ChainID       *big.Int
}

// Worker 负责平台热钱包 Token 提币签名、幂等广播、恢复和账本结算。
type Worker struct {
	chain    Chain
	contract TokenContract
	store    WorkerStore
	keys     wallet.KeyProvider
	logger   *slog.Logger
	config   WorkerConfig
}

// NewWorker 创建并校验 Token 提币 Worker。
func NewWorker(chain Chain, contract TokenContract, store WorkerStore, keys wallet.KeyProvider, logger *slog.Logger, cfg WorkerConfig) (*Worker, error) {
	if chain == nil || contract == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("Token 提币 Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil || cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 {
		return nil, errors.New("Token 提币 Worker 配置无效")
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{chain: chain, contract: contract, store: store, keys: keys, logger: logger, config: cfg}, nil
}

// Run 周期推进全部可恢复 Token 提币任务。
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.config.Interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("Token 提币 Worker 本轮存在失败，将在下轮重试", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce 查询全部可恢复 Token 提币并逐条推进，单条失败不阻塞其他任务。
func (w *Worker) RunOnce(ctx context.Context) error {
	items, err := w.store.ListProcessableTokenWithdrawals(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询待处理 Token 提币失败：%w", err)
	}
	var firstErr error
	for _, item := range items {
		if err := w.Process(ctx, item.ID); err != nil {
			w.recordError(ctx, item.ID, "process", "TOKEN_WITHDRAWAL_PROCESS_FAILED", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据持久化状态恢复并推进一笔 Token 提币。
func (w *Worker) Process(ctx context.Context, withdrawalID int64) error {
	item, err := w.store.TokenWithdrawalByID(ctx, withdrawalID)
	if err != nil {
		return fmt.Errorf("查询 Token 提币 %d 失败：%w", withdrawalID, err)
	}
	switch item.Status {
	case postgres.WithdrawalCreated:
		return w.prepare(ctx, item)
	case postgres.WithdrawalSigning:
		return w.sign(ctx, item)
	case postgres.WithdrawalSigned:
		return w.broadcast(ctx, item)
	case postgres.WithdrawalBroadcasted, postgres.WithdrawalConfirming:
		return w.confirm(ctx, item)
	default:
		return nil
	}
}

// prepare 校验平台 Token 库存和 ETH Gas 后原子分配共享平台 Nonce。
func (w *Worker) prepare(ctx context.Context, item postgres.TokenWithdrawal) error {
	platform, _, err := w.checkBalancesAndQuote(ctx, item)
	if err != nil {
		return err
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, common.HexToAddress(platform.Address))
	if err != nil {
		return fmt.Errorf("查询 Token 提币平台钱包 Pending Nonce 失败：%w", err)
	}
	item, _, err = w.store.AllocateTokenWithdrawalNonce(ctx, item.ID, chainNonce)
	if err != nil {
		return fmt.Errorf("分配 Token 提币平台 Nonce 失败：%w", err)
	}
	return w.sign(ctx, item)
}

// sign 再次复核库存和 Gas，签署标准 transfer，并在广播前保存原始交易。
func (w *Worker) sign(ctx context.Context, item postgres.TokenWithdrawal) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() || item.AmountUnits == nil || item.AmountUnits.Sign() <= 0 {
		return errors.New("Token 提币 Nonce 或金额无效")
	}
	platform, fee, err := w.checkBalancesAndQuote(ctx, item)
	if err != nil {
		return err
	}
	to := common.HexToAddress(item.ToAddress)
	calldata, err := w.contract.EncodeTransfer(to, item.AmountUnits)
	if err != nil {
		return fmt.Errorf("编码 Token 提币 calldata 失败：%w", err)
	}
	contractAddress := w.contract.Address()
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID: w.config.ChainID, Nonce: item.Nonce.Uint64(), GasTipCap: fee.MaxPriorityFeePerGasWei,
		GasFeeCap: fee.MaxFeePerGasWei, Gas: fee.GasLimit, To: &contractAddress,
		Value: new(big.Int), Data: calldata,
	})
	if err := w.validateTransaction(transaction, item); err != nil {
		return err
	}
	signed, err := w.keys.SignTx(ctx, platform.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署 Token 提币交易失败：%w", err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), signed)
	if err != nil || sender != common.HexToAddress(platform.Address) {
		return errors.New("Token 提币签名地址与平台热钱包不一致")
	}
	if err := w.validateTransaction(signed, item); err != nil {
		return err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码已签名 Token 提币交易失败：%w", err)
	}
	item, _, err = w.store.SaveSignedTokenWithdrawal(ctx, postgres.SignedTokenWithdrawal{
		WithdrawalID: item.ID, GasLimit: fee.GasLimit, MaxFeePerGasWei: fee.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: fee.MaxPriorityFeePerGasWei, RawTx: raw, TxHash: signed.Hash().Hex(),
	})
	if err != nil {
		return fmt.Errorf("持久化已签名 Token 提币失败：%w", err)
	}
	return w.broadcast(ctx, item)
}

// broadcast 复核数据库原始交易并始终重播同一 tx_hash，避免重启后重复出款。
func (w *Worker) broadcast(ctx context.Context, item postgres.TokenWithdrawal) error {
	if len(item.RawTx) == 0 || item.TxHash == "" {
		return errors.New("待广播 Token 提币缺少已签名原始交易")
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(item.RawTx); err != nil {
		return errors.New("数据库中的已签名 Token 提币交易无效")
	}
	if transaction.Hash() != common.HexToHash(item.TxHash) {
		return errors.New("Token 提币交易哈希与原始交易不一致")
	}
	if err := w.validateTransaction(&transaction, item); err != nil {
		return err
	}
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return err
	}
	sender, err := types.Sender(types.LatestSignerForChainID(w.config.ChainID), &transaction)
	if err != nil || sender != common.HexToAddress(platform.Address) {
		return errors.New("待广播 Token 提币发送方与平台热钱包不一致")
	}
	if receipt, err := w.chain.TransactionReceipt(ctx, transaction.Hash()); err == nil && receipt != nil {
		item, err = w.store.TransitionTokenWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
		if err != nil {
			return fmt.Errorf("恢复 Token 提币已广播状态失败：%w", err)
		}
		return w.confirmReceipt(ctx, item, receipt)
	} else if err != nil && !errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("查询待广播 Token 提币 Receipt 失败：%w", err)
	}
	if err := w.chain.SendTransaction(ctx, &transaction); err != nil && !alreadyKnown(err) {
		return fmt.Errorf("广播 Token 提币交易结果不明确：%w", err)
	}
	item, err = w.store.TransitionTokenWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
	if err != nil {
		return fmt.Errorf("更新 Token 提币已广播状态失败：%w", err)
	}
	w.logger.Info("Token 提币交易已广播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// confirm 查询 Token 提币 Receipt 并更新确认数或完成结算。
func (w *Worker) confirm(ctx context.Context, item postgres.TokenWithdrawal) error {
	if item.TxHash == "" {
		return errors.New("待确认 Token 提币缺少交易哈希")
	}
	receipt, err := w.chain.TransactionReceipt(ctx, common.HexToHash(item.TxHash))
	if errors.Is(err, ethereum.NotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询 Token 提币 Receipt 失败：%w", err)
	}
	return w.confirmReceipt(ctx, item, receipt)
}

// confirmReceipt 校验确认数和标准 Transfer Event，并分离结算用户 Token 与平台 Gas。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.TokenWithdrawal, receipt *types.Receipt) error {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.EffectiveGasPrice == nil {
		return errors.New("Token 提币 Receipt 内容无效")
	}
	latest, err := w.chain.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("查询 Token 提币确认高度失败：%w", err)
	}
	blockNumber := receipt.BlockNumber.Uint64()
	if blockNumber > latest || blockNumber > math.MaxInt64 {
		return errors.New("Token 提币 Receipt 区块高度无效")
	}
	confirmations := latest - blockNumber + 1
	if confirmations > math.MaxInt64 {
		return errors.New("Token 提币确认数超出数据库范围")
	}
	if confirmations < w.config.Confirmations {
		_, err := w.store.UpdateTokenWithdrawalConfirmations(ctx, item.ID, int64(confirmations))
		return err
	}
	settlement := postgres.TokenWithdrawalSettlement{
		WithdrawalID: item.ID, ActualFeeWei: new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice),
		BlockNumber: int64(blockNumber), Confirmations: int64(confirmations),
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		settlement.ErrorCode = "TOKEN_WITHDRAWAL_REVERTED"
		settlement.ErrorMessage = "Token 提币交易链上执行失败"
	} else {
		matched, err := w.hasExpectedTransfer(ctx, item, receipt.Logs)
		if err != nil {
			return err
		}
		if !matched {
			return errors.New("Token 提币 Receipt 缺少预期到账 Event，任务保留等待人工核对")
		}
		settlement.Success = true
	}
	result, changed, err := w.store.FinalizeTokenWithdrawal(ctx, settlement)
	if err != nil {
		return fmt.Errorf("结算 Token 提币失败：%w", err)
	}
	if changed {
		w.logger.Info("Token 提币已完成账本结算", "withdrawal_id", result.ID, "status", result.Status, "confirmations", confirmations)
	}
	return nil
}

// checkBalancesAndQuote 校验记录归属、热钱包 Token 库存及 ETH Gas 余额。
func (w *Worker) checkBalancesAndQuote(ctx context.Context, item postgres.TokenWithdrawal) (postgres.PlatformWallet, FeeQuote, error) {
	platform, err := w.platformFor(ctx, item)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, err
	}
	from := common.HexToAddress(platform.Address)
	tokenBalance, err := w.contract.BalanceOf(ctx, from)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, fmt.Errorf("查询平台热钱包 Token 库存失败：%w", err)
	}
	if tokenBalance == nil || tokenBalance.Cmp(item.AmountUnits) < 0 {
		return postgres.PlatformWallet{}, FeeQuote{}, ErrHotWalletTokenInsufficient
	}
	fee, err := estimateFee(ctx, w.chain, w.contract, from, common.HexToAddress(item.ToAddress), item.AmountUnits)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, err
	}
	ethBalance, err := w.chain.BalanceAt(ctx, from, nil)
	if err != nil {
		return postgres.PlatformWallet{}, FeeQuote{}, fmt.Errorf("查询平台热钱包 ETH Gas 余额失败：%w", err)
	}
	if ethBalance == nil || ethBalance.Cmp(fee.ReservedFeeWei) < 0 {
		return postgres.PlatformWallet{}, FeeQuote{}, ErrHotWalletGasInsufficient
	}
	return platform, fee, nil
}

// platformFor 查询并校验 Token 提币使用的是配置的平台热钱包。
func (w *Worker) platformFor(ctx context.Context, item postgres.TokenWithdrawal) (postgres.PlatformWallet, error) {
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return postgres.PlatformWallet{}, fmt.Errorf("查询 Token 提币平台热钱包失败：%w", err)
	}
	if platform.ID != item.PlatformWalletID {
		return postgres.PlatformWallet{}, errors.New("Token 提币记录与平台热钱包不匹配")
	}
	return platform, nil
}

// validateTransaction 严格复核链、Nonce、合约、金额和 transfer calldata。
func (w *Worker) validateTransaction(transaction *types.Transaction, item postgres.TokenWithdrawal) error {
	if transaction == nil || transaction.ChainId().Cmp(w.config.ChainID) != 0 || item.Nonce == nil ||
		!item.Nonce.IsUint64() || transaction.Nonce() != item.Nonce.Uint64() || transaction.To() == nil ||
		*transaction.To() != w.contract.Address() || transaction.Value().Sign() != 0 || transaction.Gas() == 0 ||
		transaction.GasFeeCap().Sign() <= 0 || transaction.GasTipCap().Sign() < 0 {
		return errors.New("Token 提币交易基础参数复核失败")
	}
	to, amountUnits, err := w.contract.DecodeTransferCalldata(transaction.Data())
	if err != nil {
		return fmt.Errorf("复核 Token 提币 calldata 失败：%w", err)
	}
	if to != common.HexToAddress(item.ToAddress) || amountUnits.Cmp(item.AmountUnits) != 0 {
		return errors.New("Token 提币 calldata 与数据库请求不一致")
	}
	return nil
}

// hasExpectedTransfer 校验 Receipt 存在平台热钱包到目标地址的精确 Transfer Event。
func (w *Worker) hasExpectedTransfer(ctx context.Context, item postgres.TokenWithdrawal, logs []*types.Log) (bool, error) {
	platform, err := w.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return false, fmt.Errorf("校验 Token 提币 Event 时查询平台热钱包失败：%w", err)
	}
	from := common.HexToAddress(platform.Address)
	to := common.HexToAddress(item.ToAddress)
	for _, log := range logs {
		if log == nil || log.Address != w.contract.Address() {
			continue
		}
		event, err := w.contract.DecodeTransferLog(*log)
		if err != nil {
			if errors.Is(err, erc20.ErrInvalidTransferLog) {
				continue
			}
			return false, fmt.Errorf("解析 Token 提币 Transfer Event 失败：%w", err)
		}
		if event.From == from && event.To == to && event.AmountUnits.Cmp(item.AmountUnits) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// recordError 保存经过清洗的 Token 提币 Worker 错误。
func (w *Worker) recordError(ctx context.Context, withdrawalID int64, stage, code string, workerErr error) {
	referenceID := withdrawalID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker: "token-withdrawal-worker", Stage: stage, ReferenceType: "TOKEN_WITHDRAWAL",
		ReferenceID: &referenceID, ErrorCode: code, ErrorMessage: workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录 Token 提币 Worker 错误失败", "withdrawal_id", withdrawalID, "error", err)
	}
}

// alreadyKnown 判断 RPC 错误链是否表示节点已经接收相同交易。
func alreadyKnown(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		if strings.Contains(message, "already known") || strings.Contains(message, "known transaction") {
			return true
		}
	}
	return false
}
