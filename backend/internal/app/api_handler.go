package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

const (
	defaultPageSize = 20
	maximumPageSize = 100
)

// APIStore 定义后台页面只读 API 所需的数据查询能力。
type APIStore interface {
	ListUsers(ctx context.Context) ([]postgres.User, error)
	UserByID(ctx context.Context, id int64) (postgres.User, error)
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	BalanceByUser(ctx context.Context, userID int64) (postgres.AssetBalance, error)
	ListDepositsPage(ctx context.Context, userID int64, limit, offset int) ([]postgres.Deposit, error)
	ListWithdrawalsPage(ctx context.Context, userID int64, limit, offset int) ([]postgres.Withdrawal, error)
	WithdrawalByID(ctx context.Context, id int64) (postgres.Withdrawal, error)
	ListTransactionsPage(ctx context.Context, limit, offset int) ([]postgres.TransactionRecord, error)
	ListWorkerErrorsPage(ctx context.Context, limit, offset int) ([]postgres.WorkerError, error)
}

// pageResponse 描述统一的页码分页响应。
type pageResponse[T any] struct {
	Items    []T  `json:"items"`
	Page     int  `json:"page"`
	PageSize int  `json:"page_size"`
	HasMore  bool `json:"has_more"`
}

// userResponse 描述后台页面可选择的演示用户。
type userResponse struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// assetBalanceResponse 描述 ETH 三类账本余额。
type assetBalanceResponse struct {
	Asset                string    `json:"asset"`
	AvailableWei         string    `json:"available_wei"`
	AvailableETH         string    `json:"available_eth"`
	PendingDepositWei    string    `json:"pending_deposit_wei"`
	PendingDepositETH    string    `json:"pending_deposit_eth"`
	PendingWithdrawalWei string    `json:"pending_withdrawal_wei"`
	PendingWithdrawalETH string    `json:"pending_withdrawal_eth"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// walletResponse 描述用户 Sepolia 充值地址和平台账本余额。
type walletResponse struct {
	UserID    int64                `json:"user_id"`
	Network   string               `json:"network"`
	Address   string               `json:"address"`
	Balance   assetBalanceResponse `json:"balance"`
	CreatedAt time.Time            `json:"created_at"`
}

// depositResponse 描述一笔 Sepolia ETH 充值。
type depositResponse struct {
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id"`
	Network       string    `json:"network"`
	Asset         string    `json:"asset"`
	TxHash        string    `json:"tx_hash"`
	ExplorerURL   string    `json:"explorer_url"`
	BlockNumber   int64     `json:"block_number"`
	BlockURL      string    `json:"block_url"`
	AmountWei     string    `json:"amount_wei"`
	AmountETH     string    `json:"amount_eth"`
	Confirmations int64     `json:"confirmations"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// transactionResponse 描述全局充值或提币交易。
type transactionResponse struct {
	Type          string    `json:"type"`
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id"`
	Asset         string    `json:"asset"`
	TxHash        string    `json:"tx_hash,omitempty"`
	ExplorerURL   string    `json:"explorer_url,omitempty"`
	AmountWei     string    `json:"amount_wei"`
	AmountETH     string    `json:"amount_eth"`
	BlockNumber   *int64    `json:"block_number,omitempty"`
	Confirmations int64     `json:"confirmations"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// workerErrorResponse 描述后台 Worker 的已清洗错误。
type workerErrorResponse struct {
	ID              int64     `json:"id"`
	Worker          string    `json:"worker"`
	Stage           string    `json:"stage"`
	ReferenceType   string    `json:"reference_type,omitempty"`
	ReferenceID     *int64    `json:"reference_id,omitempty"`
	ErrorCode       string    `json:"error_code"`
	ErrorMessage    string    `json:"error_message"`
	RetryCount      int32     `json:"retry_count"`
	FirstOccurredAt time.Time `json:"first_occurred_at"`
	LastOccurredAt  time.Time `json:"last_occurred_at"`
}

// chainResponse 描述 Sepolia RPC 和扫描器状态。
type chainResponse struct {
	Network       string    `json:"network"`
	Status        string    `json:"status"`
	Endpoint      string    `json:"endpoint"`
	ChainID       string    `json:"chain_id"`
	NetworkHeight uint64    `json:"network_height"`
	ScanHeight    uint64    `json:"scan_height"`
	Lag           uint64    `json:"lag"`
	LastError     string    `json:"last_error,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// listUsers 返回全部演示用户。
func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	items, err := a.apiStore.ListUsers(r.Context())
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]userResponse, 0, len(items))
	for _, item := range items {
		response = append(response, userResponse{ID: item.ID, Code: item.Code, DisplayName: item.DisplayName, CreatedAt: item.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response})
}

// getWallet 返回用户 Sepolia 地址和三类 ETH 余额。
func (a *App) getWallet(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	if _, err := a.apiStore.UserByID(r.Context(), userID); err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	walletAddress, err := a.apiStore.WalletAddressByUser(r.Context(), userID)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	balance, err := a.apiStore.BalanceByUser(r.Context(), userID)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	balanceResponse, err := mapBalance(balance)
	if err != nil {
		a.writeMappingError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, walletResponse{
		UserID: userID, Network: walletAddress.Network,
		Address: common.HexToAddress(walletAddress.Address).Hex(),
		Balance: balanceResponse, CreatedAt: walletAddress.CreatedAt,
	})
}

// listDeposits 返回用户分页充值记录。
func (a *App) listDeposits(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok || !a.ensureUser(r.Context(), w, userID) {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListDepositsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]depositResponse, 0, len(items))
	for _, item := range items {
		mapped, err := a.mapDeposit(item)
		if err != nil {
			a.writeMappingError(r.Context(), w, err)
			return
		}
		response = append(response, mapped)
	}
	writeJSON(w, http.StatusOK, pageResponse[depositResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// listWithdrawals 返回用户分页提币记录。
func (a *App) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok || !a.ensureUser(r.Context(), w, userID) {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListWithdrawalsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]withdrawalResponse, 0, len(items))
	for _, item := range items {
		mapped, err := a.mapWithdrawal(item, false)
		if err != nil {
			a.writeMappingError(r.Context(), w, err)
			return
		}
		response = append(response, mapped)
	}
	writeJSON(w, http.StatusOK, pageResponse[withdrawalResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// getWithdrawal 根据自增主键返回单笔提币状态。
func (a *App) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("withdrawal_id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_WITHDRAWAL_ID", "提币 ID 无效")
		return
	}
	item, err := a.apiStore.WithdrawalByID(r.Context(), id)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response, err := a.mapWithdrawal(item, false)
	if err != nil {
		a.writeMappingError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// listTransactions 返回全局分页充值和提币记录。
func (a *App) listTransactions(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListTransactionsPage(r.Context(), pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]transactionResponse, 0, len(items))
	for _, item := range items {
		amountETH, err := amount.FormatETH(item.AmountWei)
		if err != nil {
			a.writeMappingError(r.Context(), w, err)
			return
		}
		response = append(response, transactionResponse{
			Type: item.Type, ID: item.ID, UserID: item.UserID, Asset: item.Asset,
			TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash),
			AmountWei: item.AmountWei.String(), AmountETH: amountETH,
			BlockNumber: item.BlockNumber, Confirmations: item.Confirmations,
			Status: item.Status, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, pageResponse[transactionResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// getChainStatus 返回不额外发起 RPC 的 Sepolia 健康快照。
func (a *App) getChainStatus(w http.ResponseWriter, _ *http.Request) {
	health := a.chain.Snapshot()
	lag := uint64(0)
	if health.NetworkHeight > health.ScanHeight {
		lag = health.NetworkHeight - health.ScanHeight
	}
	writeJSON(w, http.StatusOK, chainResponse{
		Network: postgres.NetworkSepolia, Status: health.Status, Endpoint: health.Endpoint,
		ChainID: health.ChainID, NetworkHeight: health.NetworkHeight, ScanHeight: health.ScanHeight,
		Lag: lag, LastError: health.LastError, CheckedAt: health.CheckedAt,
	})
}

// listWorkerErrors 返回最近后台任务错误。
func (a *App) listWorkerErrors(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListWorkerErrorsPage(r.Context(), pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]workerErrorResponse, 0, len(items))
	for _, item := range items {
		response = append(response, workerErrorResponse{
			ID: item.ID, Worker: item.Worker, Stage: item.Stage,
			ReferenceType: item.ReferenceType, ReferenceID: item.ReferenceID,
			ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage, RetryCount: item.RetryCount,
			FirstOccurredAt: item.FirstOccurredAt, LastOccurredAt: item.LastOccurredAt,
		})
	}
	writeJSON(w, http.StatusOK, pageResponse[workerErrorResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// parseUserID 解析路由中的正整数用户 ID。
func parseUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return 0, false
	}
	return userID, true
}

// parsePage 校验页码参数并计算数据库偏移量。
func parsePage(w http.ResponseWriter, r *http.Request) (int, int, int, bool) {
	page, pageSize := 1, defaultPageSize
	var err error
	if value := strings.TrimSpace(r.URL.Query().Get("page")); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page <= 0 || page > 100_000 {
			writeAPIError(w, http.StatusBadRequest, "INVALID_PAGINATION", "page 必须是有效正整数")
			return 0, 0, 0, false
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("page_size")); value != "" {
		pageSize, err = strconv.Atoi(value)
		if err != nil || pageSize <= 0 || pageSize > maximumPageSize {
			writeAPIError(w, http.StatusBadRequest, "INVALID_PAGINATION", "page_size 必须在 1 到 100 之间")
			return 0, 0, 0, false
		}
	}
	return page, pageSize, (page - 1) * pageSize, true
}

// ensureUser 校验用户存在并写出标准未找到错误。
func (a *App) ensureUser(ctx context.Context, w http.ResponseWriter, userID int64) bool {
	if _, err := a.apiStore.UserByID(ctx, userID); err != nil {
		a.writeStoreError(ctx, w, err)
		return false
	}
	return true
}

// mapBalance 将大整数余额转换为 Wei 和 ETH 字符串。
func mapBalance(item postgres.AssetBalance) (assetBalanceResponse, error) {
	availableETH, err := amount.FormatETH(item.AvailableWei)
	if err != nil {
		return assetBalanceResponse{}, err
	}
	pendingDepositETH, err := amount.FormatETH(item.PendingDepositWei)
	if err != nil {
		return assetBalanceResponse{}, err
	}
	pendingWithdrawalETH, err := amount.FormatETH(item.PendingWithdrawalWei)
	if err != nil {
		return assetBalanceResponse{}, err
	}
	return assetBalanceResponse{
		Asset: item.Asset, AvailableWei: item.AvailableWei.String(), AvailableETH: availableETH,
		PendingDepositWei: item.PendingDepositWei.String(), PendingDepositETH: pendingDepositETH,
		PendingWithdrawalWei: item.PendingWithdrawalWei.String(), PendingWithdrawalETH: pendingWithdrawalETH,
		UpdatedAt: item.UpdatedAt,
	}, nil
}

// mapDeposit 将数据库充值转换为安全 API 响应。
func (a *App) mapDeposit(item postgres.Deposit) (depositResponse, error) {
	amountETH, err := amount.FormatETH(item.AmountWei)
	if err != nil {
		return depositResponse{}, err
	}
	return depositResponse{
		ID: item.ID, UserID: item.UserID, Network: item.Network, Asset: item.Asset,
		TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash),
		BlockNumber: item.BlockNumber, BlockURL: a.blockExplorerURL(item.BlockNumber),
		AmountWei: item.AmountWei.String(), AmountETH: amountETH,
		Confirmations: item.Confirmations, Status: item.Status,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}, nil
}

// mapWithdrawal 将数据库提币转换为不包含 raw_tx 的安全 API 响应。
func (a *App) mapWithdrawal(item postgres.Withdrawal, created bool) (withdrawalResponse, error) {
	amountETH, err := amount.FormatETH(item.AmountWei)
	if err != nil {
		return withdrawalResponse{}, err
	}
	reservedFeeETH, err := amount.FormatETH(item.ReservedFeeWei)
	if err != nil {
		return withdrawalResponse{}, err
	}
	response := withdrawalResponse{
		ID: item.ID, UserID: item.UserID, ToAddress: common.HexToAddress(item.ToAddress).Hex(),
		AmountETH: amountETH, AmountWei: item.AmountWei.String(),
		ReservedFeeWei: item.ReservedFeeWei.String(), ReservedFeeETH: reservedFeeETH,
		Status: item.Status, Created: created, TxHash: item.TxHash,
		ExplorerURL: a.transactionExplorerURL(item.TxHash), Confirmations: item.Confirmations,
		BlockNumber: item.BlockNumber, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	if item.ActualFeeWei != nil {
		response.ActualFeeWei = item.ActualFeeWei.String()
		response.ActualFeeETH, err = amount.FormatETH(item.ActualFeeWei)
		if err != nil {
			return withdrawalResponse{}, err
		}
	}
	if item.GasLimit != nil {
		response.GasLimit = uint64(*item.GasLimit)
	}
	if item.MaxFeePerGasWei != nil {
		response.MaxFeePerGasWei = item.MaxFeePerGasWei.String()
	}
	if item.MaxPriorityFeePerGasWei != nil {
		response.MaxPriorityFeePerGasWei = item.MaxPriorityFeePerGasWei.String()
	}
	if item.Nonce != nil {
		response.Nonce = item.Nonce.String()
	}
	return response, nil
}

// transactionExplorerURL 生成 Sepolia 交易浏览器链接。
func (a *App) transactionExplorerURL(hash string) string {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return ""
	}
	return fmt.Sprintf("%s/tx/%s", a.config.SepoliaExplorerBaseURL, hash)
}

// blockExplorerURL 生成 Sepolia 区块浏览器链接。
func (a *App) blockExplorerURL(blockNumber int64) string {
	if blockNumber < 0 {
		return ""
	}
	return fmt.Sprintf("%s/block/%d", a.config.SepoliaExplorerBaseURL, blockNumber)
}

// writeStoreError 将数据访问错误映射为安全 HTTP 错误并记录 request_id。
func (a *App) writeStoreError(ctx context.Context, w http.ResponseWriter, err error) {
	if errors.Is(err, postgres.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "请求的记录不存在")
		return
	}
	logging.WithContext(a.logger, ctx).Error("查询 API 数据失败", "error", err)
	writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
}

// writeMappingError 记录数据库金额映射异常并返回统一服务端错误。
func (a *App) writeMappingError(ctx context.Context, w http.ResponseWriter, err error) {
	logging.WithContext(a.logger, ctx).Error("转换 API 响应失败", "error", err)
	writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
}
