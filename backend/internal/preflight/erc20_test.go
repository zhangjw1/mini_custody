package preflight

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

const (
	testContract = "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"
	testPlatform = "0x1111111111111111111111111111111111111111"
	testExternal = "0x2222222222222222222222222222222222222222"
)

// fakeChain 为预检测试提供确定性链信息和 ETH 余额。
type fakeChain struct {
	chainID *big.Int
	block   uint64
	eth     map[common.Address]*big.Int
	err     error
}

// ChainID 返回测试 Chain ID。
func (f *fakeChain) ChainID(context.Context) (*big.Int, error) {
	if f.err != nil {
		return nil, f.err
	}
	return new(big.Int).Set(f.chainID), nil
}

// BlockNumber 返回测试最新区块高度。
func (f *fakeChain) BlockNumber(context.Context) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.block, nil
}

// BalanceAt 返回指定测试地址的 ETH 余额。
func (f *fakeChain) BalanceAt(_ context.Context, address common.Address, _ *big.Int) (*big.Int, error) {
	if f.err != nil {
		return nil, f.err
	}
	value := f.eth[address]
	if value == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(value), nil
}

// fakeToken 为预检测试提供合约元数据和 Token 余额。
type fakeToken struct {
	balances map[common.Address]*big.Int
	err      error
}

// Validate 返回匹配预期的测试 Token 元数据。
func (f *fakeToken) Validate(_ context.Context, symbol string, decimals uint8) (erc20.Metadata, error) {
	if f.err != nil {
		return erc20.Metadata{}, f.err
	}
	return erc20.Metadata{Symbol: symbol, Decimals: decimals}, nil
}

// BalanceOf 返回指定测试地址的 Token 最小单位余额。
func (f *fakeToken) BalanceOf(_ context.Context, address common.Address) (*big.Int, error) {
	if f.err != nil {
		return nil, f.err
	}
	value := f.balances[address]
	if value == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(value), nil
}

// fakeStore 为预检测试提供资产和平台钱包记录。
type fakeStore struct {
	asset    postgres.Asset
	platform postgres.PlatformWallet
	err      error
}

// AssetBySymbol 返回测试资产配置。
func (f *fakeStore) AssetBySymbol(context.Context, string, string) (postgres.Asset, error) {
	if f.err != nil {
		return postgres.Asset{}, f.err
	}
	return f.asset, nil
}

// PlatformWalletByRole 返回测试平台热钱包。
func (f *fakeStore) PlatformWalletByRole(context.Context, string, string) (postgres.PlatformWallet, error) {
	if f.err != nil {
		return postgres.PlatformWallet{}, f.err
	}
	return f.platform, nil
}

// TestRunReportsReadyEnvironment 验证完整配置和充足余额可以通过只读预检。
func TestRunReportsReadyEnvironment(t *testing.T) {
	chain, token, store, cfg := readyFixture()
	result, err := Run(context.Background(), chain, token, store, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Ready || result.ChainID != "11155111" || result.LatestBlock != 9_000_000 {
		t.Fatalf("result = %+v", result)
	}
	if result.PlatformWallet == nil || result.PlatformWallet.Token != "250" || result.ExternalWallet == nil || result.ExternalWallet.Token != "20" {
		t.Fatalf("balances = platform %+v, external %+v", result.PlatformWallet, result.ExternalWallet)
	}
	for _, check := range result.Checks {
		if check.Status != checkPassed {
			t.Fatalf("check = %+v", check)
		}
	}
}

// TestRunReportsAllInsufficientBalances 验证余额不足时报告失败且仍返回其他检查结果。
func TestRunReportsAllInsufficientBalances(t *testing.T) {
	chain, token, store, cfg := readyFixture()
	chain.eth[common.HexToAddress(testPlatform)] = big.NewInt(1)
	chain.eth[common.HexToAddress(testExternal)] = new(big.Int)
	token.balances[common.HexToAddress(testPlatform)] = new(big.Int)
	token.balances[common.HexToAddress(testExternal)] = new(big.Int)
	result, err := Run(context.Background(), chain, token, store, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Ready {
		t.Fatal("result.Ready = true, want false")
	}
	failed := make([]string, 0)
	for _, check := range result.Checks {
		if check.Status == checkFailed {
			failed = append(failed, check.Name)
		}
	}
	for _, name := range []string{"平台热钱包 ETH 储备", "平台热钱包 Token 库存", "外部测试地址 ETH", "外部测试地址 Token"} {
		if !contains(failed, name) {
			t.Fatalf("failed checks = %v, missing %s", failed, name)
		}
	}
}

// TestRunPreservesSafeRPCError 验证 RPC 查询失败会形成失败项而不会中断其余报告。
func TestRunPreservesSafeRPCError(t *testing.T) {
	chain, token, store, cfg := readyFixture()
	chain.err = errors.New("RPC 请求超时")
	result, err := Run(context.Background(), chain, token, store, cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Ready || len(result.Checks) < 5 {
		t.Fatalf("result = %+v", result)
	}
}

// TestWriteTextUsesChineseStatus 验证终端报告使用中文通过和失败状态。
func TestWriteTextUsesChineseStatus(t *testing.T) {
	var output bytes.Buffer
	result := Result{Ready: false, Checks: []Check{{Name: "测试检查", Status: checkFailed, Message: "余额不足"}}}
	if err := WriteText(&output, result); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	if !strings.Contains(output.String(), "预检：未通过") || !strings.Contains(output.String(), "[失败] 检查项 测试检查：余额不足") {
		t.Fatalf("output = %q", output.String())
	}
}

// readyFixture 创建余额充足的预检测试夹具。
func readyFixture() (*fakeChain, *fakeToken, *fakeStore, Config) {
	platform := common.HexToAddress(testPlatform)
	external := common.HexToAddress(testExternal)
	chain := &fakeChain{chainID: big.NewInt(evm.SepoliaChainID), block: 9_000_000, eth: map[common.Address]*big.Int{
		platform: big.NewInt(20_000_000_000_000_000), external: big.NewInt(1_000_000_000_000_000),
	}}
	token := &fakeToken{balances: map[common.Address]*big.Int{
		platform: big.NewInt(250_000_000), external: big.NewInt(20_000_000),
	}}
	store := &fakeStore{
		asset:    postgres.Asset{Network: postgres.NetworkSepolia, AssetType: postgres.AssetTypeERC20, Symbol: "USDC", ContractAddress: testContract, Decimals: 6, Enabled: true},
		platform: postgres.PlatformWallet{Network: postgres.NetworkSepolia, Role: postgres.PlatformRoleHot, Address: testPlatform, NextNonce: new(big.Int)},
	}
	cfg := Config{
		Network: postgres.NetworkSepolia, ERC20Enabled: true, SweepEnabled: true, ContractAddress: testContract,
		Symbol: "USDC", Decimals: 6, PlatformMinBalanceWei: big.NewInt(10_000_000_000_000_000), ExternalAddress: testExternal,
		Now: func() time.Time { return time.Date(2026, 8, 16, 16, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)) },
	}
	return chain, token, store, cfg
}

// contains 判断字符串切片是否包含目标值。
func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
