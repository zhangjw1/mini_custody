package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
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

// createBitcoinWithdrawal 创建 BTC 测试网提币请求并占用余额。
func (a *App) createBitcoinWithdrawal(w http.ResponseWriter, r *http.Request) {
	if a.bitcoinWithdrawals == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "BITCOIN_DISABLED", "Bitcoin 尚未启用")
		return
	}
	userID, ok := parseUserID(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "缺少 Idempotency-Key")
		return
	}
	var request bitcoinWithdrawalRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	item, created, err := a.bitcoinWithdrawals.Create(r.Context(), userID, key, request.ToAddress, request.AmountSats)
	if err != nil {
		a.writeBitcoinWithdrawalError(r.Context(), w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, bitcoinWithdrawalResponse{
		ID: item.ID, ToAddress: item.ToAddress, AmountSats: item.AmountSats,
		FeeRateSatVB: item.FeeRateSatVB, Status: item.Status, Created: created,
	})
}

// writeBitcoinWithdrawalError 将 BTC 提币领域与数据库错误安全映射为标准 HTTP 状态码，绝不暴露 SQLSTATE。
func (a *App) writeBitcoinWithdrawalError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "USER_OR_ASSET_NOT_FOUND", "用户或 BTC 余额记录不存在")
	case errors.Is(err, postgres.ErrUserDisabled):
		writeAPIError(w, http.StatusForbidden, "USER_DISABLED", "该托管用户已被禁用，禁止提币")
	case errors.Is(err, postgres.ErrInsufficientBalance):
		writeAPIError(w, http.StatusConflict, "INSUFFICIENT_BALANCE", "BTC 可用余额不足")
	case errors.Is(err, postgres.ErrIdempotencyConflict):
		writeAPIError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "幂等标识已用于不同的 BTC 提币请求")
	case isBTCValidationError(err):
		writeAPIError(w, http.StatusBadRequest, "INVALID_BTC_WITHDRAWAL", err.Error())
	default:
		logging.WithContext(a.logger, ctx).Error("创建 BTC 提币失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务器内部错误")
	}
}

// isBTCValidationError 判断错误是否为安全的 BTC 提币参数校验失败消息。
func isBTCValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "BTC 提币请求无效") ||
		strings.Contains(msg, "dust") ||
		strings.Contains(msg, "测试网络不匹配") ||
		strings.Contains(msg, "Signet 地址")
}
