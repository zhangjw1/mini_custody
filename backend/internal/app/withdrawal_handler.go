package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/logging"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
	"github.com/xiaoqi/mini-custody/backend/internal/withdrawal"
)

// createWithdrawalRequest 描述创建 Sepolia ETH 提币的 JSON 请求体。
type createWithdrawalRequest struct {
	ToAddress string `json:"to_address"`
	AmountETH string `json:"amount_eth"`
}

// withdrawalQuoteResponse 描述不会创建提币的只读费用估算。
type withdrawalQuoteResponse struct {
	AmountETH               string `json:"amount_eth"`
	AmountWei               string `json:"amount_wei"`
	GasLimit                uint64 `json:"gas_limit"`
	MaxFeePerGasWei         string `json:"max_fee_per_gas_wei"`
	MaxPriorityFeePerGasWei string `json:"max_priority_fee_per_gas_wei"`
	ReservedFeeWei          string `json:"reserved_fee_wei"`
	ReservedFeeETH          string `json:"reserved_fee_eth"`
}

// withdrawalResponse 描述创建提币后可安全返回的业务字段。
type withdrawalResponse struct {
	ID                      int64     `json:"id"`
	UserID                  int64     `json:"user_id"`
	ToAddress               string    `json:"to_address"`
	AmountETH               string    `json:"amount_eth"`
	AmountWei               string    `json:"amount_wei"`
	ReservedFeeWei          string    `json:"reserved_fee_wei"`
	ReservedFeeETH          string    `json:"reserved_fee_eth"`
	ActualFeeWei            string    `json:"actual_fee_wei,omitempty"`
	ActualFeeETH            string    `json:"actual_fee_eth,omitempty"`
	GasLimit                uint64    `json:"gas_limit"`
	MaxFeePerGasWei         string    `json:"max_fee_per_gas_wei"`
	MaxPriorityFeePerGasWei string    `json:"max_priority_fee_per_gas_wei"`
	Status                  string    `json:"status"`
	Created                 bool      `json:"created"`
	TxHash                  string    `json:"tx_hash,omitempty"`
	ExplorerURL             string    `json:"explorer_url,omitempty"`
	Confirmations           int64     `json:"confirmations"`
	Nonce                   string    `json:"nonce,omitempty"`
	BlockNumber             *int64    `json:"block_number,omitempty"`
	ErrorCode               string    `json:"error_code,omitempty"`
	ErrorMessage            string    `json:"error_message,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// apiErrorResponse 描述不会泄露底层数据库或 RPC 信息的 API 错误。
type apiErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// createWithdrawal 校验请求并通过提币服务完成费用预留和幂等创建。
func (a *App) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request createWithdrawalRequest
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体格式无效")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体只能包含一个 JSON 对象")
		return
	}
	result, err := a.withdrawals.Create(r.Context(), withdrawal.CreateRequest{
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
		UserID:         userID,
		ToAddress:      request.ToAddress,
		AmountETH:      request.AmountETH,
	})
	if err != nil {
		a.writeWithdrawalError(r.Context(), w, err)
		return
	}
	amountETH, err := amount.FormatETH(result.Withdrawal.AmountWei)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "提币金额格式化失败")
		return
	}
	reservedFeeETH, err := amount.FormatETH(result.Withdrawal.ReservedFeeWei)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "提币费用格式化失败")
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, withdrawalResponse{
		ID:                      result.Withdrawal.ID,
		UserID:                  result.Withdrawal.UserID,
		ToAddress:               result.Withdrawal.ToAddress,
		AmountETH:               amountETH,
		AmountWei:               result.Withdrawal.AmountWei.String(),
		ReservedFeeWei:          result.Withdrawal.ReservedFeeWei.String(),
		ReservedFeeETH:          reservedFeeETH,
		GasLimit:                result.Fee.GasLimit,
		MaxFeePerGasWei:         result.Fee.MaxFeePerGasWei.String(),
		MaxPriorityFeePerGasWei: result.Fee.MaxPriorityFeePerGasWei.String(),
		Status:                  result.Withdrawal.Status,
		Created:                 result.Created,
		TxHash:                  result.Withdrawal.TxHash,
		ExplorerURL:             a.transactionExplorerURL(result.Withdrawal.TxHash),
		Confirmations:           result.Withdrawal.Confirmations,
		BlockNumber:             result.Withdrawal.BlockNumber,
		CreatedAt:               result.Withdrawal.CreatedAt,
		UpdatedAt:               result.Withdrawal.UpdatedAt,
	})
}

// quoteWithdrawal 校验参数并返回不占用余额的当前最大网络费估算。
func (a *App) quoteWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request createWithdrawalRequest
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体格式无效")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体只能包含一个 JSON 对象")
		return
	}
	result, err := a.withdrawals.Quote(r.Context(), withdrawal.QuoteRequest{
		UserID: userID, ToAddress: request.ToAddress, AmountETH: request.AmountETH,
	})
	if err != nil {
		a.writeWithdrawalError(r.Context(), w, err)
		return
	}
	amountETH, err := amount.FormatETH(result.AmountWei)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "提币金额格式化失败")
		return
	}
	reservedFeeETH, err := amount.FormatETH(result.Fee.ReservedFeeWei)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "提币费用格式化失败")
		return
	}
	writeJSON(w, http.StatusOK, withdrawalQuoteResponse{
		AmountETH: amountETH, AmountWei: result.AmountWei.String(), GasLimit: result.Fee.GasLimit,
		MaxFeePerGasWei:         result.Fee.MaxFeePerGasWei.String(),
		MaxPriorityFeePerGasWei: result.Fee.MaxPriorityFeePerGasWei.String(),
		ReservedFeeWei:          result.Fee.ReservedFeeWei.String(), ReservedFeeETH: reservedFeeETH,
	})
}

// writeWithdrawalError 将提币领域错误映射为稳定且安全的 HTTP 错误。
func (a *App) writeWithdrawalError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, withdrawal.ErrInvalidRequest):
		writeAPIError(w, http.StatusBadRequest, "INVALID_WITHDRAWAL", err.Error())
	case errors.Is(err, postgres.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "USER_NOT_FOUND", "用户或托管地址不存在")
	case errors.Is(err, postgres.ErrInsufficientBalance):
		writeAPIError(w, http.StatusConflict, "INSUFFICIENT_BALANCE", "可用余额不足以支付提币金额和最大网络费")
	case errors.Is(err, postgres.ErrIdempotencyConflict):
		writeAPIError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "幂等标识已用于不同的提币请求")
	default:
		logging.WithContext(a.logger, ctx).Error("创建 Sepolia 提币失败", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "WITHDRAWAL_UNAVAILABLE", "当前无法创建提币，请稍后重试")
	}
}

// writeAPIError 输出统一 JSON API 错误。
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorResponse{Code: code, Message: message, RequestID: w.Header().Get("X-Request-ID")})
}

// writeJSON 输出指定 HTTP 状态的 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
