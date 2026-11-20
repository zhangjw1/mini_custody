package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/auth"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	tokendeposit "github.com/xiaoqi/mini-custody/backend/internal/token/deposit"
	"github.com/xiaoqi/mini-custody/backend/internal/token/gasstation"
	tokensweep "github.com/xiaoqi/mini-custody/backend/internal/token/sweep"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// contractStore 为 HTTP API 契约测试提供固定数据。
type contractStore struct {
	createdAt     time.Time
	adminDisabled bool
	adminPwdHash  string
}

// ListAssets 返回 ETH 和 USDC 测试资产。
func (c *contractStore) ListAssets(context.Context) ([]postgres.Asset, error) {
	return []postgres.Asset{
		{ID: 1, Network: postgres.NetworkSepolia, AssetType: postgres.AssetTypeNative, Symbol: "ETH", Decimals: 18, Enabled: true, UpdatedAt: c.createdAt},
		{ID: 2, Network: postgres.NetworkSepolia, AssetType: postgres.AssetTypeERC20, Symbol: "USDC", ContractAddress: "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238", Decimals: 6, Enabled: true, UpdatedAt: c.createdAt},
	}, nil
}

// ListUsers 返回一个测试用户。
func (c *contractStore) ListUsers(context.Context) ([]postgres.User, error) {
	return []postgres.User{{ID: 1, Code: "alice", DisplayName: "Alice", Status: postgres.UserStatusActive, CreatedAt: c.createdAt}}, nil
}

// UserByID 返回测试用户或未找到错误。
func (c *contractStore) UserByID(_ context.Context, id int64) (postgres.User, error) {
	if id != 1 {
		return postgres.User{}, postgres.ErrNotFound
	}
	return postgres.User{ID: 1, Code: "alice", DisplayName: "Alice", Status: postgres.UserStatusActive, CreatedAt: c.createdAt}, nil
}

// AdminByUsername 返回测试管理员记录。
func (c *contractStore) AdminByUsername(_ context.Context, username string) (postgres.AdminUser, error) {
	if username != "admin" {
		return postgres.AdminUser{}, postgres.ErrNotFound
	}
	hash := c.adminPwdHash
	if hash == "" {
		hash, _ = auth.HashPassword("Admin@2026")
	}
	return postgres.AdminUser{
		ID: 1, Username: "admin", PasswordHash: hash, DisplayName: "系统管理员",
		Role: postgres.AdminRole, Disabled: c.adminDisabled, CreatedAt: c.createdAt, UpdatedAt: c.createdAt,
	}, nil
}

// AdminByID 返回测试管理员记录。
func (c *contractStore) AdminByID(_ context.Context, id int64) (postgres.AdminUser, error) {
	if id != 1 {
		return postgres.AdminUser{}, postgres.ErrNotFound
	}
	return c.AdminByUsername(context.Background(), "admin")
}

// UpdateAdminLastLogin 在契约测试中空操作返回 nil。
func (c *contractStore) UpdateAdminLastLogin(context.Context, int64) error { return nil }

// CreateManagedUser 返回新建托管用户及确定性测试地址。
func (c *contractStore) CreateManagedUser(_ context.Context, code, displayName, note string, _ wallet.KeyProvider, _ *wallet.MnemonicKeyProvider) (postgres.User, postgres.WalletAddress, error) {
	return postgres.User{ID: 2, Code: code, DisplayName: displayName, Status: postgres.UserStatusActive, Note: note, CreatedAt: c.createdAt},
		postgres.WalletAddress{ID: 2, UserID: 2, Address: "0x2222222222222222222222222222222222222222"}, nil
}

// UpdateManagedUser 返回更新后的测试用户。
func (c *contractStore) UpdateManagedUser(_ context.Context, id int64, displayName, note, status string) (postgres.User, error) {
	if id != 1 {
		return postgres.User{}, postgres.ErrNotFound
	}
	return postgres.User{ID: 1, Code: "alice", DisplayName: displayName, Note: note, Status: status, CreatedAt: c.createdAt}, nil
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

// BalanceByUserAndAsset 返回测试用户指定资产余额。
func (c *contractStore) BalanceByUserAndAsset(ctx context.Context, userID, assetID int64) (postgres.AssetBalance, error) {
	if assetID == 1 {
		return c.BalanceByUser(ctx, userID)
	}
	return postgres.AssetBalance{UserID: userID, AssetID: 2, Asset: "USDC", AvailableWei: big.NewInt(1_500_000), PendingDepositWei: new(big.Int), PendingWithdrawalWei: new(big.Int), UpdatedAt: c.createdAt}, nil
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
		Type: "WITHDRAWAL", ID: 1, UserID: 1, Asset: postgres.AssetETH, Decimals: 18,
		TxHash:    "0x269734416482480057a442d0a0b6023c79c7d28de83f860e4ee708d536fe07fd",
		AmountWei: big.NewInt(1_000_000_000_000_000), BlockNumber: &block,
		Confirmations: 3, Status: postgres.WithdrawalCompleted, CreatedAt: c.createdAt, UpdatedAt: c.createdAt,
	}}, nil
}

// ListTransactionsFilteredPage 返回统一流水测试数据。
func (c *contractStore) ListTransactionsFilteredPage(ctx context.Context, _, _ string, limit, offset int) ([]postgres.TransactionRecord, error) {
	return c.ListTransactionsPage(ctx, limit, offset)
}

// ListTokenDepositsPage 返回空 Token 充值测试页。
func (c *contractStore) ListTokenDepositsPage(context.Context, int64, int, int) ([]postgres.TokenDeposit, error) {
	return []postgres.TokenDeposit{}, nil
}

// ListTokenWithdrawalsPage 返回空 Token 提币测试页。
func (c *contractStore) ListTokenWithdrawalsPage(context.Context, int64, int, int) ([]postgres.TokenWithdrawal, error) {
	return []postgres.TokenWithdrawal{}, nil
}

// TokenWithdrawalByID 返回未找到测试错误。
func (c *contractStore) TokenWithdrawalByID(context.Context, int64) (postgres.TokenWithdrawal, error) {
	return postgres.TokenWithdrawal{}, postgres.ErrNotFound
}

// ListTokenSweepsPage 返回空 Token 归集测试页。
func (c *contractStore) ListTokenSweepsPage(context.Context, int, int) ([]postgres.TokenSweep, error) {
	return []postgres.TokenSweep{}, nil
}

// ListInternalTransfersPage 返回空内部转账测试页。
func (c *contractStore) ListInternalTransfersPage(context.Context, int, int) ([]postgres.InternalTransfer, error) {
	return []postgres.InternalTransfer{}, nil
}

// PlatformWalletByRole 返回测试平台热钱包。
func (c *contractStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	return postgres.PlatformWallet{ID: 1, Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot, Address: "0x1111111111111111111111111111111111111111", NextNonce: big.NewInt(7)}, nil
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
	token := loginForContractToken(t, handler)
	tests := []struct {
		path string
		want string
	}{
		{"/api/v1/users", `"code":"alice"`},
		{"/api/v1/assets", `"symbol":"USDC"`},
		{"/api/v1/users/1/balances", `"available":"1.5"`},
		{"/api/v1/users/1/wallet", `"available_wei":"976580409736000"`},
		{"/api/v1/users/1/deposits?page=1&page_size=20", `"amount_eth":"0.002"`},
		{"/api/v1/users/1/withdrawals", `"actual_fee_wei":"23419590264000"`},
		{"/api/v1/withdrawals/1", `"status":"COMPLETED"`},
		{"/api/v1/transactions", `https://sepolia.etherscan.io/tx/0x269734`},
		{"/api/v1/worker-errors", `"error_message":"RPC 请求超时"`},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
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
	token := loginForContractToken(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/users/999/wallet", nil)
	request.Header.Set("Authorization", "Bearer "+token)
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
	token := loginForContractToken(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/transactions?page_size=101", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_PAGINATION") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

// TestPhase8QueryEndpointsDoNotExposeSensitiveFields 验证 Token 和运维查询路由可用且不返回敏感字段。
func TestPhase8QueryEndpointsDoNotExposeSensitiveFields(t *testing.T) {
	handler := contractHandler(t)
	token := loginForContractToken(t, handler)
	paths := []string{
		"/api/v1/users/1/token-deposits",
		"/api/v1/users/1/token-withdrawals",
		"/api/v1/sweeps",
		"/api/v1/internal-transfers",
		"/api/v1/system/platform-wallet",
		"/api/v1/transactions?asset=USDC&type=TOKEN_DEPOSIT",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
		for _, secret := range []string{"raw_tx", "derivation_path", "private_key", "mnemonic"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("GET %s leaked %q: %s", path, secret, response.Body.String())
			}
		}
	}
}

// TestAdminAuthMiddlewareAndUserManagement 验证未认证请求返回 401、JWT 与 APIKey 认证、用户管理及错误脱敏。
func TestAdminAuthMiddlewareAndUserManagement(t *testing.T) {
	handler := contractHandler(t)

	// 未携带认证凭证访问业务接口必须返回 401。
	unauth := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	unauthResp := httptest.NewRecorder()
	handler.ServeHTTP(unauthResp, unauth)
	if unauthResp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthResp.Code)
	}

	// /livez 无需鉴权即可返回 200。
	liveReq := httptest.NewRequest(http.MethodGet, "/livez", nil)
	liveResp := httptest.NewRecorder()
	handler.ServeHTTP(liveResp, liveReq)
	if liveResp.Code != http.StatusOK {
		t.Fatalf("/livez status = %d, want 200", liveResp.Code)
	}

	// 错误密码登录返回 401。
	badLogin := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrong-password"}`))
	badLoginResp := httptest.NewRecorder()
	handler.ServeHTTP(badLoginResp, badLogin)
	if badLoginResp.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", badLoginResp.Code)
	}

	// 携带配置的 X-API-Key 允许访问管理员接口。
	apiKeyReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	apiKeyReq.Header.Set("X-API-Key", "test-api-key-0123456789abcdef0123")
	apiKeyResp := httptest.NewRecorder()
	handler.ServeHTTP(apiKeyResp, apiKeyReq)
	if apiKeyResp.Code != http.StatusOK {
		t.Fatalf("api key /me status = %d, body = %s", apiKeyResp.Code, apiKeyResp.Body.String())
	}

	// JWT 登录后可新建并更新托管用户。
	token := loginForContractToken(t, handler)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"code":"custody-charlie","display_name":"Charlie","note":"VIP"}`))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated || !strings.Contains(createResp.Body.String(), `"code":"custody-charlie"`) {
		t.Fatalf("create user status = %d, body = %s", createResp.Code, createResp.Body.String())
	}

	updateReq := httptest.NewRequest(http.MethodPatch, "/api/v1/users/1", strings.NewReader(`{"display_name":"Alice v2","note":"已停用","status":"DISABLED"}`))
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateResp := httptest.NewRecorder()
	handler.ServeHTTP(updateResp, updateReq)
	if updateResp.Code != http.StatusOK || !strings.Contains(updateResp.Body.String(), `"status":"DISABLED"`) {
		t.Fatalf("update user status = %d, body = %s", updateResp.Code, updateResp.Body.String())
	}
}

// TestBitcoinWithdrawalErrorMappingRedactsSQLState 验证 BTC 提币错误映射状态码准确且绝不暴露 SQLSTATE。
func TestBitcoinWithdrawalErrorMappingRedactsSQLState(t *testing.T) {
	application := &App{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"insufficient", postgres.ErrInsufficientBalance, http.StatusConflict, "INSUFFICIENT_BALANCE"},
		{"idempotency", postgres.ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{"disabled", postgres.ErrUserDisabled, http.StatusForbidden, "USER_DISABLED"},
		{"notfound", postgres.ErrNotFound, http.StatusNotFound, "USER_OR_ASSET_NOT_FOUND"},
		{"internal", errors.New(`数据库错误：ERROR: duplicate key value violates unique constraint "x" (SQLSTATE 23505)`), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			application.writeBitcoinWithdrawalError(context.Background(), rec, tc.err)
			body := rec.Body.String()
			if rec.Code != tc.wantStatus || !strings.Contains(body, tc.wantCode) {
				t.Fatalf("status=%d body=%s, want status=%d code=%s", rec.Code, body, tc.wantStatus, tc.wantCode)
			}
			if strings.Contains(body, "SQLSTATE") || strings.Contains(body, "constraint") {
				t.Fatalf("error response leaked internal DB details: %s", body)
			}
		})
	}
}

// loginForContractToken 调用登录接口获取契约测试使用的有效 JWT。
func loginForContractToken(t *testing.T, handler http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"Admin@2026"}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var payload loginResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil || payload.Token == "" {
		t.Fatalf("decode login response err=%v token=%q", err, payload.Token)
	}
	return payload.Token
}

// contractHandler 装配包含 request_id 与管理员 JWT 鉴权中间件的 API 契约测试路由。
func contractHandler(t *testing.T) http.Handler {
	t.Helper()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	createdAt := time.Date(2026, 8, 15, 11, 10, 0, 0, location)
	pwdHash, err := auth.HashPassword("Admin@2026")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	store := &contractStore{createdAt: createdAt, adminPwdHash: pwdHash}
	provider, err := wallet.NewMnemonicKeyProvider("tag volcano eight thank tide danger coast health above argue embrace heavy")
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	application := &App{
		config: config.Config{
			Timezone:               location,
			SepoliaExplorerBaseURL: "https://sepolia.etherscan.io",
			Auth: config.AuthConfig{
				JWTTTL:         time.Hour,
				APIKey:         "test-api-key-0123456789abcdef0123",
				APIKeyUsername: "system-ops",
			},
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiStore:  store,
		authStore: store,
		jwtSecret: []byte("0123456789abcdef0123456789abcdef"),
		keys:      provider,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", application.livez)
	mux.HandleFunc("POST /api/v1/auth/login", application.login)
	mux.HandleFunc("GET /api/v1/auth/me", application.requireAdminAuth(application.me))
	mux.HandleFunc("GET /api/v1/users", application.requireAdminAuth(application.listUsers))
	mux.HandleFunc("POST /api/v1/users", application.requireAdminAuth(application.createUser))
	mux.HandleFunc("PATCH /api/v1/users/{user_id}", application.requireAdminAuth(application.updateUser))
	mux.HandleFunc("GET /api/v1/assets", application.requireAdminAuth(application.listAssets))
	mux.HandleFunc("GET /api/v1/users/{user_id}/wallet", application.requireAdminAuth(application.getWallet))
	mux.HandleFunc("GET /api/v1/users/{user_id}/balances", application.requireAdminAuth(application.listUserBalances))
	mux.HandleFunc("GET /api/v1/users/{user_id}/deposits", application.requireAdminAuth(application.listDeposits))
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-deposits", application.requireAdminAuth(application.listTokenDeposits))
	mux.HandleFunc("GET /api/v1/users/{user_id}/withdrawals", application.requireAdminAuth(application.listWithdrawals))
	mux.HandleFunc("GET /api/v1/users/{user_id}/token-withdrawals", application.requireAdminAuth(application.listTokenWithdrawals))
	mux.HandleFunc("GET /api/v1/withdrawals/{withdrawal_id}", application.requireAdminAuth(application.getWithdrawal))
	mux.HandleFunc("GET /api/v1/token-withdrawals/{withdrawal_id}", application.requireAdminAuth(application.getTokenWithdrawal))
	mux.HandleFunc("GET /api/v1/transactions", application.requireAdminAuth(application.listTransactions))
	mux.HandleFunc("GET /api/v1/sweeps", application.requireAdminAuth(application.listTokenSweeps))
	mux.HandleFunc("GET /api/v1/internal-transfers", application.requireAdminAuth(application.listInternalTransfers))
	mux.HandleFunc("GET /api/v1/system/platform-wallet", application.requireAdminAuth(application.getPlatformWalletStatus))
	mux.HandleFunc("GET /api/v1/worker-errors", application.requireAdminAuth(application.listWorkerErrors))
	return application.requestContext(mux)
}

// TestAuthHardeningJ1ToJ3 验证登录限流锁定、防账号枚举统一错误码与禁用后 JWT 立即失效。
func TestAuthHardeningJ1ToJ3(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword("Admin@2026")
	if err != nil {
		t.Fatal(err)
	}
	store := &contractStore{createdAt: time.Now(), adminPwdHash: hash}
	application := &App{
		config: config.Config{
			Timezone: location,
			Auth: config.AuthConfig{
				JWTTTL:                 time.Hour,
				LoginRateLimitPerSec:   100,
				LoginRateLimitBurst:    100,
				LoginMaxFailedAttempts: 3,
				LoginLockDuration:      time.Minute,
			},
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiStore:  store,
		authStore: store,
		jwtSecret: []byte("0123456789abcdef0123456789abcdef"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", application.login)
	mux.HandleFunc("GET /api/v1/auth/me", application.requireAdminAuth(application.me))
	mux.HandleFunc("GET /api/v1/users", application.requireAdminAuth(application.listUsers))
	handler := application.requestContext(mux)

	// J2：用户不存在、密码错误、禁用账号必须统一返回 401 INVALID_CREDENTIALS，不能暴露 ADMIN_DISABLED。
	store.adminDisabled = true
	for _, body := range []string{
		`{"username":"missing-user","password":"wrong"}`,
		`{"username":"admin","password":"wrong-password"}`,
		`{"username":"admin","password":"Admin@2026"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.1:1234"
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized || !strings.Contains(resp.Body.String(), `"code":"INVALID_CREDENTIALS"`) {
			t.Fatalf("login %s -> status=%d body=%s, want 401 INVALID_CREDENTIALS", body, resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), "ADMIN_DISABLED") {
			t.Fatalf("login %s leaked ADMIN_DISABLED: %s", body, resp.Body.String())
		}
	}

	// J3：登录获得有效 JWT 后若管理员被禁用，下一次请求必须在中间件直接被拒绝。
	store.adminDisabled = false
	token := loginForContractToken(t, handler)
	store.adminDisabled = true
	usersReq := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	usersReq.Header.Set("Authorization", "Bearer "+token)
	usersResp := httptest.NewRecorder()
	handler.ServeHTTP(usersResp, usersReq)
	if usersResp.Code != http.StatusUnauthorized {
		t.Fatalf("disabled admin JWT /api/v1/users status=%d body=%s, want 401", usersResp.Code, usersResp.Body.String())
	}

	// J1：同一 IP + 用户名连续失败达到上限后被锁定（返回 429）。
	store.adminDisabled = false
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"bad"}`))
		req.RemoteAddr = "10.0.0.9:5678"
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d, want 401", i+1, resp.Code)
		}
	}
	lockedReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"Admin@2026"}`))
	lockedReq.RemoteAddr = "10.0.0.9:5678"
	lockedResp := httptest.NewRecorder()
	handler.ServeHTTP(lockedResp, lockedReq)
	if lockedResp.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login status=%d body=%s, want 429", lockedResp.Code, lockedResp.Body.String())
	}

	// J1 令牌桶限流：Burst=1 时同 IP+用户名 连发两次触发 429 RATE_LIMITED。
	rateApp := &App{
		config: config.Config{
			Timezone: location,
			Auth: config.AuthConfig{
				JWTTTL:                 time.Hour,
				LoginRateLimitPerSec:   0.1,
				LoginRateLimitBurst:    1,
				LoginMaxFailedAttempts: 10,
				LoginLockDuration:      time.Minute,
			},
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiStore:  store,
		authStore: store,
		jwtSecret: []byte("0123456789abcdef0123456789abcdef"),
	}
	rateMux := http.NewServeMux()
	rateMux.HandleFunc("POST /api/v1/auth/login", rateApp.login)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"bad"}`))
	req1.RemoteAddr = "10.0.0.88:4321"
	resp1 := httptest.NewRecorder()
	rateMux.ServeHTTP(resp1, req1)
	if resp1.Code != http.StatusUnauthorized {
		t.Fatalf("first burst login status=%d, want 401", resp1.Code)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"bad"}`))
	req2.RemoteAddr = "10.0.0.88:4321"
	resp2 := httptest.NewRecorder()
	rateMux.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusTooManyRequests || !strings.Contains(resp2.Body.String(), `"code":"RATE_LIMITED"`) {
		t.Fatalf("second burst login status=%d body=%s, want 429 RATE_LIMITED", resp2.Code, resp2.Body.String())
	}
}

// TestHealthEndpointsRedactErrorsAndExposeDetailsAfterAuth 验证未鉴权健康检查只返回状态枚举，详细错误仅在鉴权系统接口返回。
func TestHealthEndpointsRedactErrorsAndExposeDetailsAfterAuth(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Shanghai")
	hash, _ := auth.HashPassword("Admin@2026")
	store := &contractStore{createdAt: time.Now(), adminPwdHash: hash}
	application := &App{
		config: config.Config{
			Timezone: location,
			Auth:     config.AuthConfig{JWTTTL: time.Hour},
			Bitcoin:  config.BitcoinConfig{Enabled: true, Network: "signet"},
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		apiStore:  store,
		authStore: store,
		jwtSecret: []byte("0123456789abcdef0123456789abcdef"),
		healthHooks: &healthTestHooks{
			pingDB: func(context.Context) error { return nil },
			chainHealth: func(context.Context) evm.HealthSnapshot {
				return evm.HealthSnapshot{Status: "DOWN", Endpoint: "primary", ChainID: "11155111", LastError: "sepolia-secret-rpc-error"}
			},
			tokenSnap: func() tokendeposit.HealthSnapshot {
				return tokendeposit.HealthSnapshot{Status: "DOWN", LastError: "token-secret-rpc-error"}
			},
			gasSnap: func() gasstation.HealthSnapshot {
				return gasstation.HealthSnapshot{Status: "DOWN", BalanceWei: big.NewInt(1), MinBalanceWei: big.NewInt(10), LastError: "gas-secret-rpc-error"}
			},
			sweepSnap: func() tokensweep.HealthSnapshot {
				return tokensweep.HealthSnapshot{Status: "DOWN", Symbol: "USDC", BalanceUnits: big.NewInt(0), LastError: "sweep-secret-rpc-error"}
			},
			btcHeight: func(context.Context) (int64, error) {
				return 0, errors.New("节点异常-btc-secret-rpc-error")
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", application.health)
	mux.HandleFunc("GET /readyz", application.health)
	mux.HandleFunc("POST /api/v1/auth/login", application.login)
	mux.HandleFunc("GET /api/v1/system/chains/sepolia", application.requireAdminAuth(application.getChainStatus))
	handler := application.requestContext(mux)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		body := resp.Body.String()
		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d body=%s, want 503", path, resp.Code, body)
		}
		for _, secret := range []string{
			"last_error", "sepolia-secret-rpc-error", "token-secret-rpc-error",
			"gas-secret-rpc-error", "sweep-secret-rpc-error", "btc-secret-rpc-error", "chain_endpoint",
		} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked %q: %s", path, secret, body)
			}
		}
	}

	token := loginForContractToken(t, handler)
	sysReq := httptest.NewRequest(http.MethodGet, "/api/v1/system/chains/sepolia", nil)
	sysReq.Header.Set("Authorization", "Bearer "+token)
	sysResp := httptest.NewRecorder()
	handler.ServeHTTP(sysResp, sysReq)
	if sysResp.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/system/chains/sepolia status=%d body=%s", sysResp.Code, sysResp.Body.String())
	}
	sysBody := sysResp.Body.String()
	for _, want := range []string{"token-secret-rpc-error", "gas-secret-rpc-error", "sweep-secret-rpc-error", "btc-secret-rpc-error"} {
		if !strings.Contains(sysBody, want) {
			t.Fatalf("authenticated system status missing %q: %s", want, sysBody)
		}
	}
}

// TestWaitRemainingComponentsLogsUnexitedOnTimeout 验证组件退出超时会在日志与错误中明确列出未退出组件名称。
func TestWaitRemainingComponentsLogsUnexitedOnTimeout(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ch := make(chan componentResult, 1)
	ch <- componentResult{name: "deposit"}
	err := waitRemainingComponents(logger, ch, []string{"deposit", "withdrawal", "btc-sweep"}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "btc-sweep") || !strings.Contains(err.Error(), "withdrawal") {
		t.Fatalf("waitRemainingComponents() err = %v, want pending component names", err)
	}
	if !strings.Contains(buf.String(), "btc-sweep") || !strings.Contains(buf.String(), "withdrawal") {
		t.Fatalf("log output missing pending component names: %s", buf.String())
	}
}
