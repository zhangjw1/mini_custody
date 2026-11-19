package app

import (
	"errors"
	"net/http"

	"github.com/xiaoqi/mini-custody/backend/internal/btc"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

// getBitcoinWithdrawal 返回 BTC 提币状态。
func (a *App) getBitcoinWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePositivePathID(w, r, "withdrawal_id", "INVALID_WITHDRAWAL_ID", "提币 ID 无效")
	if !ok {
		return
	}
	item, err := a.store.BTCWithdrawalByID(r.Context(), id)
	if errors.Is(err, postgres.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "WITHDRAWAL_NOT_FOUND", "BTC 提币不存在")
		return
	}
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("查询 BTC 提币失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// listBitcoinWithdrawals 返回用户 BTC 提币列表。
func (a *App) listBitcoinWithdrawals(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	page, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListBTCWithdrawalsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("分页查询 BTC 提币失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, pageResponse[btc.Withdrawal]{Items: items, Page: page, PageSize: pageSize, HasMore: hasMore})
}
