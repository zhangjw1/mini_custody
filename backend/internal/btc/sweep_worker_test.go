package btc

import (
	"context"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

type fakeSweepStore struct {
	saved, broadcasted, manualReview, failed int
	item                                     Sweep
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

// MarkBTCSweepManualReview 标记归集进入人工审核。
func (f *fakeSweepStore) MarkBTCSweepManualReview(_ context.Context, _ int64, _, _ string) error {
	f.manualReview++
	f.item.Status = SweepManualReview
	return nil
}

// FailBTCSweep 记录测试失败状态。
func (f *fakeSweepStore) FailBTCSweep(context.Context, int64, string, string) error {
	f.failed++
	return nil
}

// TestSweepWorkerReplaysPersistedSignedTransaction 验证重启后只重播已持久化交易。
func TestSweepWorkerReplaysPersistedSignedTransaction(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	chain := &testBitcoinHTTPChain{}
	store := &fakeSweepStore{item: Sweep{ID: 4, Status: SweepSigned, RawTx: []byte{1, 2, 3}, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	worker, err := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if chain.sent != 1 || store.saved != 0 || store.broadcasted != 1 {
		t.Fatalf("calls=%d saved=%d broadcasted=%d", chain.sent, store.saved, store.broadcasted)
	}
}

// TestSweepWorkerRecoversBroadcastUnknownWithoutRebuild 验证广播未知恢复不重新签名或预检查。
func TestSweepWorkerRecoversBroadcastUnknownWithoutRebuild(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	chain := &testBitcoinHTTPChain{}
	store := &fakeSweepStore{item: Sweep{ID: 5, Status: SweepBroadcastUnknown, RawTx: []byte{4, 5, 6}, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	worker, _ := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if chain.sent != 1 || chain.mempoolCalls != 0 || store.saved != 0 {
		t.Fatalf("calls=%d mempool=%d saved=%d", chain.sent, chain.mempoolCalls, store.saved)
	}
}

// TestSweepWorkerRecoversAlreadyBroadcastedSigned 验证已上链但状态为 SIGNED 时直接推进到 BROADCASTED。
func TestSweepWorkerRecoversAlreadyBroadcastedSigned(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	txid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	chain := &testBitcoinHTTPChain{
		onChain: map[string]bitcoin.RawTransaction{txid: {TxID: txid, Confirmations: 1}},
	}
	store := &fakeSweepStore{item: Sweep{ID: 7, Status: SweepSigned, RawTx: []byte{4, 5, 6}, TxID: txid}}
	worker, _ := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if chain.sent != 0 || chain.mempoolCalls != 0 || store.broadcasted != 1 {
		t.Fatalf("calls=%d mempool=%d broadcasted=%d", chain.sent, chain.mempoolCalls, store.broadcasted)
	}
}

// TestSweepWorkerMovesRejectedSignedToManualReview 验证 SIGNED 被 mempool 拒绝且链上无记录时转入人工审核而不是自动失败。
func TestSweepWorkerMovesRejectedSignedToManualReview(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	chain := &testBitcoinHTTPChain{mempoolRejected: true}
	store := &fakeSweepStore{item: Sweep{ID: 8, Status: SweepSigned, RawTx: []byte{4, 5, 6}, TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	worker, _ := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if store.manualReview != 1 || store.failed != 0 {
		t.Fatalf("manualReview=%d failed=%d", store.manualReview, store.failed)
	}
}

// TestSweepWorkerPersistsBeforeBroadcast 验证归集先持久化再广播。
func TestSweepWorkerPersistsBeforeBroadcast(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider("tag volcano eight thank tide danger coast health above argue embrace heavy")
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	target, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	store := &fakeSweepStore{item: Sweep{ID: 9, Status: SweepCreated, From: Address{Path: wallet.BitcoinUserPath(1)}, To: Address{ScriptPubKey: target.ScriptPubKey}, UTXO: UTXO{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: 100000, ScriptPubKey: source.ScriptPubKey}}}
	chain := &testBitcoinHTTPChain{}
	worker, err := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if store.saved != 1 || chain.sent != 1 || store.broadcasted != 1 {
		t.Fatalf("saved=%d broadcast=%d calls=%d", store.saved, store.broadcasted, chain.sent)
	}
}

// TestSweepWorkerFailsDustUTXOAndStopsRetrying 验证金额不足支付手续费或低于 dust 时调用 FailBTCSweep 收口不再阻塞重试。
func TestSweepWorkerFailsDustUTXOAndStopsRetrying(t *testing.T) {
	provider, _ := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	target, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	chain := &testBitcoinHTTPChain{}

	for _, sats := range []int64{100, 350} {
		store := &fakeSweepStore{item: Sweep{
			ID:     12,
			Status: SweepCreated,
			From:   Address{Path: wallet.BitcoinUserPath(1)},
			To:     Address{ScriptPubKey: target.ScriptPubKey},
			UTXO:   UTXO{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: sats, ScriptPubKey: source.ScriptPubKey},
		}}
		worker, err := NewSweepWorker(chain.client(t), store, provider, time.Second, 2, 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.Process(context.Background(), store.item); err != nil {
			t.Fatalf("sats=%d Process() error = %v, want nil after FailBTCSweep", sats, err)
		}
		if store.failed != 1 {
			t.Fatalf("sats=%d failed=%d, want 1", sats, store.failed)
		}
	}
}
