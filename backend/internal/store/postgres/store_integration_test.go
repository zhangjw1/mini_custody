package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/btc"
	"github.com/xiaoqi/mini-custody/backend/internal/config"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
	"github.com/xiaoqi/mini-custody/backend/migrations"
)

const integrationTestMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// TestIntegrationBTCReorgReversesPendingDeposit 验证 BTC 重组原子撤销待确认余额、充值和 UTXO。
func TestIntegrationBTCReorgReversesPendingDeposit(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureBitcoinWallets(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	addresses, err := store.ListBTCAddresses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var address btc.Address
	for _, item := range addresses {
		address = item
		break
	}
	if address.ID == 0 {
		t.Fatal("missing BTC address")
	}
	observation := btc.DepositObservation{UserID: address.UserID, AddressID: address.ID, TxID: strings.Repeat("a", 64), Vout: 0, BlockHash: strings.Repeat("b", 64), BlockHeight: 10, AmountSats: 1000, ScriptPubKey: address.ScriptPubKey}
	if _, err = store.RecordBTCDepositsAndCheckpoint(context.Background(), []btc.DepositObservation{observation}, btc.Checkpoint{Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 10, LastScannedHash: observation.BlockHash}); err != nil {
		t.Fatal(err)
	}
	if err = store.RewindBTCCheckpoint(context.Background(), 9, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListConfirmingBTCDeposits(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("confirming deposits = %d, want 0", len(items))
	}
	var depositStatus, utxoStatus string
	var entryCount int
	var pendingBalance string
	if err = store.pool.QueryRow(context.Background(), `SELECT status FROM btc_deposits WHERE txid=$1`, observation.TxID).Scan(&depositStatus); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT status FROM btc_utxos WHERE txid=$1`, observation.TxID).Scan(&utxoStatus); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM balance_entries WHERE reference_type='BTC_DEPOSIT' AND entry_type IN ('DEPOSIT_PENDING','DEPOSIT_REVERSAL')`).Scan(&entryCount); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT pending_deposit_wei::text FROM asset_balances WHERE user_id=$1 AND asset='BTC'`, address.UserID).Scan(&pendingBalance); err != nil {
		t.Fatal(err)
	}
	if depositStatus != DepositOrphaned || utxoStatus != btc.UTXOStatusUnknown || entryCount != 2 || pendingBalance != "0" {
		t.Fatalf("after rewind: depositStatus=%s utxoStatus=%s entryCount=%d pending=%s", depositStatus, utxoStatus, entryCount, pendingBalance)
	}
}

// TestIntegrationBTCLockUTXOsConcurrent 验证并发提币选币只允许一个事务锁定同一组平台 UTXO。
func TestIntegrationBTCLockUTXOsConcurrent(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureBitcoinWallets(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	users, err := store.ListUsers(context.Background())
	if err != nil || len(users) == 0 {
		t.Fatalf("ListUsers() error = %v", err)
	}
	user := users[0]
	var assetID, platformAddrID int64
	var platformScript string
	if err = store.pool.QueryRow(context.Background(), `SELECT id FROM assets WHERE network=$1 AND symbol='BTC'`, NetworkBitcoinSignet).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT id, script_pub_key FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE' LIMIT 1`, NetworkBitcoinSignet).Scan(&platformAddrID, &platformScript); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(context.Background(), `UPDATE asset_balances SET available_wei=50000 WHERE user_id=$1 AND asset_id=$2`, user.ID, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(context.Background(), `INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) VALUES(NULL,$1,$2,$3,0,5000,$4,10,'UNSPENT')`, platformAddrID, NetworkBitcoinSignet, strings.Repeat("d", 64), platformScript); err != nil {
		t.Fatal(err)
	}
	userBTC, err := store.BTCAddressByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	w1, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-lock-1", userBTC.Address, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	w2, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-lock-2", userBTC.Address, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		count int
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, wID := range []int64{w1.ID, w2.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			items, _, _, _, lockErr := store.LockWithdrawalInputs(context.Background(), id, 1000, btc.WithdrawalLocker(id))
			results <- result{len(items), lockErr}
		}(wID)
	}
	wg.Wait()
	close(results)
	success := 0
	for item := range results {
		if item.err == nil {
			success++
			if item.count == 0 {
				t.Fatal("successful lock returned no UTXO")
			}
		}
	}
	if success != 1 {
		t.Fatalf("successful concurrent locks=%d, want 1", success)
	}
}

// TestIntegrationBTCFixesA1ToA5 验证 A1-A5 资金安全修复：raw_tx/txid落库、防重复出款、找零入库与幂等结算、充值UTXO锁定隔离及重入复用。
func TestIntegrationBTCFixesA1ToA5(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureBitcoinWallets(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	user := users[0]
	userBTC, err := store.BTCAddressByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var platformAddrID int64
	var platformScriptHex, platformPath string
	if err = store.pool.QueryRow(context.Background(), `SELECT id, script_pub_key, derivation_path FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE' LIMIT 1`, NetworkBitcoinSignet).Scan(&platformAddrID, &platformScriptHex, &platformPath); err != nil {
		t.Fatal(err)
	}

	// 1. 验证 A4：充值入账直接锁定 UTXO 给归集任务，提币绝不选中充值 UTXO；归集完成后 UTXO 为 SPENT，输出为 UNSPENT。
	obs := btc.DepositObservation{
		UserID: user.ID, AddressID: userBTC.ID, TxID: strings.Repeat("1", 64), Vout: 0,
		BlockHash: strings.Repeat("2", 64), BlockHeight: 30, AmountSats: 20_000, ScriptPubKey: userBTC.ScriptPubKey,
	}
	if _, err = store.RecordBTCDepositsAndCheckpoint(context.Background(), []btc.DepositObservation{obs}, btc.Checkpoint{
		Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 30, LastScannedHash: obs.BlockHash,
	}); err != nil {
		t.Fatal(err)
	}
	var depID int64
	if err = store.pool.QueryRow(context.Background(), `SELECT id FROM btc_deposits WHERE txid=$1`, obs.TxID).Scan(&depID); err != nil {
		t.Fatal(err)
	}
	if err = store.CreditBTCDeposit(context.Background(), depID, 3); err != nil {
		t.Fatal(err)
	}
	var utxoStatus, lockedBy string
	if err = store.pool.QueryRow(context.Background(), `SELECT status, locked_by FROM btc_utxos WHERE deposit_id=$1`, depID).Scan(&utxoStatus, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if utxoStatus != btc.UTXOStatusLocked || lockedBy != btc.SweepLockBy {
		t.Fatalf("deposit utxo status=%s locked_by=%s, want LOCKED/%s", utxoStatus, lockedBy, btc.SweepLockBy)
	}
	// 此时没有 PLATFORM_CHANGE UTXO，提币必须失败而不能选中充值 UTXO。
	wNoFund, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-no-platform-utxo", userBTC.Address, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, lockErr := store.LockWithdrawalInputs(context.Background(), wNoFund.ID, wNoFund.AmountSats, btc.WithdrawalLocker(wNoFund.ID)); lockErr == nil {
		t.Fatal("expected withdrawal to fail when only USER_DEPOSIT UTXO exists")
	}
	if err = store.FailBTCWithdrawal(context.Background(), wNoFund.ID, "NO_UTXO", "缺少平台 UTXO"); err != nil {
		t.Fatal(err)
	}
	var releaseCount int
	if err = store.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM balance_entries WHERE reference_type='BTC_WITHDRAWAL' AND reference_id=$1 AND entry_type='WITHDRAW_RELEASE'`, wNoFund.ID).Scan(&releaseCount); err != nil || releaseCount != 1 {
		t.Fatalf("WITHDRAW_RELEASE count=%d err=%v", releaseCount, err)
	}

	// 完成归集，产生平台 UNSPENT UTXO。
	sweeps, err := store.ListProcessableBTCSweeps(context.Background(), 10)
	if err != nil || len(sweeps) != 1 {
		t.Fatalf("ListProcessableBTCSweeps() count=%d err=%v", len(sweeps), err)
	}
	builtSweep, err := btc.BuildSweep(provider, sweeps[0].From.Path, sweeps[0].UTXO, sweeps[0].To, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveBTCSweepSigned(context.Background(), sweeps[0].ID, builtSweep.RawTx, builtSweep.TxID, builtSweep.OutputValueSats, builtSweep.FeeSats, builtSweep.FeeRateSatVB); err != nil {
		t.Fatal(err)
	}
	if _, err = store.MarkBTCSweepBroadcasted(context.Background(), sweeps[0].ID, builtSweep.TxID); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBTCSweep(context.Background(), sweeps[0].ID, 3, 35); err != nil {
		t.Fatal(err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT status FROM btc_utxos WHERE deposit_id=$1`, depID).Scan(&utxoStatus); err != nil || utxoStatus != btc.UTXOStatusSpent {
		t.Fatalf("deposit utxo status=%s err=%v, want SPENT", utxoStatus, err)
	}

	// 2. 验证 A5 + A1 + A2 + A3：LockWithdrawalInputs 推进状态并复用、raw_tx/txid 落库、SIGNED 禁退款、并发 CompleteBTCWithdrawal 幂等和找零入库。
	w, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-main-flow", userBTC.Address, 5_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	inputs1, addrs1, targetScript, changeScript, err := store.LockWithdrawalInputs(context.Background(), w.ID, w.AmountSats, btc.WithdrawalLocker(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	var wStatus string
	if err = store.pool.QueryRow(context.Background(), `SELECT status FROM btc_withdrawals WHERE id=$1`, w.ID).Scan(&wStatus); err != nil || wStatus != btc.WithdrawalInputsLocked {
		t.Fatalf("status=%s err=%v, want INPUTS_LOCKED", wStatus, err)
	}
	// 模拟崩溃重试：再次调用 LockWithdrawalInputs 必须返回完全相同的一批输入，不新锁 UTXO。
	inputs2, _, _, _, err := store.LockWithdrawalInputs(context.Background(), w.ID, w.AmountSats, btc.WithdrawalLocker(w.ID))
	if err != nil || len(inputs2) != len(inputs1) || inputs2[0].ID != inputs1[0].ID {
		t.Fatalf("retry LockWithdrawalInputs mismatch: inputs1=%+v inputs2=%+v err=%v", inputs1, inputs2, err)
	}
	raw, fee, changeAmt, err := btc.BuildWithdrawal(provider, inputs1, addrs1, targetScript, changeScript, w.AmountSats, w.FeeRateSatVB)
	if err != nil {
		t.Fatal(err)
	}
	signedItem, err := store.SaveBTCWithdrawalSigned(context.Background(), w.ID, raw, fee, changeAmt)
	if err != nil {
		t.Fatal(err)
	}
	if len(signedItem.RawTx) == 0 || len(signedItem.TxID) != 64 {
		t.Fatalf("expected raw_tx and txid saved, got len(raw)=%d txid=%q", len(signedItem.RawTx), signedItem.TxID)
	}
	// SIGNED 状态下不允许 FailBTCWithdrawal 退款。
	if failErr := store.FailBTCWithdrawal(context.Background(), w.ID, "MEMPOOL_REJECTED", "不应退款"); !errors.Is(failErr, ErrInvalidState) {
		t.Fatalf("FailBTCWithdrawal on SIGNED err = %v, want ErrInvalidState", failErr)
	}
	// 推进到 BROADCASTED 后，并发调用两次 CompleteBTCWithdrawal。
	if err = store.MarkBTCWithdrawalBroadcasted(context.Background(), w.ID, signedItem.TxID); err != nil {
		t.Fatal(err)
	}
	var completeWG sync.WaitGroup
	completeErrs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		completeWG.Add(1)
		go func() {
			defer completeWG.Done()
			completeErrs <- store.CompleteBTCWithdrawal(context.Background(), w.ID, 3, 40)
		}()
	}
	completeWG.Wait()
	close(completeErrs)
	for cErr := range completeErrs {
		if cErr != nil {
			t.Fatalf("CompleteBTCWithdrawal() concurrent error = %v", cErr)
		}
	}
	var finalizeCount int
	if err = store.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM balance_entries WHERE reference_type='BTC_WITHDRAWAL' AND reference_id=$1 AND entry_type='WITHDRAW_FINALIZE'`, w.ID).Scan(&finalizeCount); err != nil || finalizeCount != 1 {
		t.Fatalf("WITHDRAW_FINALIZE count=%d err=%v", finalizeCount, err)
	}
	var pendingStr string
	if err = store.pool.QueryRow(context.Background(), `SELECT pending_withdrawal_wei::text FROM asset_balances WHERE user_id=$1 AND asset='BTC'`, user.ID).Scan(&pendingStr); err != nil || pendingStr != "0" {
		t.Fatalf("pending_withdrawal_wei=%s err=%v, want 0", pendingStr, err)
	}
	// 验证找零 UTXO 已入库为 PLATFORM_CHANGE UNSPENT，且下一笔提币可以选中它。
	var changeUTXOValue int64
	if err = store.pool.QueryRow(context.Background(), `SELECT value_sats FROM btc_utxos WHERE txid=$1 AND vout=$2 AND status='UNSPENT'`, signedItem.TxID, btc.ChangeOutputVout).Scan(&changeUTXOValue); err != nil || changeUTXOValue != changeAmt {
		t.Fatalf("change utxo value=%d want %d err=%v", changeUTXOValue, changeAmt, err)
	}
	wNext, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-use-change", userBTC.Address, 1_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	nextInputs, _, _, _, err := store.LockWithdrawalInputs(context.Background(), wNext.ID, wNext.AmountSats, btc.WithdrawalLocker(wNext.ID))
	if err != nil || len(nextInputs) != 1 || nextInputs[0].TxID != signedItem.TxID {
		t.Fatalf("next withdrawal failed to select change UTXO: inputs=%+v err=%v", nextInputs, err)
	}
}

// TestIntegrationBTCManualReviewKeepsBalanceAndUTXO 验证 SIGNED 转入人工审核时保留余额占用与 UTXO 锁并写入 worker_errors。
func TestIntegrationBTCManualReviewKeepsBalanceAndUTXO(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureBitcoinWallets(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	users, _ := store.ListUsers(context.Background())
	user := users[0]
	userBTC, _ := store.BTCAddressByUser(context.Background(), user.ID)
	var assetID, platformAddrID int64
	var platformScript string
	_ = store.pool.QueryRow(context.Background(), `SELECT id FROM assets WHERE network=$1 AND symbol='BTC'`, NetworkBitcoinSignet).Scan(&assetID)
	_ = store.pool.QueryRow(context.Background(), `SELECT id, script_pub_key FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE' LIMIT 1`, NetworkBitcoinSignet).Scan(&platformAddrID, &platformScript)
	_, _ = store.pool.Exec(context.Background(), `UPDATE asset_balances SET available_wei=50000 WHERE user_id=$1 AND asset_id=$2`, user.ID, assetID)
	_, _ = store.pool.Exec(context.Background(), `INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) VALUES(NULL,$1,$2,$3,0,20000,$4,10,'UNSPENT')`, platformAddrID, NetworkBitcoinSignet, strings.Repeat("9", 64), platformScript)

	w, _, err := store.CreateBTCWithdrawal(context.Background(), user.ID, "btc-manual-review", userBTC.Address, 2_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	inputs, addrs, targetScript, changeScript, err := store.LockWithdrawalInputs(context.Background(), w.ID, w.AmountSats, btc.WithdrawalLocker(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	raw, fee, changeAmt, err := btc.BuildWithdrawal(provider, inputs, addrs, targetScript, changeScript, w.AmountSats, w.FeeRateSatVB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveBTCWithdrawalSigned(context.Background(), w.ID, raw, fee, changeAmt); err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBTCWithdrawalManualReview(context.Background(), w.ID, "MEMPOOL_REJECTED", "missing-inputs"); err != nil {
		t.Fatal(err)
	}
	var status, available, pending string
	_ = store.pool.QueryRow(context.Background(), `SELECT status FROM btc_withdrawals WHERE id=$1`, w.ID).Scan(&status)
	_ = store.pool.QueryRow(context.Background(), `SELECT available_wei::text, pending_withdrawal_wei::text FROM asset_balances WHERE user_id=$1 AND asset_id=$2`, user.ID, assetID).Scan(&available, &pending)
	if status != btc.WithdrawalManualReview || available != "48000" || pending != "2000" {
		t.Fatalf("status=%s available=%s pending=%s", status, available, pending)
	}
	var errCount int
	_ = store.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM worker_errors WHERE reference_type='BTC_WITHDRAWAL' AND reference_id=$1`, w.ID).Scan(&errCount)
	if errCount != 1 {
		t.Fatalf("worker_errors count=%d, want 1", errCount)
	}
}

// TestIntegrationManagedUserCreationAndDisable 验证并发创建托管用户派生索引不冲突、禁用用户禁止提币及管理员初始化幂等性。
func TestIntegrationManagedUserCreationAndDisable(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()

	// 验证首个管理员初始化幂等不覆盖既有账户。
	if err := store.EnsureBootstrapAdmin(ctx, "admin", "$2a$10$abcdefghijklmnopqrstuvwx1234567890abcdefghijklmnop", "系统管理员"); err != nil {
		t.Fatalf("EnsureBootstrapAdmin() #1 error = %v", err)
	}
	if err := store.EnsureBootstrapAdmin(ctx, "other-admin", "$2a$10$zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", "第二管理员"); err != nil {
		t.Fatalf("EnsureBootstrapAdmin() #2 error = %v", err)
	}
	if _, err := store.AdminByUsername(ctx, "other-admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second bootstrap admin should not be created when admin_users not empty, err=%v", err)
	}

	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	// 并发创建 3 个托管用户，验证 derivation_index 严格递增无碰撞。
	type createdResult struct {
		user   User
		wallet WalletAddress
		err    error
	}
	ch := make(chan createdResult, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, w, createErr := store.CreateManagedUser(ctx, fmt.Sprintf("managed-u%d", idx), fmt.Sprintf("User %d", idx), "并发测试", provider, nil)
			ch <- createdResult{u, w, createErr}
		}(i)
	}
	wg.Wait()
	close(ch)
	seenIndexes := make(map[uint32]bool)
	var lastCreatedUser User
	var lastCreatedWallet WalletAddress
	for item := range ch {
		if item.err != nil {
			t.Fatalf("CreateManagedUser() concurrent error = %v", item.err)
		}
		if seenIndexes[item.wallet.DerivationIndex] {
			t.Fatalf("duplicate derivation index %d", item.wallet.DerivationIndex)
		}
		seenIndexes[item.wallet.DerivationIndex] = true
		lastCreatedUser = item.user
		lastCreatedWallet = item.wallet
	}

	// 充值后禁用用户，验证禁用状态拒绝创建提币。
	fundWallet(t, store, lastCreatedUser.ID, lastCreatedWallet.ID, 500, 10_000)
	updated, err := store.UpdateManagedUser(ctx, lastCreatedUser.ID, "User Disabled", "风控冻结", UserStatusDisabled)
	if err != nil || updated.Status != UserStatusDisabled {
		t.Fatalf("UpdateManagedUser() = %+v, err=%v", updated, err)
	}
	if _, _, err := store.ReserveWithdrawal(ctx, withdrawalRequest("disabled-withdraw-1", lastCreatedUser.ID, lastCreatedWallet.ID, 1_000, 100)); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("ReserveWithdrawal for disabled user err = %v, want ErrUserDisabled", err)
	}
}

// TestIntegrationReleaseWithdrawalRejectsAllocatedNonce 验证 B1：已分配 Nonce 的提币调用 ReleaseWithdrawal 会被拒绝，并验证错误去重累加。
func TestIntegrationReleaseWithdrawalRejectsAllocatedNonce(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 600, 10_000)

	item, _, err := store.ReserveWithdrawal(ctx, withdrawalRequest("b1-nonce-release", user.ID, address.ID, 2_000, 500))
	if err != nil {
		t.Fatal(err)
	}
	allocated, _, err := store.AllocateWithdrawalNonce(ctx, item.ID, 0)
	if err != nil || allocated.Nonce == nil || allocated.Status != WithdrawalSigning {
		t.Fatalf("AllocateWithdrawalNonce() = %+v, err=%v", allocated, err)
	}
	if _, _, err := store.ReleaseWithdrawal(ctx, item.ID, "TEST", "should fail"); !errors.Is(err, ErrUnsafeRelease) {
		t.Fatalf("ReleaseWithdrawal() on allocated nonce err = %v, want ErrUnsafeRelease", err)
	}
	// 验证钱包 next_nonce 未回退。
	addrAfter, err := store.WalletAddressByUser(ctx, user.ID)
	if err != nil || addrAfter.NextNonce.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("next_nonce = %v, err=%v, want 1", addrAfter.NextNonce, err)
	}

	// 验证 RecordWorkerError 合并累加同一错误而不生成重复行。
	refID := item.ID
	for i := 0; i < 3; i++ {
		if _, err := store.RecordWorkerError(ctx, WorkerError{
			Worker: "withdrawal-worker", Stage: "signing", ReferenceType: "WITHDRAWAL",
			ReferenceID: &refID, ErrorCode: "SIGNING_MANUAL_INTERVENTION", ErrorMessage: "签名前异常",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var rowCount, retryCount int
	if err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(retry_count), 0)
		FROM worker_errors
		WHERE reference_type = 'WITHDRAWAL' AND reference_id = $1 AND error_code = 'SIGNING_MANUAL_INTERVENTION'`,
		item.ID,
	).Scan(&rowCount, &retryCount); err != nil || rowCount != 1 || retryCount != 2 {
		t.Fatalf("worker_errors rowCount=%d retryCount=%d err=%v, want 1 and 2", rowCount, retryCount, err)
	}
}

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
	for {
		currentVersion, err := runner.Version(ctx)
		if err != nil {
			t.Fatalf("runner.Version() error = %v", err)
		}
		if currentVersion <= 1 {
			break
		}
		if err := runner.Down(ctx); err != nil {
			t.Fatalf("migrations.Down() from %d error = %v", currentVersion, err)
		}
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
	databaseURL, configErr := config.TestDatabaseURL()
	if configErr != nil {
		t.Fatalf("load test config: %v", configErr)
	}
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

// TestIntegrationEVMReplacementAttemptsFeeReserveAndManualReview 验证 B2 候选交易记录、ETH 费用追加替换、旧尝试结算与人工核查。
func TestIntegrationEVMReplacementAttemptsFeeReserveAndManualReview(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)
	fundWallet(t, store, user.ID, address.ID, 100, 1_000)

	item, _, err := store.ReserveWithdrawal(ctx, withdrawalRequest("b2-replace-1", user.ID, address.ID, 500, 200))
	if err != nil {
		t.Fatalf("ReserveWithdrawal() error = %v", err)
	}
	item, _, err = store.AllocateWithdrawalNonce(ctx, item.ID, 4)
	if err != nil {
		t.Fatalf("AllocateWithdrawalNonce() error = %v", err)
	}
	firstHash := fmt.Sprintf("0x%064x", 8801)
	item, _, err = store.SaveSignedWithdrawal(ctx, SignedWithdrawal{
		WithdrawalID: item.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(20),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{1, 2, 3}, TxHash: firstHash,
	})
	if err != nil {
		t.Fatalf("SaveSignedWithdrawal() error = %v", err)
	}
	attempts, err := store.ListEVMTxAttempts(ctx, EVMBusinessWithdrawal, item.ID)
	if err != nil || len(attempts) != 1 || attempts[0].AttemptNo != 1 || attempts[0].TxHash != firstHash {
		t.Fatalf("attempts after sign = %+v, error = %v", attempts, err)
	}
	if _, err := store.TransitionWithdrawal(ctx, item.ID, WithdrawalBroadcasting); err != nil {
		t.Fatalf("TransitionWithdrawal(BROADCASTING) error = %v", err)
	}
	item, err = store.TransitionWithdrawal(ctx, item.ID, WithdrawalBroadcasted)
	if err != nil || item.BroadcastedAt == nil || item.LastBroadcastAt == nil {
		t.Fatalf("TransitionWithdrawal(BROADCASTED) = %+v, error = %v", item, err)
	}

	if _, _, err := store.ReplaceWithdrawalTx(ctx, SignedWithdrawal{
		WithdrawalID: item.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(50),
		MaxPriorityFeePerGasWei: big.NewInt(5), RawTx: []byte{9, 9, 9}, TxHash: fmt.Sprintf("0x%064x", 8802),
	}, big.NewInt(900)); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("over-balance ReplaceWithdrawalTx() error = %v, want ErrInsufficientBalance", err)
	}

	secondHash := fmt.Sprintf("0x%064x", 8803)
	replaced, changed, err := store.ReplaceWithdrawalTx(ctx, SignedWithdrawal{
		WithdrawalID: item.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(30),
		MaxPriorityFeePerGasWei: big.NewInt(3), RawTx: []byte{4, 5, 6}, TxHash: secondHash,
	}, big.NewInt(350))
	if err != nil || !changed || replaced.BumpCount != 1 || replaced.TxHash != secondHash || replaced.ReservedFeeWei.Cmp(big.NewInt(350)) != 0 {
		t.Fatalf("ReplaceWithdrawalTx() = %+v, changed = %v, error = %v", replaced, changed, err)
	}
	attempts, err = store.ListEVMTxAttempts(ctx, EVMBusinessWithdrawal, item.ID)
	if err != nil || len(attempts) != 2 || attempts[0].AttemptNo != 2 || attempts[1].TxHash != firstHash {
		t.Fatalf("attempts after replace = %+v, error = %v", attempts, err)
	}
	balance, err := store.BalanceByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available after fee bump", balance.AvailableWei, 150)
	assertBigInt(t, "pending after fee bump", balance.PendingWithdrawalWei, 850)

	if _, err := store.TouchWithdrawalBroadcast(ctx, item.ID); err != nil {
		t.Fatalf("TouchWithdrawalBroadcast() error = %v", err)
	}
	settled, changed, err := store.FinalizeWithdrawal(ctx, WithdrawalSettlement{
		WithdrawalID: item.ID, ActualFeeWei: big.NewInt(120), Success: true,
		BlockNumber: 200, Confirmations: 3, TxHash: firstHash,
	})
	if err != nil || !changed || settled.Status != WithdrawalCompleted || settled.TxHash != firstHash {
		t.Fatalf("FinalizeWithdrawal() = %+v, changed = %v, error = %v", settled, changed, err)
	}
	balance, err = store.BalanceByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() after settle error = %v", err)
	}
	assertBigInt(t, "available after first-attempt settle", balance.AvailableWei, 380)
	assertBigInt(t, "pending after first-attempt settle", balance.PendingWithdrawalWei, 0)

	manualItem, _, err := store.ReserveWithdrawal(ctx, withdrawalRequest("b2-manual-1", user.ID, address.ID, 100, 50))
	if err != nil {
		t.Fatalf("ReserveWithdrawal(manual) error = %v", err)
	}
	manualItem, _, err = store.AllocateWithdrawalNonce(ctx, manualItem.ID, 5)
	if err != nil {
		t.Fatalf("AllocateWithdrawalNonce(manual) error = %v", err)
	}
	manualItem, _, err = store.SaveSignedWithdrawal(ctx, SignedWithdrawal{
		WithdrawalID: manualItem.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(20),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{7, 7, 7}, TxHash: fmt.Sprintf("0x%064x", 8804),
	})
	if err != nil {
		t.Fatalf("SaveSignedWithdrawal(manual) error = %v", err)
	}
	manualItem, changed, err = store.MarkWithdrawalManualReview(ctx, manualItem.ID, "EVM_TX_MANUAL_REVIEW", "nonce skipped")
	if err != nil || !changed || manualItem.Status != WithdrawalManualReview {
		t.Fatalf("MarkWithdrawalManualReview() = %+v, changed = %v, error = %v", manualItem, changed, err)
	}
}

// TestIntegrationInternalTransferReplacementMatchesAttemptHashes 验证内部补气替换后所有尝试哈希都能被识别为内部交易。
func TestIntegrationInternalTransferReplacementMatchesAttemptHashes(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	sweep, address, platform := createGasTopupFixture(t, store, provider)
	transfer, _, err := store.CreateOrGetGasTopup(ctx, GasTopupRequest{
		SweepID: sweep.ID, PlatformWalletID: platform.ID, FromAddress: platform.Address,
		ToAddress: address.Address, AmountWei: big.NewInt(12_000_000), ChainPendingNonce: 11,
	})
	if err != nil {
		t.Fatalf("CreateOrGetGasTopup() error = %v", err)
	}
	hash1 := fmt.Sprintf("0x%064x", 8901)
	hash2 := fmt.Sprintf("0x%064x", 8902)
	if _, _, err := store.SaveSignedInternalTransfer(ctx, SignedInternalTransfer{
		TransferID: transfer.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(20),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{1, 1, 1}, TxHash: hash1,
	}); err != nil {
		t.Fatalf("SaveSignedInternalTransfer() error = %v", err)
	}
	if _, err := store.TransitionInternalTransfer(ctx, transfer.ID, InternalTransferSent); err != nil {
		t.Fatalf("TransitionInternalTransfer() error = %v", err)
	}
	if _, changed, err := store.ReplaceInternalTransferTx(ctx, SignedInternalTransfer{
		TransferID: transfer.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(30),
		MaxPriorityFeePerGasWei: big.NewInt(3), RawTx: []byte{2, 2, 2}, TxHash: hash2,
	}); err != nil || !changed {
		t.Fatalf("ReplaceInternalTransferTx() changed = %v, error = %v", changed, err)
	}
	for _, hash := range []string{hash1, hash2} {
		ok, err := store.IsInternalTransferTx(ctx, hash)
		if err != nil || !ok {
			t.Fatalf("IsInternalTransferTx(%s) = %v, error = %v", hash, ok, err)
		}
	}
	attempts, err := store.ListEVMTxAttempts(ctx, EVMBusinessInternalTransfer, transfer.ID)
	if err != nil || len(attempts) != 2 || attempts[0].TxHash != hash2 || attempts[1].TxHash != hash1 {
		t.Fatalf("internal transfer attempts = %+v, error = %v", attempts, err)
	}
}

// TestIntegrationEVMDepositReorgRewindUpdateOrphanAndCreditedAlert 验证真实数据库中 EVM/Token 充值重组回退、跨块更新、孤块冲正流水和已入账告警。
func TestIntegrationEVMDepositReorgRewindUpdateOrphanAndCreditedAlert(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	user, address := firstDemoWallet(t, store)

	// 1. 写入两笔待确认 ETH 充值和一笔已入账 ETH 充值。
	relocatedObs := depositObservation(user.ID, address.ID, 21, 100)
	orphanedObs := depositObservation(user.ID, address.ID, 22, 200)
	creditedObs := depositObservation(user.ID, address.ID, 20, 300)
	if _, _, err := store.RecordDepositAndCheckpoint(ctx, creditedObs); err != nil {
		t.Fatalf("RecordDepositAndCheckpoint(credited) error = %v", err)
	}
	creditedItem, _, err := store.RecordDepositAndCheckpoint(ctx, creditedObs)
	if err != nil {
		t.Fatalf("reload credited error = %v", err)
	}
	if _, _, err := store.CreditDeposit(ctx, creditedItem.ID, 3); err != nil {
		t.Fatalf("CreditDeposit() error = %v", err)
	}
	if _, _, err := store.RecordDepositAndCheckpoint(ctx, relocatedObs); err != nil {
		t.Fatalf("RecordDepositAndCheckpoint(relocated) error = %v", err)
	}
	orphanedItem, _, err := store.RecordDepositAndCheckpoint(ctx, orphanedObs)
	if err != nil {
		t.Fatalf("RecordDepositAndCheckpoint(orphaned) error = %v", err)
	}

	// 2. 模拟检查点回退到 19 并重新扫到重定位后的交易。
	rewindHash := fmt.Sprintf("0x%064x", 10_019)
	if err := store.RewindCheckpoint(ctx, NetworkSepolia, "deposit-scanner", 19, rewindHash); err != nil {
		t.Fatalf("RewindCheckpoint() error = %v", err)
	}
	cp, err := store.Checkpoint(ctx, NetworkSepolia, "deposit-scanner")
	if err != nil || cp.LastScannedBlock != 19 || cp.LastScannedHash != rewindHash {
		t.Fatalf("rewound checkpoint = %+v, err = %v", cp, err)
	}

	relocatedObs.BlockNumber = 23
	relocatedObs.BlockHash = fmt.Sprintf("0x%064x", 20_023)
	relocatedObs.TxIndex = 4
	relocatedObs.Checkpoint.LastScannedBlock = 23
	relocatedObs.Checkpoint.LastScannedHash = relocatedObs.BlockHash
	updatedItem, created, err := store.RecordDepositAndCheckpoint(ctx, relocatedObs)
	if err != nil || created || updatedItem.BlockNumber != 23 || updatedItem.BlockHash != relocatedObs.BlockHash || updatedItem.TxIndex != 4 {
		t.Fatalf("relocated deposit = %+v, created = %v, err = %v", updatedItem, created, err)
	}

	// 3. 孤立消失的 pending 充值：校验 ORPHANED 状态、pending 余额减扣、负向冲正流水，以及同 tx_hash 重新入链不冲突。
	orphanedAfter, changed, err := store.OrphanDeposit(ctx, orphanedItem.ID)
	if err != nil || !changed || orphanedAfter.Status != DepositOrphaned {
		t.Fatalf("OrphanDeposit() = %+v, changed = %v, err = %v", orphanedAfter, changed, err)
	}
	ethBalance, err := store.BalanceByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available after orphan", ethBalance.AvailableWei, 300)
	assertBigInt(t, "pending after orphan", ethBalance.PendingDepositWei, 100)

	var reversalAmount string
	if err := store.pool.QueryRow(ctx, `
		SELECT amount_wei::text FROM balance_entries
		WHERE entry_type = $1 AND reference_type = 'DEPOSIT' AND reference_id = $2`,
		EntryDepositReversal, orphanedItem.ID,
	).Scan(&reversalAmount); err != nil || reversalAmount != "-200" {
		t.Fatalf("ETH reversal entry amount = %q, err = %v", reversalAmount, err)
	}

	// 同 tx_hash 在后续块重新上链应被允许且生成新记录。
	reminedObs := orphanedObs
	reminedObs.BlockNumber = 24
	reminedObs.BlockHash = fmt.Sprintf("0x%064x", 20_024)
	reminedObs.Checkpoint.LastScannedBlock = 24
	reminedObs.Checkpoint.LastScannedHash = reminedObs.BlockHash
	reminedItem, created, err := store.RecordDepositAndCheckpoint(ctx, reminedObs)
	if err != nil || !created || reminedItem.ID == orphanedItem.ID {
		t.Fatalf("remined deposit = %+v, created = %v, err = %v", reminedItem, created, err)
	}

	// 4. 已入账充值在重组后位置变化时不扣款，写 worker_errors 告警。
	creditedObs.BlockNumber = 25
	creditedObs.BlockHash = fmt.Sprintf("0x%064x", 20_025)
	creditedObs.Checkpoint.LastScannedBlock = 25
	creditedObs.Checkpoint.LastScannedHash = creditedObs.BlockHash
	if _, _, err := store.RecordDepositAndCheckpoint(ctx, creditedObs); err != nil {
		t.Fatalf("re-observed credited deposit error = %v", err)
	}
	if _, _, err := store.OrphanDeposit(ctx, creditedItem.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("OrphanDeposit(credited) error = %v, want ErrInvalidState", err)
	}
	ethBalanceAfterCreditedReorg, err := store.BalanceByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("BalanceByUser() error = %v", err)
	}
	assertBigInt(t, "available remains untouched on credited reorg", ethBalanceAfterCreditedReorg.AvailableWei, 300)
	var alertCount int
	if err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM worker_errors
		WHERE error_code = 'CREDITED_DEPOSIT_REORG' AND reference_id = $1`, creditedItem.ID,
	).Scan(&alertCount); err != nil || alertCount == 0 {
		t.Fatalf("credited reorg alertCount = %d, err = %v", alertCount, err)
	}

	// 5. Token 孤块充值冲正测试。
	tokenAsset := ensureTestUSDC(t, store)
	tokenObs := tokenDepositObservation(user.ID, address, tokenAsset.ID, 30, 1, 777)
	tokenItems, _, err := store.RecordTokenDepositsAndCheckpoint(ctx, []TokenDepositObservation{tokenObs}, ChainCheckpoint{
		Network: NetworkSepolia, Scanner: "erc20:test", LastScannedBlock: 30, LastScannedHash: tokenObs.BlockHash,
	})
	if err != nil || len(tokenItems) != 1 {
		t.Fatalf("RecordTokenDepositsAndCheckpoint() items = %+v, err = %v", tokenItems, err)
	}
	orphanedToken, changed, err := store.OrphanTokenDeposit(ctx, tokenItems[0].ID)
	if err != nil || !changed || orphanedToken.Status != DepositOrphaned {
		t.Fatalf("OrphanTokenDeposit() = %+v, changed = %v, err = %v", orphanedToken, changed, err)
	}
	var tokenPending string
	if err := store.pool.QueryRow(ctx, `SELECT pending_deposit_wei::text FROM asset_balances WHERE user_id = $1 AND asset_id = $2`, user.ID, tokenAsset.ID).Scan(&tokenPending); err != nil || tokenPending != "0" {
		t.Fatalf("token pending after orphan = %q, err = %v", tokenPending, err)
	}
}

// TestIntegrationBatchEPerformanceQueriesAndBTCTransactions 验证 Batch E 的统一交易分页（含 BTC）、增量地址加载、批量内部交易过滤与批量充值提交。
func TestIntegrationBatchEPerformanceQueriesAndBTCTransactions(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	user, address := firstDemoWallet(t, store)
	if err := store.EnsureBitcoinWallets(ctx, provider); err != nil {
		t.Fatalf("EnsureBitcoinWallets() error = %v", err)
	}

	// 1. 多区块批量充值与检查点提交。
	obs1 := depositObservation(user.ID, address.ID, 40, 100)
	obs2 := depositObservation(user.ID, address.ID, 41, 200)
	items, created, err := store.RecordDepositsBatchAndCheckpoint(ctx, []DepositObservation{obs1, obs2}, ChainCheckpoint{
		Network: NetworkSepolia, Scanner: "deposit-scanner", LastScannedBlock: 41, LastScannedHash: obs2.BlockHash,
	})
	if err != nil || created != 2 || len(items) != 2 || items[0].Status != DepositConfirming || items[1].Status != DepositConfirming {
		t.Fatalf("RecordDepositsBatchAndCheckpoint() = %+v, created = %d, err = %v", items, created, err)
	}

	// 2. 批量内部转账哈希过滤与 JOIN 查询 Gas Station 归集任务。
	sweep, _, platform := createGasTopupFixture(t, store, provider)
	sweeps, err := store.ListGasStationSweeps(ctx, 20)
	if err != nil || len(sweeps) == 0 || sweeps[0].ID != sweep.ID {
		t.Fatalf("ListGasStationSweeps() = %+v, err = %v", sweeps, err)
	}
	transfer, _, err := store.CreateOrGetGasTopup(ctx, GasTopupRequest{
		SweepID: sweep.ID, PlatformWalletID: platform.ID, FromAddress: platform.Address,
		ToAddress: address.Address, AmountWei: big.NewInt(1_000_000), ChainPendingNonce: 20,
	})
	if err != nil {
		t.Fatalf("CreateOrGetGasTopup() error = %v", err)
	}
	internalHash := fmt.Sprintf("0x%064x", 9901)
	if _, _, err := store.SaveSignedInternalTransfer(ctx, SignedInternalTransfer{
		TransferID: transfer.ID, GasLimit: 21_000, MaxFeePerGasWei: big.NewInt(20),
		MaxPriorityFeePerGasWei: big.NewInt(2), RawTx: []byte{1, 2, 3}, TxHash: internalHash,
	}); err != nil {
		t.Fatalf("SaveSignedInternalTransfer() error = %v", err)
	}
	externalHash := fmt.Sprintf("0x%064x", 9902)
	matched, err := store.FilterInternalTransferTxs(ctx, []string{internalHash, externalHash, internalHash})
	if err != nil || !matched[internalHash] || matched[externalHash] || len(matched) != 1 {
		t.Fatalf("FilterInternalTransferTxs() = %+v, err = %v", matched, err)
	}

	// 3. 增量地址加载：首次 afterID=0 返回现有地址，游标之后仅返回新增地址。
	allAddrs, err := store.ListWalletAddressesAfterID(ctx, 0)
	if err != nil || len(allAddrs) < 2 {
		t.Fatalf("ListWalletAddressesAfterID(0) = %d, err = %v", len(allAddrs), err)
	}
	maxID := allAddrs[len(allAddrs)-1].ID
	emptyAddrs, err := store.ListWalletAddressesAfterID(ctx, maxID)
	if err != nil || len(emptyAddrs) != 0 {
		t.Fatalf("ListWalletAddressesAfterID(maxID) = %d, err = %v", len(emptyAddrs), err)
	}
	newUser, _, err := store.CreateManagedUser(ctx, "batch-e-user", "Batch E 用户", "", provider, provider)
	if err != nil {
		t.Fatalf("CreateManagedUser() error = %v", err)
	}
	newAddrs, err := store.ListWalletAddressesAfterID(ctx, maxID)
	if err != nil || len(newAddrs) != 1 || newAddrs[0].UserID != newUser.ID {
		t.Fatalf("incremental ListWalletAddressesAfterID() = %+v, err = %v", newAddrs, err)
	}

	// 4. 统一流水分页包含 BTC_DEPOSIT 与 BTC_WITHDRAWAL，并支持资产和类型筛选及分页。
	btcAddr, err := store.BTCAddressByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("BTCAddressByUser() error = %v", err)
	}
	btcObs := btc.DepositObservation{
		UserID: user.ID, AddressID: btcAddr.ID, TxID: strings.Repeat("a", 64), Vout: 0,
		BlockHash: strings.Repeat("b", 64), BlockHeight: 100, AmountSats: 50_000, ScriptPubKey: btcAddr.ScriptPubKey,
	}
	if _, err := store.RecordBTCDepositsAndCheckpoint(ctx, []btc.DepositObservation{btcObs}, btc.Checkpoint{
		Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 100, LastScannedHash: btcObs.BlockHash,
	}); err != nil {
		t.Fatalf("RecordBTCDepositsAndCheckpoint() error = %v", err)
	}
	var btcDepID int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM btc_deposits WHERE txid = $1`, btcObs.TxID).Scan(&btcDepID); err != nil {
		t.Fatalf("query btc deposit id: %v", err)
	}
	if err := store.CreditBTCDeposit(ctx, btcDepID, 3); err != nil {
		t.Fatalf("CreditBTCDeposit() error = %v", err)
	}
	if _, _, err := store.CreateBTCWithdrawal(ctx, user.ID, "batch-e-btc-w", btcAddr.Address, 10_000, 2); err != nil {
		t.Fatalf("CreateBTCWithdrawal() error = %v", err)
	}

	btcOnly, err := store.ListTransactionsFilteredPage(ctx, "BTC", "", 10, 0)
	if err != nil || len(btcOnly) != 2 {
		t.Fatalf("ListTransactionsFilteredPage(BTC) = %+v, err = %v", btcOnly, err)
	}
	btcWithdrawOnly, err := store.ListTransactionsFilteredPage(ctx, "BTC", "BTC_WITHDRAWAL", 10, 0)
	if err != nil || len(btcWithdrawOnly) != 1 || btcWithdrawOnly[0].Type != "BTC_WITHDRAWAL" || btcWithdrawOnly[0].Decimals != 8 {
		t.Fatalf("ListTransactionsFilteredPage(BTC_WITHDRAWAL) = %+v, err = %v", btcWithdrawOnly, err)
	}
	page2, err := store.ListTransactionsFilteredPage(ctx, "", "", 2, 2)
	if err != nil || len(page2) != 2 {
		t.Fatalf("ListTransactionsFilteredPage(limit=2, offset=2) = %+v, err = %v", page2, err)
	}
}

// TestIntegrationBTCBatchGFixesAndAssertions 验证 G3 快照消耗回滚与转人工审核、G5 有界选币锁、G6 失败写告警以及 G7 补充断言。
func TestIntegrationBTCBatchGFixesAndAssertions(t *testing.T) {
	store, cleanup := integrationStore(t)
	defer cleanup()
	ctx := context.Background()
	provider, err := wallet.NewMnemonicKeyProvider(integrationTestMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureBitcoinWallets(ctx, provider); err != nil {
		t.Fatal(err)
	}
	users, _ := store.ListUsers(ctx)
	user := users[0]
	userBTC, _ := store.BTCAddressByUser(ctx, user.ID)
	var assetID, platformAddrID int64
	var platformScript string
	_ = store.pool.QueryRow(ctx, `SELECT id FROM assets WHERE network=$1 AND symbol='BTC'`, NetworkBitcoinSignet).Scan(&assetID)
	_ = store.pool.QueryRow(ctx, `SELECT id, script_pub_key FROM btc_addresses WHERE network=$1 AND purpose='PLATFORM_CHANGE' LIMIT 1`, NetworkBitcoinSignet).Scan(&platformAddrID, &platformScript)

	// G7-1：存在 USER_DEPOSIT 的 UNSPENT UTXO 时，提币绝不能选中它。
	_, err = store.pool.Exec(ctx, `
		INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status)
		VALUES(NULL,$1,$2,$3,0,500000,$4,10,'UNSPENT')`,
		userBTC.ID, NetworkBitcoinSignet, strings.Repeat("1", 64), hex.EncodeToString(userBTC.ScriptPubKey))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.pool.Exec(ctx, `UPDATE asset_balances SET available_wei=500000 WHERE user_id=$1 AND asset_id=$2`, user.ID, assetID)
	wUserDepositOnly, _, err := store.CreateBTCWithdrawal(ctx, user.ID, "g7-user-deposit-only", userBTC.Address, 10_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, lockErr := store.LockWithdrawalInputs(ctx, wUserDepositOnly.ID, wUserDepositOnly.AmountSats, btc.WithdrawalLocker(wUserDepositOnly.ID)); lockErr == nil {
		t.Fatal("expected withdrawal input selection to ignore USER_DEPOSIT UNSPENT UTXO")
	}

	// G7-2：CompleteBTCWithdrawal 拒绝 CREATED 与 FAILED 状态。
	if err = store.CompleteBTCWithdrawal(ctx, wUserDepositOnly.ID, 3, 20); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("CompleteBTCWithdrawal(CREATED) err = %v, want ErrInvalidState", err)
	}
	if err = store.FailBTCWithdrawal(ctx, wUserDepositOnly.ID, "TEST_FAIL", "测试失败释放"); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBTCWithdrawal(ctx, wUserDepositOnly.ID, 3, 20); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("CompleteBTCWithdrawal(FAILED) err = %v, want ErrInvalidState", err)
	}

	// G6：FailBTCSweep 写入一条 worker_errors 并释放 UTXO。
	sweepDepObs := btc.DepositObservation{
		UserID: user.ID, AddressID: userBTC.ID, TxID: strings.Repeat("2", 64), Vout: 0,
		BlockHash: strings.Repeat("a", 64), BlockHeight: 20, AmountSats: 200, ScriptPubKey: userBTC.ScriptPubKey,
	}
	if _, err = store.RecordBTCDepositsAndCheckpoint(ctx, []btc.DepositObservation{sweepDepObs}, btc.Checkpoint{
		Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 20, LastScannedHash: sweepDepObs.BlockHash,
	}); err != nil {
		t.Fatal(err)
	}
	var dustDepID int64
	_ = store.pool.QueryRow(ctx, `SELECT id FROM btc_deposits WHERE txid=$1`, sweepDepObs.TxID).Scan(&dustDepID)
	if err = store.CreditBTCDeposit(ctx, dustDepID, 3); err != nil {
		t.Fatal(err)
	}
	var dustSweepID int64
	_ = store.pool.QueryRow(ctx, `SELECT id FROM btc_sweeps WHERE deposit_id=$1`, dustDepID).Scan(&dustSweepID)
	if err = store.FailBTCSweep(ctx, dustSweepID, "SWEEP_DUST", "归集输出低于粉尘阈值"); err != nil {
		t.Fatal(err)
	}
	var sweepErrCount int
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM worker_errors WHERE reference_type='BTC_SWEEP' AND reference_id=$1 AND error_code='SWEEP_DUST'`, dustSweepID).Scan(&sweepErrCount)
	if sweepErrCount != 1 {
		t.Fatalf("worker_errors count for failed dust sweep = %d, want 1", sweepErrCount)
	}

	// G7-3：CompleteBTCSweep 重复调用幂等；以及 G3-归集：目标 UTXO 状态不匹配时回滚并转 MANUAL_REVIEW。
	okDepObs := btc.DepositObservation{
		UserID: user.ID, AddressID: userBTC.ID, TxID: strings.Repeat("3", 64), Vout: 0,
		BlockHash: strings.Repeat("b", 64), BlockHeight: 21, AmountSats: 50_000, ScriptPubKey: userBTC.ScriptPubKey,
	}
	if _, err = store.RecordBTCDepositsAndCheckpoint(ctx, []btc.DepositObservation{okDepObs}, btc.Checkpoint{
		Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 21, LastScannedHash: okDepObs.BlockHash,
	}); err != nil {
		t.Fatal(err)
	}
	var okDepID int64
	_ = store.pool.QueryRow(ctx, `SELECT id FROM btc_deposits WHERE txid=$1`, okDepObs.TxID).Scan(&okDepID)
	if err = store.CreditBTCDeposit(ctx, okDepID, 3); err != nil {
		t.Fatal(err)
	}
	sweeps, err := store.ListProcessableBTCSweeps(ctx, 10)
	if err != nil || len(sweeps) != 1 {
		t.Fatalf("ListProcessableBTCSweeps() count=%d err=%v", len(sweeps), err)
	}
	builtSweep, err := btc.BuildSweep(provider, sweeps[0].From.Path, sweeps[0].UTXO, sweeps[0].To, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveBTCSweepSigned(ctx, sweeps[0].ID, builtSweep.RawTx, builtSweep.TxID, builtSweep.OutputValueSats, builtSweep.FeeSats, builtSweep.FeeRateSatVB); err != nil {
		t.Fatal(err)
	}
	if _, err = store.MarkBTCSweepBroadcasted(ctx, sweeps[0].ID, builtSweep.TxID); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBTCSweep(ctx, sweeps[0].ID, 3, 25); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBTCSweep(ctx, sweeps[0].ID, 6, 28); err != nil {
		t.Fatalf("duplicate CompleteBTCSweep() error = %v", err)
	}
	var sweepOutputCount int
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM btc_utxos WHERE txid=$1 AND vout=0`, builtSweep.TxID).Scan(&sweepOutputCount)
	if sweepOutputCount != 1 {
		t.Fatalf("sweep output count = %d, want 1", sweepOutputCount)
	}

	// G3 归集 UTXO 消耗不一致时回滚并转 MANUAL_REVIEW。
	badSweepDep := btc.DepositObservation{
		UserID: user.ID, AddressID: userBTC.ID, TxID: strings.Repeat("4", 64), Vout: 0,
		BlockHash: strings.Repeat("c", 64), BlockHeight: 22, AmountSats: 40_000, ScriptPubKey: userBTC.ScriptPubKey,
	}
	if _, err = store.RecordBTCDepositsAndCheckpoint(ctx, []btc.DepositObservation{badSweepDep}, btc.Checkpoint{
		Network: btc.Network, Scanner: "btc-deposit", LastScannedBlock: 22, LastScannedHash: badSweepDep.BlockHash,
	}); err != nil {
		t.Fatal(err)
	}
	var badSweepDepID int64
	_ = store.pool.QueryRow(ctx, `SELECT id FROM btc_deposits WHERE txid=$1`, badSweepDep.TxID).Scan(&badSweepDepID)
	_ = store.CreditBTCDeposit(ctx, badSweepDepID, 3)
	badSweeps, _ := store.ListProcessableBTCSweeps(ctx, 10)
	builtBadSweep, _ := btc.BuildSweep(provider, badSweeps[0].From.Path, badSweeps[0].UTXO, badSweeps[0].To, 2)
	_, _ = store.SaveBTCSweepSigned(ctx, badSweeps[0].ID, builtBadSweep.RawTx, builtBadSweep.TxID, builtBadSweep.OutputValueSats, builtBadSweep.FeeSats, builtBadSweep.FeeRateSatVB)
	_, _ = store.MarkBTCSweepBroadcasted(ctx, badSweeps[0].ID, builtBadSweep.TxID)
	// 人为破坏 UTXO 状态，模拟快照与当前 UTXO 状态不一致。
	_, _ = store.pool.Exec(ctx, `UPDATE btc_utxos SET status='UNSPENT', locked_by=NULL, locked_until=NULL WHERE id=$1`, badSweeps[0].UTXO.ID)
	if err = store.CompleteBTCSweep(ctx, badSweeps[0].ID, 3, 29); err == nil {
		t.Fatal("expected CompleteBTCSweep to fail when input UTXO lock is missing")
	}
	var badSweepStatus string
	_ = store.pool.QueryRow(ctx, `SELECT status FROM btc_sweeps WHERE id=$1`, badSweeps[0].ID).Scan(&badSweepStatus)
	if badSweepStatus != btc.SweepManualReview {
		t.Fatalf("badSweepStatus = %s, want MANUAL_REVIEW", badSweepStatus)
	}
	var badSweepOutCount int
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM btc_utxos WHERE txid=$1`, builtBadSweep.TxID).Scan(&badSweepOutCount)
	if badSweepOutCount != 0 {
		t.Fatalf("rolled back sweep still inserted output count=%d", badSweepOutCount)
	}

	// G5：55 个小 UTXO + 5 个大 UTXO，选币只锁选中的 1 个，其余 59 个不被全表锁住。
	for i := 0; i < 55; i++ {
		txHex := fmt.Sprintf("%064x", 1000+i)
		_, _ = store.pool.Exec(ctx, `INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) VALUES(NULL,$1,$2,$3,0,500,$4,30,'UNSPENT')`, platformAddrID, NetworkBitcoinSignet, txHex, platformScript)
	}
	for i := 0; i < 5; i++ {
		txHex := fmt.Sprintf("%064x", 2000+i)
		_, _ = store.pool.Exec(ctx, `INSERT INTO btc_utxos(deposit_id,address_id,network,txid,vout,value_sats,script_pub_key,block_height,status) VALUES(NULL,$1,$2,$3,0,25000,$4,30,'UNSPENT')`, platformAddrID, NetworkBitcoinSignet, txHex, platformScript)
	}
	wScoped, _, err := store.CreateBTCWithdrawal(ctx, user.ID, "g5-scoped-lock", userBTC.Address, 12_000, 2)
	if err != nil {
		t.Fatal(err)
	}
	lockedInputs, addrs, targetScript, changeScript, err := store.LockWithdrawalInputs(ctx, wScoped.ID, wScoped.AmountSats, btc.WithdrawalLocker(wScoped.ID))
	if err != nil || len(lockedInputs) != 1 {
		t.Fatalf("LockWithdrawalInputs() len=%d err=%v", len(lockedInputs), err)
	}
	// G3 提币 UTXO 消耗不一致时回滚并转 MANUAL_REVIEW。
	rawW, feeW, changeW, err := btc.BuildWithdrawal(provider, lockedInputs, addrs, targetScript, changeScript, wScoped.AmountSats, wScoped.FeeRateSatVB)
	if err != nil {
		t.Fatal(err)
	}
	signedW, err := store.SaveBTCWithdrawalSigned(ctx, wScoped.ID, rawW, feeW, changeW)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBTCWithdrawalBroadcasted(ctx, wScoped.ID, signedW.TxID); err != nil {
		t.Fatal(err)
	}
	// 人为破坏被锁 UTXO 的 locked_by 标记，触发 RowsAffected != len(inputs)。
	_, _ = store.pool.Exec(ctx, `UPDATE btc_utxos SET status='SPENT', locked_by=NULL WHERE id=$1`, lockedInputs[0].ID)
	if err = store.CompleteBTCWithdrawal(ctx, wScoped.ID, 3, 40); err == nil {
		t.Fatal("expected CompleteBTCWithdrawal to fail on UTXO consume mismatch")
	}
	var wBadStatus, wPending string
	_ = store.pool.QueryRow(ctx, `SELECT status FROM btc_withdrawals WHERE id=$1`, wScoped.ID).Scan(&wBadStatus)
	_ = store.pool.QueryRow(ctx, `SELECT pending_withdrawal_wei::text FROM asset_balances WHERE user_id=$1 AND asset_id=$2`, user.ID, assetID).Scan(&wPending)
	if wBadStatus != btc.WithdrawalManualReview || wPending != "12000" {
		t.Fatalf("status=%s pending=%s, want MANUAL_REVIEW and 12000", wBadStatus, wPending)
	}
	var wChangeCount int
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM btc_utxos WHERE txid=$1 AND vout=$2`, signedW.TxID, btc.ChangeOutputVout).Scan(&wChangeCount)
	if wChangeCount != 0 {
		t.Fatalf("rolled back withdrawal inserted change utxo count=%d", wChangeCount)
	}
	var wErrCount int
	_ = store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM worker_errors WHERE reference_type='BTC_WITHDRAWAL' AND reference_id=$1`, wScoped.ID).Scan(&wErrCount)
	if wErrCount != 1 {
		t.Fatalf("worker_errors count for bad withdrawal = %d, want 1", wErrCount)
	}
}
