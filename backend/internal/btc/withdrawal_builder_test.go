package btc

import (
	"context"
	"testing"

	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// TestBuildWithdrawalMatchesSelectionBoundaries 验证选币与构建在刚好够、找零和 dust 找零场景结果完全一致。
func TestBuildWithdrawalMatchesSelectionBoundaries(t *testing.T) {
	provider, err := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	source, err := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	if err != nil {
		t.Fatal(err)
	}
	changeAddr, err := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	if err != nil {
		t.Fatal(err)
	}

	const target int64 = 50_000
	const feeRate int64 = 2
	feeNoChange, _ := EstimateFee(1, 1, feeRate)
	feeWithChange, _ := EstimateFee(1, 2, feeRate)

	cases := []struct {
		name       string
		utxoValue  int64
		wantFee    int64
		wantChange int64
	}{
		{name: "刚好够", utxoValue: target + feeNoChange, wantFee: feeNoChange, wantChange: 0},
		{name: "需要找零", utxoValue: target + feeWithChange + DustThresholdSats + 500, wantFee: feeWithChange, wantChange: DustThresholdSats + 500},
		{name: "找零为dust并入手续费", utxoValue: target + feeWithChange + DustThresholdSats - 1, wantFee: feeWithChange + DustThresholdSats - 1, wantChange: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			utxo := UTXO{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: tc.utxoValue, ScriptPubKey: source.ScriptPubKey}
			chosen, selectedFee, err := SelectUTXOs([]UTXO{utxo}, target, feeRate)
			if err != nil {
				t.Fatalf("SelectUTXOs() error = %v", err)
			}
			raw, builtFee, changeAmount, err := BuildWithdrawal(provider, chosen, []Address{{Path: wallet.BitcoinUserPath(1)}}, source.ScriptPubKey, changeAddr.ScriptPubKey, target, feeRate)
			if err != nil {
				t.Fatalf("BuildWithdrawal() error = %v", err)
			}
			if len(raw) == 0 || selectedFee != builtFee || builtFee != tc.wantFee || changeAmount != tc.wantChange {
				t.Fatalf("selectedFee=%d builtFee=%d wantFee=%d change=%d wantChange=%d", selectedFee, builtFee, tc.wantFee, changeAmount, tc.wantChange)
			}
		})
	}
}
