package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

const (
	checkPassed = "PASSED"
	checkFailed = "FAILED"
)

var ErrNotReady = errors.New("Sepolia ERC-20 端到端环境尚未就绪")

// ChainReader 定义预检所需的只读 Sepolia RPC 能力。
type ChainReader interface {
	ChainID(ctx context.Context) (*big.Int, error)
	BlockNumber(ctx context.Context) (uint64, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
}

// TokenReader 定义预检所需的只读 ERC-20 合约能力。
type TokenReader interface {
	Validate(ctx context.Context, expectedSymbol string, expectedDecimals uint8) (erc20.Metadata, error)
	BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error)
}

// StoreReader 定义预检所需的只读资产和平台钱包查询。
type StoreReader interface {
	AssetBySymbol(ctx context.Context, network, symbol string) (postgres.Asset, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
}

// Config 描述 Sepolia ERC-20 端到端验收的预期配置。
type Config struct {
	Network               string
	ERC20Enabled          bool
	SweepEnabled          bool
	ContractAddress       string
	Symbol                string
	Decimals              uint8
	PlatformMinBalanceWei *big.Int
	ExternalAddress       string
	Now                   func() time.Time
}

// Check 描述一项独立的只读预检结果。
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// AddressBalance 描述地址在预检时刻的 ETH 和 Token 链上余额。
type AddressBalance struct {
	Address    string `json:"address"`
	ETHWei     string `json:"eth_wei"`
	ETH        string `json:"eth"`
	TokenUnits string `json:"token_units"`
	Token      string `json:"token"`
}

// Result 描述完整的 Sepolia ERC-20 只读预检报告。
type Result struct {
	Ready          bool            `json:"ready"`
	Network        string          `json:"network"`
	ChainID        string          `json:"chain_id,omitempty"`
	LatestBlock    uint64          `json:"latest_block,omitempty"`
	Contract       string          `json:"contract"`
	Symbol         string          `json:"symbol"`
	Decimals       uint8           `json:"decimals"`
	PlatformWallet *AddressBalance `json:"platform_wallet,omitempty"`
	ExternalWallet *AddressBalance `json:"external_wallet,omitempty"`
	Checks         []Check         `json:"checks"`
	CheckedAt      time.Time       `json:"checked_at"`
}

// Run 执行不签名、不广播、不修改数据库的 Sepolia ERC-20 环境预检。
func Run(ctx context.Context, chain ChainReader, token TokenReader, store StoreReader, cfg Config) (Result, error) {
	if chain == nil || token == nil || store == nil {
		return Result{}, errors.New("预检必须提供链客户端、Token 合约和数据访问对象")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PlatformMinBalanceWei == nil || cfg.PlatformMinBalanceWei.Sign() < 0 {
		return Result{}, errors.New("平台热钱包最低 ETH 余额配置无效")
	}
	if cfg.Network == "" || cfg.Symbol == "" || cfg.Decimals == 0 || !common.IsHexAddress(cfg.ContractAddress) {
		return Result{}, errors.New("ERC-20 预检配置无效")
	}
	if !common.IsHexAddress(cfg.ExternalAddress) || common.HexToAddress(cfg.ExternalAddress) == (common.Address{}) {
		return Result{}, errors.New("外部测试地址无效")
	}

	result := Result{
		Ready: true, Network: cfg.Network, Contract: common.HexToAddress(cfg.ContractAddress).Hex(),
		Symbol: cfg.Symbol, Decimals: cfg.Decimals, Checks: make([]Check, 0, 12), CheckedAt: cfg.Now(),
	}
	result.addBooleanCheck("ERC-20 功能开关", cfg.ERC20Enabled, "ERC20_ENABLED=true", "ERC20_ENABLED 尚未开启")
	result.addBooleanCheck("自动归集开关", cfg.SweepEnabled, "ERC20_SWEEP_ENABLED=true", "ERC20_SWEEP_ENABLED 尚未开启")

	chainID, err := chain.ChainID(ctx)
	if err != nil {
		result.addFailure("Sepolia Chain ID", err.Error())
	} else {
		result.ChainID = chainID.String()
		result.addBooleanCheck("Sepolia Chain ID", chainID.Cmp(big.NewInt(evm.SepoliaChainID)) == 0,
			fmt.Sprintf("Chain ID=%s", chainID), fmt.Sprintf("Chain ID=%s，不是 Sepolia", chainID))
	}
	latestBlock, err := chain.BlockNumber(ctx)
	if err != nil {
		result.addFailure("Sepolia 最新区块", err.Error())
	} else {
		result.LatestBlock = latestBlock
		result.addBooleanCheck("Sepolia 最新区块", latestBlock > 0,
			fmt.Sprintf("最新区块=%d", latestBlock), "RPC 返回的最新区块高度无效")
	}

	metadata, err := token.Validate(ctx, cfg.Symbol, cfg.Decimals)
	if err != nil {
		result.addFailure("ERC-20 合约元数据", err.Error())
	} else {
		result.addSuccess("ERC-20 合约元数据", fmt.Sprintf("symbol=%s，decimals=%d", metadata.Symbol, metadata.Decimals))
	}
	asset, err := store.AssetBySymbol(ctx, cfg.Network, cfg.Symbol)
	if err != nil {
		result.addFailure("数据库资产配置", err.Error())
	} else {
		matches := asset.Enabled && asset.AssetType == postgres.AssetTypeERC20 && asset.Decimals == cfg.Decimals &&
			strings.EqualFold(asset.ContractAddress, cfg.ContractAddress)
		result.addBooleanCheck("数据库资产配置", matches, "数据库资产与链上配置一致", "数据库资产与启动配置不一致或未启用")
	}

	platform, err := store.PlatformWalletByRole(ctx, cfg.Network, postgres.PlatformRoleHot)
	if err != nil {
		result.addFailure("平台热钱包记录", err.Error())
	} else if !common.IsHexAddress(platform.Address) || common.HexToAddress(platform.Address) == (common.Address{}) {
		result.addFailure("平台热钱包记录", "数据库平台热钱包地址无效")
	} else {
		result.addSuccess("平台热钱包记录", common.HexToAddress(platform.Address).Hex())
		balance := inspectAddress(ctx, &result, chain, token, common.HexToAddress(platform.Address), cfg.Symbol, cfg.Decimals, "平台热钱包")
		result.PlatformWallet = &balance
		if ethWei, ok := new(big.Int).SetString(balance.ETHWei, 10); ok {
			result.addBooleanCheck("平台热钱包 ETH 储备", ethWei.Cmp(cfg.PlatformMinBalanceWei) >= 0,
				"平台 ETH 余额达到配置阈值", "平台 ETH 余额低于 PLATFORM_MIN_ETH_BALANCE_WEI")
		}
		if tokenUnits, ok := new(big.Int).SetString(balance.TokenUnits, 10); ok {
			result.addBooleanCheck("平台热钱包 Token 库存", tokenUnits.Sign() > 0,
				"平台 Token 库存可用于提币", "平台 Token 库存为零，无法验收 Token 提币")
		}
	}

	external := common.HexToAddress(cfg.ExternalAddress)
	externalBalance := inspectAddress(ctx, &result, chain, token, external, cfg.Symbol, cfg.Decimals, "外部测试地址")
	result.ExternalWallet = &externalBalance
	if ethWei, ok := new(big.Int).SetString(externalBalance.ETHWei, 10); ok {
		result.addBooleanCheck("外部测试地址 ETH", ethWei.Sign() > 0,
			"外部地址有 ETH 可支付充值 Gas", "外部地址 ETH 为零，无法发送 Token 充值")
	}
	if tokenUnits, ok := new(big.Int).SetString(externalBalance.TokenUnits, 10); ok {
		result.addBooleanCheck("外部测试地址 Token", tokenUnits.Sign() > 0,
			"外部地址有 Token 可用于充值", "外部地址 Token 为零，无法执行充值验收")
	}
	return result, nil
}

// WriteText 将预检报告输出为便于终端阅读的中文文本。
func WriteText(writer io.Writer, result Result) error {
	if writer == nil {
		return errors.New("预检输出目标不能为空")
	}
	status := "通过"
	if !result.Ready {
		status = "未通过"
	}
	if _, err := fmt.Fprintf(writer, "Sepolia ERC-20 预检：%s\n", status); err != nil {
		return err
	}
	for _, check := range result.Checks {
		label := "通过"
		if check.Status == checkFailed {
			label = "失败"
		}
		if _, err := fmt.Fprintf(writer, "[%s] 检查项 %s：%s\n", label, check.Name, check.Message); err != nil {
			return err
		}
	}
	return nil
}

// inspectAddress 查询并格式化一个地址的 ETH 和 Token 链上余额。
func inspectAddress(ctx context.Context, result *Result, chain ChainReader, token TokenReader, address common.Address, symbol string, decimals uint8, label string) AddressBalance {
	balance := AddressBalance{Address: address.Hex()}
	ethWei, err := chain.BalanceAt(ctx, address, nil)
	if err != nil {
		result.addFailure(label+" ETH 余额查询", err.Error())
	} else if eth, formatErr := amount.FormatETH(ethWei); formatErr != nil {
		result.addFailure(label+" ETH 余额查询", formatErr.Error())
	} else {
		balance.ETHWei = ethWei.String()
		balance.ETH = eth
		result.addSuccess(label+" ETH 余额查询", fmt.Sprintf("%s ETH", eth))
	}
	tokenUnits, err := token.BalanceOf(ctx, address)
	if err != nil {
		result.addFailure(label+" Token 余额查询", err.Error())
	} else if formatted, formatErr := amount.FormatDecimal(tokenUnits, decimals); formatErr != nil {
		result.addFailure(label+" Token 余额查询", formatErr.Error())
	} else {
		balance.TokenUnits = tokenUnits.String()
		balance.Token = formatted
		result.addSuccess(label+" Token 余额查询", fmt.Sprintf("%s %s", formatted, symbol))
	}
	return balance
}

// addSuccess 向报告追加一项通过检查。
func (r *Result) addSuccess(name, message string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: checkPassed, Message: message})
}

// addFailure 向报告追加一项失败检查并标记环境未就绪。
func (r *Result) addFailure(name, message string) {
	r.Ready = false
	r.Checks = append(r.Checks, Check{Name: name, Status: checkFailed, Message: message})
}

// addBooleanCheck 根据条件向报告追加通过或失败检查。
func (r *Result) addBooleanCheck(name string, passed bool, successMessage, failureMessage string) {
	if passed {
		r.addSuccess(name, successMessage)
		return
	}
	r.addFailure(name, failureMessage)
}
