package btc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/chain/bitcoin"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// testBitcoinHTTPChain 使用 httptest.Server 和真实 bitcoin.Client 模拟 Bitcoin Core JSON-RPC 节点，避免 fake 直接构造 *RPCError。
type testBitcoinHTTPChain struct {
	sent            int
	mempoolCalls    int
	mempoolRejected bool
	onChain         map[string]bitcoin.RawTransaction
	blockHeight     int64
}

// client 创建指向本地 httptest Bitcoin RPC 服务的真实 *bitcoin.Client。
func (c *testBitcoinHTTPChain) client(t *testing.T) *bitcoin.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode RPC request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "getrawtransaction":
			txid, _ := req.Params[0].(string)
			if c.onChain != nil {
				if tx, ok := c.onChain[txid]; ok {
					_ = json.NewEncoder(w).Encode(map[string]any{"result": tx, "error": nil, "id": req.ID})
					return
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": nil,
				"error":  map[string]any{"code": -5, "message": "No such mempool or blockchain transaction"},
				"id":     req.ID,
			})
		case "testmempoolaccept":
			c.mempoolCalls++
			res := map[string]any{"allowed": true}
			if c.mempoolRejected {
				res = map[string]any{"allowed": false, "reject-reason": "missing-inputs"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": []any{res}, "error": nil, "id": req.ID})
		case "sendrawtransaction":
			c.sent++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				"error":  nil,
				"id":     req.ID,
			})
		case "getblock":
			h := c.blockHeight
			if h == 0 {
				h = 120
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"height": h}, "error": nil, "id": req.ID})
		default:
			t.Fatalf("unexpected RPC method: %s", req.Method)
		}
	}))
	t.Cleanup(server.Close)
	client, err := bitcoin.NewClient(server.URL, "", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
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
	chain := &testBitcoinHTTPChain{
		mempoolRejected: true,
		onChain:         map[string]bitcoin.RawTransaction{txid: {TxID: txid, Confirmations: 0}},
	}
	store := &fakeWithdrawalStore{item: Withdrawal{ID: 1, Status: WithdrawalSigned, RawTx: raw, TxID: txid}}
	worker, err := NewWithdrawalWorker(chain.client(t), store, provider, time.Second, 3, nil)
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
	chain := &testBitcoinHTTPChain{mempoolRejected: true}
	store := &fakeWithdrawalStore{item: Withdrawal{ID: 2, Status: WithdrawalSigned, RawTx: raw, TxID: txid}}
	worker, err := NewWithdrawalWorker(chain.client(t), store, provider, time.Second, 3, nil)
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

// TestWithdrawalAndSweepWorkersBroadcastViaRealClientOnHTTP500NotFound 验证使用真实 HTTP 客户端且节点对 getrawtransaction 返回 HTTP 500 + code -5 时提币与归集均可正常广播。
func TestWithdrawalAndSweepWorkersBroadcastViaRealClientOnHTTP500NotFound(t *testing.T) {
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

	var sendCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "getrawtransaction":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": nil,
				"error":  map[string]any{"code": -5, "message": "No such mempool or blockchain transaction"},
				"id":     req.ID,
			})
		case "testmempoolaccept":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": []map[string]any{{"txid": txid, "allowed": true}},
				"error":  nil,
				"id":     req.ID,
			})
		case "sendrawtransaction":
			sendCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": txid,
				"error":  nil,
				"id":     req.ID,
			})
		default:
			t.Fatalf("unexpected RPC method: %s", req.Method)
		}
	}))
	defer server.Close()

	client, err := bitcoin.NewClient(server.URL, "", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	wStore := &fakeWithdrawalStore{item: Withdrawal{ID: 10, Status: WithdrawalSigned, RawTx: raw, TxID: txid}}
	wWorker, err := NewWithdrawalWorker(client, wStore, provider, time.Second, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wWorker.Process(context.Background(), wStore.item); err != nil {
		t.Fatalf("withdrawal Process() error = %v", err)
	}
	if wStore.broadcasted != 1 || wStore.item.Status != WithdrawalBroadcasted {
		t.Fatalf("withdrawal broadcasted=%d status=%s, want 1 and BROADCASTED", wStore.broadcasted, wStore.item.Status)
	}

	sStore := &fakeSweepStore{item: Sweep{ID: 11, Status: SweepSigned, RawTx: raw, TxID: txid}}
	sWorker, err := NewSweepWorker(client, sStore, provider, time.Second, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sWorker.Process(context.Background(), sStore.item); err != nil {
		t.Fatalf("sweep Process() error = %v", err)
	}
	if sStore.broadcasted != 1 || sStore.item.Status != SweepBroadcasted || sendCalls != 2 {
		t.Fatalf("sweep broadcasted=%d status=%s sendCalls=%d", sStore.broadcasted, sStore.item.Status, sendCalls)
	}
}

// TestIsBitcoinTxNotFoundRequiresCodeMinusFive 验证仅识别 RPCError code -5，不再按字符串包含 "not found" 误判。
func TestIsBitcoinTxNotFoundRequiresCodeMinusFive(t *testing.T) {
	if !isBitcoinTxNotFound(&bitcoin.RPCError{Code: -5, Message: "No such mempool or blockchain transaction"}) {
		t.Fatal("expected code -5 to match")
	}
	if isBitcoinTxNotFound(errors.New("请求端点 not found")) {
		t.Fatal("plain 'not found' string must not be treated as tx-not-found")
	}
	if isBitcoinTxNotFound(&bitcoin.RPCError{Code: -1, Message: "not found"}) {
		t.Fatal("non -5 RPC error must not be treated as tx-not-found")
	}
}
