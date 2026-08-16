package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5"
)

const sepoliaChainID = 11155111

type settings struct {
	apiBaseURL    string
	databaseURL   string
	rpcURL        string
	userID        int64
	walletAddress common.Address
	depositHash   common.Hash
	withdrawHash  common.Hash
	confirmations uint64
}

type walletResponse struct {
	UserID  int64  `json:"user_id"`
	Network string `json:"network"`
	Address string `json:"address"`
	Balance struct {
		AvailableWei         string `json:"available_wei"`
		PendingDepositWei    string `json:"pending_deposit_wei"`
		PendingWithdrawalWei string `json:"pending_withdrawal_wei"`
	} `json:"balance"`
}

type depositResponse struct {
	ID        int64  `json:"id"`
	TxHash    string `json:"tx_hash"`
	AmountWei string `json:"amount_wei"`
	Status    string `json:"status"`
}

type withdrawalResponse struct {
	ID           int64  `json:"id"`
	TxHash       string `json:"tx_hash"`
	AmountWei    string `json:"amount_wei"`
	ActualFeeWei string `json:"actual_fee_wei"`
	Status       string `json:"status"`
}

type pageResponse[T any] struct {
	Items []T `json:"items"`
}

type depositRow struct {
	id            int64
	addressID     int64
	txHash        string
	blockNumber   int64
	blockHash     string
	amountWei     *big.Int
	confirmations int64
	status        string
}

type withdrawalRow struct {
	id             int64
	idempotencyKey string
	toAddress      string
	amountWei      *big.Int
	actualFeeWei   *big.Int
	rawTx          []byte
	txHash         string
	blockNumber    int64
	confirmations  int64
	status         string
}

// TestSepoliaEvidence 只读校验已有 Sepolia 交易证据，不创建交易或修改数据库。
func TestSepoliaEvidence(t *testing.T) {
	cfg := loadSettings(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	db, err := pgx.Connect(ctx, cfg.databaseURL)
	if err != nil {
		t.Fatal("connect to E2E PostgreSQL failed")
	}
	defer func() { _ = db.Close(context.Background()) }()

	rpc, err := ethclient.DialContext(ctx, cfg.rpcURL)
	if err != nil {
		t.Fatal("connect to E2E Sepolia RPC failed")
	}
	defer rpc.Close()
	httpClient := &http.Client{Timeout: 10 * time.Second}

	t.Run("chain and deterministic wallet", func(t *testing.T) {
		chainID, err := rpc.ChainID(ctx)
		if err != nil {
			t.Fatal("read Sepolia chain ID failed")
		}
		if chainID.Cmp(big.NewInt(sepoliaChainID)) != 0 {
			t.Fatalf("chain ID = %s, want %d", chainID, sepoliaChainID)
		}

		wallet := getJSON[walletResponse](t, httpClient, cfg.apiBaseURL+fmt.Sprintf("/api/v1/users/%d/wallet", cfg.userID))
		if wallet.UserID != cfg.userID || wallet.Network != "ethereum-sepolia" {
			t.Fatalf("wallet identity = (%d, %s)", wallet.UserID, wallet.Network)
		}
		if !strings.EqualFold(wallet.Address, cfg.walletAddress.Hex()) {
			t.Fatalf("wallet address = %s, want %s", wallet.Address, cfg.walletAddress.Hex())
		}

		var dbAddress, available, pendingDeposit, pendingWithdrawal string
		err = db.QueryRow(ctx, `
			SELECT wa.address, ab.available_wei::text, ab.pending_deposit_wei::text,
			       ab.pending_withdrawal_wei::text
			FROM wallet_addresses wa
			JOIN asset_balances ab ON ab.user_id = wa.user_id AND ab.asset = 'ETH'
			WHERE wa.user_id = $1 AND wa.network = 'ethereum-sepolia'`, cfg.userID,
		).Scan(&dbAddress, &available, &pendingDeposit, &pendingWithdrawal)
		if err != nil {
			t.Fatalf("query wallet ledger: %v", err)
		}
		if !strings.EqualFold(dbAddress, wallet.Address) || wallet.Balance.AvailableWei != available ||
			wallet.Balance.PendingDepositWei != pendingDeposit || wallet.Balance.PendingWithdrawalWei != pendingWithdrawal {
			t.Fatal("API wallet and PostgreSQL ledger differ")
		}
	})

	t.Run("credited deposit is unique and on chain", func(t *testing.T) {
		row := queryDeposit(t, ctx, db, cfg)
		if row.status != "CREDITED" || row.confirmations < int64(cfg.confirmations) {
			t.Fatalf("deposit state = %s/%d", row.status, row.confirmations)
		}
		assertCount(t, ctx, db, `SELECT count(*) FROM deposits WHERE network = 'ethereum-sepolia' AND tx_hash = $1 AND address_id = $2`, 1, row.txHash, row.addressID)
		assertCount(t, ctx, db, `SELECT count(*) FROM balance_entries WHERE entry_type = 'DEPOSIT_PENDING' AND reference_type = 'DEPOSIT' AND reference_id = $1`, 1, row.id)
		assertCount(t, ctx, db, `SELECT count(*) FROM balance_entries WHERE entry_type = 'DEPOSIT_CREDIT' AND reference_type = 'DEPOSIT' AND reference_id = $1`, 1, row.id)

		tx, pending, err := rpc.TransactionByHash(ctx, cfg.depositHash)
		if err != nil {
			t.Fatal("read mined deposit transaction failed")
		}
		if pending {
			t.Fatal("deposit transaction is still pending")
		}
		receipt, err := rpc.TransactionReceipt(ctx, cfg.depositHash)
		if err != nil {
			t.Fatal("read deposit receipt failed")
		}
		if receipt.Status != types.ReceiptStatusSuccessful || tx.To() == nil ||
			!strings.EqualFold(tx.To().Hex(), cfg.walletAddress.Hex()) || tx.Value().Cmp(row.amountWei) != 0 {
			t.Fatal("deposit transaction does not match the credited ledger record")
		}
		if receipt.BlockNumber.Int64() != row.blockNumber || !strings.EqualFold(receipt.BlockHash.Hex(), row.blockHash) {
			t.Fatal("deposit receipt block does not match PostgreSQL")
		}
		assertOnChainConfirmations(t, ctx, rpc, receipt, cfg.confirmations)

		var checkpoint int64
		if err := db.QueryRow(ctx, `SELECT last_scanned_block FROM chain_checkpoints WHERE network = 'ethereum-sepolia' AND scanner = 'eth-deposit'`).Scan(&checkpoint); err != nil {
			t.Fatalf("query deposit checkpoint: %v", err)
		}
		if checkpoint < row.blockNumber {
			t.Fatalf("checkpoint = %d, deposit block = %d", checkpoint, row.blockNumber)
		}

		page := getJSON[pageResponse[depositResponse]](t, httpClient, cfg.apiBaseURL+fmt.Sprintf("/api/v1/users/%d/deposits?page_size=100", cfg.userID))
		match := findDeposit(page.Items, row.txHash)
		if match == nil || match.ID != row.id || match.Status != row.status || match.AmountWei != row.amountWei.String() {
			t.Fatal("deposit API does not match PostgreSQL")
		}
	})

	t.Run("completed withdrawal fee matches receipt", func(t *testing.T) {
		row := queryWithdrawal(t, ctx, db, cfg)
		if row.status != "COMPLETED" || row.confirmations < int64(cfg.confirmations) {
			t.Fatalf("withdrawal state = %s/%d", row.status, row.confirmations)
		}
		assertCount(t, ctx, db, `SELECT count(*) FROM withdrawals WHERE user_id = $1 AND idempotency_key = $2`, 1, cfg.userID, row.idempotencyKey)
		assertCount(t, ctx, db, `SELECT count(*) FROM withdrawals WHERE tx_hash = $1`, 1, row.txHash)
		assertCount(t, ctx, db, `SELECT count(*) FROM balance_entries WHERE entry_type = 'WITHDRAW_RESERVE' AND reference_type = 'WITHDRAWAL' AND reference_id = $1`, 1, row.id)
		assertCount(t, ctx, db, `SELECT count(*) FROM balance_entries WHERE entry_type = 'WITHDRAW_FINALIZE' AND reference_type = 'WITHDRAWAL' AND reference_id = $1`, 1, row.id)

		var signed types.Transaction
		if err := signed.UnmarshalBinary(row.rawTx); err != nil {
			t.Fatalf("decode persisted raw transaction: %v", err)
		}
		if signed.Hash() != cfg.withdrawHash || signed.To() == nil ||
			!strings.EqualFold(signed.To().Hex(), row.toAddress) || signed.Value().Cmp(row.amountWei) != 0 {
			t.Fatal("persisted raw transaction does not match withdrawal record")
		}
		receipt, err := rpc.TransactionReceipt(ctx, cfg.withdrawHash)
		if err != nil {
			t.Fatal("read withdrawal receipt failed")
		}
		if receipt.Status != types.ReceiptStatusSuccessful || receipt.EffectiveGasPrice == nil ||
			receipt.BlockNumber.Int64() != row.blockNumber {
			t.Fatal("withdrawal receipt does not match completed database state")
		}
		actualFee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
		if actualFee.Cmp(row.actualFeeWei) != 0 {
			t.Fatalf("actual fee = %s, database = %s", actualFee, row.actualFeeWei)
		}
		assertOnChainConfirmations(t, ctx, rpc, receipt, cfg.confirmations)

		page := getJSON[pageResponse[withdrawalResponse]](t, httpClient, cfg.apiBaseURL+fmt.Sprintf("/api/v1/users/%d/withdrawals?page_size=100", cfg.userID))
		match := findWithdrawal(page.Items, row.txHash)
		if match == nil || match.ID != row.id || match.Status != row.status ||
			match.AmountWei != row.amountWei.String() || match.ActualFeeWei != row.actualFeeWei.String() {
			t.Fatal("withdrawal API does not match PostgreSQL and Sepolia")
		}
	})
}

// loadSettings 读取显式启用的外部验收配置并校验输入格式。
func loadSettings(t *testing.T) settings {
	t.Helper()
	if os.Getenv("E2E_SEPOLIA") != "1" {
		t.Skip("set E2E_SEPOLIA=1 or run make e2e-test to enable the external E2E suite")
	}
	userID, err := strconv.ParseInt(envOrDefault("E2E_USER_ID", "1"), 10, 64)
	if err != nil || userID <= 0 {
		t.Fatal("E2E_USER_ID must be a positive integer")
	}
	confirmations, err := strconv.ParseUint(envOrDefault("E2E_CONFIRMATIONS", "3"), 10, 64)
	if err != nil || confirmations == 0 {
		t.Fatal("E2E_CONFIRMATIONS must be a positive integer")
	}
	walletText := requiredEnv(t, "E2E_EXPECTED_WALLET_ADDRESS")
	if !common.IsHexAddress(walletText) {
		t.Fatal("E2E_EXPECTED_WALLET_ADDRESS is invalid")
	}
	return settings{
		apiBaseURL:    strings.TrimRight(envOrDefault("E2E_API_BASE_URL", "http://127.0.0.1:8080"), "/"),
		databaseURL:   envWithFallback(t, "E2E_DATABASE_URL", "DATABASE_URL"),
		rpcURL:        envWithFallback(t, "E2E_SEPOLIA_RPC_URL", "SEPOLIA_RPC_URL"),
		userID:        userID,
		walletAddress: common.HexToAddress(walletText),
		depositHash:   requiredHash(t, "E2E_DEPOSIT_TX_HASH"),
		withdrawHash:  requiredHash(t, "E2E_WITHDRAWAL_TX_HASH"),
		confirmations: confirmations,
	}
}

// queryDeposit 按用户和交易哈希读取唯一充值证据。
func queryDeposit(t *testing.T, ctx context.Context, db *pgx.Conn, cfg settings) depositRow {
	t.Helper()
	var row depositRow
	var amount string
	err := db.QueryRow(ctx, `
		SELECT id, address_id, tx_hash, block_number, block_hash, amount_wei::text, confirmations, status
		FROM deposits WHERE user_id = $1 AND tx_hash = $2`, cfg.userID, strings.ToLower(cfg.depositHash.Hex()),
	).Scan(&row.id, &row.addressID, &row.txHash, &row.blockNumber, &row.blockHash, &amount, &row.confirmations, &row.status)
	if err != nil {
		t.Fatalf("query deposit evidence: %v", err)
	}
	row.amountWei = mustBigInt(t, amount)
	return row
}

// queryWithdrawal 按用户和交易哈希读取唯一提币证据。
func queryWithdrawal(t *testing.T, ctx context.Context, db *pgx.Conn, cfg settings) withdrawalRow {
	t.Helper()
	var row withdrawalRow
	var amount, actualFee string
	err := db.QueryRow(ctx, `
		SELECT id, idempotency_key, to_address, amount_wei::text, actual_fee_wei::text,
		       raw_tx, tx_hash, block_number, confirmations, status
		FROM withdrawals WHERE user_id = $1 AND tx_hash = $2`, cfg.userID, strings.ToLower(cfg.withdrawHash.Hex()),
	).Scan(&row.id, &row.idempotencyKey, &row.toAddress, &amount, &actualFee, &row.rawTx, &row.txHash, &row.blockNumber, &row.confirmations, &row.status)
	if err != nil {
		t.Fatalf("query withdrawal evidence: %v", err)
	}
	row.amountWei = mustBigInt(t, amount)
	row.actualFeeWei = mustBigInt(t, actualFee)
	return row
}

// assertCount 校验幂等业务记录和余额流水的数据库数量。
func assertCount(t *testing.T, ctx context.Context, db *pgx.Conn, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := db.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatalf("query evidence count: %v", err)
	}
	if got != want {
		t.Fatalf("evidence count = %d, want %d", got, want)
	}
}

// assertOnChainConfirmations 根据最新区块校验链上确认数。
func assertOnChainConfirmations(t *testing.T, ctx context.Context, rpc *ethclient.Client, receipt *types.Receipt, want uint64) {
	t.Helper()
	latest, err := rpc.BlockNumber(ctx)
	if err != nil {
		t.Fatal("read latest Sepolia block failed")
	}
	if latest < receipt.BlockNumber.Uint64() || latest-receipt.BlockNumber.Uint64()+1 < want {
		t.Fatalf("on-chain confirmations are below %d", want)
	}
}

// getJSON 请求应用只读接口并解码成功响应。
func getJSON[T any](t *testing.T, client *http.Client, url string) T {
	t.Helper()
	var value T
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET application API: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET application API status = %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode application API: %v", err)
	}
	return value
}

// findDeposit 在 API 列表中按交易哈希查找充值。
func findDeposit(items []depositResponse, hash string) *depositResponse {
	for i := range items {
		if strings.EqualFold(items[i].TxHash, hash) {
			return &items[i]
		}
	}
	return nil
}

// findWithdrawal 在 API 列表中按交易哈希查找提币。
func findWithdrawal(items []withdrawalResponse, hash string) *withdrawalResponse {
	for i := range items {
		if strings.EqualFold(items[i].TxHash, hash) {
			return &items[i]
		}
	}
	return nil
}

// mustBigInt 将数据库十进制整数转换为精确的大整数。
func mustBigInt(t *testing.T, value string) *big.Int {
	t.Helper()
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid database integer %q", value)
	}
	return parsed
}

// requiredHash 读取并校验必需的 32 字节十六进制哈希。
func requiredHash(t *testing.T, name string) common.Hash {
	t.Helper()
	value := requiredEnv(t, name)
	if len(value) != 66 || !strings.HasPrefix(value, "0x") {
		t.Fatalf("%s must be a 0x-prefixed 32-byte hash", name)
	}
	if _, err := hex.DecodeString(value[2:]); err != nil {
		t.Fatalf("%s must be a hexadecimal hash", name)
	}
	return common.HexToHash(value)
}

// requiredEnv 读取不可为空的环境变量。
func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

// envWithFallback 优先读取 E2E 专用变量，否则复用应用变量。
func envWithFallback(t *testing.T, primary, fallback string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return value
	}
	return requiredEnv(t, fallback)
}

// envOrDefault 在环境变量为空时返回安全默认值。
func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
