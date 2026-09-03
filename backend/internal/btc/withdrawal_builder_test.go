package btc

import (
	"context"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"testing"
)

// TestBuildWithdrawalAddsPlatformChange 验证多输入提币找零进入平台地址。
func TestBuildWithdrawalAddsPlatformChange(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	change, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	raw, fee, changeAmount, err := BuildWithdrawal(provider, []UTXO{{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: 100000, ScriptPubKey: source.ScriptPubKey}}, []Address{{Path: wallet.BitcoinUserPath(1)}}, source.ScriptPubKey, change.ScriptPubKey, 50000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || fee <= 0 || changeAmount <= DustThresholdSats {
		t.Fatalf("raw=%d fee=%d change=%d", len(raw), fee, changeAmount)
	}
}
