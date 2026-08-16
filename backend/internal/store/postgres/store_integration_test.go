package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
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
	changedFeeRequest := request
	changedFeeRequest.ReservedFeeWei = big.NewInt(300)
	duplicate, created, err = store.ReserveWithdrawal(context.Background(), changedFeeRequest)
	if err != nil || created || duplicate.ID != item.ID || duplicate.ReservedFeeWei.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("changed fee retry: id = %d, created = %v, fee = %v, error = %v", duplicate.ID, created, duplicate.ReservedFeeWei, err)
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

// TestIntegrationConcurrentNonceAllocationIsUnique 验证同一地址并发提币获得不同 Nonce。
func TestIntegrationConcurrentNonceAllocationIsUnique(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 50, 10_000)
	first, _, err := store.ReserveWithdrawal(context.Background(), withdrawalRequest("nonce-1", user.ID, address.ID, 100, 100))
	if err != nil {
		t.Fatalf("ReserveWithdrawal(first) error = %v", err)
	}
	second, _, err := store.ReserveWithdrawal(context.Background(), withdrawalRequest("nonce-2", user.ID, address.ID, 100, 100))
	if err != nil {
		t.Fatalf("ReserveWithdrawal(second) error = %v", err)
	}
	type result struct {
		item Withdrawal
		err  error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, id := range []int64{first.ID, second.ID} {
		id := id
		group.Add(1)
		go func() {
			defer group.Done()
			item, _, err := store.AllocateWithdrawalNonce(context.Background(), id, 5)
			results <- result{item: item, err: err}
		}()
	}
	group.Wait()
	close(results)
	nonces := make(map[string]bool)
	for item := range results {
		if item.err != nil {
			t.Fatalf("AllocateWithdrawalNonce() error = %v", item.err)
		}
		nonces[item.item.Nonce.String()] = true
	}
	if !nonces["5"] || !nonces["6"] || len(nonces) != 2 {
		t.Fatalf("nonces = %v, want 5 and 6", nonces)
	}
}

// TestIntegrationFailedWithdrawalChargesOnlyActualGas 验证链上失败时退回转账金额和未使用费用。
func TestIntegrationFailedWithdrawalChargesOnlyActualGas(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 60, 1_000)
	item, _, err := store.ReserveWithdrawal(context.Background(), withdrawalRequest("failed-receipt", user.ID, address.ID, 600, 200))
	if err != nil {
		t.Fatalf("ReserveWithdrawal() error = %v", err)
	}
	for _, status := range []string{
		WithdrawalSigning, WithdrawalSigned, WithdrawalBroadcasting, WithdrawalBroadcasted,
	} {
		item, err = store.TransitionWithdrawal(context.Background(), item.ID, status)
		if err != nil {
			t.Fatalf("TransitionWithdrawal(%s) error = %v", status, err)
		}
	}
	item, changed, err := store.FinalizeWithdrawal(context.Background(), WithdrawalSettlement{
		WithdrawalID: item.ID, ActualFeeWei: big.NewInt(50), Success: false,
		BlockNumber: 100, Confirmations: 3,
	})
	if err != nil || !changed || item.Status != WithdrawalFailed {
		t.Fatalf("FinalizeWithdrawal() changed = %v, status = %s, error = %v", changed, item.Status, err)
	}
	balance, err := store.BalanceByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "failed withdrawal available", balance.AvailableWei, 950)
	assertBigInt(t, "failed withdrawal pending", balance.PendingWithdrawalWei, 0)
}

// TestIntegrationERC20AssetAndHotWalletInitialization 验证 Token 资产、用户余额和平台热钱包可幂等初始化。
func TestIntegrationERC20AssetAndHotWalletInitialization(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	user, _ := firstDemoWallet(t, store)

	configured := Asset{
		Network:         NetworkSepolia,
		Symbol:          "USDC",
		ContractAddress: "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238",
		Decimals:        6,
		Enabled:         true,
	}
	asset, err := store.EnsureERC20Asset(context.Background(), configured)
	if err != nil {
		t.Fatalf("EnsureERC20Asset() error = %v", err)
	}
	duplicate, err := store.EnsureERC20Asset(context.Background(), configured)
	if err != nil || duplicate.ID != asset.ID {
		t.Fatalf("duplicate EnsureERC20Asset() id = %d, error = %v", duplicate.ID, err)
	}
	if asset.AssetType != AssetTypeERC20 || asset.ContractAddress != strings.ToLower(configured.ContractAddress) || asset.Decimals != 6 {
		t.Fatalf("asset = %+v", asset)
	}
	balance, err := store.BalanceByUserAndAsset(context.Background(), user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() error = %v", err)
	}
	assertBigInt(t, "USDC available", balance.AvailableWei, 0)

	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	hotWallet, err := store.EnsurePlatformHotWallet(context.Background(), provider, "m/44'/60'/0'/0/0")
	if err != nil {
		t.Fatalf("EnsurePlatformHotWallet() error = %v", err)
	}
	duplicateWallet, err := store.EnsurePlatformHotWallet(context.Background(), provider, "m/44'/60'/0'/0/0")
	if err != nil || duplicateWallet.ID != hotWallet.ID || hotWallet.Role != PlatformRoleHot {
		t.Fatalf("duplicate hot wallet = %+v, error = %v", duplicateWallet, err)
	}
}

// TestIntegrationERC20ConstraintsAndScanners 验证 Token Event、活动归集、内部转账和提币的数据库约束与扫描方法。
func TestIntegrationERC20ConstraintsAndScanners(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)
	asset, err := store.EnsureERC20Asset(ctx, Asset{
		Network: NetworkSepolia, Symbol: "USDC", ContractAddress: "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238",
		Decimals: 6, Enabled: true,
	})
	if err != nil {
		t.Fatalf("EnsureERC20Asset() error = %v", err)
	}
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	hotWallet, err := store.EnsurePlatformHotWallet(ctx, provider, "m/44'/60'/0'/0/0")
	if err != nil {
		t.Fatalf("EnsurePlatformHotWallet() error = %v", err)
	}

	txHash := fmt.Sprintf("0x%064x", 7001)
	blockHash := fmt.Sprintf("0x%064x", 8001)
	var depositID int64
	insertDeposit := `
		INSERT INTO token_deposits (
			user_id, address_id, asset_id, tx_hash, log_index, block_number, block_hash,
			from_address, to_address, amount_units, status
		) VALUES ($1, $2, $3, $4, 0, 700, $5, $6, $7, 1000000, $8)
		RETURNING id`
	err = store.pool.QueryRow(ctx, insertDeposit, user.ID, address.ID, asset.ID, txHash, blockHash,
		"0x1111111111111111111111111111111111111111", address.Address, DepositConfirming,
	).Scan(&depositID)
	if err != nil {
		t.Fatalf("insert token deposit error = %v", err)
	}
	if err := store.pool.QueryRow(ctx, insertDeposit, user.ID, address.ID, asset.ID, txHash, blockHash,
		"0x1111111111111111111111111111111111111111", address.Address, DepositConfirming,
	).Scan(new(int64)); !isUniqueViolation(err) {
		t.Fatalf("duplicate token deposit error = %v, want unique violation", err)
	}
	deposit, err := store.TokenDepositByID(ctx, depositID)
	if err != nil || deposit.AmountUnits.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("TokenDepositByID() = %+v, error = %v", deposit, err)
	}

	var sweepID int64
	err = store.pool.QueryRow(ctx, `
		INSERT INTO token_sweeps (
			user_id, address_id, asset_id, trigger_deposit_id, recognized_amount_units, status
		) VALUES ($1, $2, $3, $4, 1000000, $5) RETURNING id`,
		user.ID, address.ID, asset.ID, depositID, TokenSweepCreated,
	).Scan(&sweepID)
	if err != nil {
		t.Fatalf("insert token sweep error = %v", err)
	}
	if err := store.pool.QueryRow(ctx, `
		INSERT INTO token_sweeps (
			user_id, address_id, asset_id, trigger_deposit_id, recognized_amount_units, status
		) VALUES ($1, $2, $3, $4, 1, $5) RETURNING id`,
		user.ID, address.ID, asset.ID, depositID, TokenSweepWaitingGas,
	).Scan(new(int64)); !isUniqueViolation(err) {
		t.Fatalf("duplicate active sweep error = %v, want unique violation", err)
	}
	sweep, err := store.TokenSweepByID(ctx, sweepID)
	if err != nil || sweep.RecognizedAmountUnits.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("TokenSweepByID() = %+v, error = %v", sweep, err)
	}

	var transferID int64
	err = store.pool.QueryRow(ctx, `
		INSERT INTO internal_transfers (
			platform_wallet_id, sweep_id, transfer_type, from_address, to_address, amount_wei, status
		) VALUES ($1, $2, $3, $4, $5, 1000000000000000, $6) RETURNING id`,
		hotWallet.ID, sweepID, InternalTransferGasTopup, hotWallet.Address, address.Address, InternalTransferCreated,
	).Scan(&transferID)
	if err != nil {
		t.Fatalf("insert internal transfer error = %v", err)
	}
	transfer, err := store.InternalTransferByID(ctx, transferID)
	if err != nil || transfer.AmountWei.Cmp(big.NewInt(1_000_000_000_000_000)) != 0 {
		t.Fatalf("InternalTransferByID() = %+v, error = %v", transfer, err)
	}

	var withdrawalID int64
	insertWithdrawal := `
		INSERT INTO token_withdrawals (
			idempotency_key, user_id, asset_id, platform_wallet_id, to_address, amount_units, status
		) VALUES ($1, $2, $3, $4, $5, 500000, $6) RETURNING id`
	err = store.pool.QueryRow(ctx, insertWithdrawal, "token-withdraw-1", user.ID, asset.ID, hotWallet.ID,
		"0x2222222222222222222222222222222222222222", WithdrawalCreated,
	).Scan(&withdrawalID)
	if err != nil {
		t.Fatalf("insert token withdrawal error = %v", err)
	}
	if err := store.pool.QueryRow(ctx, insertWithdrawal, "token-withdraw-1", user.ID, asset.ID, hotWallet.ID,
		"0x2222222222222222222222222222222222222222", WithdrawalCreated,
	).Scan(new(int64)); !isUniqueViolation(err) {
		t.Fatalf("duplicate token withdrawal error = %v, want unique violation", err)
	}
	withdrawal, err := store.TokenWithdrawalByID(ctx, withdrawalID)
	if err != nil || withdrawal.AmountUnits.Cmp(big.NewInt(500_000)) != 0 {
		t.Fatalf("TokenWithdrawalByID() = %+v, error = %v", withdrawal, err)
	}
	transactions, err := store.ListTransactionsFilteredPage(ctx, "USDC", "", 20, 0)
	if err != nil {
		t.Fatalf("ListTransactionsFilteredPage(USDC) error = %v", err)
	}
	if len(transactions) != 3 {
		t.Fatalf("USDC transaction count = %d, want 3", len(transactions))
	}
	for _, transaction := range transactions {
		if transaction.Asset != "USDC" || transaction.Decimals != 6 {
			t.Fatalf("USDC transaction = %+v", transaction)
		}
	}
	gasTransfers, err := store.ListTransactionsFilteredPage(ctx, "ETH", "GAS_TOPUP", 20, 0)
	if err != nil || len(gasTransfers) != 1 || gasTransfers[0].ID != transferID {
		t.Fatalf("Gas Top-up transactions = %+v, error = %v", gasTransfers, err)
	}
}

// TestIntegrationERC20MigrationRollsBackAndReapplies 验证第二版迁移可以完整回退并在保留 ETH 数据后重新应用。
func TestIntegrationERC20MigrationRollsBackAndReapplies(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	runner := migrations.NewRunner(store.pool)
	if err := runner.Down(ctx); err != nil {
		t.Fatalf("migrations.Down() error = %v", err)
	}
	version, err := runner.Version(ctx)
	if err != nil || version != 1 {
		t.Fatalf("version after down = %d, error = %v", version, err)
	}
	var tokenTableExists bool
	if err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = 'token_deposits'
		)`).Scan(&tokenTableExists); err != nil || tokenTableExists {
		t.Fatalf("token table exists = %v, error = %v", tokenTableExists, err)
	}
	var ethBalanceRows int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM asset_balances WHERE asset = 'ETH'`).Scan(&ethBalanceRows); err != nil || ethBalanceRows != 2 {
		t.Fatalf("ETH balance rows after down = %d, error = %v", ethBalanceRows, err)
	}
	if err := runner.Up(ctx); err != nil {
		t.Fatalf("migrations.Up() error = %v", err)
	}
	if _, err := migrations.VerifySchema(ctx, store.pool); err != nil {
		t.Fatalf("VerifySchema() error = %v", err)
	}
	user, _ := firstDemoWallet(t, store)
	if _, err := store.BalanceByUser(ctx, user.ID); err != nil {
		t.Fatalf("BalanceByUser() after reapply error = %v", err)
	}
}

// TestIntegrationTokenDepositIsIdempotentAndCreditsOnce 验证 Token 充值重复扫描和并发入账只影响余额一次。
func TestIntegrationTokenDepositIsIdempotentAndCreditsOnce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)
	asset := ensureTestUSDC(t, store)
	observation := tokenDepositObservation(user.ID, address, asset.ID, 900, 0, 1_000_000)
	checkpoint := ChainCheckpoint{
		Network: NetworkSepolia, Scanner: "erc20:test", LastScannedBlock: 900,
		LastScannedHash: observation.BlockHash,
	}
	items, created, err := store.RecordTokenDepositsAndCheckpoint(ctx, []TokenDepositObservation{observation}, checkpoint)
	if err != nil || created != 1 || len(items) != 1 {
		t.Fatalf("RecordTokenDepositsAndCheckpoint() created = %d, items = %d, error = %v", created, len(items), err)
	}
	deposit := items[0]
	_, created, err = store.RecordTokenDepositsAndCheckpoint(ctx, []TokenDepositObservation{observation}, checkpoint)
	if err != nil || created != 0 {
		t.Fatalf("duplicate token deposit created = %d, error = %v", created, err)
	}
	balance, err := store.BalanceByUserAndAsset(ctx, user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() error = %v", err)
	}
	assertBigInt(t, "Token pending", balance.PendingDepositWei, 1_000_000)
	assertBigInt(t, "Token available before credit", balance.AvailableWei, 0)

	type creditResult struct {
		credited bool
		err      error
	}
	results := make(chan creditResult, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, credited, err := store.CreditTokenDeposit(ctx, deposit.ID, 3)
			results <- creditResult{credited: credited, err: err}
		}()
	}
	group.Wait()
	close(results)
	creditedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("CreditTokenDeposit() error = %v", result.err)
		}
		if result.credited {
			creditedCount++
		}
	}
	if creditedCount != 1 {
		t.Fatalf("credited count = %d, want 1", creditedCount)
	}
	balance, err = store.BalanceByUserAndAsset(ctx, user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() after credit error = %v", err)
	}
	assertBigInt(t, "Token pending after credit", balance.PendingDepositWei, 0)
	assertBigInt(t, "Token available after credit", balance.AvailableWei, 1_000_000)
	var sweepCount int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM token_sweeps WHERE trigger_deposit_id = $1`, deposit.ID).Scan(&sweepCount); err != nil || sweepCount != 1 {
		t.Fatalf("sweep count = %d, error = %v", sweepCount, err)
	}
}

// TestIntegrationTokenDepositSupportsDuplicateLogsAndMultipleEvents 验证同交易多 Event 与重复日志按 log_index 幂等处理。
func TestIntegrationTokenDepositSupportsDuplicateLogsAndMultipleEvents(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers() count = %d, error = %v", len(users), err)
	}
	firstAddress, err := store.WalletAddressByUser(ctx, users[0].ID)
	if err != nil {
		t.Fatalf("WalletAddressByUser(first) error = %v", err)
	}
	secondAddress, err := store.WalletAddressByUser(ctx, users[1].ID)
	if err != nil {
		t.Fatalf("WalletAddressByUser(second) error = %v", err)
	}
	asset := ensureTestUSDC(t, store)
	first := tokenDepositObservation(users[0].ID, firstAddress, asset.ID, 910, 0, 100)
	second := tokenDepositObservation(users[1].ID, secondAddress, asset.ID, 910, 1, 200)
	second.TxHash = first.TxHash
	checkpoint := ChainCheckpoint{Network: NetworkSepolia, Scanner: "erc20:test", LastScannedBlock: 910, LastScannedHash: first.BlockHash}
	_, created, err := store.RecordTokenDepositsAndCheckpoint(ctx, []TokenDepositObservation{second, first, first}, checkpoint)
	if err != nil || created != 2 {
		t.Fatalf("multi-event created = %d, error = %v", created, err)
	}
	firstBalance, err := store.BalanceByUserAndAsset(ctx, users[0].ID, asset.ID)
	if err != nil {
		t.Fatalf("first BalanceByUserAndAsset() error = %v", err)
	}
	secondBalance, err := store.BalanceByUserAndAsset(ctx, users[1].ID, asset.ID)
	if err != nil {
		t.Fatalf("second BalanceByUserAndAsset() error = %v", err)
	}
	assertBigInt(t, "first Token pending", firstBalance.PendingDepositWei, 100)
	assertBigInt(t, "second Token pending", secondBalance.PendingDepositWei, 200)
}

// TestIntegrationTokenWithdrawalPreventsOverspendAndSettlesBothResults 验证 Token 提币并发占用、幂等、共享 Nonce 和成功失败结算。
func TestIntegrationTokenWithdrawalPreventsOverspendAndSettlesBothResults(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, _ := firstDemoWallet(t, store)
	asset := ensureTestUSDC(t, store)
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	platform, err := store.EnsurePlatformHotWallet(ctx, provider, wallet.TreasuryPath)
	if err != nil {
		t.Fatalf("EnsurePlatformHotWallet() error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		UPDATE asset_balances SET available_wei = 1000, pending_withdrawal_wei = 0
		WHERE user_id = $1 AND asset_id = $2`, user.ID, asset.ID); err != nil {
		t.Fatalf("fund Token balance error = %v", err)
	}
	requests := []TokenWithdrawalRequest{
		tokenWithdrawalRequest("token-concurrent-1", user.ID, asset.ID, platform.ID, 700),
		tokenWithdrawalRequest("token-concurrent-2", user.ID, asset.ID, platform.ID, 700),
	}
	type reserveResult struct {
		item TokenWithdrawal
		err  error
	}
	results := make(chan reserveResult, len(requests))
	var group sync.WaitGroup
	for _, request := range requests {
		request := request
		group.Add(1)
		go func() {
			defer group.Done()
			item, _, err := store.ReserveTokenWithdrawal(ctx, request)
			results <- reserveResult{item: item, err: err}
		}()
	}
	group.Wait()
	close(results)
	var winner TokenWithdrawal
	var successes, insufficient int
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			winner = result.item
		case errors.Is(result.err, ErrInsufficientBalance):
			insufficient++
		default:
			t.Fatalf("unexpected Token withdrawal error = %v", result.err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("successes = %d, insufficient = %d", successes, insufficient)
	}
	balance, err := store.BalanceByUserAndAsset(ctx, user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() error = %v", err)
	}
	assertBigInt(t, "Token available after reserve", balance.AvailableWei, 300)
	assertBigInt(t, "Token pending after reserve", balance.PendingWithdrawalWei, 700)
	duplicateRequest := tokenWithdrawalRequest(winner.IdempotencyKey, user.ID, asset.ID, platform.ID, 700)
	duplicate, created, err := store.ReserveTokenWithdrawal(ctx, duplicateRequest)
	if err != nil || created || duplicate.ID != winner.ID {
		t.Fatalf("duplicate reserve id = %d, created = %v, error = %v", duplicate.ID, created, err)
	}
	failed, _, err := store.AllocateTokenWithdrawalNonce(ctx, winner.ID, 5)
	if err != nil || failed.Nonce == nil || failed.Nonce.Uint64() != 5 {
		t.Fatalf("AllocateTokenWithdrawalNonce() nonce = %v, error = %v", failed.Nonce, err)
	}
	failed, _, err = store.SaveSignedTokenWithdrawal(ctx, signedTokenWithdrawal(failed.ID, 1))
	if err != nil {
		t.Fatalf("SaveSignedTokenWithdrawal() error = %v", err)
	}
	failed, err = store.TransitionTokenWithdrawal(ctx, failed.ID, WithdrawalBroadcasted)
	if err != nil {
		t.Fatalf("TransitionTokenWithdrawal() error = %v", err)
	}
	failed, changed, err := store.FinalizeTokenWithdrawal(ctx, TokenWithdrawalSettlement{
		WithdrawalID: failed.ID, ActualFeeWei: big.NewInt(123), Success: false,
		BlockNumber: 100, Confirmations: 3,
	})
	if err != nil || !changed || failed.Status != WithdrawalFailed || failed.ActualFeeWei.Cmp(big.NewInt(123)) != 0 {
		t.Fatalf("failed settlement = %+v, changed = %v, error = %v", failed, changed, err)
	}
	balance, err = store.BalanceByUserAndAsset(ctx, user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() after failure error = %v", err)
	}
	assertBigInt(t, "Token available after failure", balance.AvailableWei, 1_000)
	assertBigInt(t, "Token pending after failure", balance.PendingWithdrawalWei, 0)
	success, _, err := store.ReserveTokenWithdrawal(ctx, tokenWithdrawalRequest("token-success", user.ID, asset.ID, platform.ID, 400))
	if err != nil {
		t.Fatalf("ReserveTokenWithdrawal(success) error = %v", err)
	}
	success, _, err = store.AllocateTokenWithdrawalNonce(ctx, success.ID, 5)
	if err != nil || success.Nonce == nil || success.Nonce.Uint64() != 6 {
		t.Fatalf("shared platform nonce = %v, error = %v", success.Nonce, err)
	}
	success, _, err = store.SaveSignedTokenWithdrawal(ctx, signedTokenWithdrawal(success.ID, 2))
	if err != nil {
		t.Fatalf("SaveSignedTokenWithdrawal(success) error = %v", err)
	}
	if _, err := store.TransitionTokenWithdrawal(ctx, success.ID, WithdrawalBroadcasted); err != nil {
		t.Fatalf("TransitionTokenWithdrawal(success) error = %v", err)
	}
	success, changed, err = store.FinalizeTokenWithdrawal(ctx, TokenWithdrawalSettlement{
		WithdrawalID: success.ID, ActualFeeWei: big.NewInt(456), Success: true,
		BlockNumber: 101, Confirmations: 3,
	})
	if err != nil || !changed || success.Status != WithdrawalCompleted || success.ActualFeeWei.Cmp(big.NewInt(456)) != 0 {
		t.Fatalf("success settlement = %+v, changed = %v, error = %v", success, changed, err)
	}
	balance, err = store.BalanceByUserAndAsset(ctx, user.ID, asset.ID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() after success error = %v", err)
	}
	assertBigInt(t, "Token available after success", balance.AvailableWei, 600)
	assertBigInt(t, "Token pending after success", balance.PendingWithdrawalWei, 0)
}

// TestIntegrationGasTopupAllocatesOnePlatformNonce 验证并发补气只创建一笔内部转账并占用一个平台 Nonce。
func TestIntegrationGasTopupAllocatesOnePlatformNonce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, address, platform := createGasTopupFixture(t, store, provider)
	request := GasTopupRequest{
		SweepID: sweep.ID, PlatformWalletID: platform.ID, FromAddress: platform.Address,
		ToAddress: address.Address, AmountWei: big.NewInt(12_000_000), ChainPendingNonce: 7,
	}
	type result struct {
		item    InternalTransfer
		created bool
		err     error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			item, created, err := store.CreateOrGetGasTopup(ctx, request)
			results <- result{item: item, created: created, err: err}
		}()
	}
	group.Wait()
	close(results)
	createdCount := 0
	var transferID int64
	for result := range results {
		if result.err != nil {
			t.Fatalf("CreateOrGetGasTopup() error = %v", result.err)
		}
		if result.created {
			createdCount++
		}
		if transferID == 0 {
			transferID = result.item.ID
		}
		if result.item.ID != transferID || result.item.Nonce.Cmp(big.NewInt(7)) != 0 {
			t.Fatalf("transfer = %+v, want same id and nonce 7", result.item)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
	updatedPlatform, err := store.PlatformWalletByRole(ctx, NetworkSepolia, PlatformRoleHot)
	if err != nil || updatedPlatform.NextNonce.Cmp(big.NewInt(8)) != 0 {
		t.Fatalf("platform next nonce = %+v, error = %v", updatedPlatform.NextNonce, err)
	}
}

// TestIntegrationInternalGasTopupDoesNotChangeUserETHBalance 验证内部补气登记和结算不会写入用户 ETH 账本。
func TestIntegrationInternalGasTopupDoesNotChangeUserETHBalance(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, address, platform := createGasTopupFixture(t, store, provider)
	before, err := store.BalanceByUser(ctx, sweep.UserID)
	if err != nil {
		t.Fatalf("BalanceByUser() before error = %v", err)
	}
	transfer, _, err := store.CreateOrGetGasTopup(ctx, GasTopupRequest{
		SweepID: sweep.ID, PlatformWalletID: platform.ID, FromAddress: platform.Address,
		ToAddress: address.Address, AmountWei: big.NewInt(12_000_000), ChainPendingNonce: 3,
	})
	if err != nil {
		t.Fatalf("CreateOrGetGasTopup() error = %v", err)
	}
	txHash := fmt.Sprintf("0x%064x", 9911)
	transfer, _, err = store.SaveSignedInternalTransfer(ctx, SignedInternalTransfer{
		TransferID: transfer.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(202),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{1, 2, 3}, TxHash: txHash,
	})
	if err != nil {
		t.Fatalf("SaveSignedInternalTransfer() error = %v", err)
	}
	internal, err := store.IsInternalTransferTx(ctx, txHash)
	if err != nil || !internal {
		t.Fatalf("IsInternalTransferTx() = %v, error = %v", internal, err)
	}
	transfer, err = store.TransitionInternalTransfer(ctx, transfer.ID, InternalTransferSent)
	if err != nil {
		t.Fatalf("TransitionInternalTransfer() error = %v", err)
	}
	_, changed, err := store.FinalizeInternalTransfer(ctx, InternalTransferSettlement{
		TransferID: transfer.ID, ActualFeeWei: big.NewInt(2_100_000), Success: true,
		BlockNumber: 100, Confirmations: 3,
	})
	if err != nil || !changed {
		t.Fatalf("FinalizeInternalTransfer() changed = %v, error = %v", changed, err)
	}
	after, err := store.BalanceByUser(ctx, sweep.UserID)
	if err != nil {
		t.Fatalf("BalanceByUser() after error = %v", err)
	}
	if before.AvailableWei.Cmp(after.AvailableWei) != 0 || before.PendingDepositWei.Cmp(after.PendingDepositWei) != 0 ||
		before.PendingWithdrawalWei.Cmp(after.PendingWithdrawalWei) != 0 {
		t.Fatalf("ETH balance changed: before = %+v, after = %+v", before, after)
	}
	var internalEntries int
	if err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM balance_entries WHERE user_id = $1 AND reference_type = 'INTERNAL_TRANSFER'`,
		sweep.UserID,
	).Scan(&internalEntries); err != nil || internalEntries != 0 {
		t.Fatalf("internal balance entries = %d, error = %v", internalEntries, err)
	}
}

// createGasTopupFixture 创建已入账 Token、归集任务和平台热钱包测试夹具。
func createGasTopupFixture(t *testing.T, store *Store, provider wallet.KeyProvider) (TokenSweep, WalletAddress, PlatformWallet) {
	t.Helper()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)
	asset := ensureTestUSDC(t, store)
	observation := tokenDepositObservation(user.ID, address, asset.ID, 930, 0, 1_000_000)
	items, _, err := store.RecordTokenDepositsAndCheckpoint(ctx, []TokenDepositObservation{observation}, ChainCheckpoint{
		Network: NetworkSepolia, Scanner: "erc20:gas-test", LastScannedBlock: 930, LastScannedHash: observation.BlockHash,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("RecordTokenDepositsAndCheckpoint() items = %d, error = %v", len(items), err)
	}
	if _, _, err := store.CreditTokenDeposit(ctx, items[0].ID, 3); err != nil {
		t.Fatalf("CreditTokenDeposit() error = %v", err)
	}
	var sweepID int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM token_sweeps WHERE trigger_deposit_id = $1`, items[0].ID).Scan(&sweepID); err != nil {
		t.Fatalf("query token sweep error = %v", err)
	}
	sweep, err := store.TokenSweepByID(ctx, sweepID)
	if err != nil {
		t.Fatalf("TokenSweepByID() error = %v", err)
	}
	platform, err := store.EnsurePlatformHotWallet(ctx, provider, wallet.TreasuryPath)
	if err != nil {
		t.Fatalf("EnsurePlatformHotWallet() error = %v", err)
	}
	return sweep, address, platform
}

// TestIntegrationTokenSweepMergesDepositAndCreatesSuccessor 验证新充值合并到活动任务且签名后新增金额生成后继归集。
func TestIntegrationTokenSweepMergesDepositAndCreatesSuccessor(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, address, _ := createGasTopupFixture(t, store, provider)
	if _, _, err := store.MarkTokenSweepGasReady(ctx, sweep.ID); err != nil {
		t.Fatalf("MarkTokenSweepGasReady() error = %v", err)
	}
	sweep, _, err = store.AllocateTokenSweepNonce(ctx, sweep.ID, big.NewInt(1_000_000), 7)
	if err != nil {
		t.Fatalf("AllocateTokenSweepNonce() error = %v", err)
	}
	creditAdditionalToken(t, store, sweep.UserID, address, sweep.AssetID, 931, 500_000)
	sweep, err = store.TokenSweepByID(ctx, sweep.ID)
	if err != nil || sweep.RecognizedAmountUnits.Cmp(big.NewInt(1_500_000)) != 0 || sweep.SweepAmountUnits.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("merged sweep = %+v, error = %v", sweep, err)
	}
	txHash := fmt.Sprintf("0x%064x", 9921)
	sweep, _, err = store.SaveSignedTokenSweep(ctx, SignedTokenSweep{
		SweepID: sweep.ID, GasLimit: 50_000, MaxFeePerGasWei: big.NewInt(202),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{4, 5, 6}, TxHash: txHash,
	})
	if err != nil {
		t.Fatalf("SaveSignedTokenSweep() error = %v", err)
	}
	sweep, err = store.TransitionTokenSweep(ctx, sweep.ID, TokenSweepBroadcasted)
	if err != nil {
		t.Fatalf("TransitionTokenSweep() error = %v", err)
	}
	_, changed, err := store.FinalizeTokenSweep(ctx, TokenSweepSettlement{
		SweepID: sweep.ID, ActualFeeWei: big.NewInt(5_000_000), Success: true,
		BlockNumber: 940, Confirmations: 3,
	})
	if err != nil || !changed {
		t.Fatalf("FinalizeTokenSweep() changed = %v, error = %v", changed, err)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT `+tokenSweepColumns+` FROM token_sweeps WHERE address_id = $1 AND asset_id = $2 ORDER BY id`,
		address.ID, sweep.AssetID,
	)
	if err != nil {
		t.Fatalf("query token sweeps error = %v", err)
	}
	defer rows.Close()
	items := make([]TokenSweep, 0, 2)
	for rows.Next() {
		item, err := store.scanTokenSweep(rows)
		if err != nil {
			t.Fatalf("scanTokenSweep() error = %v", err)
		}
		items = append(items, item)
	}
	if len(items) != 2 || items[0].Status != TokenSweepCompleted || items[1].Status != TokenSweepCreated ||
		items[1].RecognizedAmountUnits.Cmp(big.NewInt(500_000)) != 0 {
		t.Fatalf("sweep items = %+v", items)
	}
}

// TestIntegrationTokenSweepAllocatesOneUserNonce 验证同一归集任务并发抢占只分配一个用户地址 Nonce。
func TestIntegrationTokenSweepAllocatesOneUserNonce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, address, _ := createGasTopupFixture(t, store, provider)
	if _, _, err := store.MarkTokenSweepGasReady(ctx, sweep.ID); err != nil {
		t.Fatalf("MarkTokenSweepGasReady() error = %v", err)
	}
	type result struct {
		item    TokenSweep
		changed bool
		err     error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			item, changed, err := store.AllocateTokenSweepNonce(ctx, sweep.ID, big.NewInt(1_000_000), 9)
			results <- result{item: item, changed: changed, err: err}
		}()
	}
	group.Wait()
	close(results)
	changedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("AllocateTokenSweepNonce() error = %v", result.err)
		}
		if result.changed {
			changedCount++
		}
		if result.item.Nonce == nil || result.item.Nonce.Cmp(big.NewInt(9)) != 0 {
			t.Fatalf("allocated nonce = %v, want 9", result.item.Nonce)
		}
	}
	if changedCount != 1 {
		t.Fatalf("changed count = %d, want 1", changedCount)
	}
	updatedAddress, err := store.WalletAddressByUser(ctx, sweep.UserID)
	if err != nil || updatedAddress.ID != address.ID || updatedAddress.NextNonce.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("wallet next nonce = %+v, error = %v", updatedAddress.NextNonce, err)
	}
}

// TestIntegrationFailedTokenSweepDoesNotChangeUserTokenBalance 验证归集链上失败只记录 Gas 和错误，不修改用户 Token 账本。
func TestIntegrationFailedTokenSweepDoesNotChangeUserTokenBalance(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, _, _ := createGasTopupFixture(t, store, provider)
	before, err := store.BalanceByUserAndAsset(ctx, sweep.UserID, sweep.AssetID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() before error = %v", err)
	}
	if _, _, err := store.MarkTokenSweepGasReady(ctx, sweep.ID); err != nil {
		t.Fatalf("MarkTokenSweepGasReady() error = %v", err)
	}
	sweep, _, err = store.AllocateTokenSweepNonce(ctx, sweep.ID, big.NewInt(1_000_000), 3)
	if err != nil {
		t.Fatalf("AllocateTokenSweepNonce() error = %v", err)
	}
	sweep, _, err = store.SaveSignedTokenSweep(ctx, SignedTokenSweep{
		SweepID: sweep.ID, GasLimit: 50_000, MaxFeePerGasWei: big.NewInt(202),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{7, 8, 9}, TxHash: fmt.Sprintf("0x%064x", 9931),
	})
	if err != nil {
		t.Fatalf("SaveSignedTokenSweep() error = %v", err)
	}
	sweep, err = store.TransitionTokenSweep(ctx, sweep.ID, TokenSweepBroadcasted)
	if err != nil {
		t.Fatalf("TransitionTokenSweep() error = %v", err)
	}
	result, changed, err := store.FinalizeTokenSweep(ctx, TokenSweepSettlement{
		SweepID: sweep.ID, ActualFeeWei: big.NewInt(4_900_000), Success: false,
		BlockNumber: 950, Confirmations: 3, ErrorCode: "TOKEN_SWEEP_REVERTED",
		ErrorMessage: "Token 归集交易链上执行失败",
	})
	if err != nil || !changed || result.Status != TokenSweepFailed {
		t.Fatalf("FinalizeTokenSweep() result = %+v, changed = %v, error = %v", result, changed, err)
	}
	after, err := store.BalanceByUserAndAsset(ctx, sweep.UserID, sweep.AssetID)
	if err != nil {
		t.Fatalf("BalanceByUserAndAsset() after error = %v", err)
	}
	if before.AvailableWei.Cmp(after.AvailableWei) != 0 || before.PendingDepositWei.Cmp(after.PendingDepositWei) != 0 ||
		before.PendingWithdrawalWei.Cmp(after.PendingWithdrawalWei) != 0 {
		t.Fatalf("Token balance changed: before = %+v, after = %+v", before, after)
	}
}

// creditAdditionalToken 创建并入账同一用户地址的后续 Token 充值。
func creditAdditionalToken(t *testing.T, store *Store, userID int64, address WalletAddress, assetID, block, amountUnits int64) {
	t.Helper()
	observation := tokenDepositObservation(userID, address, assetID, block, 0, amountUnits)
	items, _, err := store.RecordTokenDepositsAndCheckpoint(context.Background(), []TokenDepositObservation{observation}, ChainCheckpoint{
		Network: NetworkSepolia, Scanner: "erc20:sweep-test", LastScannedBlock: block, LastScannedHash: observation.BlockHash,
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("RecordTokenDepositsAndCheckpoint() items = %d, error = %v", len(items), err)
	}
	if _, _, err := store.CreditTokenDeposit(context.Background(), items[0].ID, 3); err != nil {
		t.Fatalf("CreditTokenDeposit() error = %v", err)
	}
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

// ensureTestUSDC 幂等初始化集成测试使用的 Sepolia USDC 资产。
func ensureTestUSDC(t *testing.T, store *Store) Asset {
	t.Helper()
	asset, err := store.EnsureERC20Asset(context.Background(), Asset{
		Network: NetworkSepolia, Symbol: "USDC", ContractAddress: "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238",
		Decimals: 6, Enabled: true,
	})
	if err != nil {
		t.Fatalf("EnsureERC20Asset() error = %v", err)
	}
	return asset
}

// tokenDepositObservation 构造确定性的 Token Event 观察数据。
func tokenDepositObservation(userID int64, address WalletAddress, assetID, block int64, logIndex int32, value int64) TokenDepositObservation {
	return TokenDepositObservation{
		UserID: userID, AddressID: address.ID, AssetID: assetID,
		TxHash: fmt.Sprintf("0x%064x", block), LogIndex: logIndex, BlockNumber: block,
		BlockHash:   fmt.Sprintf("0x%064x", block+10_000),
		FromAddress: "0x3333333333333333333333333333333333333333", ToAddress: address.Address,
		AmountUnits: big.NewInt(value),
	}
}

// tokenWithdrawalRequest 构造测试 Token 提币余额占用请求。
func tokenWithdrawalRequest(key string, userID, assetID, platformWalletID, value int64) TokenWithdrawalRequest {
	return TokenWithdrawalRequest{
		IdempotencyKey: key, UserID: userID, AssetID: assetID, PlatformWalletID: platformWalletID,
		ToAddress: "0x2222222222222222222222222222222222222222", AmountUnits: big.NewInt(value),
	}
}

// signedTokenWithdrawal 构造数据库状态机测试使用的确定性签名结果。
func signedTokenWithdrawal(withdrawalID, hashValue int64) SignedTokenWithdrawal {
	return SignedTokenWithdrawal{
		WithdrawalID: withdrawalID, GasLimit: 50_000, MaxFeePerGasWei: big.NewInt(202),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{byte(hashValue)},
		TxHash: fmt.Sprintf("0x%064x", hashValue),
	}
}
