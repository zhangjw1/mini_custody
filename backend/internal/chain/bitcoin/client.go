package bitcoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const NetworkSignet = "bitcoin-signet"

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回脱敏后的 Bitcoin RPC 错误文本。
func (e *RPCError) Error() string { return fmt.Sprintf("Bitcoin RPC %d: %s", e.Code, e.Message) }

type Block struct {
	Hash   string        `json:"hash"`
	Height int64         `json:"height"`
	Tx     []Transaction `json:"tx"`
}
type Transaction struct {
	TxID string `json:"txid"`
	Vout []Vout `json:"vout"`
}
type Vout struct {
	Value        BTCAmount    `json:"value"`
	N            uint32       `json:"n"`
	ScriptPubKey ScriptPubKey `json:"scriptPubKey"`
}
type ScriptPubKey struct {
	Hex string `json:"hex"`
}

// RawTransaction 描述 Bitcoin Core 返回的交易确认信息。
type RawTransaction struct {
	TxID          string `json:"txid"`
	BlockHash     string `json:"blockhash"`
	Confirmations int64  `json:"confirmations"`
}

// MempoolAcceptResult 描述 Bitcoin Core 对原始交易的预检查结果。
type MempoolAcceptResult struct {
	TxID         string `json:"txid"`
	Allowed      bool   `json:"allowed"`
	RejectReason string `json:"reject-reason"`
}

// UnspentOutput 描述 scantxoutset 返回的地址 UTXO。
type UnspentOutput struct {
	TxID   string    `json:"txid"`
	Vout   uint32    `json:"vout"`
	Amount BTCAmount `json:"amount"`
	Height int64     `json:"height"`
}

// ScanResult 描述地址 UTXO 扫描结果。
type ScanResult struct {
	Success     bool            `json:"success"`
	Unspents    []UnspentOutput `json:"unspents"`
	TotalAmount BTCAmount       `json:"total_amount"`
}

// BTCAmount decodes a JSON BTC decimal into exact satoshis.
type BTCAmount int64

// UnmarshalJSON 将 Bitcoin JSON 小数精确转换为 satoshi。
func (a *BTCAmount) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "-") {
		return errors.New("BTC 金额不能为负数")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts) == 0 {
		return errors.New("BTC 金额格式无效")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return errors.New("BTC 金额格式无效")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 8 {
		return errors.New("BTC 金额精度超过 8 位")
	}
	fraction += strings.Repeat("0", 8-len(fraction))
	frac := int64(0)
	if fraction != "" {
		frac, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return errors.New("BTC 金额格式无效")
		}
	}
	if whole > (int64(^uint64(0)>>1)-frac)/100_000_000 {
		return errors.New("BTC 金额超出范围")
	}
	*a = BTCAmount(whole*100_000_000 + frac)
	return nil
}

type Client struct {
	endpoint string
	username string
	password string
	http     *http.Client
	nextID   atomic.Uint64
}

// NewClient 创建 Bitcoin Core JSON-RPC 客户端。
func NewClient(endpoint, username, password string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("Bitcoin RPC 地址无效")
	}
	if timeout <= 0 {
		return nil, errors.New("Bitcoin RPC 超时必须大于零")
	}
	return &Client{endpoint: parsed.String(), username: username, password: password, http: &http.Client{Timeout: timeout}}, nil
}

// VerifySignet 校验远端节点确实运行在 Signet 网络。
func (c *Client) VerifySignet(ctx context.Context) error {
	return c.VerifyNetwork(ctx, "signet")
}

// VerifyNetwork 校验远端节点返回的链名称。
func (c *Client) VerifyNetwork(ctx context.Context, expected string) error {
	var info struct {
		Chain string `json:"chain"`
	}
	if err := c.call(ctx, "getblockchaininfo", nil, &info); err != nil {
		return err
	}
	if info.Chain != expected {
		return fmt.Errorf("Bitcoin RPC 网络为 %q，不是 %s", info.Chain, expected)
	}
	return nil
}

// BlockCount 查询 Signet 最新区块高度。
func (c *Client) BlockCount(ctx context.Context) (int64, error) {
	var n int64
	err := c.call(ctx, "getblockcount", nil, &n)
	return n, err
}

// BlockHash 查询指定高度区块哈希。
func (c *Client) BlockHash(ctx context.Context, height int64) (string, error) {
	var s string
	err := c.call(ctx, "getblockhash", []any{height}, &s)
	return strings.ToLower(s), err
}

// Block 查询包含交易详情的完整区块。
func (c *Client) Block(ctx context.Context, hash string) (Block, error) {
	var b Block
	err := c.call(ctx, "getblock", []any{hash, 2}, &b)
	b.Hash = strings.ToLower(b.Hash)
	return b, err
}

// SendRawTransaction 广播已签名原始交易。
func (c *Client) SendRawTransaction(ctx context.Context, rawHex string) (string, error) {
	var txid string
	err := c.call(ctx, "sendrawtransaction", []any{rawHex}, &txid)
	return strings.ToLower(txid), err
}

// RawTransaction 查询交易确认数和所在区块。
func (c *Client) RawTransaction(ctx context.Context, txid string) (RawTransaction, error) {
	var item RawTransaction
	err := c.call(ctx, "getrawtransaction", []any{txid, true}, &item)
	return item, err
}

// TestMempoolAccept 在广播前验证原始交易是否可被 mempool 接受。
func (c *Client) TestMempoolAccept(ctx context.Context, rawHex string) (MempoolAcceptResult, error) {
	var result []MempoolAcceptResult
	err := c.call(ctx, "testmempoolaccept", []any{[]string{rawHex}}, &result)
	if err != nil {
		return MempoolAcceptResult{}, err
	}
	if len(result) != 1 {
		return MempoolAcceptResult{}, errors.New("Bitcoin mempool 预检查响应无效")
	}
	return result[0], nil
}

// ScanAddressUTXOs 查询指定地址当前未花费输出。
func (c *Client) ScanAddressUTXOs(ctx context.Context, address string) (ScanResult, error) {
	var result ScanResult
	err := c.call(ctx, "scantxoutset", []any{"start", []string{"addr(" + address + ")"}}, &result)
	return result, err
}

// call 执行一次 JSON-RPC 请求并解析结果。
func (c *Client) call(ctx context.Context, method string, params []any, target any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{"1.0", c.nextID.Add(1), method, params})
	if err != nil {
		return fmt.Errorf("编码 Bitcoin RPC 请求失败：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("调用 Bitcoin RPC 失败：%w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("读取 Bitcoin RPC 响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bitcoin RPC HTTP 状态 %d", resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("解码 Bitcoin RPC 响应失败：%w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return fmt.Errorf("解码 Bitcoin RPC 结果失败：%w", err)
	}
	return nil
}
