package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// BitcoinReader 定义 Bitcoin Signet 预检所需的只读 RPC 能力。
type BitcoinReader interface {
	VerifyNetwork(context.Context, string) error
	BlockCount(context.Context) (int64, error)
}
type BitcoinConfig struct {
	Enabled                  bool
	Network                  string
	Confirmations, BatchSize uint64
	FeeRateSatVB             int64
}
type BitcoinResult struct {
	Ready       bool      `json:"ready"`
	Network     string    `json:"network"`
	LatestBlock int64     `json:"latest_block"`
	Checks      []Check   `json:"checks"`
	CheckedAt   time.Time `json:"checked_at"`
}

// RunBitcoin 执行不签名、不广播、不修改数据库的 Signet 预检。
func RunBitcoin(ctx context.Context, reader BitcoinReader, cfg BitcoinConfig) (BitcoinResult, error) {
	if reader == nil {
		return BitcoinResult{}, errors.New("Bitcoin 预检必须提供 RPC 客户端")
	}
	if cfg.Network == "" {
		cfg.Network = "signet"
	}
	result := BitcoinResult{Ready: true, Network: "bitcoin-" + cfg.Network, Checks: make([]Check, 0, 5), CheckedAt: time.Now()}
	add := func(name string, ok bool, message string) {
		status := checkPassed
		if !ok {
			status = checkFailed
			result.Ready = false
		}
		result.Checks = append(result.Checks, Check{Name: name, Status: status, Message: message})
	}
	add("Bitcoin 功能开关", cfg.Enabled, "BITCOIN_ENABLED")
	add("确认数", cfg.Confirmations > 0, fmt.Sprintf("confirmations=%d", cfg.Confirmations))
	add("扫描批量", cfg.BatchSize > 0 && cfg.BatchSize <= 100, fmt.Sprintf("batch_size=%d", cfg.BatchSize))
	add("归集费率", cfg.FeeRateSatVB > 0, fmt.Sprintf("fee_rate=%d sat/vB", cfg.FeeRateSatVB))
	if err := reader.VerifyNetwork(ctx, cfg.Network); err != nil {
		add("Bitcoin 网络", false, err.Error())
	} else {
		add("Bitcoin 网络", true, "chain="+cfg.Network)
	}
	height, err := reader.BlockCount(ctx)
	if err != nil {
		add("最新区块", false, err.Error())
	} else {
		result.LatestBlock = height
		add("最新区块", height >= 0, fmt.Sprintf("height=%d", height))
	}
	return result, nil
}

// WriteBitcoinText 输出便于终端阅读的 Bitcoin 预检报告。
func WriteBitcoinText(writer io.Writer, result BitcoinResult) error {
	if writer == nil {
		return errors.New("预检输出目标不能为空")
	}
	status := "通过"
	if !result.Ready {
		status = "未通过"
	}
	_, err := fmt.Fprintf(writer, "Bitcoin Signet 预检：%s，最新高度=%d\n", status, result.LatestBlock)
	return err
}
