package btc

import (
	"context"
	"errors"
	"testing"

	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const sweepTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// TestBuildSweepPreservesValueAndCanonicalTxID 验证归集金额守恒与规范 txid。
func TestBuildSweepPreservesValueAndCanonicalTxID(t *testing.T) {
	provider, err := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	target, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	utxo := UTXO{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 1, ValueSats: 100000, ScriptPubKey: source.ScriptPubKey}
	sweep, err := BuildSweep(provider, wallet.BitcoinUserPath(1), utxo, Address{ScriptPubKey: target.ScriptPubKey}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSweep(sweep); err != nil {
		t.Fatal(err)
	}
	if sweep.FeeSats != 220 || sweep.OutputValueSats != 99780 || len(sweep.TxID) != 64 {
		t.Fatalf("unexpected sweep: %#v", sweep)
	}
}

// TestBuildSweepRejectsDust 验证扣费后的尘埃输出不会广播。
func TestBuildSweepRejectsDust(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	target, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	_, err := BuildSweep(provider, wallet.BitcoinUserPath(1), UTXO{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: 400, ScriptPubKey: source.ScriptPubKey}, Address{ScriptPubKey: target.ScriptPubKey}, 1)
	if !errors.Is(err, ErrSweepDust) {
		t.Fatalf("error = %v, want dust", err)
	}
}
