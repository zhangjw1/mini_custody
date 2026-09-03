package btc

import (
	"testing"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
)

// TestScanBlockMatchesMultipleOutputsByScript 验证脚本匹配和多输出识别。
func TestScanBlockMatchesMultipleOutputsByScript(t *testing.T) {
	addresses := map[string]Address{"alice": {ID: 1, UserID: 7, ScriptPubKey: []byte{0, 20, 1, 2}}}
	block := bitcoin.Block{
		Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Height: 12,
		Tx: []bitcoin.Transaction{{TxID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: []bitcoin.Vout{
			{N: 0, Value: 1, ScriptPubKey: bitcoin.ScriptPubKey{Hex: "00140102"}},
			{N: 1, Value: 2, ScriptPubKey: bitcoin.ScriptPubKey{Hex: "00149999"}},
		}}},
	}
	got, err := ScanBlock(block, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Vout != 0 || got[0].AmountSats != 1 {
		t.Fatalf("unexpected observations: %#v", got)
	}
}
