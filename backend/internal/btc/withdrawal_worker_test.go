package btc

import (
	"context"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

type fakeWithdrawalChain struct {
	sent            int
	mempoolCalls    int
	mempoolRejected bool
	onChain         map[string]bitcoin.RawTransaction
}

// SendRawTransaction 模拟 BTC 提币广播。
func (f *fakeWithdrawalChain) SendRawTransaction(context.Context, string) (string, error) {
	f.sent++
	return "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nil
}

// TestMempoolAccept 模拟内存池预检查。
func (f *fakeWithdrawalChain) TestMempoolAccept(context.Context, string) (bitcoin.MempoolAcceptResult, error) {
	f.mempoolCalls++
	if f.mempoolRejected {
		return bitcoin.MempoolAcceptResult{Allowed: false, RejectReason: "missing-inputs"}, nil
	}
	return bitcoin.MempoolAcceptResult{Allowed: true}, nil
}

// RawTransaction 查询模拟链上或内存池交易。
func (f *fakeWithdrawalChain) RawTransaction(_ context.Context, txid string) (bitcoin.RawTransaction, error) {
	if f.onChain != nil {
		if tx, ok := f.onChain[txid]; ok {
			return tx, nil
		}
	}
	return bitcoin.RawTransaction{}, &bitcoin.RPCError{Code: -5, Message: "No such mempool or blockchain transaction"}
}

// Block 返回模拟确认区块。
func (f *fakeWithdrawalChain) Block(context.Context, string) (bitcoin.Block, error) {
	return bitcoin.Block{Height: 120}, nil
}

type fakeWithdrawalStore struct {
	item         Withdrawal
	broadcasted  int
	manualReview int
	failed       int
}

// ListProcessableBTCWithdrawals 返回待处理测试提币。
func (f *fakeWithdrawalStore) ListProcessableBTCWithdrawals(context.Context, int) ([]Withdrawal, error) {
	return []Withdrawal{f.item}, nil
}

// LockWithdrawalInputs 返回空测试输入，由测试场景控制。
func (f *fakeWithdrawalStore) LockWithdrawalInputs(context.Context, int64, int64, string) ([]UTXO, []Address, []byte, []byte, error) {
	return nil, nil, nil, nil, nil
}

// SaveBTCWithdrawalSigned 保存已签名提币测试数据。
func (f *fakeWithdrawalStore) SaveBTCWithdrawalSigned(_ context.Context, _ int64, raw []byte, _, _ int64) (Withdrawal, error) {
	f.item.RawTx = raw
	f.item.Status = WithdrawalSigned
	return f.item, nil
}

// MarkBTCWithdrawalBroadcastUnknown 标记广播未知。
func (f *fakeWithdrawalStore) MarkBTCWithdrawalBroadcastUnknown(context.Context, int64, string) error {
	f.item.Status = WithdrawalBroadcastUnknown
	return nil
}

// MarkBTCWithdrawalBroadcasted 标记广播成功。
func (f *fakeWithdrawalStore) MarkBTCWithdrawalBroadcasted(_ context.Context, _ int64, txid string) error {
	f.broadcasted++
	f.item.TxID = txid
	f.item.Status = WithdrawalBroadcasted
	return nil
}

// MarkBTCWithdrawalManualReview 标记转入人工审核。
func (f *fakeWithdrawalStore) MarkBTCWithdrawalManualReview(_ context.Context, _ int64, _, _ string) error {
	f.manualReview++
	f.item.Status = WithdrawalManualReview
	return nil
}

// CompleteBTCWithdrawal 标记测试提币完成。
func (f *fakeWithdrawalStore) CompleteBTCWithdrawal(context.Context, int64, int64, int64) error {
	return nil
}

// FailBTCWithdrawal 标记测试提币失败。
func (f *fakeWithdrawalStore) FailBTCWithdrawal(context.Context, int64, string, string) error {
	f.failed++
	return nil
}

// TestWithdrawalWorkerRecoversBroadcastedSignedWithoutRefund 验证已广播但状态为 SIGNED 崩溃恢复时直接推进为 BROADCASTED。
func TestWithdrawalWorkerRecoversBroadcastedSignedWithoutRefund(t *testing.T) {
	provider, err := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	changeAddr, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	raw, _, _, err := BuildWithdrawal(provider, []UTXO{{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: 100_000, ScriptPubKey: source.ScriptPubKey}}, []Address{{Path: wallet.BitcoinUserPath(1)}}, source.ScriptPubKey, changeAddr.ScriptPubKey, 20_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	txid, err := txHashFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	chain := &fakeWithdrawalChain{
		mempoolRejected: true,
		onChain:         map[string]bitcoin.RawTransaction{txid: {TxID: txid, Confirmations: 0}},
	}
	store := &fakeWithdrawalStore{item: Withdrawal{ID: 1, Status: WithdrawalSigned, RawTx: raw, TxID: txid}}
	worker, err := NewWithdrawalWorker(chain, store, provider, time.Second, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if store.broadcasted != 1 || store.failed != 0 || store.manualReview != 0 || chain.sent != 0 {
		t.Fatalf("broadcasted=%d failed=%d manual=%d sent=%d", store.broadcasted, store.failed, store.manualReview, chain.sent)
	}
}

// TestWithdrawalWorkerMovesRejectedSignedToManualReview 验证 SIGNED 被 mempool 拒绝且链上未发现时不退款，转入人工审核。
func TestWithdrawalWorkerMovesRejectedSignedToManualReview(t *testing.T) {
	provider, err := wallet.NewMnemonicKeyProvider(sweepTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinUserPath(1))
	changeAddr, _ := provider.BitcoinAddress(context.Background(), wallet.BitcoinTreasuryPath)
	raw, _, _, err := BuildWithdrawal(provider, []UTXO{{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, ValueSats: 100_000, ScriptPubKey: source.ScriptPubKey}}, []Address{{Path: wallet.BitcoinUserPath(1)}}, source.ScriptPubKey, changeAddr.ScriptPubKey, 20_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	txid, _ := txHashFromRaw(raw)
	chain := &fakeWithdrawalChain{mempoolRejected: true}
	store := &fakeWithdrawalStore{item: Withdrawal{ID: 2, Status: WithdrawalSigned, RawTx: raw, TxID: txid}}
	worker, err := NewWithdrawalWorker(chain, store, provider, time.Second, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Process(context.Background(), store.item); err != nil {
		t.Fatal(err)
	}
	if store.manualReview != 1 || store.failed != 0 {
		t.Fatalf("manual=%d failed=%d", store.manualReview, store.failed)
	}
}
