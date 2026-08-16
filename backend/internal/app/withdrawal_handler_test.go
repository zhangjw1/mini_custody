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

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/withdrawal"
)

// handlerChain 为创建提币接口提供固定费用估算。
type handlerChain struct{}

// BlockNumber 返回测试区块高度。
func (handlerChain) BlockNumber(context.Context) (uint64, error) { return 1, nil }

// HeaderByNumber 返回带 Base Fee 的测试区块头。
func (handlerChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return &types.Header{BaseFee: big.NewInt(100)}, nil
}

// BalanceAt 返回测试链上余额。
func (handlerChain) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return big.NewInt(1_000_000_000), nil
}

// PendingNonceAt 返回测试 Nonce。
func (handlerChain) PendingNonceAt(context.Context, common.Address) (uint64, error) { return 0, nil }

// SuggestGasTipCap 返回测试优先费。
func (handlerChain) SuggestGasTipCap(context.Context) (*big.Int, error) { return big.NewInt(2), nil }

// EstimateGas 返回普通 ETH 转账 Gas。
func (handlerChain) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 21_000, nil
}

// SendTransaction 在接口测试中不执行广播。
func (handlerChain) SendTransaction(context.Context, *types.Transaction) error { return nil }

// TransactionReceipt 在接口测试中不返回 Receipt。
func (handlerChain) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, ethereum.NotFound
}

// handlerStore 记录创建接口提交的提币占用请求。
type handlerStore struct {
	called  bool
	request postgres.WithdrawalRequest
}

// WalletAddressByUser 返回测试用户托管地址。
func (h *handlerStore) WalletAddressByUser(context.Context, int64) (postgres.WalletAddress, error) {
	return postgres.WalletAddress{ID: 10, UserID: 1, Address: "0x1111111111111111111111111111111111111111"}, nil
}

// WithdrawalByIdempotencyKey 表示接口测试中尚无相同幂等请求。
func (h *handlerStore) WithdrawalByIdempotencyKey(context.Context, int64, string) (postgres.Withdrawal, error) {
	return postgres.Withdrawal{}, postgres.ErrNotFound
}

// ReserveWithdrawal 记录测试请求并返回已创建提币。
func (h *handlerStore) ReserveWithdrawal(_ context.Context, request postgres.WithdrawalRequest) (postgres.Withdrawal, bool, error) {
	h.called = true
	h.request = request
	return postgres.Withdrawal{
		ID: 3, UserID: request.UserID, ToAddress: request.ToAddress,
		AmountWei: request.AmountWei, ReservedFeeWei: request.ReservedFeeWei, Status: postgres.WithdrawalCreated,
	}, true, nil
}

// TestCreateWithdrawalEndpointCreatesReservedWithdrawal 验证接口解析幂等键和 ETH 金额并返回创建结果。
func TestCreateWithdrawalEndpointCreatesReservedWithdrawal(t *testing.T) {
	application, store := testWithdrawalApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawals", application.createWithdrawal)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users/1/withdrawals", strings.NewReader(`{"to_address":"0x2222222222222222222222222222222222222222","amount_eth":"0.002"}`))
	request.Header.Set("Idempotency-Key", "request-1")
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !store.called {
		t.Fatalf("status = %d, store called = %v, body = %s", response.Code, store.called, response.Body.String())
	}
	if store.request.AmountWei.String() != "2000000000000000" || store.request.IdempotencyKey != "request-1" {
		t.Fatalf("reserve request = %+v", store.request)
	}
}

// TestCreateWithdrawalEndpointRejectsMissingIdempotencyKey 验证缺少幂等键时不会创建提币。
func TestCreateWithdrawalEndpointRejectsMissingIdempotencyKey(t *testing.T) {
	application, store := testWithdrawalApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/users/{user_id}/withdrawals", application.createWithdrawal)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users/1/withdrawals", strings.NewReader(`{"to_address":"0x2222222222222222222222222222222222222222","amount_eth":"0.002"}`))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || store.called {
		t.Fatalf("status = %d, store called = %v, body = %s", response.Code, store.called, response.Body.String())
	}
}

// testWithdrawalApp 创建只装配提币接口依赖的测试应用。
func testWithdrawalApp(t *testing.T) (*App, *handlerStore) {
	t.Helper()
	store := &handlerStore{}
	service, err := withdrawal.NewService(handlerChain{}, store)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &App{withdrawals: service, logger: logger}, store
}
