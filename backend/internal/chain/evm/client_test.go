package evm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	code, err := client.CodeAt(ctx, common.HexToAddress("0x1111111111111111111111111111111111111111"), nil)
	if err != nil || len(code) != 2 {
		t.Fatalf("CodeAt() = %x, error = %v", code, err)
	}
	callResult, err := client.CallContract(ctx, ethereum.CallMsg{To: new(common.Address)}, nil)
	if err != nil || len(callResult) != 2 {
		t.Fatalf("CallContract() = %x, error = %v", callResult, err)
	}
	logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(2)})
	if err != nil || len(logs) != 0 {
		t.Fatalf("FilterLogs() count = %d, error = %v", len(logs), err)
	}
}

// TestClientSnapshotContainsChainIDAfterInitialization 验证启动校验会立即写入健康快照 Chain ID。
func TestClientSnapshotContainsChainIDAfterInitialization(t *testing.T) {
	mock := newMockRPC(t)
	client := newTestClient(t, mock.URL())
	defer client.Close()
	if got := client.Snapshot().ChainID; got != "11155111" {
		t.Fatalf("Snapshot().ChainID = %q, want 11155111", got)
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

// TestClientFilterLogsRetriesRateLimit 验证日志查询遇到 HTTP 429 时会重试且不会返回伪造空结果。
func TestClientFilterLogsRetriesRateLimit(t *testing.T) {
	mock := newMockRPC(t)
	mock.failStatus("eth_getLogs", http.StatusTooManyRequests, 1)
	client := newTestClientWithPolicy(t, mock.URL(), "", RetryPolicy{MaxAttempts: 2})
	defer client.Close()

	logs, err := client.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(2)})
	if err != nil || len(logs) != 0 {
		t.Fatalf("FilterLogs() count = %d, error = %v", len(logs), err)
	}
	if got := mock.calls("eth_getLogs"); got != 2 {
		t.Fatalf("eth_getLogs calls = %d, want 2", got)
	}
}

// TestClientFilterLogsRetriesTimeout 验证日志查询超时会按策略重试且保留失败语义。
func TestClientFilterLogsRetriesTimeout(t *testing.T) {
	mock := newMockRPC(t)
	mock.delay("eth_getLogs", 180*time.Millisecond)
	client, err := New(context.Background(), Config{
		PrimaryURL: mock.URL(), Timeout: 60 * time.Millisecond, Retry: RetryPolicy{MaxAttempts: 2},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer client.Close()

	_, err = client.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(2)})
	if err == nil {
		t.Fatal("FilterLogs() error = nil, want timeout")
	}
	if got := mock.calls("eth_getLogs"); got != 2 {
		t.Fatalf("eth_getLogs calls = %d, want 2", got)
	}
}

// TestClientFilterLogsSwitchesToFallback 验证主端点日志查询持续失败时会切换备用端点。
func TestClientFilterLogsSwitchesToFallback(t *testing.T) {
	primary := newMockRPC(t)
	fallback := newMockRPC(t)
	primary.failStatus("eth_getLogs", http.StatusServiceUnavailable, -1)
	client := newTestClientWithPolicy(t, primary.URL(), fallback.URL(), RetryPolicy{MaxAttempts: 1})
	defer client.Close()

	_, err := client.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(2)})
	if err != nil || client.ActiveEndpoint() != "fallback" {
		t.Fatalf("FilterLogs() endpoint = %s, error = %v", client.ActiveEndpoint(), err)
	}
	if got := fallback.calls("eth_getLogs"); got != 1 {
		t.Fatalf("fallback eth_getLogs calls = %d, want 1", got)
	}
}

// TestClientClassifiesLogRangeTooLarge 验证日志范围超限可被扫描器识别且不会原样重试。
func TestClientClassifiesLogRangeTooLarge(t *testing.T) {
	mock := newMockRPC(t)
	mock.failRPC("eth_getLogs", -32005, "query returned more than 10000 results", -1)
	client := newTestClientWithPolicy(t, mock.URL(), "", RetryPolicy{MaxAttempts: 3})
	defer client.Close()

	_, err := client.FilterLogs(context.Background(), ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(20_000)})
	if err == nil || !IsLogRangeTooLarge(err) {
		t.Fatalf("FilterLogs() error = %v, want range error", err)
	}
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.Class != FailureLogRangeTooLarge {
		t.Fatalf("RPC error = %+v", rpcError)
	}
	if got := mock.calls("eth_getLogs"); got != 1 {
		t.Fatalf("eth_getLogs calls = %d, want 1", got)
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
	rpcFails    map[string]mockRPCFailure
	delays      map[string]time.Duration
	results     map[string]any
	chainID     string
	blockNumber string
}

type mockFailure struct {
	status int
	left   int
}

type mockRPCFailure struct {
	code    int
	message string
	left    int
}

// newMockRPC 创建响应固定的 JSON-RPC 测试服务器。
func newMockRPC(t *testing.T) *mockRPC {
	mock := &mockRPC{
		counts:      make(map[string]int),
		statusFails: make(map[string]mockFailure),
		rpcFails:    make(map[string]mockRPCFailure),
		delays:      make(map[string]time.Duration),
		results:     make(map[string]any),
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

// failRPC 配置指定方法返回固定次数或无限次的 JSON-RPC 错误。
func (m *mockRPC) failRPC(method string, code int, message string, times int) {
	m.mu.Lock()
	m.rpcFails[method] = mockRPCFailure{code: code, message: message, left: times}
	m.mu.Unlock()
}

// setResult 配置指定 JSON-RPC 方法的自定义成功结果。
func (m *mockRPC) setResult(method string, result any) {
	m.mu.Lock()
	m.results[method] = result
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
	rpcFailure, shouldFailRPC := m.rpcFails[payload.Method]
	shouldFailRPC = shouldFailRPC && (rpcFailure.left == -1 || rpcFailure.left > 0)
	if shouldFailRPC && rpcFailure.left > 0 {
		rpcFailure.left--
		m.rpcFails[payload.Method] = rpcFailure
	}
	if shouldFailRPC {
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": payload.ID,
			"error": map[string]any{"code": rpcFailure.code, "message": rpcFailure.message},
		})
		return
	}
	chainID := m.chainID
	blockNumberValue := m.blockNumber
	delay := m.delays[payload.Method]
	customResult, hasCustomResult := m.results[payload.Method]
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	result := mockResult(payload.Method, chainID, blockNumberValue)
	if hasCustomResult {
		result = customResult
	}
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
	case "eth_getCode":
		return "0x6000"
	case "eth_call":
		return "0x1234"
	case "eth_getLogs":
		return []any{}
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

// TestClientSwitchesEndpointOnConnectionRefused 验证主端点返回 connection refused 时自动切换备用端点。
func TestClientSwitchesEndpointOnConnectionRefused(t *testing.T) {
	primary := newMockRPC(t)
	fallback := newMockRPC(t)
	client := newTestClientWithPolicy(t, primary.URL(), fallback.URL(), RetryPolicy{MaxAttempts: 2})
	defer client.Close()

	if _, err := client.BlockNumber(context.Background()); err != nil {
		t.Fatalf("initial BlockNumber() error = %v", err)
	}
	primary.server.Close()

	height, err := client.BlockNumber(context.Background())
	if err != nil || height != 42 {
		t.Fatalf("BlockNumber() after primary down = %d, error = %v", height, err)
	}
	if got := client.ActiveEndpoint(); got != "fallback" {
		t.Fatalf("ActiveEndpoint() = %q, want fallback", got)
	}
}

// TestClientDoesNotDegradeOnNotFoundOrBusinessRevert 验证 NotFound 与业务 revert 不改变健康状态也不切换端点。
func TestClientDoesNotDegradeOnNotFoundOrBusinessRevert(t *testing.T) {
	primary := newMockRPC(t)
	fallback := newMockRPC(t)
	client := newTestClientWithPolicy(t, primary.URL(), fallback.URL(), RetryPolicy{MaxAttempts: 2})
	defer client.Close()

	primary.results["eth_getTransactionReceipt"] = nil
	_, err := client.TransactionReceipt(context.Background(), common.Hash{})
	if !errors.Is(err, ethereum.NotFound) {
		t.Fatalf("TransactionReceipt() err = %v, want ethereum.NotFound", err)
	}
	if snap := client.Snapshot(); snap.Status != "HEALTHY" || snap.LastError != "" {
		t.Fatalf("Snapshot after NotFound = %+v, want HEALTHY", snap)
	}
	if fallback.calls("eth_getTransactionReceipt") != 0 {
		t.Fatalf("fallback should not be called on NotFound, got %d", fallback.calls("eth_getTransactionReceipt"))
	}

	primary.failRPC("eth_estimateGas", -32000, "execution reverted", 1)
	if _, err := client.EstimateGas(context.Background(), ethereum.CallMsg{}); err == nil {
		t.Fatal("expected execution reverted error")
	}
	if snap := client.Snapshot(); snap.Status != "HEALTHY" || snap.LastError != "" {
		t.Fatalf("Snapshot after business revert = %+v, want HEALTHY", snap)
	}
	if fallback.calls("eth_estimateGas") != 0 {
		t.Fatalf("fallback should not be called on business revert, got %d", fallback.calls("eth_estimateGas"))
	}
}

// TestClientBatchBlocksAndHeaders 验证 BlocksByNumbers 与 HeadersByNumbers 在单次 JSON-RPC Batch 中批量获取区块与区块头。
func TestClientBatchBlocksAndHeaders(t *testing.T) {
	var batchPosts int
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	sampleTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(SepoliaChainID),
		Nonce:     1,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       21_000,
		To:        &target,
		Value:     big.NewInt(500),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		trimmed := strings.TrimSpace(string(body))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(trimmed, "[") {
			batchPosts++
			var reqs []struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params []any           `json:"params"`
			}
			_ = json.Unmarshal([]byte(trimmed), &reqs)
			resps := make([]map[string]any, len(reqs))
			for i, req := range reqs {
				height := big.NewInt(int64(100 + i))
				header := &types.Header{
					Number: height, Difficulty: big.NewInt(0), GasLimit: 30_000_000, BaseFee: big.NewInt(1),
					UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash,
				}
				encodedHeader, _ := json.Marshal(header)
				var result map[string]any
				_ = json.Unmarshal(encodedHeader, &result)
				if len(req.Params) > 1 && req.Params[1] == true {
					txJSON, _ := sampleTx.MarshalJSON()
					var txObj map[string]any
					_ = json.Unmarshal(txJSON, &txObj)
					result["transactions"] = []any{txObj}
				}
				resps[i] = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			}
			_ = json.NewEncoder(w).Encode(resps)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal([]byte(trimmed), &req)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": mockChainID})
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	defer client.Close()

	nums := []*big.Int{big.NewInt(100), big.NewInt(101), big.NewInt(102)}
	blocks, err := client.BlocksByNumbers(context.Background(), nums)
	if err != nil {
		t.Fatalf("BlocksByNumbers() error = %v (cause=%v)", err, errors.Unwrap(err))
	}
	if len(blocks) != 3 || blocks[0].NumberU64() != 100 || blocks[2].NumberU64() != 102 || len(blocks[0].Transactions()) != 1 {
		t.Fatalf("BlocksByNumbers() unexpected blocks = %+v", blocks)
	}
	headers, err := client.HeadersByNumbers(context.Background(), nums)
	if err != nil {
		t.Fatalf("HeadersByNumbers() error = %v", err)
	}
	if len(headers) != 3 || headers[1].Number.Uint64() != 101 {
		t.Fatalf("HeadersByNumbers() unexpected headers = %+v", headers)
	}
	if batchPosts != 2 {
		t.Fatalf("batchPosts = %d, want 2 (1 for blocks + 1 for headers)", batchPosts)
	}
}

// TestClientDoesNotSwitchEndpointOnInsufficientFundsOrNonceTooLow 验证节点返回 insufficient funds 和 nonce too low（含非标准 HTTP 错误包装）时不切换端点也不标记 DEGRADED。
func TestClientDoesNotSwitchEndpointOnInsufficientFundsOrNonceTooLow(t *testing.T) {
	primary := newMockRPC(t)
	fallback := newMockRPC(t)
	client := newTestClientWithPolicy(t, primary.URL(), fallback.URL(), RetryPolicy{MaxAttempts: 2})
	defer client.Close()

	sampleTx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(SepoliaChainID),
		Nonce:     1,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       21_000,
		To:        &common.Address{},
		Value:     big.NewInt(100),
	})

	primary.failRPC("eth_sendRawTransaction", -32000, "insufficient funds for gas * price + value", 1)
	if err := client.SendTransaction(context.Background(), sampleTx); err == nil {
		t.Fatal("expected insufficient funds error")
	}
	if fallback.calls("eth_sendRawTransaction") != 0 || client.Snapshot().Status != "HEALTHY" {
		t.Fatalf("insufficient funds triggered fallback=%d snap=%+v", fallback.calls("eth_sendRawTransaction"), client.Snapshot())
	}

	primary.failRPC("eth_sendRawTransaction", -32000, "nonce too low", 1)
	if err := client.SendTransaction(context.Background(), sampleTx); err == nil {
		t.Fatal("expected nonce too low error")
	}
	if fallback.calls("eth_sendRawTransaction") != 0 || client.Snapshot().Status != "HEALTHY" {
		t.Fatalf("nonce too low triggered fallback=%d snap=%+v", fallback.calls("eth_sendRawTransaction"), client.Snapshot())
	}

	// 非 200 HTTP 响应但错误消息明确为业务错误（insufficient funds / nonce too low）时同样不得切换端点。
	var primaryHTTPNon200Calls int
	httpErrPrimary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "eth_chainId" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": mockChainID})
			return
		}
		primaryHTTPNon200Calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"insufficient funds for intrinsic transaction cost"}`))
	}))
	defer httpErrPrimary.Close()
	client2 := newTestClientWithPolicy(t, httpErrPrimary.URL, fallback.URL(), RetryPolicy{MaxAttempts: 2})
	defer client2.Close()

	if err := client2.SendTransaction(context.Background(), sampleTx); err == nil {
		t.Fatal("expected HTTP 400 insufficient funds error")
	}
	if primaryHTTPNon200Calls != 1 || fallback.calls("eth_sendRawTransaction") != 0 || client2.Snapshot().Status != "HEALTHY" {
		t.Fatalf("HTTP 400 insufficient funds switched/retried unexpectedly: primaryCalls=%d fallback=%d snap=%+v",
			primaryHTTPNon200Calls, fallback.calls("eth_sendRawTransaction"), client2.Snapshot())
	}
}
