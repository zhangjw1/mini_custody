package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/chain/evm"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/preflight"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

// main 加载本地配置并执行只读 Sepolia ERC-20 环境预检。
func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Sepolia ERC-20 预检执行失败：", err)
		os.Exit(1)
	}
}

// run 解析命令参数、装配只读依赖并输出预检报告。
func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("erc20-preflight", flag.ContinueOnError)
	flags.SetOutput(stderr)
	externalAddress := flags.String("external-address", strings.TrimSpace(os.Getenv("E2E_EXTERNAL_ADDRESS")), "外部 Sepolia 测试地址")
	jsonOutput := flags.Bool("json", false, "使用 JSON 输出预检结果")
	timeout := flags.Duration("timeout", 45*time.Second, "预检总超时时间")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("用法：erc20-preflight [--external-address 0x...] [--json] [--timeout 45s]")
	}
	if *timeout <= 0 {
		return errors.New("预检超时时间必须大于零")
	}
	if !common.IsHexAddress(*externalAddress) || common.HexToAddress(*externalAddress) == (common.Address{}) {
		return errors.New("必须通过 --external-address 或 E2E_EXTERNAL_ADDRESS 配置有效的外部测试地址")
	}

	cfg, err := config.LoadPreflight()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.Timezone)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := postgres.NewStore(pool, cfg.Timezone)
	if err != nil {
		return err
	}
	chain, err := evm.NewFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer chain.Close()
	token, err := erc20.NewContract(chain, common.HexToAddress(cfg.ERC20.ContractAddress))
	if err != nil {
		return err
	}
	result, err := preflight.Run(ctx, chain, token, store, preflight.Config{
		Network: postgres.NetworkSepolia, ERC20Enabled: cfg.ERC20.Enabled, SweepEnabled: cfg.ERC20.SweepEnabled,
		ContractAddress: cfg.ERC20.ContractAddress, Symbol: cfg.ERC20.Symbol, Decimals: cfg.ERC20.Decimals,
		PlatformMinBalanceWei: cfg.PlatformWallet.MinETHBalanceWei, ExternalAddress: *externalAddress,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("输出 JSON 预检报告失败：%w", err)
		}
	} else if err := preflight.WriteText(stdout, result); err != nil {
		return fmt.Errorf("输出预检报告失败：%w", err)
	}
	if !result.Ready {
		return preflight.ErrNotReady
	}
	return nil
}
