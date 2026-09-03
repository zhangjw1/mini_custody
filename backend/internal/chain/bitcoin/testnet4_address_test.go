package bitcoin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/config"
)

// TestIntegrationTestnet4AddressUTXOs 查询用户提供的 Testnet4 地址余额。
func TestIntegrationTestnet4AddressUTXOs(t *testing.T) {
	loadBitcoinTestEnv(t)
	network, endpoint, user, password, address, timeout, err := config.BitcoinTestSettings()
	if err != nil {
		t.Fatalf("load Bitcoin config: %v", err)
	}
	if network != "testnet4" {
		t.Skip("BITCOIN_NETWORK 不是 testnet4，跳过 Testnet4 地址查询")
	}
	if endpoint == "" {
		t.Skip("未配置 BITCOIN_RPC_URL，跳过 Testnet4 地址查询")
	}
	if address == "" {
		t.Skip("未配置 BITCOIN_TEST_ADDRESS，跳过 Testnet4 地址查询")
	}
	client, err := NewClient(endpoint, user, password, timeout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = client.VerifyNetwork(ctx, "testnet4"); err != nil {
		t.Fatal(err)
	}
	result, err := client.ScanAddressUTXOs(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Testnet4 地址 %s: unspents=%d total_sats=%d", address, len(result.Unspents), result.TotalAmount)
	for _, item := range result.Unspents {
		t.Logf("utxo txid=%s vout=%d amount_sats=%d height=%d", item.TxID, item.Vout, item.Amount, item.Height)
	}
}
