package evm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	DefaultWorkerBatchSize             = 100
	DefaultConfirmBatchSize            = 200
	DefaultMaxSigningRetryCount        = 5
	DefaultRebroadcastAge              = 60 * time.Second
	DefaultBumpAge                     = 180 * time.Second
	DefaultBumpBlockedManualReviewAfter = 30 * time.Minute
	DefaultBumpPercent                 = 15
	DefaultMaxFeeCapWei                = 100_000_000_000
	BaseFeeMultiplier                  = 2
	MinBumpPercent                     = 10

	rpcErrCodeRateLimitOrTimeout = -32002
	rpcErrCodeLimitExceeded      = -32005
)

// EIP1559FeeQuote 描述一次 EVM EIP-1559 交易手续费估算结果。
type EIP1559FeeQuote struct {
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	ReservedFeeWei          *big.Int
}

// ReplacementSettings 描述 EVM 卡单重播与 Gas 加价替换策略参数。
type ReplacementSettings struct {
	RebroadcastAfter           time.Duration
	BumpAfter                  time.Duration
	BumpBlockedManualReviewAfter time.Duration
	BumpPercent                uint64
	MaxBumps                   int
	MaxFeeCapWei               *big.Int
}

// FeeEstimatorChain 定义 FeeEstimator 查询最新区块头和建议优先费所需的链能力。
type FeeEstimatorChain interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
}

// FeeEstimator 封装 EVM EIP-1559 费用估算、BaseFee 查询与替换加价计算。
type FeeEstimator struct {
	chain FeeEstimatorChain
}

// NewFeeEstimator 创建 EVM 手续费估算器。
func NewFeeEstimator(chain FeeEstimatorChain) FeeEstimator {
	return FeeEstimator{chain: chain}
}

// LatestBaseFee 读取并校验最新区块的 Base Fee，优先命中当前轮次缓存。
func (e FeeEstimator) LatestBaseFee(ctx context.Context) (*big.Int, error) {
	header, err := CachedLatestHeader(ctx, e.chain.HeaderByNumber)
	if err != nil {
		return nil, fmt.Errorf("查询最新区块费用失败：%w", err)
	}
	if header == nil || header.BaseFee == nil || header.BaseFee.Sign() < 0 {
		return nil, errors.New("最新区块缺少有效 Base Fee")
	}
	return new(big.Int).Set(header.BaseFee), nil
}

// QuoteWithGasLimit 根据已知 Gas Limit、最新 Base Fee 与建议优先费计算 EIP-1559 费用上限。
func (e FeeEstimator) QuoteWithGasLimit(ctx context.Context, gasLimit uint64) (EIP1559FeeQuote, error) {
	if gasLimit == 0 {
		return EIP1559FeeQuote{}, errors.New("RPC 返回的 Gas Limit 无效")
	}
	baseFee, err := e.LatestBaseFee(ctx)
	if err != nil {
		return EIP1559FeeQuote{}, err
	}
	tip, err := CachedGasTipCap(ctx, e.chain.SuggestGasTipCap)
	if err != nil {
		return EIP1559FeeQuote{}, fmt.Errorf("查询建议优先费失败：%w", err)
	}
	if tip == nil || tip.Sign() < 0 {
		return EIP1559FeeQuote{}, errors.New("RPC 返回的建议优先费无效")
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(BaseFeeMultiplier)), tip)
	reserved := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), maxFee)
	return EIP1559FeeQuote{
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         maxFee,
		MaxPriorityFeePerGasWei: new(big.Int).Set(tip),
		ReservedFeeWei:          reserved,
	}, nil
}

// NormalizeReplacementSettings 校验并填充重播与 Gas 加价替换策略的默认值。
func NormalizeReplacementSettings(cfg *ReplacementSettings) error {
	if cfg.RebroadcastAfter < 0 || cfg.BumpAfter < 0 || cfg.BumpBlockedManualReviewAfter < 0 || cfg.MaxBumps < 0 {
		return errors.New("卡单重播与 Gas 加价参数不能为负数")
	}
	if cfg.RebroadcastAfter == 0 {
		cfg.RebroadcastAfter = DefaultRebroadcastAge
	}
	if cfg.BumpAfter == 0 {
		cfg.BumpAfter = DefaultBumpAge
	}
	if cfg.BumpBlockedManualReviewAfter == 0 {
		cfg.BumpBlockedManualReviewAfter = DefaultBumpBlockedManualReviewAfter
	}
	if cfg.BumpPercent == 0 {
		cfg.BumpPercent = DefaultBumpPercent
	} else if cfg.BumpPercent < MinBumpPercent {
		return errors.New("Gas 加价比例不能低于 10%")
	}
	if cfg.MaxFeeCapWei == nil {
		cfg.MaxFeeCapWei = big.NewInt(DefaultMaxFeeCapWei)
	} else if cfg.MaxFeeCapWei.Sign() <= 0 {
		return errors.New("最大费用上限必须大于零")
	} else {
		cfg.MaxFeeCapWei = new(big.Int).Set(cfg.MaxFeeCapWei)
	}
	return nil
}

// ComputeBumpedFees 计算满足 EIP-1559 最低加价比例和链上市场费率的替换手续费上限。
func ComputeBumpedFees(prevFeeCap, prevTip, baseFee, suggestTip *big.Int, bumpPercent uint64, maxFeeCapWei *big.Int) (*big.Int, *big.Int, bool) {
	minFeeCap := CeilBumpPercent(prevFeeCap, bumpPercent)
	if minFeeCap.Cmp(prevFeeCap) <= 0 {
		minFeeCap = new(big.Int).Add(prevFeeCap, big.NewInt(1))
	}
	minTip := CeilBumpPercent(prevTip, bumpPercent)
	if prevTip.Sign() > 0 && minTip.Cmp(prevTip) <= 0 {
		minTip = new(big.Int).Add(prevTip, big.NewInt(1))
	}
	if maxFeeCapWei != nil && minFeeCap.Cmp(maxFeeCapWei) > 0 {
		return nil, nil, false
	}
	tip := new(big.Int).Set(minTip)
	if suggestTip != nil && suggestTip.Cmp(tip) > 0 {
		tip.Set(suggestTip)
	}
	feeCap := new(big.Int).Set(minFeeCap)
	if baseFee != nil {
		marketFeeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(BaseFeeMultiplier)), tip)
		if marketFeeCap.Cmp(feeCap) > 0 {
			feeCap.Set(marketFeeCap)
		}
	}
	if maxFeeCapWei != nil && feeCap.Cmp(maxFeeCapWei) > 0 {
		feeCap.Set(maxFeeCapWei)
		if tip.Cmp(feeCap) > 0 {
			tip.Set(feeCap)
		}
	}
	if feeCap.Cmp(tip) < 0 {
		feeCap.Set(tip)
	}
	return feeCap, tip, true
}

// CeilBumpPercent 按百分比向上取整放大费用数值。
func CeilBumpPercent(value *big.Int, percent uint64) *big.Int {
	if value == nil || value.Sign() <= 0 {
		return new(big.Int)
	}
	factor := new(big.Int).SetUint64(100 + percent)
	numerator := new(big.Int).Mul(new(big.Int).Set(value), factor)
	numerator.Add(numerator, big.NewInt(99))
	return numerator.Div(numerator, big.NewInt(100))
}

// BroadcasterChain 定义 Broadcaster 所需的交易查询和广播能力。
type BroadcasterChain interface {
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// Broadcaster 封装已签名原始交易解码、哈希复核及去重广播。
type Broadcaster struct {
	chain BroadcasterChain
}

// NewBroadcaster 创建 EVM 交易广播器。
func NewBroadcaster(chain BroadcasterChain) Broadcaster {
	return Broadcaster{chain: chain}
}

// DecodeSignedTx 反序列化数据库保存的原始交易并校验其哈希与预期一致。
func (b Broadcaster) DecodeSignedTx(rawTx []byte, txHash string) (*types.Transaction, error) {
	if len(rawTx) == 0 || strings.TrimSpace(txHash) == "" {
		return nil, errors.New("待广播交易缺少已签名原始数据")
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(rawTx); err != nil {
		return nil, errors.New("数据库中的已签名交易无效")
	}
	if transaction.Hash() != common.HexToHash(txHash) {
		return nil, errors.New("交易哈希与已签名原始交易不一致")
	}
	return &transaction, nil
}

// CheckExistingReceipt 查询交易回执，不存在时返回 nil, nil 以便继续广播流程。
func (b Broadcaster) CheckExistingReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	receipt, err := b.chain.TransactionReceipt(ctx, txHash)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询交易 Receipt 失败：%w", err)
	}
	return receipt, nil
}

// SendIdempotent 广播交易，对 already known 视为已提交，对 nonce too low 交给回调恢复，否则返回错误。
func (b Broadcaster) SendIdempotent(ctx context.Context, transaction *types.Transaction, onNonceTooLow func(context.Context) (bool, error)) (handledByRecovery bool, err error) {
	if err := b.chain.SendTransaction(ctx, transaction); err != nil && !AlreadyKnown(err) {
		if NonceTooLow(err) && onNonceTooLow != nil {
			handled, checkErr := onNonceTooLow(ctx)
			if checkErr != nil {
				return false, checkErr
			}
			if handled {
				return true, nil
			}
		}
		return false, err
	}
	return false, nil
}

// AlreadyKnown 判断 RPC 错误链是否表示节点已经接收过同一交易。
func AlreadyKnown(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		if strings.Contains(message, "already known") || strings.Contains(message, "known transaction") {
			return true
		}
	}
	return false
}

// NonceTooLow 判断 RPC 错误链是否表示当前交易 Nonce 已被链上消费。
func NonceTooLow(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := strings.ToLower(current.Error())
		if strings.Contains(message, "nonce too low") || strings.Contains(message, "nonce is too low") {
			return true
		}
	}
	return false
}

// ConfirmerChain 定义 Confirmer 查询回执与最新区块高度所需的链能力。
type ConfirmerChain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// Confirmer 封装候选交易回执检索、确认数计算及替换阶梯计时辅助逻辑。
type Confirmer struct {
	chain ConfirmerChain
}

// NewConfirmer 创建 EVM 交易确认器。
func NewConfirmer(chain ConfirmerChain) Confirmer {
	return Confirmer{chain: chain}
}

// CollectCandidateHashes 按尝试列表倒序及当前主记录哈希去重生成候选交易哈希列表。
func CollectCandidateHashes[T any](attempts []T, getHash func(T) string, primaryHash string) ([]string, error) {
	seen := make(map[string]struct{}, len(attempts)+1)
	candidates := make([]string, 0, len(attempts)+1)
	for _, attempt := range attempts {
		hash := strings.ToLower(strings.TrimSpace(getHash(attempt)))
		if hash == "" {
			continue
		}
		if _, ok := seen[hash]; !ok {
			seen[hash] = struct{}{}
			candidates = append(candidates, hash)
		}
	}
	if trimmed := strings.ToLower(strings.TrimSpace(primaryHash)); trimmed != "" {
		if _, ok := seen[trimmed]; !ok {
			candidates = append(candidates, trimmed)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("待确认交易缺少候选哈希")
	}
	return candidates, nil
}

// FindFirstReceipt 按候选哈希顺序查找第一个已上链的交易 Receipt。
func (c Confirmer) FindFirstReceipt(ctx context.Context, candidates []string) (*types.Receipt, string, error) {
	for _, candidateHash := range candidates {
		receipt, err := c.chain.TransactionReceipt(ctx, common.HexToHash(candidateHash))
		if errors.Is(err, ethereum.NotFound) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("查询交易 Receipt 失败：%w", err)
		}
		if receipt != nil {
			return receipt, candidateHash, nil
		}
	}
	return nil, "", nil
}

// EvaluateConfirmations 校验 Receipt 字段完整性并结合单轮区块高度缓存计算确认数。
func (c Confirmer) EvaluateConfirmations(ctx context.Context, receipt *types.Receipt) (blockNumber int64, confirmations int64, err error) {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.EffectiveGasPrice == nil {
		return 0, 0, errors.New("交易 Receipt 内容无效")
	}
	latest, err := CachedBlockNumber(ctx, c.chain.BlockNumber)
	if err != nil {
		return 0, 0, fmt.Errorf("查询确认最新区块高度失败：%w", err)
	}
	rawBlock := receipt.BlockNumber.Uint64()
	if rawBlock > latest || rawBlock > math.MaxInt64 {
		return 0, 0, errors.New("交易 Receipt 区块高度无效")
	}
	rawConfs := latest - rawBlock + 1
	if rawConfs > math.MaxInt64 {
		return 0, 0, errors.New("交易确认数超出数据库范围")
	}
	return int64(rawBlock), int64(rawConfs), nil
}

// ReplacementTierStart 返回当前加价阶梯的起始时间，用于判断是否到达下一次加价时机。
func ReplacementTierStart(bumpCount int32, broadcastedAt, lastBroadcastAt, latestAttemptAt *time.Time) *time.Time {
	if bumpCount > 0 {
		if latestAttemptAt != nil && !latestAttemptAt.IsZero() {
			t := *latestAttemptAt
			return &t
		}
		return lastBroadcastAt
	}
	if broadcastedAt != nil {
		return broadcastedAt
	}
	return lastBroadcastAt
}

// SigningFailureTracker 并发安全地统计分配 Nonce 后的连续签名失败次数并在达阈值时暂停自动重试。
type SigningFailureTracker struct {
	mu       sync.Mutex
	maxCount int
	failures map[int64]int
	paused   map[int64]bool
}

// NewSigningFailureTracker 创建签名失败熔断跟踪器。
func NewSigningFailureTracker(maxCount int) *SigningFailureTracker {
	if maxCount <= 0 {
		maxCount = DefaultMaxSigningRetryCount
	}
	return &SigningFailureTracker{
		maxCount: maxCount,
		failures: make(map[int64]int),
		paused:   make(map[int64]bool),
	}
}

// IsPaused 判断指定任务 ID 是否已经因连续签名失败而暂停自动重试。
func (s *SigningFailureTracker) IsPaused(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused[id]
}

// Clear 在签名完成后清除指定任务 ID 的失败次数与暂停标记。
func (s *SigningFailureTracker) Clear(id int64) {
	s.mu.Lock()
	delete(s.failures, id)
	delete(s.paused, id)
	s.mu.Unlock()
}

// RecordFailure 累加签名失败次数；若本次首次达到暂停阈值则调用 onPause 一次以写入告警。
func (s *SigningFailureTracker) RecordFailure(id int64, onPause func()) {
	s.mu.Lock()
	s.failures[id]++
	count := s.failures[id]
	alreadyPaused := s.paused[id]
	shouldTrigger := count >= s.maxCount && !alreadyPaused
	if count >= s.maxCount {
		s.paused[id] = true
	}
	s.mu.Unlock()
	if shouldTrigger && onPause != nil {
		onPause()
	}
}

// RunPollLoop 按固定间隔运行单轮任务函数直到上下文取消，单轮异常交由 onError 处理而不中断循环。
func RunPollLoop(ctx context.Context, interval time.Duration, step func(context.Context) error, onError func(error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := step(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if onError != nil {
				onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// MaxCoveredSweepBumps 计算给定补气安全余量基点与单次加价比例下可安全覆盖的最大归集加价次数。
func MaxCoveredSweepBumps(gasSafetyBPS, bumpPercent uint64) int {
	if gasSafetyBPS == 0 || bumpPercent == 0 {
		return 0
	}
	limitNum := new(big.Int).SetUint64(10_000 + gasSafetyBPS)
	stepNum := new(big.Int).SetUint64(100 + bumpPercent)
	stepDen := big.NewInt(100)
	curNum := big.NewInt(1)
	curDen := big.NewInt(1)
	bpsScale := big.NewInt(10_000)

	count := 0
	for i := 0; i < 32; i++ {
		nextNum := new(big.Int).Mul(curNum, stepNum)
		nextDen := new(big.Int).Mul(curDen, stepDen)
		lhs := new(big.Int).Mul(nextNum, bpsScale)
		rhs := new(big.Int).Mul(limitNum, nextDen)
		if lhs.Cmp(rhs) > 0 {
			break
		}
		curNum = nextNum
		curDen = nextDen
		count++
	}
	return count
}

// BumpBlockedAction 定义 EVM 加价替换因余额或费率上限受阻后的统一处置策略。
type BumpBlockedAction struct {
	RecordBlocked     func(ctx context.Context, code, message string) (time.Time, error)
	MarkManualReview  func(ctx context.Context, code, message string) error
	Rebroadcast       func(ctx context.Context) error
	LastBroadcastAt   *time.Time
	RebroadcastAfter  time.Duration
	ManualReviewAfter time.Duration
	ReasonCode        string
	ReasonMessage     string
}

// HandleBumpBlocked 统一持久化首次阻塞原因并去重告警，持续受阻超阈值时转人工核查，否则按重播间隔退回重播原交易。
func HandleBumpBlocked(ctx context.Context, action BumpBlockedAction) error {
	var blockedAt time.Time
	if action.RecordBlocked != nil {
		recordedAt, err := action.RecordBlocked(ctx, action.ReasonCode, action.ReasonMessage)
		if err != nil {
			return err
		}
		blockedAt = recordedAt
	}
	if action.ManualReviewAfter > 0 && !blockedAt.IsZero() && time.Since(blockedAt) >= action.ManualReviewAfter && action.MarkManualReview != nil {
		msg := fmt.Sprintf("Gas 加价替换连续受阻超过 %s（原因：%s），需人工核查", action.ManualReviewAfter, action.ReasonCode)
		return action.MarkManualReview(ctx, "EVM_BUMP_BLOCKED_MANUAL_REVIEW", msg)
	}
	if action.Rebroadcast != nil && action.LastBroadcastAt != nil && time.Since(*action.LastBroadcastAt) >= action.RebroadcastAfter {
		return action.Rebroadcast(ctx)
	}
	return nil
}
