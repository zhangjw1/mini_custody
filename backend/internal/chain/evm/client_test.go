package evm

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const mockChainID = "0xaa36a7"

// TestClientQueriesSupportedMethods 验证客户端可以调用 Phase 3 所需的基础查询。
func TestClientQueriesSupportedMethods(t *testing.T) {
	mock := newMockRPC(t)
	client := newTestClient(t, mock.URL())
	defer client.Close()

	ctx := context.Background()
	chainID, err := client.ChainID(ctx)
	if err != nil || chainID.Cmp(big.NewInt(SepoliaChainID)) != 0 {
		t.Fatalf("ChainID() = %v, error = %v", chainID, err)
	}
	if height, err := client.BlockNumber(ctx); err != nil || height != 42 {
		t.Fatalf("BlockNumber() = %d, error = %v", height, err)
	}
	balance, err := client.BalanceAt(ctx, common.Address{}, nil)
	if err != nil || balance.Cmp(big.NewInt(1_000_000_000_000_000_000)) != 0 {
		t.Fatalf("BalanceAt() = %v, error = %v", balance, err)
	}
	nonce, err := client.PendingNonceAt(ctx, common.Address{})
	if err != nil || nonce != 7 {
		t.Fatalf("PendingNonceAt() = %d, error = %v", nonce, err)
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil || tip.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Fatalf("SuggestGasTipCap() = %v, error = %v", tip, err)
	}
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: common.Address{}})
	if err != nil || gas != 21_000 {
		t.Fatalf("EstimateGas() = %d, error = %v", gas, err)
	}
	block, err := client.BlockByNumber(ctx, big.NewInt(42))
	if err != nil || block.NumberU64() != 42 {
		t.Fatalf("BlockByNumber() number = %v, error = %v", blockNumber(block), err)
	}
	header, err := client.HeaderByNumber(ctx, big.NewInt(42))
	if err != nil || header.Number.Uint64() != 42 {
		t.Fatalf("HeaderByNumber() number = %v, error = %v", headerNumber(header), err)
	}
	receipt, err := client.TransactionReceipt(ctx, common.Hash{})
	if err != nil || receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("TransactionReceipt() status = %d, error = %v", receiptStatus(receipt), err)
	}
}

// TestClientRetriesRateLimit 验证 HTTP 429 会按策略重试并最终成功。
func TestClientRetriesRateLimit(t *testing.T) {
	mock := newMockRPC(t)
	mock.failStatus("eth_blockNumber", http.StatusTooManyRequests, 1)
	client := newTestClientWithPolicy(t, mock.URL(), "", RetryPolicy{MaxAttempts: 3})
	defer client.Close()

	height, err := client.BlockNumber(context.Background())
	if err != nil || height != 42 {
		t.Fatalf("BlockNumber() = %d, error = %v", height, err)
	}
	if got := mock.calls("eth_blockNumber"); got != 2 {
		t.Fatalf("eth_blockNumber calls = %d, want 2", got)
	}
}

// TestClientSwitchesToFallback 验证主端点持续 503 时会切换并重新校验备用端点。
func TestClientSwitchesToFallback(t *testing.T) {
	primary := newMockRPC(t)
	fallback := newMockRPC(t)
	primary.failStatus("eth_blockNumber", http.StatusServiceUnavailable, -1)
	fallback.blockNumber = "0x2b"
	client := newTestClientWithPolicy(t, primary.URL(), fallback.URL(), RetryPolicy{MaxAttempts: 2})
	defer client.Close()

	height, err := client.BlockNumber(context.Background())
	if err != nil || height != 43 {
		t.Fatalf("BlockNumber() = %d, error = %v", height, err)
	}
	if client.ActiveEndpoint() != "fallback" {
		t.Fatalf("ActiveEndpoint() = %q, want fallback", client.ActiveEndpoint())
	}
	if got := fallback.calls("eth_chainId"); got != 1 {
		t.Fatalf("fallback Chain ID validation calls = %d, want 1", got)
	}
}

// TestClientRejectsWrongChain 验证启动时错误网络不会被接受。
func TestClientRejectsWrongChain(t *testing.T) {
	mock := newMockRPC(t)
	mock.chainID = "0x1"
	_, err := New(context.Background(), testConfig(mock.URL(), ""))
	if err == nil || !errors.Is(err, ErrWrongChain) {
		t.Fatalf("New() error = %v, want ErrWrongChain", err)
	}
}

// TestClientDoesNotRetryNonTemporaryHTTPError 验证非临时 HTTP 错误不会重试。
func TestClientDoesNotRetryNonTemporaryHTTPError(t *testing.T) {
	mock := newMockRPC(t)
	mock.failStatus("eth_blockNumber", http.StatusBadRequest, -1)
	client := newTestClientWithPolicy(t, mock.URL()+"?api-key=secret-token", "", RetryPolicy{MaxAttempts: 3})
	defer client.Close()

	_, err := client.BlockNumber(context.Background())
	if err == nil || !strings.Contains(err.Error(), "请求失败") {
		t.Fatalf("BlockNumber() error = %v, want sanitized RPC error", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("RPC error leaked endpoint URL: %v", err)
	}
	if got := mock.calls("eth_blockNumber"); got != 1 {
		t.Fatalf("eth_blockNumber calls = %d, want 1", got)
	}
}

// TestClientRetriesTimeout 验证 HTTP 请求超时会按配置次数重试。
func TestClientRetriesTimeout(t *testing.T) {
	mock := newMockRPC(t)
	mock.delay("eth_blockNumber", 100*time.Millisecond)
	client, err := New(context.Background(), Config{
		PrimaryURL: mock.URL(),
		Timeout:    20 * time.Millisecond,
		Retry:      RetryPolicy{MaxAttempts: 2},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()

	_, err = client.BlockNumber(context.Background())
	if err == nil {
		t.Fatal("BlockNumber() error = nil, want timeout")
	}
	if got := mock.calls("eth_blockNumber"); got != 2 {
		t.Fatalf("eth_blockNumber calls = %d, want 2", got)
	}
}

// TestClientHealthSnapshot 验证健康快照包含端点、Chain ID、高度和扫描高度。
func TestClientHealthSnapshot(t *testing.T) {
	mock := newMockRPC(t)
	client := newTestClient(t, mock.URL())
	defer client.Close()
	client.SetScanHeight(40)

	snapshot := client.Health(context.Background())
	if snapshot.Status != "HEALTHY" || snapshot.Endpoint != "primary" || snapshot.ChainID != "11155111" ||
		snapshot.NetworkHeight != 42 || snapshot.ScanHeight != 40 || snapshot.LastError != "" {
		t.Fatalf("Health() = %+v", snapshot)
	}
}

// testConfig 构造无等待的 RPC 测试配置。
func testConfig(primary, fallback string) Config {
	return Config{PrimaryURL: primary, FallbackURL: fallback, Timeout: time.Second, Retry: RetryPolicy{MaxAttempts: 2}}
}

// newTestClient 创建默认 RPC 测试客户端。
func newTestClient(t *testing.T, primary string) *Client {
	return newTestClientWithPolicy(t, primary, "", RetryPolicy{MaxAttempts: 2})
}

// newTestClientWithPolicy 创建可注入重试策略的 RPC 测试客户端。
func newTestClientWithPolicy(t *testing.T, primary, fallback string, policy RetryPolicy) *Client {
	t.Helper()
	policy.BaseDelay = 0
	policy.MaxDelay = 0
	policy.JitterRatio = 0
	client, err := New(context.Background(), Config{PrimaryURL: primary, FallbackURL: fallback, Timeout: time.Second, Retry: policy})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

// blockNumber 返回区块高度，避免测试失败时解引用空指针。
func blockNumber(block *types.Block) any {
	if block == nil {
		return nil
	}
	return block.NumberU64()
}

// headerNumber 返回区块头高度，避免测试失败时解引用空指针。
func headerNumber(header *types.Header) any {
	if header == nil || header.Number == nil {
		return nil
	}
	return header.Number.Uint64()
}

// receiptStatus 返回 Receipt 状态，避免测试失败时解引用空指针。
func receiptStatus(receipt *types.Receipt) uint64 {
	if receipt == nil {
		return 0
	}
	return receipt.Status
}

type mockRPC struct {
	server      *httptest.Server
	mu          sync.Mutex
	counts      map[string]int
	statusFails map[string]mockFailure
	delays      map[string]time.Duration
	chainID     string
	blockNumber string
}

type mockFailure struct {
	status int
	left   int
}

// newMockRPC 创建响应固定的 JSON-RPC 测试服务器。
func newMockRPC(t *testing.T) *mockRPC {
	mock := &mockRPC{
		counts:      make(map[string]int),
		statusFails: make(map[string]mockFailure),
		delays:      make(map[string]time.Duration),
		chainID:     mockChainID,
		blockNumber: "0x2a",
	}
	mock.server = httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(mock.server.Close)
	return mock
}

// delay 配置指定方法的响应延迟。
func (m *mockRPC) delay(method string, duration time.Duration) {
	m.mu.Lock()
	m.delays[method] = duration
	m.mu.Unlock()
}

// URL 返回 Mock RPC 测试服务器地址。
func (m *mockRPC) URL() string { return m.server.URL }

// failStatus 配置指定方法返回固定次数或无限次的 HTTP 错误。
func (m *mockRPC) failStatus(method string, status, times int) {
	m.mu.Lock()
	m.statusFails[method] = mockFailure{status: status, left: times}
	m.mu.Unlock()
}

// calls 返回指定 JSON-RPC 方法的调用次数。
func (m *mockRPC) calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[method]
}

// handler 处理 Mock JSON-RPC 请求并返回测试数据。
func (m *mockRPC) handler(w http.ResponseWriter, request *http.Request) {
	var payload struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.counts[payload.Method]++
	failure, shouldFail := m.statusFails[payload.Method]
	shouldFail = shouldFail && (failure.left == -1 || failure.left > 0)
	if shouldFail && failure.left > 0 {
		failure.left--
		m.statusFails[payload.Method] = failure
	}
	if shouldFail {
		m.mu.Unlock()
		w.WriteHeader(failure.status)
		return
	}
	chainID := m.chainID
	blockNumberValue := m.blockNumber
	delay := m.delays[payload.Method]
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	result := mockResult(payload.Method, chainID, blockNumberValue)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": payload.ID, "result": result})
}

// mockResult 返回各个基础 EVM RPC 方法的最小有效响应。
func mockResult(method, chainID, blockNumberValue string) any {
	switch method {
	case "eth_chainId":
		return chainID
	case "eth_blockNumber":
		return blockNumberValue
	case "eth_getBalance":
		return "0xde0b6b3a7640000"
	case "eth_getTransactionCount":
		return "0x7"
	case "eth_maxPriorityFeePerGas":
		return "0x3b9aca00"
	case "eth_estimateGas":
		return "0x5208"
	case "eth_getBlockByNumber":
		block := types.NewBlockWithHeader(&types.Header{
			Number: big.NewInt(42), GasLimit: 30_000_000, BaseFee: big.NewInt(1),
			UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash,
		})
		encoded, _ := json.Marshal(block.Header())
		var result map[string]any
		_ = json.Unmarshal(encoded, &result)
		result["transactions"] = []any{}
		result["uncles"] = []any{}
		return result
	case "eth_getTransactionReceipt":
		receipt := &types.Receipt{Status: types.ReceiptStatusSuccessful, CumulativeGasUsed: 21_000, GasUsed: 21_000, EffectiveGasPrice: big.NewInt(1_000_000_000), Logs: []*types.Log{}}
		encoded, _ := json.Marshal(receipt)
		var result any
		_ = json.Unmarshal(encoded, &result)
		return result
	default:
		return nil
	}
}
