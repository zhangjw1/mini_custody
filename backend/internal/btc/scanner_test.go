package btc

import (
	"context"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
)

type fakeScannerChain struct {
	tip    int64
	hashes map[int64]string
	blocks map[string]bitcoin.Block
}

// BlockCount 返回测试链高度。
func (f *fakeScannerChain) BlockCount(context.Context) (int64, error) { return f.tip, nil }

// BlockHash 返回测试区块哈希。
func (f *fakeScannerChain) BlockHash(_ context.Context, h int64) (string, error) {
	return f.hashes[h], nil
}

// Block 返回测试区块内容。
func (f *fakeScannerChain) Block(_ context.Context, hash string) (bitcoin.Block, error) {
	return f.blocks[hash], nil
}

type fakeScannerStore struct {
	addresses  map[string]Address
	checkpoint int64
	cpHash     string
	confirming []ConfirmingDeposit
	credited   map[int64]int64
	orphaned   map[int64]bool
	rewinds    int
	errors     []string
}

// ListBTCAddresses 返回测试地址索引。
func (f *fakeScannerStore) ListBTCAddresses(context.Context) (map[string]Address, error) {
	return f.addresses, nil
}

// BTCCheckpoint 返回测试检查点。
func (f *fakeScannerStore) BTCCheckpoint(context.Context) (int64, string, error) {
	return f.checkpoint, f.cpHash, nil
}

// RecordBTCDepositsAndCheckpoint 记录扫描结果并推进测试检查点。
func (f *fakeScannerStore) RecordBTCDepositsAndCheckpoint(_ context.Context, _ []DepositObservation, cp Checkpoint) (int, error) {
	f.checkpoint = cp.LastScannedBlock
	f.cpHash = cp.LastScannedHash
	return 0, nil
}

// ListConfirmingBTCDeposits 返回测试待确认充值。
func (f *fakeScannerStore) ListConfirmingBTCDeposits(context.Context, int) ([]ConfirmingDeposit, error) {
	return f.confirming, nil
}

// UpdateBTCDepositConfirmations 记录确认数更新。
func (f *fakeScannerStore) UpdateBTCDepositConfirmations(context.Context, int64, int64) error {
	return nil
}

// CreditBTCDeposit 记录测试充值入账。
func (f *fakeScannerStore) CreditBTCDeposit(_ context.Context, id, conf int64) error {
	if f.credited == nil {
		f.credited = make(map[int64]int64)
	}
	f.credited[id] = conf
	return nil
}

// RewindBTCCheckpoint 记录测试检查点回退。
func (f *fakeScannerStore) RewindBTCCheckpoint(_ context.Context, height int64, hash string) error {
	f.rewinds++
	f.checkpoint = height
	f.cpHash = hash
	return nil
}

// OrphanBTCDeposit 记录测试孤块充值冲正。
func (f *fakeScannerStore) OrphanBTCDeposit(_ context.Context, id int64) error {
	if f.orphaned == nil {
		f.orphaned = make(map[int64]bool)
	}
	f.orphaned[id] = true
	return nil
}

// RecordBTCWorkerError 记录测试错误码。
func (f *fakeScannerStore) RecordBTCWorkerError(_ context.Context, _ string, _ string, _ *int64, code, _ string) error {
	f.errors = append(f.errors, code)
	return nil
}

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

// TestScannerConfirmIsolatesOrphanedDepositAndCreditsOthers 验证单笔孤块充值不会阻塞同批次其他合法 BTC 充值入账。
func TestScannerConfirmIsolatesOrphanedDepositAndCreditsOthers(t *testing.T) {
	chain := &fakeScannerChain{
		tip: 12,
		hashes: map[int64]string{
			7:  "7777",
			10: "aaaa",
			12: "cccc",
		},
		blocks: map[string]bitcoin.Block{},
	}
	store := &fakeScannerStore{
		addresses:  map[string]Address{},
		checkpoint: 12,
		cpHash:     "cccc",
		confirming: []ConfirmingDeposit{
			{ID: 1, BlockHeight: 10, BlockHash: "dead"},
			{ID: 2, BlockHeight: 10, BlockHash: "aaaa"},
		},
	}
	scanner, err := NewScanner(chain, store, store.addresses, 10, 3, 10, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if !store.orphaned[1] || store.credited[1] != 0 {
		t.Fatalf("deposit 1 orphaned=%v credited=%d, want orphaned without credit", store.orphaned[1], store.credited[1])
	}
	if store.credited[2] != 3 {
		t.Fatalf("deposit 2 credited=%d, want 3", store.credited[2])
	}
	if store.rewinds != 1 || len(store.errors) == 0 {
		t.Fatalf("rewinds=%d errors=%v", store.rewinds, store.errors)
	}
}
