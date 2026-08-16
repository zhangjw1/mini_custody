package app

import (
	"errors"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

// assetResponse 描述可供后台选择的链上资产。
type assetResponse struct {
	ID              int64     `json:"id"`
	Network         string    `json:"network"`
	AssetType       string    `json:"asset_type"`
	Symbol          string    `json:"symbol"`
	ContractAddress string    `json:"contract_address,omitempty"`
	Decimals        uint8     `json:"decimals"`
	Enabled         bool      `json:"enabled"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// multiAssetBalanceResponse 描述一个用户指定资产的三类账本余额。
type multiAssetBalanceResponse struct {
	AssetID                int64     `json:"asset_id"`
	Asset                  string    `json:"asset"`
	AssetType              string    `json:"asset_type"`
	ContractAddress        string    `json:"contract_address,omitempty"`
	Decimals               uint8     `json:"decimals"`
	AvailableUnits         string    `json:"available_units"`
	Available              string    `json:"available"`
	PendingDepositUnits    string    `json:"pending_deposit_units"`
	PendingDeposit         string    `json:"pending_deposit"`
	PendingWithdrawalUnits string    `json:"pending_withdrawal_units"`
	PendingWithdrawal      string    `json:"pending_withdrawal"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// tokenDepositResponse 描述一笔用户 Token 充值 Event。
type tokenDepositResponse struct {
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id"`
	Asset         string    `json:"asset"`
	Decimals      uint8     `json:"decimals"`
	TxHash        string    `json:"tx_hash"`
	ExplorerURL   string    `json:"explorer_url"`
	LogIndex      int32     `json:"log_index"`
	BlockNumber   int64     `json:"block_number"`
	BlockURL      string    `json:"block_url"`
	FromAddress   string    `json:"from_address"`
	ToAddress     string    `json:"to_address"`
	Amount        string    `json:"amount"`
	AmountUnits   string    `json:"amount_units"`
	Confirmations int64     `json:"confirmations"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// tokenSweepResponse 描述不包含 raw_tx 的 Token 归集任务。
type tokenSweepResponse struct {
	ID                    int64     `json:"id"`
	UserID                int64     `json:"user_id"`
	Asset                 string    `json:"asset"`
	RecognizedAmount      string    `json:"recognized_amount"`
	RecognizedAmountUnits string    `json:"recognized_amount_units"`
	SweepAmount           string    `json:"sweep_amount,omitempty"`
	SweepAmountUnits      string    `json:"sweep_amount_units,omitempty"`
	GasTopupTransferID    *int64    `json:"gas_topup_transfer_id,omitempty"`
	TxHash                string    `json:"tx_hash,omitempty"`
	ExplorerURL           string    `json:"explorer_url,omitempty"`
	Confirmations         int64     `json:"confirmations"`
	ActualFeeWei          string    `json:"actual_fee_wei,omitempty"`
	ActualFeeETH          string    `json:"actual_fee_eth,omitempty"`
	Status                string    `json:"status"`
	ErrorCode             string    `json:"error_code,omitempty"`
	ErrorMessage          string    `json:"error_message,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// internalTransferResponse 描述平台 Gas 补充转账。
type internalTransferResponse struct {
	ID            int64     `json:"id"`
	SweepID       int64     `json:"sweep_id"`
	TransferType  string    `json:"transfer_type"`
	FromAddress   string    `json:"from_address"`
	ToAddress     string    `json:"to_address"`
	AmountWei     string    `json:"amount_wei"`
	AmountETH     string    `json:"amount_eth"`
	TxHash        string    `json:"tx_hash,omitempty"`
	ExplorerURL   string    `json:"explorer_url,omitempty"`
	Confirmations int64     `json:"confirmations"`
	ActualFeeWei  string    `json:"actual_fee_wei,omitempty"`
	ActualFeeETH  string    `json:"actual_fee_eth,omitempty"`
	Status        string    `json:"status"`
	ErrorCode     string    `json:"error_code,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// platformWalletResponse 描述平台热钱包公开状态和最近健康快照。
type platformWalletResponse struct {
	Network           string `json:"network"`
	Role              string `json:"role"`
	Address           string `json:"address"`
	NextNonce         string `json:"next_nonce"`
	GasStatus         string `json:"gas_status"`
	ETHBalanceWei     string `json:"eth_balance_wei"`
	ETHBalance        string `json:"eth_balance"`
	TokenStatus       string `json:"token_status"`
	TokenSymbol       string `json:"token_symbol,omitempty"`
	TokenBalanceUnits string `json:"token_balance_units,omitempty"`
	TokenBalance      string `json:"token_balance,omitempty"`
	LastError         string `json:"last_error,omitempty"`
}

// listAssets 返回全部资产配置，合约地址使用 EIP-55 展示。
func (a *App) listAssets(w http.ResponseWriter, r *http.Request) {
	items, err := a.apiStore.ListAssets(r.Context())
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]assetResponse, 0, len(items))
	for _, item := range items {
		contractAddress := ""
		if item.ContractAddress != "" {
			contractAddress = common.HexToAddress(item.ContractAddress).Hex()
		}
		response = append(response, assetResponse{ID: item.ID, Network: item.Network, AssetType: item.AssetType, Symbol: item.Symbol, ContractAddress: contractAddress, Decimals: item.Decimals, Enabled: item.Enabled, UpdatedAt: item.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response})
}

// listUserBalances 返回用户全部已登记资产的账本余额。
func (a *App) listUserBalances(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok || !a.ensureUser(r.Context(), w, userID) {
		return
	}
	assets, err := a.apiStore.ListAssets(r.Context())
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]multiAssetBalanceResponse, 0, len(assets))
	for _, asset := range assets {
		balance, err := a.apiStore.BalanceByUserAndAsset(r.Context(), userID, asset.ID)
		if err != nil {
			a.writeStoreError(r.Context(), w, err)
			return
		}
		available, _ := amount.FormatDecimal(balance.AvailableWei, asset.Decimals)
		pendingDeposit, _ := amount.FormatDecimal(balance.PendingDepositWei, asset.Decimals)
		pendingWithdrawal, _ := amount.FormatDecimal(balance.PendingWithdrawalWei, asset.Decimals)
		contractAddress := ""
		if asset.ContractAddress != "" {
			contractAddress = common.HexToAddress(asset.ContractAddress).Hex()
		}
		response = append(response, multiAssetBalanceResponse{
			AssetID: asset.ID, Asset: asset.Symbol, AssetType: asset.AssetType, ContractAddress: contractAddress, Decimals: asset.Decimals,
			AvailableUnits: balance.AvailableWei.String(), Available: available,
			PendingDepositUnits: balance.PendingDepositWei.String(), PendingDeposit: pendingDeposit,
			PendingWithdrawalUnits: balance.PendingWithdrawalWei.String(), PendingWithdrawal: pendingWithdrawal, UpdatedAt: balance.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response})
}

// listTokenDeposits 返回用户分页 Token 充值记录。
func (a *App) listTokenDeposits(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok || !a.ensureUser(r.Context(), w, userID) {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListTokenDepositsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	assets, err := a.assetIndex(r)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]tokenDepositResponse, 0, len(items))
	for _, item := range items {
		asset, found := assets[item.AssetID]
		if !found {
			a.writeMappingError(r.Context(), w, errors.New("Token 充值资产不存在"))
			return
		}
		formatted, err := amount.FormatDecimal(item.AmountUnits, asset.Decimals)
		if err != nil {
			a.writeMappingError(r.Context(), w, err)
			return
		}
		response = append(response, tokenDepositResponse{ID: item.ID, UserID: item.UserID, Asset: asset.Symbol, Decimals: asset.Decimals, TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash), LogIndex: item.LogIndex, BlockNumber: item.BlockNumber, BlockURL: a.blockExplorerURL(item.BlockNumber), FromAddress: common.HexToAddress(item.FromAddress).Hex(), ToAddress: common.HexToAddress(item.ToAddress).Hex(), Amount: formatted, AmountUnits: item.AmountUnits.String(), Confirmations: item.Confirmations, Status: item.Status, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, pageResponse[tokenDepositResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// listTokenWithdrawals 返回用户分页 Token 提币记录。
func (a *App) listTokenWithdrawals(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok || !a.ensureUser(r.Context(), w, userID) {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListTokenWithdrawalsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	assets, err := a.assetIndex(r)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]tokenWithdrawalResponse, 0, len(items))
	for _, item := range items {
		mapped, err := a.mapStoredTokenWithdrawal(item, assets[item.AssetID])
		if err != nil {
			a.writeMappingError(r.Context(), w, err)
			return
		}
		response = append(response, mapped)
	}
	writeJSON(w, http.StatusOK, pageResponse[tokenWithdrawalResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// getTokenWithdrawal 根据自增主键返回单笔 Token 提币状态。
func (a *App) getTokenWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("withdrawal_id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_TOKEN_WITHDRAWAL_ID", "Token 提币 ID 无效")
		return
	}
	item, err := a.apiStore.TokenWithdrawalByID(r.Context(), id)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	assets, err := a.assetIndex(r)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response, err := a.mapStoredTokenWithdrawal(item, assets[item.AssetID])
	if err != nil {
		a.writeMappingError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// listTokenSweeps 返回分页 Token 归集任务。
func (a *App) listTokenSweeps(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListTokenSweepsPage(r.Context(), pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	assets, err := a.assetIndex(r)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := make([]tokenSweepResponse, 0, len(items))
	for _, item := range items {
		asset := assets[item.AssetID]
		recognized, _ := amount.FormatDecimal(item.RecognizedAmountUnits, asset.Decimals)
		mapped := tokenSweepResponse{ID: item.ID, UserID: item.UserID, Asset: asset.Symbol, RecognizedAmount: recognized, RecognizedAmountUnits: item.RecognizedAmountUnits.String(), GasTopupTransferID: item.GasTopupTransferID, TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash), Confirmations: item.Confirmations, Status: item.Status, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
		if item.SweepAmountUnits != nil {
			mapped.SweepAmountUnits = item.SweepAmountUnits.String()
			mapped.SweepAmount, _ = amount.FormatDecimal(item.SweepAmountUnits, asset.Decimals)
		}
		if item.ActualFeeWei != nil {
			mapped.ActualFeeWei = item.ActualFeeWei.String()
			mapped.ActualFeeETH, _ = amount.FormatETH(item.ActualFeeWei)
		}
		response = append(response, mapped)
	}
	writeJSON(w, http.StatusOK, pageResponse[tokenSweepResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// listInternalTransfers 返回分页平台 Gas 补充转账。
func (a *App) listInternalTransfers(w http.ResponseWriter, r *http.Request) {
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.apiStore.ListInternalTransfersPage(r.Context(), pageSize+1, offset)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]internalTransferResponse, 0, len(items))
	for _, item := range items {
		amountETH, _ := amount.FormatETH(item.AmountWei)
		mapped := internalTransferResponse{ID: item.ID, SweepID: item.SweepID, TransferType: item.TransferType, FromAddress: common.HexToAddress(item.FromAddress).Hex(), ToAddress: common.HexToAddress(item.ToAddress).Hex(), AmountWei: item.AmountWei.String(), AmountETH: amountETH, TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash), Confirmations: item.Confirmations, Status: item.Status, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
		if item.ActualFeeWei != nil {
			mapped.ActualFeeWei = item.ActualFeeWei.String()
			mapped.ActualFeeETH, _ = amount.FormatETH(item.ActualFeeWei)
		}
		response = append(response, mapped)
	}
	writeJSON(w, http.StatusOK, pageResponse[internalTransferResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// getPlatformWalletStatus 返回平台热钱包公开信息和已有 Worker 健康快照。
func (a *App) getPlatformWalletStatus(w http.ResponseWriter, r *http.Request) {
	platform, err := a.apiStore.PlatformWalletByRole(r.Context(), postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		a.writeStoreError(r.Context(), w, err)
		return
	}
	response := platformWalletResponse{Network: platform.Network, Role: platform.Role, Address: common.HexToAddress(platform.Address).Hex(), NextNonce: platform.NextNonce.String(), GasStatus: "DISABLED", TokenStatus: "DISABLED"}
	if a.gasStation != nil {
		snapshot := a.gasStation.Snapshot()
		response.GasStatus = snapshot.Status
		response.ETHBalanceWei = snapshot.BalanceWei.String()
		response.ETHBalance, _ = amount.FormatETH(snapshot.BalanceWei)
		response.LastError = snapshot.LastError
	}
	if a.tokenSweeper != nil {
		snapshot := a.tokenSweeper.Snapshot()
		response.TokenStatus = snapshot.Status
		response.TokenSymbol = snapshot.Symbol
		response.TokenBalanceUnits = snapshot.BalanceUnits.String()
		response.TokenBalance, _ = amount.FormatDecimal(snapshot.BalanceUnits, a.tokenAsset.Decimals)
		if snapshot.LastError != "" {
			response.LastError = snapshot.LastError
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// assetIndex 返回以资产 ID 为键的资产配置索引。
func (a *App) assetIndex(r *http.Request) (map[int64]postgres.Asset, error) {
	items, err := a.apiStore.ListAssets(r.Context())
	if err != nil {
		return nil, err
	}
	result := make(map[int64]postgres.Asset, len(items))
	for _, item := range items {
		result[item.ID] = item
	}
	return result, nil
}

// mapStoredTokenWithdrawal 将数据库 Token 提币映射为不暴露原始交易的响应。
func (a *App) mapStoredTokenWithdrawal(item postgres.TokenWithdrawal, asset postgres.Asset) (tokenWithdrawalResponse, error) {
	if asset.ID == 0 {
		return tokenWithdrawalResponse{}, errors.New("Token 提币资产不存在")
	}
	formatted, err := amount.FormatDecimal(item.AmountUnits, asset.Decimals)
	if err != nil {
		return tokenWithdrawalResponse{}, err
	}
	response := tokenWithdrawalResponse{ID: item.ID, UserID: item.UserID, Asset: asset.Symbol, Decimals: asset.Decimals, ToAddress: common.HexToAddress(item.ToAddress).Hex(), Amount: formatted, AmountUnits: item.AmountUnits.String(), Status: item.Status, TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash), Confirmations: item.Confirmations, BlockNumber: item.BlockNumber, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	if item.Nonce != nil {
		response.Nonce = item.Nonce.String()
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
	if item.GasLimit != nil && item.MaxFeePerGasWei != nil {
		estimated := new(big.Int).Mul(new(big.Int).SetInt64(*item.GasLimit), item.MaxFeePerGasWei)
		response.EstimatedGasWei = estimated.String()
		response.EstimatedGasETH, _ = amount.FormatETH(estimated)
	}
	if item.ActualFeeWei != nil {
		response.ActualFeeWei = item.ActualFeeWei.String()
		response.ActualFeeETH, _ = amount.FormatETH(item.ActualFeeWei)
	}
	return response, nil
}

// normalizeTransactionFilter 将空值和 ALL 归一化为空筛选条件。
func normalizeTransactionFilter(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "ALL" {
		return ""
	}
	return value
}
