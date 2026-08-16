package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
)

const SepoliaChainID int64 = 11155111

type FailureClass string

const (
	FailureTimeout            FailureClass = "timeout"
	FailureRateLimited        FailureClass = "rate_limited"
	FailureBadGateway         FailureClass = "bad_gateway"
	FailureServiceUnavailable FailureClass = "service_unavailable"
	FailureNetwork            FailureClass = "network"
	FailureRPC                FailureClass = "rpc"
	FailureUnknown            FailureClass = "unknown"
)

var ErrWrongChain = errors.New("RPC 返回的 Chain ID 不是 Sepolia")

// RPCError 表示经过脱敏的 RPC 端点错误，不会输出完整 URL。
type RPCError struct {
	Alias string
	Class FailureClass
	Err   error
}

// Error 返回不包含 RPC 地址和密钥的中文错误信息。
func (e *RPCError) Error() string {
	return fmt.Sprintf("RPC 端点 %s 请求失败（%s）", e.Alias, failureText(e.Class))
}

// Unwrap 保留底层错误供 errors.Is 和 errors.As 使用。
func (e *RPCError) Unwrap() error { return e.Err }

// WrongChainError 表示端点返回了错误的网络 Chain ID。
type WrongChainError struct {
	Alias   string
	ChainID *big.Int
}

// Error 返回错误网络的安全描述。
func (e *WrongChainError) Error() string {
	return fmt.Sprintf("RPC 端点 %s 返回错误 Chain ID（%s）", e.Alias, e.ChainID.String())
}

// Unwrap 将错误归类为错误网络。
func (e *WrongChainError) Unwrap() error { return ErrWrongChain }

// EVMClient 定义业务层需要的窄化 EVM RPC 接口。
type EVMClient interface {
	// ChainID 查询当前网络 Chain ID。
	ChainID(ctx context.Context) (*big.Int, error)
	// BlockNumber 查询最新区块高度。
	BlockNumber(ctx context.Context) (uint64, error)
	// BlockByNumber 查询指定高度的完整区块。
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
	// HeaderByNumber 查询指定高度的区块头。
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	// BalanceAt 查询地址 ETH 余额。
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	// PendingNonceAt 查询地址待处理 Nonce。
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	// SuggestGasTipCap 查询建议优先费。
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	// EstimateGas 估算交易 Gas。
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	// SendTransaction 广播已签名交易。
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	// TransactionReceipt 查询交易执行回执。
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// RetryPolicy 定义 RPC 重试次数、退避和随机抖动策略。
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	JitterRatio float64
	Sleep       func(context.Context, time.Duration) error
	Random      func() float64
}

// Config 定义 Sepolia 主备端点和 RPC 客户端策略。
type Config struct {
	PrimaryURL  string
	FallbackURL string
	Timeout     time.Duration
	Retry       RetryPolicy
}

type endpoint struct {
	alias     string
	client    *ethclient.Client
	validated bool
}

// Client 是支持主备端点和自动重试的 Sepolia RPC 客户端。
type Client struct {
	mu            sync.RWMutex
	endpoints     []endpoint
	active        int
	timeout       time.Duration
	retry         RetryPolicy
	chainID       string
	networkHeight uint64
	scanHeight    uint64
	lastError     string
	checkedAt     time.Time
}

// HealthSnapshot 描述 RPC 连接、网络高度和扫描高度的当前状态。
type HealthSnapshot struct {
	Status        string    `json:"status"`
	Endpoint      string    `json:"endpoint"`
	ChainID       string    `json:"chain_id"`
	NetworkHeight uint64    `json:"network_height"`
	ScanHeight    uint64    `json:"scan_height"`
	LastError     string    `json:"last_error,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// New 根据配置连接并校验至少一个 Sepolia RPC 端点。
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		return nil, errors.New("RPC 超时时间必须大于零")
	}
	cfg.Retry = normalizeRetryPolicy(cfg.Retry)
	urls := []struct{ alias, value string }{{"primary", cfg.PrimaryURL}}
	if cfg.FallbackURL != "" && cfg.FallbackURL != cfg.PrimaryURL {
		urls = append(urls, struct{ alias, value string }{"fallback", cfg.FallbackURL})
	}
	if urls[0].value == "" {
		return nil, errors.New("必须配置主 RPC 地址")
	}

	client := &Client{timeout: cfg.Timeout, retry: cfg.Retry, active: -1}
	for _, item := range urls {
		endpoint, err := newEndpoint(item.alias, item.value, cfg.Timeout)
		if err != nil {
			for _, existing := range client.endpoints {
				existing.client.Close()
			}
			return nil, err
		}
		client.endpoints = append(client.endpoints, endpoint)
	}
	var lastErr error
	for index := range client.endpoints {
		if err := client.validateEndpoint(ctx, index); err != nil {
			lastErr = err
			continue
		}
		client.mu.Lock()
		client.active = index
		client.mu.Unlock()
		return client, nil
	}
	for _, endpoint := range client.endpoints {
		endpoint.client.Close()
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的 Sepolia RPC 端点")
	}
	return nil, fmt.Errorf("初始化 Sepolia RPC 失败：%w", lastErr)
}

// NewFromConfig 将应用配置转换为 Sepolia RPC 客户端配置。
func NewFromConfig(ctx context.Context, cfg config.Config) (*Client, error) {
	return New(ctx, Config{
		PrimaryURL:  cfg.SepoliaRPCURL,
		FallbackURL: cfg.SepoliaRPCFallbackURL,
		Timeout:     cfg.SepoliaRPCTimeout,
		Retry: RetryPolicy{
			MaxAttempts: cfg.SepoliaRPCMaxAttempts,
			BaseDelay:   cfg.SepoliaRPCBaseDelay,
			MaxDelay:    cfg.SepoliaRPCMaxDelay,
			JitterRatio: 0.2,
		},
	})
}

// Close 关闭所有底层 RPC 连接。
func (c *Client) Close() {
	if c == nil {
		return
	}
	for index := range c.endpoints {
		c.endpoints[index].client.Close()
	}
}

// ChainID 查询当前 RPC 网络 Chain ID。
func (c *Client) ChainID(ctx context.Context) (*big.Int, error) {
	var result *big.Int
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.ChainID(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.chainID = result.String()
	c.mu.Unlock()
	return result, nil
}

// BlockNumber 查询最新区块高度。
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	var result uint64
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.BlockNumber(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.networkHeight = result
	c.mu.Unlock()
	return result, nil
}

// BlockByNumber 查询指定高度的完整区块和交易。
func (c *Client) BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error) {
	var result *types.Block
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.BlockByNumber(ctx, number)
		return err
	})
	return result, err
}

// HeaderByNumber 查询指定高度的区块头。
func (c *Client) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	var result *types.Header
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.HeaderByNumber(ctx, number)
		return err
	})
	return result, err
}

// BalanceAt 查询地址在指定区块的 ETH 余额。
func (c *Client) BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error) {
	var result *big.Int
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.BalanceAt(ctx, account, number)
		return err
	})
	return result, err
}

// PendingNonceAt 查询地址的待处理 Nonce。
func (c *Client) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	var result uint64
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.PendingNonceAt(ctx, account)
		return err
	})
	return result, err
}

// SuggestGasTipCap 查询节点建议的 EIP-1559 优先费。
func (c *Client) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	var result *big.Int
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.SuggestGasTipCap(ctx)
		return err
	})
	return result, err
}

// EstimateGas 估算交易调用所需的 Gas。
func (c *Client) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	var result uint64
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.EstimateGas(ctx, call)
		return err
	})
	return result, err
}

// SendTransaction 广播已签名交易。
func (c *Client) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	return c.invoke(ctx, func(client *ethclient.Client) error {
		return client.SendTransaction(ctx, tx)
	})
}

// TransactionReceipt 查询交易 Receipt，不存在时保留 ethereum.NotFound 语义。
func (c *Client) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	var result *types.Receipt
	err := c.invoke(ctx, func(client *ethclient.Client) error {
		var err error
		result, err = client.TransactionReceipt(ctx, txHash)
		return err
	})
	return result, err
}

// SetScanHeight 更新扫描器最近完成的区块高度。
func (c *Client) SetScanHeight(height uint64) {
	c.mu.Lock()
	c.scanHeight = height
	c.checkedAt = time.Now()
	c.mu.Unlock()
}

// Health 查询 RPC 并返回链健康状态快照。
func (c *Client) Health(ctx context.Context) HealthSnapshot {
	_, chainErr := c.ChainID(ctx)
	_, blockErr := c.BlockNumber(ctx)
	c.mu.RLock()
	snapshot := c.snapshotLocked()
	c.mu.RUnlock()
	if chainErr != nil || blockErr != nil {
		snapshot.Status = "DOWN"
	}
	return snapshot
}

// Snapshot 返回不发起网络请求的当前健康状态快照。
func (c *Client) Snapshot() HealthSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked()
}

// ActiveEndpoint 返回当前活跃端点的匿名别名。
func (c *Client) ActiveEndpoint() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.active < 0 || c.active >= len(c.endpoints) {
		return "unknown"
	}
	return c.endpoints[c.active].alias
}

// invoke 对单个 EVM RPC 操作执行重试和主备切换。
func (c *Client) invoke(ctx context.Context, call func(*ethclient.Client) error) error {
	if c == nil || len(c.endpoints) == 0 {
		return errors.New("没有可用的 RPC 端点")
	}
	order := c.endpointOrder()
	var lastErr error
	for _, index := range order {
		if err := c.validateEndpoint(ctx, index); err != nil {
			lastErr = err
			continue
		}
		for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
			err := call(c.endpoints[index].client)
			if err == nil {
				c.markSuccess(index)
				return nil
			}
			lastErr = c.wrapError(index, err)
			c.markFailure(lastErr)
			if !retryable(err) {
				return lastErr
			}
			if attempt == c.retry.MaxAttempts {
				break
			}
			if err := c.retry.Sleep(ctx, c.backoff(attempt)); err != nil {
				return fmt.Errorf("等待 RPC 重试失败：%w", err)
			}
		}
	}
	if lastErr == nil {
		return errors.New("所有 RPC 端点均不可用")
	}
	return lastErr
}

// endpointOrder 返回从当前端点开始的主备访问顺序。
func (c *Client) endpointOrder() []int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	order := make([]int, 0, len(c.endpoints))
	if c.active >= 0 && c.active < len(c.endpoints) {
		order = append(order, c.active)
	}
	for index := range c.endpoints {
		if index != c.active {
			order = append(order, index)
		}
	}
	return order
}

// validateEndpoint 在使用端点前重新校验 Chain ID。
func (c *Client) validateEndpoint(ctx context.Context, index int) error {
	c.mu.RLock()
	endpoint := c.endpoints[index]
	isActive := index == c.active
	c.mu.RUnlock()
	if endpoint.validated && isActive {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	chainID, err := endpoint.client.ChainID(probeCtx)
	if err != nil {
		return c.wrapError(index, err)
	}
	if chainID.Cmp(big.NewInt(SepoliaChainID)) != 0 {
		return &WrongChainError{Alias: endpoint.alias, ChainID: chainID}
	}
	c.mu.Lock()
	c.endpoints[index].validated = true
	c.chainID = chainID.String()
	c.mu.Unlock()
	return nil
}

// newEndpoint 创建带超时 HTTP 客户端的端点连接。
func newEndpoint(alias, rawURL string, timeout time.Duration) (endpoint, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return endpoint{}, fmt.Errorf("%s RPC 地址无效", alias)
	}
	client, err := rpc.DialHTTPWithClient(rawURL, &http.Client{Timeout: timeout})
	if err != nil {
		return endpoint{}, fmt.Errorf("连接 %s RPC 端点失败：%w", alias, err)
	}
	return endpoint{alias: alias, client: ethclient.NewClient(client)}, nil
}

// normalizeRetryPolicy 填充重试策略默认值并限制抖动范围。
func normalizeRetryPolicy(policy RetryPolicy) RetryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 3
	}
	if policy.BaseDelay < 0 {
		policy.BaseDelay = 0
	}
	if policy.MaxDelay < policy.BaseDelay {
		policy.MaxDelay = policy.BaseDelay
	}
	if policy.JitterRatio < 0 {
		policy.JitterRatio = 0
	}
	if policy.JitterRatio > 1 {
		policy.JitterRatio = 1
	}
	if policy.Sleep == nil {
		policy.Sleep = sleepContext
	}
	if policy.Random == nil {
		policy.Random = rand.Float64
	}
	return policy
}

// backoff 计算当前重试次数对应的指数退避时间和随机抖动。
func (c *Client) backoff(attempt int) time.Duration {
	delay := c.retry.BaseDelay
	for i := 1; i < attempt && delay < c.retry.MaxDelay; i++ {
		delay *= 2
		if delay > c.retry.MaxDelay {
			delay = c.retry.MaxDelay
		}
	}
	if c.retry.JitterRatio == 0 || delay == 0 {
		return delay
	}
	factor := 1 + (c.retry.Random()*2-1)*c.retry.JitterRatio
	return time.Duration(float64(delay) * factor)
}

// markSuccess 记录成功端点并清理最近一次错误。
func (c *Client) markSuccess(index int) {
	c.mu.Lock()
	c.active = index
	c.lastError = ""
	c.checkedAt = time.Now()
	c.mu.Unlock()
}

// markFailure 记录经过脱敏的最近一次 RPC 错误。
func (c *Client) markFailure(err error) {
	c.mu.Lock()
	c.lastError = err.Error()
	c.checkedAt = time.Now()
	c.mu.Unlock()
}

// wrapError 将底层 RPC 错误包装为不泄露 URL 的错误。
func (c *Client) wrapError(index int, err error) error {
	if wrong, ok := err.(*WrongChainError); ok {
		return wrong
	}
	return &RPCError{Alias: c.endpoints[index].alias, Class: classify(err), Err: err}
}

// snapshotLocked 将当前状态转换为健康快照。
func (c *Client) snapshotLocked() HealthSnapshot {
	status := "DEGRADED"
	if c.lastError == "" {
		status = "HEALTHY"
	}
	endpoint := "unknown"
	if c.active >= 0 && c.active < len(c.endpoints) {
		endpoint = c.endpoints[c.active].alias
	}
	return HealthSnapshot{
		Status:        status,
		Endpoint:      endpoint,
		ChainID:       c.chainID,
		NetworkHeight: c.networkHeight,
		ScanHeight:    c.scanHeight,
		LastError:     c.lastError,
		CheckedAt:     c.checkedAt,
	}
}

// retryable 判断错误是否属于可安全重试的临时故障。
func retryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode == http.StatusBadGateway ||
			httpErr.StatusCode == http.StatusServiceUnavailable || httpErr.StatusCode == http.StatusGatewayTimeout
	}
	var pointerHTTPError *rpc.HTTPError
	if errors.As(err, &pointerHTTPError) && pointerHTTPError != nil {
		return pointerHTTPError.StatusCode == http.StatusTooManyRequests || pointerHTTPError.StatusCode == http.StatusBadGateway ||
			pointerHTTPError.StatusCode == http.StatusServiceUnavailable || pointerHTTPError.StatusCode == http.StatusGatewayTimeout
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.ErrorCode() == -32002
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// classify 将底层故障归类为不包含敏感信息的类型。
func classify(err error) FailureClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	var httpErr rpc.HTTPError
	if errors.As(err, &httpErr) {
		return classifyHTTPStatus(httpErr.StatusCode)
	}
	var pointerHTTPError *rpc.HTTPError
	if errors.As(err, &pointerHTTPError) && pointerHTTPError != nil {
		return classifyHTTPStatus(pointerHTTPError.StatusCode)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return FailureNetwork
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		return FailureRPC
	}
	return FailureUnknown
}

// classifyHTTPStatus 将 HTTP 状态码映射为 RPC 故障类型。
func classifyHTTPStatus(status int) FailureClass {
	switch status {
	case http.StatusTooManyRequests:
		return FailureRateLimited
	case http.StatusBadGateway:
		return FailureBadGateway
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return FailureServiceUnavailable
	default:
		return FailureUnknown
	}
}

// failureText 将内部故障类型转换为中文描述。
func failureText(class FailureClass) string {
	switch class {
	case FailureTimeout:
		return "请求超时"
	case FailureRateLimited:
		return "请求被限流"
	case FailureBadGateway:
		return "网关错误"
	case FailureServiceUnavailable:
		return "服务暂不可用"
	case FailureNetwork:
		return "网络错误"
	case FailureRPC:
		return "JSON-RPC 错误"
	default:
		return "未知错误"
	}
}

// sleepContext 按上下文可取消地等待退避时间。
func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
