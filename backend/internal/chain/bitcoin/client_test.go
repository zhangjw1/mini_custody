package bitcoin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 提供测试用 HTTP 传输实现。
func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// TestBTCAmountIsExact 验证 BTC 金额精确转换为 satoshi。
func TestBTCAmountIsExact(t *testing.T) {
	for input, want := range map[string]int64{"0.00000001": 1, "1.23456789": 123456789, "21": 2100000000} {
		var got BTCAmount
		if err := json.Unmarshal([]byte(input), &got); err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if int64(got) != want {
			t.Fatalf("%s = %d, want %d", input, got, want)
		}
	}
	var invalid BTCAmount
	if json.Unmarshal([]byte("0.000000001"), &invalid) == nil {
		t.Fatal("expected sub-satoshi rejection")
	}
}

// TestClientVerifiesSignetAndDecodesBlock 验证 Signet 节点和区块解码。
func TestClientVerifiesSignetAndDecodesBlock(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		result := any(map[string]any{"chain": "signet"})
		if request.Method == "getblock" {
			result = map[string]any{"hash": "AA", "height": 7, "tx": []any{map[string]any{"txid": "BB", "vout": []any{map[string]any{"value": json.Number("0.00000001"), "n": 0, "scriptPubKey": map[string]any{"hex": "0014aa"}}}}}}
		}
		body := new(bytes.Buffer)
		_ = json.NewEncoder(body).Encode(map[string]any{"result": result, "error": nil, "id": request.ID})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Header: make(http.Header)}, nil
	})
	client, err := NewClient("http://bitcoin.test", "", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = transport
	if err := client.VerifySignet(context.Background()); err != nil {
		t.Fatal(err)
	}
	block, err := client.Block(context.Background(), "aa")
	if err != nil {
		t.Fatal(err)
	}
	if block.Height != 7 || len(block.Tx) != 1 || int64(block.Tx[0].Vout[0].Value) != 1 {
		t.Fatalf("unexpected block: %#v", block)
	}
}
