package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/preflight"
)

// main 执行只读 Bitcoin Signet 环境预检。
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Bitcoin Signet 预检失败：", err)
		os.Exit(1)
	}
}

// run 从环境变量装配只读 RPC 并输出检查结果。
func run() error {
	endpoint := strings.TrimSpace(os.Getenv("BITCOIN_RPC_URL"))
	if endpoint == "" {
		return fmt.Errorf("缺少 BITCOIN_RPC_URL")
	}
	timeout := 10 * time.Second
	if value := strings.TrimSpace(os.Getenv("BITCOIN_RPC_TIMEOUT")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		timeout = parsed
	}
	client, err := bitcoin.NewClient(endpoint, os.Getenv("BITCOIN_RPC_USER"), os.Getenv("BITCOIN_RPC_PASSWORD"), timeout)
	if err != nil {
		return err
	}
	confirmations := uintEnv("BITCOIN_CONFIRMATIONS", 3)
	batch := uintEnv("BITCOIN_SCAN_BATCH_SIZE", 20)
	fee := int64(uintEnv("BITCOIN_SWEEP_FEE_RATE_SAT_VB", 2))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	network := strings.TrimSpace(os.Getenv("BITCOIN_NETWORK"))
	if network == "" {
		network = "signet"
	}
	result, err := preflight.RunBitcoin(ctx, client, preflight.BitcoinConfig{Enabled: true, Network: network, Confirmations: confirmations, BatchSize: batch, FeeRateSatVB: fee})
	if err != nil {
		return err
	}
	if err = preflight.WriteBitcoinText(os.Stdout, result); err != nil {
		return err
	}
	if !result.Ready {
		return fmt.Errorf("环境未就绪")
	}
	return nil
}

// uintEnv 读取无符号整数环境变量。
func uintEnv(name string, fallback uint64) uint64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
