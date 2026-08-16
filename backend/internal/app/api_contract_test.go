package app

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

// contractStore 为 HTTP API 契约测试提供固定数据。
type contractStore struct {
	createdAt time.Time
}

// ListUsers 返回一个测试用户。
func (c *contractStore) ListUsers(context.Context) ([]postgres.User, error) {
	return []postgres.User{{ID: 1, Code: "demo-alice", DisplayName: "演示用户 Alice", CreatedAt: c.createdAt}}, nil
}

// UserByID 返回测试用户或未找到错误。
func (c *contractStore) UserByID(_ context.Context, id int64) (postgres.User, error) {
	if id != 1 {
		return postgres.User{}, postgres.ErrNotFound
	}
	return postgres.User{ID: 1, Code: "demo-alice", DisplayName: "演示用户 Alice", CreatedAt: c.createdAt}, nil
}

// WalletAddressByUser 返回测试托管地址。
func (c *contractStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return postgres.WalletAddress{
		ID: 1, UserID: 1, Network: postgres.NetworkSepolia,
		Address: "0xd3197b634d458724c41e3019ad06a8224ad07571", CreatedAt: c.createdAt,
	}, nil
}

// BalanceByUser 返回包含高精度字符串的测试余额。
func (c *contractStore) BalanceByUser(context.Context, int64) (postgres.AssetBalance, error) {
	return postgres.AssetBalance{
		UserID: 1, Asset: postgres.AssetETH,
		AvailableWei: big.NewInt(976_580_409_736_000), PendingDepositWei: big.NewInt(0),
		PendingWithdrawalWei: big.NewInt(0), UpdatedAt: c.createdAt,
	}, nil
}

// ListDepositsPage 返回一笔测试充值。
func (c *contractStore) ListDepositsPage(context.Context, int64, int, int) ([]postgres.Deposit, error) {
	return []postgres.Deposit{{
		ID: 1, UserID: 1, Network: postgres.NetworkSepolia, Asset: postgres.AssetETH,
		TxHash:      "0x7aa7a5984230f6bdf4d70267474cb08120a459e3db3f5e9d1cb3c47d3bda1e9b",
		BlockNumber: 11491276, AmountWei: big.NewInt(2_000_000_000_000_000),
		Confirmations: 3, Status: postgres.DepositCredited, CreatedAt: c.createdAt, UpdatedAt: c.createdAt,
	}}, nil
}

// ListWithdrawalsPage 返回一笔不暴露 raw_tx 的测试提币。
func (c *contractStore) ListWithdrawalsPage(context.Context, int64, int, int) ([]postgres.Withdrawal, error) {
	return []postgres.Withdrawal{c.withdrawal()}, nil
}

// WithdrawalByID 返回测试提币。
func (c *contractStore) WithdrawalByID(_ context.Context, id int64) (postgres.Withdrawal, error) {
	if id != 1 {
		return postgres.Withdrawal{}, postgres.ErrNotFound
	}
	return c.withdrawal(), nil
}

// ListTransactionsPage 返回一笔测试全局交易。
func (c *contractStore) ListTransactionsPage(context.Context, int, int) ([]postgres.TransactionRecord, error) {
	block := int64(11491455)
	return []postgres.TransactionRecord{{
		Type: "WITHDRAWAL", ID: 1, UserID: 1, Asset: postgres.AssetETH,
		TxHash:    "0x269734416482480057a442d0a0b6023c79c7d28de83f860e4ee708d536fe07fd",
		AmountWei: big.NewInt(1_000_000_000_000_000), BlockNumber: &block,
		Confirmations: 3, Status: postgres.WithdrawalCompleted, CreatedAt: c.createdAt, UpdatedAt: c.createdAt,
	}}, nil
}

// ListWorkerErrorsPage 返回一笔已清洗测试错误。
func (c *contractStore) ListWorkerErrorsPage(context.Context, int, int) ([]postgres.WorkerError, error) {
	referenceID := int64(1)
	return []postgres.WorkerError{{
		ID: 1, Worker: "withdrawal-worker", Stage: "process", ReferenceType: "WITHDRAWAL",
		ReferenceID: &referenceID, ErrorCode: "RPC_TIMEOUT", ErrorMessage: "RPC 请求超时",
		FirstOccurredAt: c.createdAt, LastOccurredAt: c.createdAt,
	}}, nil
}

// withdrawal 构造包含敏感 raw_tx 但 API 不应返回的测试提币。
func (c *contractStore) withdrawal() postgres.Withdrawal {
	gasLimit := int64(21_000)
	block := int64(11491455)
	return postgres.Withdrawal{
		ID: 1, UserID: 1, AddressID: 1,
		ToAddress: "0xb043ce34da539a714a92d47258f36188d8decb4e",
		AmountWei: big.NewInt(1_000_000_000_000_000), ReservedFeeWei: big.NewInt(43_050_057_330_000),
		ActualFeeWei: big.NewInt(23_419_590_264_000), Nonce: big.NewInt(0), GasLimit: &gasLimit,
		MaxFeePerGasWei: big.NewInt(2_050_002_730), MaxPriorityFeePerGasWei: big.NewInt(1_000_000),
		RawTx:       []byte("绝不能返回的签名交易"),
		TxHash:      "0x269734416482480057a442d0a0b6023c79c7d28de83f860e4ee708d536fe07fd",
		BlockNumber: &block, Confirmations: 3, Status: postgres.WithdrawalCompleted,
		CreatedAt: c.createdAt, UpdatedAt: c.createdAt,
	}
}

// TestAPIContractReturnsPreciseAmountsLinksAndRequestID 验证主要查询接口的金额、链接、时间和请求标识契约。
func TestAPIContractReturnsPreciseAmountsLinksAndRequestID(t *testing.T) {
	handler := contractHandler(t)
	tests := []struct {
		path string
		want string
	}{
		{"/api/v1/users", `"code":"demo-alice"`},
		{"/api/v1/users/1/wallet", `"available_wei":"976580409736000"`},
		{"/api/v1/users/1/deposits?page=1&page_size=20", `"amount_eth":"0.002"`},
		{"/api/v1/users/1/withdrawals", `"actual_fee_wei":"23419590264000"`},
		{"/api/v1/withdrawals/1", `"status":"COMPLETED"`},
		{"/api/v1/transactions", `https://sepolia.etherscan.io/tx/0x269734`},
		{"/api/v1/worker-errors", `"error_message":"RPC 请求超时"`},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, test.want) {
			t.Fatalf("GET %s status = %d, body = %s, want %s", test.path, response.Code, body, test.want)
		}
		if len(response.Header().Get("X-Request-ID")) != 32 {
			t.Fatalf("GET %s request ID = %q", test.path, response.Header().Get("X-Request-ID"))
		}
		if !strings.Contains(body, "+08:00") {
			t.Fatalf("GET %s body does not contain UTC+8 time: %s", test.path, body)
		}
		for _, secret := range []string{"raw_tx", "绝不能返回的签名交易", "mnemonic", "private_key"} {
			if strings.Contains(body, secret) {
				t.Fatalf("GET %s leaked %q: %s", test.path, secret, body)
			}
		}
	}
}

// TestAPIContractErrorContainsMatchingRequestID 验证错误 JSON 和响应头使用相同 request_id。
func TestAPIContractErrorContainsMatchingRequestID(t *testing.T) {
	handler := contractHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/users/999/wallet", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requestID := response.Header().Get("X-Request-ID")
	if response.Code != http.StatusNotFound || requestID == "" || !strings.Contains(response.Body.String(), `"request_id":"`+requestID+`"`) {
		t.Fatalf("status = %d, request ID = %q, body = %s", response.Code, requestID, response.Body.String())
	}
}

// TestAPIContractRejectsInvalidPagination 验证非法分页不会进入数据库查询。
func TestAPIContractRejectsInvalidPagination(t *testing.T) {
	handler := contractHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?page_size=101", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_PAGINATION") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

// contractHandler 装配包含 request_id 中间件的 API 契约测试路由。
func contractHandler(t *testing.T) http.Handler {
	t.Helper()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	createdAt := time.Date(2026, 8, 15, 11, 10, 0, 0, location)
	application := &App{
		config: config.Config{Timezone: location, SepoliaExplorerBaseURL: "https://sepolia.etherscan.io"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), apiStore: &contractStore{createdAt: createdAt},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users", application.listUsers)
	mux.HandleFunc("GET /api/v1/users/{user_id}/wallet", application.getWallet)
	mux.HandleFunc("GET /api/v1/users/{user_id}/deposits", application.listDeposits)
	mux.HandleFunc("GET /api/v1/users/{user_id}/withdrawals", application.listWithdrawals)
	mux.HandleFunc("GET /api/v1/withdrawals/{withdrawal_id}", application.getWithdrawal)
	mux.HandleFunc("GET /api/v1/transactions", application.listTransactions)
	mux.HandleFunc("GET /api/v1/worker-errors", application.listWorkerErrors)
	return application.requestContext(mux)
}
