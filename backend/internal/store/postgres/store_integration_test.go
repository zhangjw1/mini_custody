package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

const integrationTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// TestIntegrationDepositIsIdempotentAndCreditsOnce 验证充值发现和入账的数据库幂等性。
func TestIntegrationDepositIsIdempotentAndCreditsOnce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)

	observation := depositObservation(user.ID, address.ID, 10, 1_000)
	deposit, created, err := store.RecordDepositAndCheckpoint(context.Background(), observation)
	if err != nil {
		t.Fatalf("RecordDepositAndCheckpoint() error = %v", err)
	}
	if !created || deposit.Status != DepositConfirming {
		t.Fatalf("created = %v, status = %s", created, deposit.Status)
	}
	_, created, err = store.RecordDepositAndCheckpoint(context.Background(), observation)
	if err != nil {
		t.Fatalf("duplicate RecordDepositAndCheckpoint() error = %v", err)
	}
	if created {
		t.Fatal("duplicate deposit was created")
	}

	deposit, credited, err := store.CreditDeposit(context.Background(), deposit.ID, 3)
	if err != nil {
		t.Fatalf("CreditDeposit() error = %v", err)
	}
	if !credited || deposit.Status != DepositCredited {
		t.Fatalf("credited = %v, status = %s", credited, deposit.Status)
	}
	_, credited, err = store.CreditDeposit(context.Background(), deposit.ID, 4)
	if err != nil {
		t.Fatalf("duplicate CreditDeposit() error = %v", err)
	}
	if credited {
		t.Fatal("deposit was credited twice")
	}

	balance, err := store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available", balance.AvailableWei, 1_000)
	assertBigInt(t, "pending deposit", balance.PendingDepositWei, 0)
	entries, err := store.ListBalanceEntries(context.Background(), user.ID, 10)
	if err != nil {
		t.Fatalf("ListBalanceEntries() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("balance entry count = %d, want 2", len(entries))
	}
}

// TestIntegrationBlockDepositsAreAtomicAndIdempotent 验证同一区块多笔充值原子落库且重扫不重复增加余额。
func TestIntegrationBlockDepositsAreAtomicAndIdempotent(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	users, err := store.ListUsers(context.Background())
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers() count = %d, error = %v", len(users), err)
	}
	first, err := store.WalletAddressByUser(context.Background(), users[0].ID)
	if err != nil {
		t.Fatalf("WalletAddressByUser(first) error = %v", err)
	}
	second, err := store.WalletAddressByUser(context.Background(), users[1].ID)
	if err != nil {
		t.Fatalf("WalletAddressByUser(second) error = %v", err)
	}
	blockHash := fmt.Sprintf("0x%064x", 2_000)
	checkpoint := ChainCheckpoint{Network: NetworkSepolia, Scanner: "eth-deposit", LastScannedBlock: 100, LastScannedHash: blockHash}
	observations := []DepositObservation{
		{UserID: users[0].ID, AddressID: first.ID, TxHash: fmt.Sprintf("0x%064x", 101), TxIndex: 0, BlockNumber: 100, BlockHash: blockHash, AmountWei: big.NewInt(100)},
		{UserID: users[1].ID, AddressID: second.ID, TxHash: fmt.Sprintf("0x%064x", 102), TxIndex: 1, BlockNumber: 100, BlockHash: blockHash, AmountWei: big.NewInt(200)},
	}

	items, created, err := store.RecordDepositsAndCheckpoint(context.Background(), observations, checkpoint)
	if err != nil || created != 2 || len(items) != 2 {
		t.Fatalf("RecordDepositsAndCheckpoint() created = %d, count = %d, error = %v", created, len(items), err)
	}
	_, created, err = store.RecordDepositsAndCheckpoint(context.Background(), observations, checkpoint)
	if err != nil || created != 0 {
		t.Fatalf("duplicate block created = %d, error = %v", created, err)
	}
	firstBalance, err := store.BalanceByUser(context.Background(), users[0].ID)
	if err != nil {
		t.Fatalf("BalanceByUser(first) error = %v", err)
	}
	secondBalance, err := store.BalanceByUser(context.Background(), users[1].ID)
	if err != nil {
		t.Fatalf("BalanceByUser(second) error = %v", err)
	}
	assertBigInt(t, "first pending deposit", firstBalance.PendingDepositWei, 100)
	assertBigInt(t, "second pending deposit", secondBalance.PendingDepositWei, 200)
}

// TestIntegrationConcurrentDepositCreditOnlyAppliesOnce 验证并发确认同一充值时余额只入账一次。
func TestIntegrationConcurrentDepositCreditOnlyAppliesOnce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)
	deposit, _, err := store.RecordDepositAndCheckpoint(context.Background(), depositObservation(user.ID, address.ID, 30, 500))
	if err != nil {
		t.Fatalf("RecordDepositAndCheckpoint() error = %v", err)
	}
	type result struct {
		credited bool
		err      error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, credited, err := store.CreditDeposit(context.Background(), deposit.ID, 3)
			results <- result{credited: credited, err: err}
		}()
	}
	group.Wait()
	close(results)
	creditedCount := 0
	for item := range results {
		if item.err != nil {
			t.Fatalf("CreditDeposit() error = %v", item.err)
		}
		if item.credited {
			creditedCount++
		}
	}
	if creditedCount != 1 {
		t.Fatalf("credited count = %d, want 1", creditedCount)
	}
	balance, err := store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available", balance.AvailableWei, 500)
	assertBigInt(t, "pending deposit", balance.PendingDepositWei, 0)
}

// TestIntegrationConcurrentWithdrawalCannotOverspend 验证并发提币无法超额占用余额。
func TestIntegrationConcurrentWithdrawalCannotOverspend(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 20, 1_000)

	requests := []WithdrawalRequest{
		withdrawalRequest("concurrent-1", user.ID, address.ID, 600, 100),
		withdrawalRequest("concurrent-2", user.ID, address.ID, 600, 100),
	}
	type result struct {
		item    Withdrawal
		created bool
		err     error
	}
	results := make(chan result, len(requests))
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, created, err := store.ReserveWithdrawal(context.Background(), request)
			results <- result{item: item, created: created, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var winner Withdrawal
	var successes, insufficient int
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			winner = result.item
		case errors.Is(result.err, ErrInsufficientBalance):
			insufficient++
		default:
			t.Fatalf("unexpected withdrawal error = %v", result.err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("successes = %d, insufficient = %d", successes, insufficient)
	}
	balance, err := store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available", balance.AvailableWei, 300)
	assertBigInt(t, "pending withdrawal", balance.PendingWithdrawalWei, 700)

	released, changed, err := store.ReleaseWithdrawal(context.Background(), winner.ID, "TEST_RELEASE", "integration test")
	if err != nil {
		t.Fatalf("ReleaseWithdrawal() error = %v", err)
	}
	if !changed || released.Status != WithdrawalFailed {
		t.Fatalf("changed = %v, status = %s", changed, released.Status)
	}
	balance, err = store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() after release error = %v", err)
	}
	assertBigInt(t, "available after release", balance.AvailableWei, 1_000)
	assertBigInt(t, "pending after release", balance.PendingWithdrawalWei, 0)
}

// TestIntegrationWithdrawalIdempotencyAndFeeSettlement 验证提币幂等和实际费用结算。
func TestIntegrationWithdrawalIdempotencyAndFeeSettlement(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 30, 2_000)

	request := withdrawalRequest("settlement-1", user.ID, address.ID, 1_000, 200)
	item, created, err := store.ReserveWithdrawal(context.Background(), request)
	if err != nil || !created {
		t.Fatalf("ReserveWithdrawal() created = %v, error = %v", created, err)
	}
	duplicate, created, err := store.ReserveWithdrawal(context.Background(), request)
	if err != nil || created || duplicate.ID != item.ID {
		t.Fatalf("duplicate reserve: id = %d, created = %v, error = %v", duplicate.ID, created, err)
	}
	for _, status := range []string{
		WithdrawalSigning,
		WithdrawalSigned,
		WithdrawalBroadcasting,
		WithdrawalBroadcasted,
		WithdrawalConfirming,
	} {
		item, err = store.TransitionWithdrawal(context.Background(), item.ID, status)
		if err != nil {
			t.Fatalf("TransitionWithdrawal(%s) error = %v", status, err)
		}
	}
	item, changed, err := store.FinalizeWithdrawal(context.Background(), WithdrawalSettlement{
		WithdrawalID:  item.ID,
		ActualFeeWei:  big.NewInt(100),
		Success:       true,
		BlockNumber:   100,
		Confirmations: 3,
	})
	if err != nil {
		t.Fatalf("FinalizeWithdrawal() error = %v", err)
	}
	if !changed || item.Status != WithdrawalCompleted {
		t.Fatalf("changed = %v, status = %s", changed, item.Status)
	}
	balance, err := store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "settled available", balance.AvailableWei, 900)
	assertBigInt(t, "settled pending", balance.PendingWithdrawalWei, 0)
}

// integrationStore 创建使用独立临时 Schema 的集成测试数据访问对象。
func integrationStore(t *testing.T) (*Store, func()) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	timezone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	adminPool, err := Open(ctx, databaseURL, timezone)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	schema := fmt.Sprintf("mini_custody_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	schemaIdentifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, `CREATE SCHEMA `+schemaIdentifier); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	dropSchema := func() {
		if _, err := adminPool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schemaIdentifier+` CASCADE`); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		adminPool.Close()
	}
	pool, err := OpenInSchema(ctx, databaseURL, timezone, schema)
	if err != nil {
		dropSchema()
		t.Fatalf("OpenInSchema() error = %v", err)
	}
	if err := migrations.NewRunner(pool).Up(ctx); err != nil {
		pool.Close()
		dropSchema()
		t.Fatalf("migrations.Up() error = %v", err)
	}
	store, err := NewStore(pool, timezone)
	if err != nil {
		pool.Close()
		dropSchema()
		t.Fatalf("NewStore() error = %v", err)
	}
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		pool.Close()
		dropSchema()
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	if err := store.BootstrapDemoUsers(context.Background(), provider); err != nil {
		pool.Close()
		dropSchema()
		t.Fatalf("BootstrapDemoUsers() error = %v", err)
	}
	return store, func() {
		pool.Close()
		dropSchema()
	}
}

// firstDemoWallet 返回第一个演示用户及其托管地址。
func firstDemoWallet(t *testing.T, store *Store) (User, WalletAddress) {
	t.Helper()
	users, err := store.ListUsers(context.Background())
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers() count = %d, error = %v", len(users), err)
	}
	address, err := store.WalletAddressByUser(context.Background(), users[0].ID)
	if err != nil {
		t.Fatalf("WalletAddressByUser() error = %v", err)
	}
	return users[0], address
}

// fundWallet 通过充值事务为测试用户增加可用余额。
func fundWallet(t *testing.T, store *Store, userID, addressID, block, value int64) {
	t.Helper()
	deposit, _, err := store.RecordDepositAndCheckpoint(context.Background(), depositObservation(userID, addressID, block, value))
	if err != nil {
		t.Fatalf("RecordDepositAndCheckpoint() error = %v", err)
	}
	if _, _, err := store.CreditDeposit(context.Background(), deposit.ID, 3); err != nil {
		t.Fatalf("CreditDeposit() error = %v", err)
	}
}

// depositObservation 构造确定性的测试充值观察数据。
func depositObservation(userID, addressID, block, value int64) DepositObservation {
	txHash := fmt.Sprintf("0x%064x", block)
	blockHash := fmt.Sprintf("0x%064x", block+1_000)
	return DepositObservation{
		UserID:      userID,
		AddressID:   addressID,
		TxHash:      txHash,
		TxIndex:     0,
		BlockNumber: block,
		BlockHash:   blockHash,
		AmountWei:   big.NewInt(value),
		Checkpoint: ChainCheckpoint{
			Network:          NetworkSepolia,
			Scanner:          "deposit-scanner",
			LastScannedBlock: block,
			LastScannedHash:  blockHash,
		},
	}
}

// withdrawalRequest 构造测试提币占用请求。
func withdrawalRequest(key string, userID, addressID, value, fee int64) WithdrawalRequest {
	return WithdrawalRequest{
		IdempotencyKey: key,
		UserID:         userID,
		AddressID:      addressID,
		ToAddress:      "0x1111111111111111111111111111111111111111",
		AmountWei:      big.NewInt(value),
		ReservedFeeWei: big.NewInt(fee),
	}
}

// assertBigInt 断言大整数等于预期的小整数值。
func assertBigInt(t *testing.T, label string, got *big.Int, want int64) {
	t.Helper()
	if got == nil || got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s = %v, want %d", label, got, want)
	}
}
