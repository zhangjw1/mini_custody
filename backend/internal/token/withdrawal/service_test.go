package withdrawal

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

// serviceStore 在内存中记录 Token 提币创建请求并模拟幂等查询。
type serviceStore struct {
	platform     postgres.PlatformWallet
	existing     *postgres.TokenWithdrawal
	request      postgres.TokenWithdrawalRequest
	reserveErr   error
	reserveCalls int
}

// TokenWithdrawalByIdempotencyKey 返回内存中的幂等 Token 提币。
func (s *serviceStore) TokenWithdrawalByIdempotencyKey(context.Context, int64, string) (postgres.TokenWithdrawal, error) {
	if s.existing == nil {
		return postgres.TokenWithdrawal{}, postgres.ErrNotFound
	}
	return *s.existing, nil
}

// PlatformWalletByRole 返回测试平台热钱包。
func (s *serviceStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	return s.platform, nil
}

// ReserveTokenWithdrawal 记录余额占用请求并模拟数据库创建结果。
func (s *serviceStore) ReserveTokenWithdrawal(_ context.Context, request postgres.TokenWithdrawalRequest) (postgres.TokenWithdrawal, bool, error) {
	s.reserveCalls++
	s.request = request
	if s.reserveErr != nil {
		return postgres.TokenWithdrawal{}, false, s.reserveErr
	}
	item := postgres.TokenWithdrawal{
		ID: 11, IdempotencyKey: request.IdempotencyKey, UserID: request.UserID, AssetID: request.AssetID,
		PlatformWalletID: request.PlatformWalletID, ToAddress: request.ToAddress,
		AmountUnits: new(big.Int).Set(request.AmountUnits), Status: postgres.WithdrawalCreated,
	}
	s.existing = &item
	return item, true, nil
}

// TestServiceQuotesExactTokenAmountAndPlatformGas 验证 Token 金额和平台 ETH Gas 均使用整数精确计算。
func TestServiceQuotesExactTokenAmountAndPlatformGas(t *testing.T) {
	service, _, _ := newTestService(t)
	result, err := service.Quote(context.Background(), QuoteRequest{
		UserID: 3, ToAddress: "0x2222222222222222222222222222222222222222", Amount: "1.000001",
	})
	if err != nil {
		t.Fatalf("Quote() error = %v", err)
	}
	if result.AmountUnits.Cmp(big.NewInt(1_000_001)) != 0 || result.Fee.GasLimit != 50_000 ||
		result.Fee.MaxFeePerGasWei.Cmp(big.NewInt(202)) != 0 || result.Fee.ReservedFeeWei.Cmp(big.NewInt(10_100_000)) != 0 {
		t.Fatalf("quote = %+v", result)
	}
}

// TestServiceCreatesOneIdempotentTokenWithdrawal 验证相同幂等标识只占用一次用户 Token 余额。
func TestServiceCreatesOneIdempotentTokenWithdrawal(t *testing.T) {
	service, store, _ := newTestService(t)
	request := CreateRequest{IdempotencyKey: "token-request-1", UserID: 3, ToAddress: "0x2222222222222222222222222222222222222222", Amount: "2.5"}
	first, err := service.Create(context.Background(), request)
	if err != nil || !first.Created {
		t.Fatalf("first Create() created = %v, error = %v", first.Created, err)
	}
	second, err := service.Create(context.Background(), request)
	if err != nil || second.Created || second.Withdrawal.ID != first.Withdrawal.ID {
		t.Fatalf("second Create() = %+v, error = %v", second, err)
	}
	if store.reserveCalls != 1 || store.request.AmountUnits.Cmp(big.NewInt(2_500_000)) != 0 {
		t.Fatalf("reserve calls = %d, amount = %v", store.reserveCalls, store.request.AmountUnits)
	}
}

// TestServiceReturnsInsufficientUserTokenBalance 验证数据库余额不足错误不会被转换成平台 Gas 错误。
func TestServiceReturnsInsufficientUserTokenBalance(t *testing.T) {
	service, store, _ := newTestService(t)
	store.reserveErr = postgres.ErrInsufficientBalance
	_, err := service.Create(context.Background(), CreateRequest{
		IdempotencyKey: "token-request-2", UserID: 3,
		ToAddress: "0x2222222222222222222222222222222222222222", Amount: "1",
	})
	if !errors.Is(err, postgres.ErrInsufficientBalance) {
		t.Fatalf("Create() error = %v", err)
	}
}

// newTestService 创建使用固定费用和标准 Token 配置的提币服务。
func newTestService(t *testing.T) (*Service, *serviceStore, *fakeContract) {
	t.Helper()
	tokenABI, err := erc20.StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	contractAddress := common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238")
	contract := &fakeContract{address: contractAddress, tokenABI: tokenABI, balance: big.NewInt(10_000_000), gas: 50_000}
	chain := &fakeChain{header: &types.Header{BaseFee: big.NewInt(100)}, tip: big.NewInt(2), ethBalance: big.NewInt(20_000_000)}
	store := &serviceStore{platform: postgres.PlatformWallet{
		ID: 7, Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot,
		Address: "0x1111111111111111111111111111111111111111", NextNonce: new(big.Int),
	}}
	service, err := NewService(chain, contract, store, postgres.Asset{
		ID: 5, Network: postgres.NetworkSepolia, AssetType: postgres.AssetTypeERC20, Symbol: "USDC",
		ContractAddress: contractAddress.Hex(), Decimals: 6, Enabled: true,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service, store, contract
}
