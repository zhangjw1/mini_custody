package bitcoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestClientParsesRPCErrorOnHTTP500 验证 Bitcoin Core 返回 HTTP 500 且带 JSON-RPC error 时能解析为 *RPCError。
func TestClientParsesRPCErrorOnHTTP500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": nil,
			"error": map[string]any{
				"code":    -5,
				"message": "No such mempool or blockchain transaction. Use gettransaction for wallet transactions.",
			},
			"id": req.ID,
		})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RawTransaction(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("expected *RPCError, got nil")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -5 {
		t.Fatalf("rpcErr.Code = %d, want -5", rpcErr.Code)
	}
}

// TestClientReturnsHTTPStatusWhenNonJSONErrorBody 验证非 200 且响应体不是合法 RPC 错误时返回 HTTP 状态错误。
func TestClientReturnsHTTPStatusWhenNonJSONErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RawTransaction(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("expected HTTP error, got nil")
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		t.Fatalf("unexpected *RPCError: %+v", rpcErr)
	}
}
