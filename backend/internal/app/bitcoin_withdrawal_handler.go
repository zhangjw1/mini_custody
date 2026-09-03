package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type bitcoinWithdrawalRequest struct {
	ToAddress  string `json:"to_address"`
	AmountSats int64  `json:"amount_sats"`
}
type bitcoinWithdrawalResponse struct {
	ID           int64  `json:"id"`
	ToAddress    string `json:"to_address"`
	AmountSats   int64  `json:"amount_sats"`
	FeeRateSatVB int64  `json:"fee_rate_sat_vb"`
	Status       string `json:"status"`
	Created      bool   `json:"created"`
}

// createBitcoinWithdrawal 创建 BTC Signet 提币请求并占用余额。
func (a *App) createBitcoinWithdrawal(w http.ResponseWriter, r *http.Request) {
	if a.bitcoinWithdrawals == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_DISABLED", "Bitcoin 尚未启用")
		return
	}
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "缺少 Idempotency-Key")
		return
	}
	var request bitcoinWithdrawalRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求格式无效")
		return
	}
	item, created, err := a.bitcoinWithdrawals.Create(r.Context(), userID, key, request.ToAddress, request.AmountSats)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BTC_WITHDRAWAL", err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, bitcoinWithdrawalResponse{ID: item.ID, ToAddress: item.ToAddress, AmountSats: item.AmountSats, FeeRateSatVB: item.FeeRateSatVB, Status: item.Status, Created: created})
}
