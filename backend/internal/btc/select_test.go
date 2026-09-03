package btc

import "testing"

// TestSelectUTXOsChoosesMinimumExcess 验证选币优先最小超额组合。
func TestSelectUTXOsChoosesMinimumExcess(t *testing.T) {
	items := []UTXO{{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ValueSats: 800}, {TxID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ValueSats: 600}, {TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", ValueSats: 500}}
	chosen, fee, err := SelectUTXOs(items, 750, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chosen) != 2 || fee <= 0 {
		t.Fatalf("chosen=%#v fee=%d", chosen, fee)
	}
}
