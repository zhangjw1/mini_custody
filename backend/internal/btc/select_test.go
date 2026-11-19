package btc

import (
	"testing"
)

// TestSelectUTXOsBoundaries 验证选币覆盖刚好够、差 1 sat、需要找零以及找零为 dust 四种边界情况。
func TestSelectUTXOsBoundaries(t *testing.T) {
	const feeRate int64 = 2
	const target int64 = 1000

	feeNoChange, err := EstimateFee(1, 1, feeRate)
	if err != nil {
		t.Fatal(err)
	}
	feeWithChange, err := EstimateFee(1, 2, feeRate)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("刚好够无找零", func(t *testing.T) {
		items := []UTXO{{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: target + feeNoChange}}
		chosen, fee, err := SelectUTXOs(items, target, feeRate)
		if err != nil {
			t.Fatalf("SelectUTXOs() error = %v", err)
		}
		if len(chosen) != 1 || fee != feeNoChange {
			t.Fatalf("chosen=%d fee=%d, want 1 and %d", len(chosen), fee, feeNoChange)
		}
	})

	t.Run("差1sat失败", func(t *testing.T) {
		items := []UTXO{{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: target + feeNoChange - 1}}
		if _, _, err := SelectUTXOs(items, target, feeRate); err == nil {
			t.Fatal("expected insufficient UTXO error when short by 1 sat")
		}
	})

	t.Run("找零达到dust时按双输出算费", func(t *testing.T) {
		exactForChange := target + feeWithChange + DustThresholdSats
		items := []UTXO{{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: exactForChange}}
		chosen, fee, err := SelectUTXOs(items, target, feeRate)
		if err != nil {
			t.Fatalf("SelectUTXOs() error = %v", err)
		}
		if len(chosen) != 1 || fee != feeWithChange {
			t.Fatalf("chosen=%d fee=%d, want 1 and %d", len(chosen), fee, feeWithChange)
		}
	})

	t.Run("找零低于dust时并入手续费", func(t *testing.T) {
		dustRemainder := target + feeWithChange + DustThresholdSats - 1
		items := []UTXO{{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: dustRemainder}}
		chosen, fee, err := SelectUTXOs(items, target, feeRate)
		if err != nil {
			t.Fatalf("SelectUTXOs() error = %v", err)
		}
		wantFee := dustRemainder - target
		if len(chosen) != 1 || fee != wantFee {
			t.Fatalf("chosen=%d fee=%d, want 1 and %d", len(chosen), fee, wantFee)
		}
	})

	t.Run("提币金额低于dust被拒绝", func(t *testing.T) {
		items := []UTXO{{ID: 1, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: 10_000}}
		if _, _, err := SelectUTXOs(items, DustThresholdSats-1, feeRate); err == nil {
			t.Fatal("expected dust rejection")
		}
	})
}
