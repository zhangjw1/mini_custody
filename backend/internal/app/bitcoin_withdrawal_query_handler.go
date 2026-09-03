package app

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

// getBitcoinWithdrawal 返回 BTC 提币状态。
func (a *App) getBitcoinWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("withdrawal_id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_WITHDRAWAL_ID", "提币 ID 无效")
		return
	}
	item, err := a.store.BTCWithdrawalByID(r.Context(), id)
	if errors.Is(err, postgres.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "WITHDRAWAL_NOT_FOUND", "BTC 提币不存在")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_UNAVAILABLE", "BTC 提币查询失败")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// listBitcoinWithdrawals 返回用户 BTC 提币列表。
func (a *App) listBitcoinWithdrawals(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return
	}
	_, pageSize, offset, ok := parsePage(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListBTCWithdrawalsPage(r.Context(), userID, pageSize+1, offset)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_UNAVAILABLE", "BTC 提币查询失败")
		return
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "page_size": pageSize, "has_more": hasMore})
}
