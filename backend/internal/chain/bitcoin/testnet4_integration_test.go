package bitcoin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/config"
)

// TestIntegrationTestnet4RPC 验证真实 Testnet4 RPC 的网络、区块高度和区块读取能力。
func TestIntegrationTestnet4RPC(t *testing.T) {
	loadBitcoinTestEnv(t)
	network, endpoint, user, password, _, timeout, err := config.BitcoinTestSettings()
	if err != nil {
		t.Fatalf("load Bitcoin config: %v", err)
	}
	if network != "testnet4" {
		t.Skip("BITCOIN_NETWORK 不是 testnet4，跳过真实 Testnet4 RPC 测试")
	}
	if endpoint == "" {
		t.Skip("未配置 BITCOIN_RPC_URL，跳过真实 Testnet4 RPC 测试")
	}
	client, err := NewClient(
		endpoint,
		user,
		password,
		timeout,
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.VerifyNetwork(ctx, "testnet4"); err != nil {
		t.Fatalf("VerifyNetwork(testnet4) error = %v", err)
	}
	height, err := client.BlockCount(ctx)
	if err != nil {
		t.Fatalf("BlockCount() error = %v", err)
	}
	if height < 0 {
		t.Fatalf("BlockCount() = %d, want non-negative", height)
	}
	hash, err := client.BlockHash(ctx, height)
	if err != nil {
		t.Fatalf("BlockHash(%d) error = %v", height, err)
	}
	if len(hash) != 64 {
		t.Fatalf("BlockHash(%d) length = %d, want 64", height, len(hash))
	}
	block, err := client.Block(ctx, hash)
	if err != nil {
		t.Fatalf("Block(%s) error = %v", hash, err)
	}
	if block.Height != height || !strings.EqualFold(block.Hash, hash) {
		t.Fatalf("Block() height/hash = %d/%s, want %d/%s", block.Height, block.Hash, height, hash)
	}
	t.Logf("Testnet4 RPC 正常：height=%d hash=%s transactions=%d", height, hash, len(block.Tx))
}
