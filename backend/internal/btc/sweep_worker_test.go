package btc

import (
	"context"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

type fakeSweepChain struct{ calls, mempoolCalls int }

// SendRawTransaction 模拟交易广播。
func (f *fakeSweepChain) SendRawTransaction(context.Context, string) (string, error) {
	f.calls++
	return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}

// RawTransaction 返回测试确认信息。
func (f *fakeSweepChain) RawTransaction(context.Context, string) (bitcoin.RawTransaction, error) {
	return bitcoin.RawTransaction{Confirmations: 3, BlockHash: "cc"}, nil
}

// Block 返回测试归集确认区块。
func (f *fakeSweepChain) Block(context.Context, string) (bitcoin.Block, error) {
	return bitcoin.Block{Height: 99}, nil
}

// TestMempoolAccept 返回测试通过结果。
func (f *fakeSweepChain) TestMempoolAccept(context.Context, string) (bitcoin.MempoolAcceptResult, error) {
	f.mempoolCalls++
	return bitcoin.MempoolAcceptResult{Allowed: true}, nil
}

type fakeSweepStore struct {
	saved, broadcasted int
	item               Sweep
}

// ListProcessableBTCSweeps 返回测试归集任务。
func (f *fakeSweepStore) ListProcessableBTCSweeps(context.Context, int) ([]Sweep, error) {
	return []Sweep{f.item}, nil
}

// SaveBTCSweepSigned 保存测试签名交易。
func (f *fakeSweepStore) SaveBTCSweepSigned(_ context.Context, id int64, raw []byte, txid string, out, fee, rate int64) (Sweep, error) {
	f.saved++
	f.item.ID = id
	f.item.RawTx = raw
	f.item.TxID = txid
	f.item.OutputValueSats = out
	f.item.FeeSats = fee
	f.item.FeeRateSatVB = rate
	f.item.Status = SweepSigned
	return f.item, nil
}

// MarkBTCSweepBroadcasted 标记测试广播状态。
func (f *fakeSweepStore) MarkBTCSweepBroadcasted(_ context.Context, id int64, txid string) (Sweep, error) {
	f.broadcasted++
	f.item.ID = id
	f.item.TxID = txid
	f.item.Status = SweepBroadcasted
	return f.item, nil
}

// CompleteBTCSweep 标记测试归集完成。
func (f *fakeSweepStore) CompleteBTCSweep(_ context.Context, _ int64, _ int64, _ int64) error {
	return nil
}

// MarkBTCSweepBroadcastUnknown 记录测试广播未知状态。
func (f *fakeSweepStore) MarkBTCSweepBroadcastUnknown(context.Context, int64, string) error {
	return nil
}

// FailBTCSweep 记录测试失败状态。
func (f *fakeSweepStore) FailBTCSweep(context.Context, int64, string, string) error { return nil }

// TestSweepWorkerReplaysPersistedSignedTransaction 验证重启后只重播已持久化交易。
func TestSweepWorkerReplaysPersistedSignedTransaction(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	chain := &fakeSweepChain{}
	store := &fakeSweepStore{item: Sweep{ID: 4, Status: SweepSigned, RawTx: []byte{1, 2, 3}, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	worker, err := NewSweepWorker(chain, store, provider, time.Second, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if chain.calls != 1 || store.saved != 0 || store.broadcasted != 1 {
		t.Fatalf("calls=%d saved=%d broadcasted=%d", chain.calls, store.saved, store.broadcasted)
	}
}

// TestSweepWorkerRecoversBroadcastUnknownWithoutRebuild 验证广播未知恢复不重新签名或预检查。
func TestSweepWorkerRecoversBroadcastUnknownWithoutRebuild(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	chain := &fakeSweepChain{}
	store := &fakeSweepStore{item: Sweep{ID: 5, Status: "BROADCAST_UNKNOWN", RawTx: []byte{4, 5, 6}, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	worker, _ := NewSweepWorker(chain, store, provider, time.Second, 2, 3, nil)
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if chain.calls != 1 || chain.mempoolCalls != 0 || store.saved != 0 {
		t.Fatalf("calls=%d mempool=%d saved=%d", chain.calls, chain.mempoolCalls, store.saved)
	}
}

// TestSweepWorkerPersistsBeforeBroadcast 验证归集先持久化再广播。
func TestSweepWorkerPersistsBeforeBroadcast(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider("tag volcano eight thank tide danger coast health above argue embrace heavy")
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	target, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	store := &fakeSweepStore{item: Sweep{ID: 9, Status: SweepCreated, From: Address{Path: wallet.BitcoinUserPath(1)}, To: Address{ScriptPubKey: target.ScriptPubKey}, UTXO: UTXO{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: 100000, ScriptPubKey: source.ScriptPubKey}}}
	chain := &fakeSweepChain{}
	worker, err := NewSweepWorker(chain, store, provider, time.Second, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if store.saved != 1 || chain.calls != 1 || store.broadcasted != 1 {
		t.Fatalf("saved=%d broadcast=%d calls=%d", store.saved, store.broadcasted, chain.calls)
	}
}
