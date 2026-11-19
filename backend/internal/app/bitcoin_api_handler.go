package app

import (
	"errors"
	"net/http"

	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

type bitcoinWalletResponse struct {
	Network     string `json:"network"`
	Address     string `json:"address"`
	AddressType string `json:"address_type"`
}

type bitcoinSweepResponse struct {
	ID                 int64  `json:"id"`
	SourceAddress      string `json:"source_address"`
	DestinationAddress string `json:"destination_address"`
	InputSats          int64  `json:"input_sats"`
	OutputSats         int64  `json:"output_sats"`
	FeeSats            int64  `json:"fee_sats"`
	FeeRateSatVB       int64  `json:"fee_rate_sat_vb"`
	TxID               string `json:"txid,omitempty"`
	Status             string `json:"status"`
}

type bitcoinDepositResponse struct {
	ID            int64  `json:"id"`
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	BlockHeight   int64  `json:"block_height"`
	AmountSats    int64  `json:"amount_sats"`
	Confirmations int64  `json:"confirmations"`
	Status        string `json:"status"`
}

// getBitcoinWallet 返回用户 Bitcoin 测试网充值地址。
func (a *App) getBitcoinWallet(w http.ResponseWriter, r *http.Request) {
	if !a.config.Bitcoin.Enabled {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_DISABLED", "Bitcoin 尚未启用")
		return
	}
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	item, err := a.store.BTCAddressByUser(r.Context(), userID)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "BITCOIN_WALLET_NOT_FOUND", "Bitcoin 地址不存在")
			return
		}
		logging.WithContext(a.logger, r.Context()).Error("查询 BTC 充值地址失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, bitcoinWalletResponse{Network: postgres.NetworkBitcoinSignet, Address: item.Address, AddressType: "P2WPKH"})
}

// listBitcoinSweeps 返回不包含原始签名交易的 BTC 归集任务。
func (a *App) listBitcoinSweeps(w http.ResponseWriter, r *http.Request) {
	if !a.config.Bitcoin.Enabled {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_DISABLED", "Bitcoin 尚未启用")
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListBTCSweepsPage(r.Context(), pageSize+1, offset)
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("查询 BTC 归集任务失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]bitcoinSweepResponse, 0, len(items))
	for _, item := range items {
		response = append(response, bitcoinSweepResponse{
			ID: item.ID, SourceAddress: item.From.Address, DestinationAddress: item.To.Address,
			InputSats: item.InputValueSats, OutputSats: item.OutputValueSats, FeeSats: item.FeeSats,
			FeeRateSatVB: item.FeeRateSatVB, TxID: item.TxID, Status: item.Status,
		})
	}
	writeJSON(w, http.StatusOK, pageResponse[bitcoinSweepResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}

// listBitcoinDeposits 返回用户 BTC 充值记录。
func (a *App) listBitcoinDeposits(w http.ResponseWriter, r *http.Request) {
	if !a.config.Bitcoin.Enabled {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_DISABLED", "Bitcoin 尚未启用")
		return
	}
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListBTCDepositsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("查询 BTC 充值记录失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	response := make([]bitcoinDepositResponse, 0, len(items))
	for _, item := range items {
		response = append(response, bitcoinDepositResponse{
			ID: item.ID, TxID: item.TxID, Vout: item.Vout, BlockHeight: item.BlockHeight,
			AmountSats: item.AmountSats, Confirmations: item.Confirmations, Status: item.Status,
		})
	}
	writeJSON(w, http.StatusOK, pageResponse[bitcoinDepositResponse]{Items: response, Page: page, PageSize: pageSize, HasMore: hasMore})
}
