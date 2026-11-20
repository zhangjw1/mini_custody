package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const workerBatchSize = evm.DefaultWorkerBatchSize

// WorkerStore 定义提币 Worker 所需的持久化事务和恢复查询。
type WorkerStore interface {
	WithdrawalByID(ctx context.Context, id int64) (postgres.Withdrawal, error)
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	BalanceByUser(ctx context.Context, userID int64) (postgres.AssetBalance, error)
	ListProcessableWithdrawals(ctx context.Context, limit int) ([]postgres.Withdrawal, error)
	ListEVMTxAttempts(ctx context.Context, businessType string, businessID int64) ([]postgres.EVMTxAttempt, error)
	IncreaseWithdrawalFee(ctx context.Context, withdrawalID int64, newReservedFeeWei *big.Int) (postgres.Withdrawal, bool, error)
	AllocateWithdrawalNonce(ctx context.Context, withdrawalID int64, chainPendingNonce uint64, gas postgres.PreparedGasParams) (postgres.Withdrawal, bool, error)
	SaveSignedWithdrawal(ctx context.Context, signed postgres.SignedWithdrawal) (postgres.Withdrawal, bool, error)
	ReplaceWithdrawalTx(ctx context.Context, replacement postgres.SignedWithdrawal, newReservedFeeWei *big.Int) (postgres.Withdrawal, bool, error)
	TouchWithdrawalBroadcast(ctx context.Context, withdrawalID int64) (postgres.Withdrawal, error)
	MarkWithdrawalManualReview(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (postgres.Withdrawal, bool, error)
	TransitionWithdrawal(ctx context.Context, withdrawalID int64, target string) (postgres.Withdrawal, error)
	ReleaseWithdrawal(ctx context.Context, withdrawalID int64, errorCode, errorMessage string) (postgres.Withdrawal, bool, error)
	UpdateWithdrawalConfirmations(ctx context.Context, withdrawalID, confirmations int64) (postgres.Withdrawal, error)
	FinalizeWithdrawal(ctx context.Context, settlement postgres.WithdrawalSettlement) (postgres.Withdrawal, bool, error)
	RecordEVMSigningFailure(ctx context.Context, businessType string, businessID int64, maxRetries int, worker, refType string, refID int64, errorCode, errorMessage string) (bool, error)
	RecordEVMBumpBlocked(ctx context.Context, businessType string, businessID int64, worker, refType string, refID int64, reasonCode, reasonMessage string) (time.Time, bool, error)
	RecordWorkerError(ctx context.Context, item postgres.WorkerError) (int64, error)
}

// WorkerConfig 定义提币轮询间隔、确认数、卡单重播加价策略和 Sepolia Chain ID。
type WorkerConfig struct {
	Interval                     time.Duration
	Confirmations                uint64
	ChainID                      *big.Int
	RebroadcastAfter             time.Duration
	BumpAfter                    time.Duration
	BumpBlockedManualReviewAfter time.Duration
	BumpPercent                  uint64
	MaxBumps                     int
	MaxFeeCapWei                 *big.Int
}

// Worker 负责提币签名、幂等广播、崩溃恢复和费用结算。
type Worker struct {
	chain       Chain
	store       WorkerStore
	keys        wallet.KeyProvider
	logger      *slog.Logger
	config      WorkerConfig
	fees        evm.FeeEstimator
	broadcaster evm.Broadcaster
	confirmer   evm.Confirmer
}

// NewWorker 创建并校验提币 Worker。
func NewWorker(chain Chain, store WorkerStore, keys wallet.KeyProvider, logger *slog.Logger, cfg WorkerConfig) (*Worker, error) {
	if chain == nil || store == nil || keys == nil || logger == nil {
		return nil, errors.New("提币 Worker 依赖不能为空")
	}
	if cfg.Interval <= 0 || cfg.Confirmations == 0 || cfg.ChainID == nil || cfg.ChainID.Cmp(big.NewInt(evm.SepoliaChainID)) != 0 {
		return nil, errors.New("提币 Worker 配置无效")
	}
	if err := normalizeReplacementConfig(&cfg); err != nil {
		return nil, err
	}
	cfg.ChainID = new(big.Int).Set(cfg.ChainID)
	return &Worker{
		chain:       chain,
		store:       store,
		keys:        keys,
		logger:      logger,
		config:      cfg,
		fees:        evm.NewFeeEstimator(chain),
		broadcaster: evm.NewBroadcaster(chain),
		confirmer:   evm.NewConfirmer(chain),
	}, nil
}

// Run 周期处理可恢复提币，单条失败不会阻止其他提币继续推进。
func (w *Worker) Run(ctx context.Context) error {
	return evm.RunPollLoop(ctx, w.config.Interval, w.RunOnce, func(err error) {
		w.logger.Warn("Sepolia 提币 Worker 本轮存在失败，将在下轮重试", "error", err)
	})
}

// RunOnce 查询全部可恢复提币并逐条推进状态。
func (w *Worker) RunOnce(ctx context.Context) error {
	ctx = evm.WithTickCache(ctx)
	items, err := w.store.ListProcessableWithdrawals(ctx, workerBatchSize)
	if err != nil {
		return fmt.Errorf("查询待处理提币失败：%w", err)
	}
	var firstErr error
	for _, item := range items {
		if err := w.Process(ctx, item.ID); err != nil {
			w.recordError(ctx, item.ID, "process", "WITHDRAWAL_PROCESS_FAILED", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Process 根据持久化状态恢复并推进一笔提币。
func (w *Worker) Process(ctx context.Context, withdrawalID int64) error {
	item, err := w.store.WithdrawalByID(ctx, withdrawalID)
	if err != nil {
		return fmt.Errorf("查询提币 %d 失败：%w", withdrawalID, err)
	}
	switch item.Status {
	case postgres.WithdrawalCreated:
		return w.prepare(ctx, item)
	case postgres.WithdrawalSigning:
		return w.sign(ctx, item)
	case postgres.WithdrawalSigned, postgres.WithdrawalBroadcasting, postgres.WithdrawalBroadcastUnknown:
		return w.broadcast(ctx, item)
	case postgres.WithdrawalBroadcasted, postgres.WithdrawalConfirming:
		return w.confirm(ctx, item)
	default:
		return nil
	}
}

// prepare 重新估算费用、校验链上余额并原子分配 Nonce。
func (w *Worker) prepare(ctx context.Context, item postgres.Withdrawal) error {
	walletAddress, quote, err := w.quoteFor(ctx, item)
	if err != nil {
		return err
	}
	item, _, err = w.store.IncreaseWithdrawalFee(ctx, item.ID, quote.ReservedFeeWei)
	if errors.Is(err, postgres.ErrInsufficientBalance) {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "FEE_RESERVE_INSUFFICIENT", "可用余额不足以补充最新最大网络费")
		return releaseErr
	}
	if err != nil {
		return fmt.Errorf("调整提币 %d 最大网络费失败：%w", item.ID, err)
	}
	required := new(big.Int).Add(item.AmountWei, item.ReservedFeeWei)
	chainBalance, err := w.chain.BalanceAt(ctx, common.HexToAddress(walletAddress.Address), nil)
	if err != nil {
		return fmt.Errorf("查询提币地址链上余额失败：%w", err)
	}
	if chainBalance == nil || chainBalance.Cmp(required) < 0 {
		_, _, releaseErr := w.store.ReleaseWithdrawal(ctx, item.ID, "CHAIN_BALANCE_INSUFFICIENT", "托管地址链上余额不足以支付提币金额和最大网络费")
		return releaseErr
	}
	chainNonce, err := w.chain.PendingNonceAt(ctx, common.HexToAddress(walletAddress.Address))
	if err != nil {
		return fmt.Errorf("查询提币地址 Pending Nonce 失败：%w", err)
	}
	item, _, err = w.store.AllocateWithdrawalNonce(ctx, item.ID, chainNonce, postgres.PreparedGasParams{
		GasLimit:                quote.GasLimit,
		MaxFeePerGasWei:         quote.MaxFeePerGasWei,
		MaxPriorityFeePerGasWei: quote.MaxPriorityFeePerGasWei,
	})
	if err != nil {
		return fmt.Errorf("分配提币 %d Nonce 失败：%w", item.ID, err)
	}
	return w.sign(ctx, item)
}

// sign 在已分配 Nonce 后仅读取数据库已落库的 Gas 参数构建、签名并持久化交易，不再调用会失败的链上 RPC。
func (w *Worker) sign(ctx context.Context, item postgres.Withdrawal) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return w.handleSigningFailure(ctx, item.ID, errors.New("提币 Nonce 无效"))
	}
	if item.GasLimit == nil || *item.GasLimit <= 0 || item.MaxFeePerGasWei == nil ||
		item.MaxFeePerGasWei.Sign() <= 0 || item.MaxPriorityFeePerGasWei == nil || item.MaxPriorityFeePerGasWei.Sign() < 0 {
		return w.handleSigningFailure(ctx, item.ID, errors.New("已分配 Nonce 的提币缺少有效 Gas 参数"))
	}
	walletAddress, err := w.store.WalletAddressByUser(ctx, item.UserID)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("查询提币地址失败：%w", err))
	}
	gasLimit := uint64(*item.GasLimit)
	feeCap := new(big.Int).Set(item.MaxFeePerGasWei)
	tip := new(big.Int).Set(item.MaxPriorityFeePerGasWei)

	// H4：重签阶段保证 gasLimit*feeCap 不超过冻结预留费；若超出则先尝试增额，余额不足则钳制在冻结额度内。
	requiredFee := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	if requiredFee.Cmp(item.ReservedFeeWei) > 0 {
		updated, _, feeErr := w.store.IncreaseWithdrawalFee(ctx, item.ID, requiredFee)
		if feeErr == nil {
			item = updated
		} else if errors.Is(feeErr, postgres.ErrInsufficientBalance) {
			feeCap = new(big.Int).Div(item.ReservedFeeWei, new(big.Int).SetUint64(gasLimit))
			if feeCap.Sign() <= 0 {
				return w.handleSigningFailure(ctx, item.ID, errors.New("冻结预留费不足以覆盖任何有效 Gas 费率"))
			}
			if tip.Cmp(feeCap) > 0 {
				tip = new(big.Int).Set(feeCap)
			}
		} else {
			return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("重签阶段校验提币预留费用失败：%w", feeErr))
		}
	}

	to := common.HexToAddress(item.ToAddress)
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(w.config.ChainID),
		Nonce:     item.Nonce.Uint64(),
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     new(big.Int).Set(item.AmountWei),
	})
	signed, err := w.keys.SignTx(ctx, walletAddress.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("签署提币交易失败：%w", err))
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("编码已签名提币交易失败：%w", err))
	}
	item, _, err = w.store.SaveSignedWithdrawal(ctx, postgres.SignedWithdrawal{
		WithdrawalID:            item.ID,
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         feeCap,
		MaxPriorityFeePerGasWei: tip,
		RawTx:                   raw,
		TxHash:                  signed.Hash().Hex(),
	})
	if err != nil {
		return w.handleSigningFailure(ctx, item.ID, fmt.Errorf("持久化已签名提币失败：%w", err))
	}
	return w.broadcast(ctx, item)
}

// handleSigningFailure 持久化累计分配 Nonce 后的连续签名失败次数，超限后转入 MANUAL_REVIEW 并写一次告警。
func (w *Worker) handleSigningFailure(ctx context.Context, id int64, err error) error {
	escalated, recErr := w.store.RecordEVMSigningFailure(
		ctx, postgres.EVMBusinessWithdrawal, id, evm.DefaultMaxSigningRetryCount,
		"withdrawal-worker", postgres.EVMBusinessWithdrawal, id,
		"SIGNING_MANUAL_INTERVENTION", err.Error(),
	)
	if recErr != nil {
		w.logger.Error("记录提币签名失败计数异常", "withdrawal_id", id, "error", recErr)
	} else if escalated {
		w.logger.Warn("提币连续签名失败超限，已转入人工核查状态", "withdrawal_id", id)
	}
	return err
}

// broadcast 优先查询原交易 Receipt，并始终重播数据库中相同的 raw_tx。
func (w *Worker) broadcast(ctx context.Context, item postgres.Withdrawal) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	if receipt, err := w.broadcaster.CheckExistingReceipt(ctx, transaction.Hash()); err != nil {
		return fmt.Errorf("查询待广播提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		item, err = w.ensureBroadcasted(ctx, item)
		if err != nil {
			return err
		}
		return w.confirmReceipt(ctx, item, receipt, item.TxHash)
	}
	if item.Status == postgres.WithdrawalSigned {
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasting)
		if err != nil {
			return fmt.Errorf("提币进入广播中状态失败：%w", err)
		}
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		if item.Status == postgres.WithdrawalBroadcasting {
			_, _ = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcastUnknown)
		}
		return fmt.Errorf("广播提币交易结果不明确：%w", err)
	}
	if handled {
		return nil
	}
	item, err = w.ensureBroadcasted(ctx, item)
	if err != nil {
		return err
	}
	w.logger.Info("Sepolia 提币交易已广播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// ensureBroadcasted 将不同恢复状态安全推进到已广播状态。
func (w *Worker) ensureBroadcasted(ctx context.Context, item postgres.Withdrawal) (postgres.Withdrawal, error) {
	var err error
	if item.Status == postgres.WithdrawalSigned {
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasting)
		if err != nil {
			return postgres.Withdrawal{}, fmt.Errorf("提币进入广播中状态失败：%w", err)
		}
	}
	if item.Status == postgres.WithdrawalBroadcasting || item.Status == postgres.WithdrawalBroadcastUnknown {
		item, err = w.store.TransitionWithdrawal(ctx, item.ID, postgres.WithdrawalBroadcasted)
		if err != nil {
			return postgres.Withdrawal{}, fmt.Errorf("提币进入已广播状态失败：%w", err)
		}
	}
	return item, nil
}

// confirm 查询全部候选交易 Receipt，在卡单超时后执行重播或 EIP-1559 Gas 加价替换。
func (w *Worker) confirm(ctx context.Context, item postgres.Withdrawal) error {
	attempts, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return err
	}
	receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates)
	if err != nil {
		return fmt.Errorf("查询提币 Receipt 失败：%w", err)
	}
	if receipt != nil {
		return w.confirmReceipt(ctx, item, receipt, matchedHash)
	}
	consumed, err := w.checkNonceConsumed(ctx, item, candidates)
	if err != nil {
		return err
	}
	if consumed {
		return nil
	}
	if item.Status != postgres.WithdrawalBroadcasted {
		return nil
	}
	tierStart := replacementTierStart(item.BumpCount, item.BroadcastedAt, item.LastBroadcastAt, attempts)
	if tierStart != nil && time.Since(*tierStart) >= w.config.BumpAfter && int(item.BumpCount) < w.config.MaxBumps {
		return w.bumpAndBroadcast(ctx, item, attempts)
	}
	if item.LastBroadcastAt != nil && time.Since(*item.LastBroadcastAt) >= w.config.RebroadcastAfter {
		return w.rebroadcast(ctx, item)
	}
	return nil
}

// confirmReceipt 根据 Receipt 和最新高度更新确认数并结算余额。
func (w *Worker) confirmReceipt(ctx context.Context, item postgres.Withdrawal, receipt *types.Receipt, matchedTxHash string) error {
	blockNumber, confirmations, err := w.confirmer.EvaluateConfirmations(ctx, receipt)
	if err != nil {
		return err
	}
	if uint64(confirmations) < w.config.Confirmations {
		_, err := w.store.UpdateWithdrawalConfirmations(ctx, item.ID, confirmations)
		return err
	}
	actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	result, changed, err := w.store.FinalizeWithdrawal(ctx, postgres.WithdrawalSettlement{
		WithdrawalID:  item.ID,
		ActualFeeWei:  actualFee,
		Success:       receipt.Status == types.ReceiptStatusSuccessful,
		BlockNumber:   blockNumber,
		Confirmations: confirmations,
		TxHash:        matchedTxHash,
	})
	if err != nil {
		return fmt.Errorf("结算提币 %d 失败：%w", item.ID, err)
	}
	if changed {
		w.logger.Info("Sepolia 提币已完成费用结算", "withdrawal_id", result.ID, "status", result.Status, "confirmations", confirmations, "tx_hash", result.TxHash)
	}
	return nil
}

// quoteFor 查询提币出款地址并生成最新费用估算。
func (w *Worker) quoteFor(ctx context.Context, item postgres.Withdrawal) (postgres.WalletAddress, FeeQuote, error) {
	walletAddress, err := w.store.WalletAddressByUser(ctx, item.UserID)
	if err != nil {
		return postgres.WalletAddress{}, FeeQuote{}, fmt.Errorf("查询提币托管地址失败：%w", err)
	}
	if walletAddress.ID != item.AddressID {
		return postgres.WalletAddress{}, FeeQuote{}, errors.New("提币记录与托管地址不匹配")
	}
	quote, err := estimateFee(ctx, w.chain, common.HexToAddress(walletAddress.Address), common.HexToAddress(item.ToAddress), item.AmountWei)
	return walletAddress, quote, err
}

// recordError 保存经过清洗的提币 Worker 错误，记录失败不覆盖原业务错误。
func (w *Worker) recordError(ctx context.Context, withdrawalID int64, stage, code string, workerErr error) {
	referenceID := withdrawalID
	if _, err := w.store.RecordWorkerError(ctx, postgres.WorkerError{
		Worker:        "withdrawal-worker",
		Stage:         stage,
		ReferenceType: "WITHDRAWAL",
		ReferenceID:   &referenceID,
		ErrorCode:     code,
		ErrorMessage:  workerErr.Error(),
	}); err != nil {
		w.logger.Error("记录提币 Worker 错误失败", "withdrawal_id", withdrawalID, "error", err)
	}
}

// normalizeReplacementConfig 校验并填充提币重播与 Gas 加价配置默认值。
func normalizeReplacementConfig(cfg *WorkerConfig) error {
	settings := evm.ReplacementSettings{
		RebroadcastAfter:             cfg.RebroadcastAfter,
		BumpAfter:                    cfg.BumpAfter,
		BumpBlockedManualReviewAfter: cfg.BumpBlockedManualReviewAfter,
		BumpPercent:                  cfg.BumpPercent,
		MaxBumps:                     cfg.MaxBumps,
		MaxFeeCapWei:                 cfg.MaxFeeCapWei,
	}
	if err := evm.NormalizeReplacementSettings(&settings); err != nil {
		return err
	}
	cfg.RebroadcastAfter = settings.RebroadcastAfter
	cfg.BumpAfter = settings.BumpAfter
	cfg.BumpBlockedManualReviewAfter = settings.BumpBlockedManualReviewAfter
	cfg.BumpPercent = settings.BumpPercent
	cfg.MaxBumps = settings.MaxBumps
	cfg.MaxFeeCapWei = settings.MaxFeeCapWei
	return nil
}

// candidateHashes 查询同一笔提币按尝试序号倒序排列的所有候选交易哈希。
func (w *Worker) candidateHashes(ctx context.Context, item postgres.Withdrawal) ([]postgres.EVMTxAttempt, []string, error) {
	attempts, err := w.store.ListEVMTxAttempts(ctx, postgres.EVMBusinessWithdrawal, item.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("查询提币候选交易记录失败：%w", err)
	}
	candidates, err := evm.CollectCandidateHashes(attempts, func(a postgres.EVMTxAttempt) string { return a.TxHash }, item.TxHash)
	if err != nil {
		return nil, nil, errors.New("待确认提币缺少交易哈希")
	}
	return attempts, candidates, nil
}

// checkNonceConsumed 在所有候选交易均无 Receipt 时校验最新已确认 Nonce 是否已越过本单 Nonce。
func (w *Worker) checkNonceConsumed(ctx context.Context, item postgres.Withdrawal, candidates []string) (bool, error) {
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return false, nil
	}
	walletAddress, err := w.store.WalletAddressByUser(ctx, item.UserID)
	if err != nil {
		return false, fmt.Errorf("校验提币 Nonce 时查询托管地址失败：%w", err)
	}
	confirmedNonce, err := w.chain.NonceAt(ctx, common.HexToAddress(walletAddress.Address), nil)
	if err != nil {
		return false, fmt.Errorf("查询提币已确认 Nonce 失败：%w", err)
	}
	if confirmedNonce <= item.Nonce.Uint64() {
		return false, nil
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("二次复核提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		return true, w.confirmReceipt(ctx, item, receipt, matchedHash)
	}
	reason := fmt.Sprintf("地址最新确认 Nonce(%d) 已超过提币 Nonce(%d)，但所有候选交易均未上链，需人工核查", confirmedNonce, item.Nonce.Uint64())
	_, changed, err := w.store.MarkWithdrawalManualReview(ctx, item.ID, "EVM_TX_MANUAL_REVIEW", reason)
	if err != nil {
		return false, fmt.Errorf("转入提币人工核查状态失败：%w", err)
	}
	if changed {
		w.recordError(ctx, item.ID, "confirm", "EVM_TX_MANUAL_REVIEW", errors.New(reason))
		w.logger.Warn("Sepolia 提币转入人工核查状态", "withdrawal_id", item.ID, "nonce", item.Nonce.String(), "confirmed_nonce", confirmedNonce)
	}
	return true, nil
}

// handleNonceTooLow 在广播遇到 nonce too low 时优先复核所有候选 Receipt 与已确认 Nonce。
func (w *Worker) handleNonceTooLow(ctx context.Context, item postgres.Withdrawal) (bool, error) {
	_, candidates, err := w.candidateHashes(ctx, item)
	if err != nil {
		return false, err
	}
	if receipt, matchedHash, err := w.confirmer.FindFirstReceipt(ctx, candidates); err != nil {
		return false, fmt.Errorf("复核提币 Receipt 失败：%w", err)
	} else if receipt != nil {
		ensured, ensureErr := w.ensureBroadcasted(ctx, item)
		if ensureErr != nil {
			return false, ensureErr
		}
		return true, w.confirmReceipt(ctx, ensured, receipt, matchedHash)
	}
	return w.checkNonceConsumed(ctx, item, candidates)
}

// bumpAndBroadcast 按同一 Nonce 提升 Gas 费用重新签名、落库并广播替换交易。
func (w *Worker) bumpAndBroadcast(ctx context.Context, item postgres.Withdrawal, attempts []postgres.EVMTxAttempt) error {
	if item.Nonce == nil || !item.Nonce.IsUint64() {
		return errors.New("待加价提币缺少有效 Nonce")
	}
	prevFeeCap := item.MaxFeePerGasWei
	prevTip := item.MaxPriorityFeePerGasWei
	if (prevFeeCap == nil || prevTip == nil) && len(attempts) > 0 {
		prevFeeCap = attempts[0].MaxFeePerGasWei
		prevTip = attempts[0].MaxPriorityFeePerGasWei
	}
	if prevFeeCap == nil || prevFeeCap.Sign() <= 0 || prevTip == nil || prevTip.Sign() < 0 {
		return errors.New("待加价提币缺少有效历史 Gas 参数")
	}
	walletAddress, quote, err := w.quoteFor(ctx, item)
	if err != nil {
		return err
	}
	gasLimit := quote.GasLimit
	if item.GasLimit != nil && *item.GasLimit > 0 && uint64(*item.GasLimit) > gasLimit {
		gasLimit = uint64(*item.GasLimit)
	}
	baseFee, err := w.fees.LatestBaseFee(ctx)
	if err != nil {
		return fmt.Errorf("提币加价查询 Base Fee 失败：%w", err)
	}
	bumpedFeeCap, bumpedTip, ok := evm.ComputeBumpedFees(prevFeeCap, prevTip, baseFee, quote.MaxPriorityFeePerGasWei, w.config.BumpPercent, w.config.MaxFeeCapWei)
	if !ok {
		return w.handleBumpBlocked(ctx, item, "EVM_BUMP_FEE_CAP_EXCEEDED", "提币加价后手续费上限超过配置最大值，停止继续加价")
	}
	newReservedFee := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), bumpedFeeCap)
	if newReservedFee.Cmp(item.ReservedFeeWei) < 0 {
		newReservedFee = new(big.Int).Set(item.ReservedFeeWei)
	}
	if newReservedFee.Cmp(item.ReservedFeeWei) > 0 {
		balance, balErr := w.store.BalanceByUser(ctx, item.UserID)
		if balErr != nil {
			return fmt.Errorf("提币加价预检用户可用余额失败：%w", balErr)
		}
		diff := new(big.Int).Sub(newReservedFee, item.ReservedFeeWei)
		if balance.AvailableWei == nil || balance.AvailableWei.Cmp(diff) < 0 {
			return w.handleBumpBlocked(ctx, item, "FEE_RESERVE_INSUFFICIENT", "用户可用余额不足以支付提币加价替换费用")
		}
	}
	chainBalance, err := w.chain.BalanceAt(ctx, common.HexToAddress(walletAddress.Address), nil)
	if err != nil {
		return fmt.Errorf("提币加价查询链上余额失败：%w", err)
	}
	required := new(big.Int).Add(item.AmountWei, newReservedFee)
	if chainBalance == nil || chainBalance.Cmp(required) < 0 {
		return w.handleBumpBlocked(ctx, item, "CHAIN_BALANCE_INSUFFICIENT", "托管地址链上余额不足以支持提币加价替换")
	}
	to := common.HexToAddress(item.ToAddress)
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(w.config.ChainID),
		Nonce:     item.Nonce.Uint64(),
		GasTipCap: bumpedTip,
		GasFeeCap: bumpedFeeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     new(big.Int).Set(item.AmountWei),
	})
	signed, err := w.keys.SignTx(ctx, walletAddress.DerivationPath, transaction, w.config.ChainID)
	if err != nil {
		return fmt.Errorf("签署提币加价替换交易失败：%w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("编码提币加价替换交易失败：%w", err)
	}
	replaced, changed, err := w.store.ReplaceWithdrawalTx(ctx, postgres.SignedWithdrawal{
		WithdrawalID:            item.ID,
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         bumpedFeeCap,
		MaxPriorityFeePerGasWei: bumpedTip,
		RawTx:                   raw,
		TxHash:                  strings.ToLower(signed.Hash().Hex()),
	}, newReservedFee)
	if errors.Is(err, postgres.ErrInsufficientBalance) {
		return w.handleBumpBlocked(ctx, item, "FEE_RESERVE_INSUFFICIENT", "用户可用余额不足以支付提币加价替换费用")
	}
	if err != nil {
		return fmt.Errorf("保存提币加价替换交易失败：%w", err)
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, signed, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, replaced)
	})
	if err != nil {
		return fmt.Errorf("广播提币加价替换交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if changed {
		w.logger.Info("Sepolia 提币已完成 Gas 加价替换并广播", "withdrawal_id", replaced.ID, "bump_count", replaced.BumpCount, "tx_hash", replaced.TxHash)
	}
	return nil
}

// rebroadcast 使用相同 raw_tx 原样重播卡在内存池的提币交易。
func (w *Worker) rebroadcast(ctx context.Context, item postgres.Withdrawal) error {
	transaction, err := w.broadcaster.DecodeSignedTx(item.RawTx, item.TxHash)
	if err != nil {
		return err
	}
	handled, err := w.broadcaster.SendIdempotent(ctx, transaction, func(c context.Context) (bool, error) {
		return w.handleNonceTooLow(c, item)
	})
	if err != nil {
		return fmt.Errorf("重播提币交易失败：%w", err)
	}
	if handled {
		return nil
	}
	if _, err := w.store.TouchWithdrawalBroadcast(ctx, item.ID); err != nil {
		return fmt.Errorf("更新提币重播时间失败：%w", err)
	}
	w.logger.Info("Sepolia 提币交易已重播", "withdrawal_id", item.ID, "tx_hash", item.TxHash)
	return nil
}

// replacementTierStart 返回当前费率阶梯的起始时间，用于判断是否满足加价等待阈值。
func replacementTierStart(bumpCount int32, broadcastedAt, lastBroadcastAt *time.Time, attempts []postgres.EVMTxAttempt) *time.Time {
	var latestAttemptAt *time.Time
	if len(attempts) > 0 {
		latestAttemptAt = &attempts[0].CreatedAt
	}
	return evm.ReplacementTierStart(bumpCount, broadcastedAt, lastBroadcastAt, latestAttemptAt)
}

// handleBumpBlocked 统一持久化提币加价阻塞原因、按间隔重播原交易并在超时后转入人工核查。
func (w *Worker) handleBumpBlocked(ctx context.Context, item postgres.Withdrawal, code, message string) error {
	return evm.HandleBumpBlocked(ctx, evm.BumpBlockedAction{
		RecordBlocked: func(c context.Context, cCode, cMsg string) (time.Time, error) {
			at, _, err := w.store.RecordEVMBumpBlocked(c, postgres.EVMBusinessWithdrawal, item.ID, "withdrawal-worker", postgres.EVMBusinessWithdrawal, item.ID, cCode, cMsg)
			return at, err
		},
		MarkManualReview: func(c context.Context, cCode, cMsg string) error {
			_, changed, err := w.store.MarkWithdrawalManualReview(c, item.ID, cCode, cMsg)
			if err != nil {
				return err
			}
			if changed {
				w.recordError(c, item.ID, "bump", cCode, errors.New(cMsg))
				w.logger.Warn("Sepolia 提币加价长期受阻，已转入人工核查", "withdrawal_id", item.ID, "reason", code)
			}
			return nil
		},
		Rebroadcast:       func(c context.Context) error { return w.rebroadcast(c, item) },
		LastBroadcastAt:   item.LastBroadcastAt,
		RebroadcastAfter:  w.config.RebroadcastAfter,
		ManualReviewAfter: w.config.BumpBlockedManualReviewAfter,
		ReasonCode:        code,
		ReasonMessage:     message,
	})
}
