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
	tokenwithdrawal "github.com/xiaoqi/mini-custody/backend/internal/token/withdrawal"
)

// tokenWithdrawalRequest 描述 Token 提币试算和创建的 JSON 请求体。
type tokenWithdrawalRequest struct {
	ToAddress string `json:"to_address"`
	Amount    string `json:"amount"`
}

// tokenWithdrawalQuoteResponse 描述 Token 金额和平台预计承担的 ETH Gas。
type tokenWithdrawalQuoteResponse struct {
	Asset                   string `json:"asset"`
	Decimals                uint8  `json:"decimals"`
	Amount                  string `json:"amount"`
	AmountUnits             string `json:"amount_units"`
	GasLimit                uint64 `json:"gas_limit"`
	MaxFeePerGasWei         string `json:"max_fee_per_gas_wei"`
	MaxPriorityFeePerGasWei string `json:"max_priority_fee_per_gas_wei"`
	EstimatedGasWei         string `json:"estimated_gas_wei"`
	EstimatedGasETH         string `json:"estimated_gas_eth"`
}

// tokenWithdrawalResponse 描述创建后可安全返回的 Token 提币字段。
type tokenWithdrawalResponse struct {
	ID                      int64     `json:"id"`
	UserID                  int64     `json:"user_id"`
	Asset                   string    `json:"asset"`
	Decimals                uint8     `json:"decimals"`
	ToAddress               string    `json:"to_address"`
	Amount                  string    `json:"amount"`
	AmountUnits             string    `json:"amount_units"`
	EstimatedGasWei         string    `json:"estimated_gas_wei"`
	EstimatedGasETH         string    `json:"estimated_gas_eth"`
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
	ActualFeeWei            string    `json:"actual_fee_wei,omitempty"`
	ActualFeeETH            string    `json:"actual_fee_eth,omitempty"`
	ErrorCode               string    `json:"error_code,omitempty"`
	ErrorMessage            string    `json:"error_message,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// quoteTokenWithdrawal 校验 Token 提币参数并返回只读费用试算。
func (a *App) quoteTokenWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, request, ok := decodeTokenWithdrawalRequest(w, r)
	if !ok {
		return
	}
	result, err := a.tokenWithdrawals.Quote(r.Context(), tokenwithdrawal.QuoteRequest{
		UserID: userID, ToAddress: request.ToAddress, Amount: request.Amount,
	})
	if err != nil {
		a.writeTokenWithdrawalError(r.Context(), w, err)
		return
	}
	amountText, err := amount.FormatDecimal(result.AmountUnits, a.tokenAsset.Decimals)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "Token 提币金额格式化失败")
		return
	}
	gasETH, err := amount.FormatETH(result.Fee.ReservedFeeWei)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "Token 提币 Gas 格式化失败")
		return
	}
	writeJSON(w, http.StatusOK, tokenWithdrawalQuoteResponse{
		Asset: a.tokenAsset.Symbol, Decimals: a.tokenAsset.Decimals, Amount: amountText,
		AmountUnits: result.AmountUnits.String(), GasLimit: result.Fee.GasLimit,
		MaxFeePerGasWei:         result.Fee.MaxFeePerGasWei.String(),
		MaxPriorityFeePerGasWei: result.Fee.MaxPriorityFeePerGasWei.String(),
		EstimatedGasWei:         result.Fee.ReservedFeeWei.String(), EstimatedGasETH: gasETH,
	})
}

// createTokenWithdrawal 校验请求并完成 Token 余额占用和幂等创建。
func (a *App) createTokenWithdrawal(w http.ResponseWriter, r *http.Request) {
	userID, request, ok := decodeTokenWithdrawalRequest(w, r)
	if !ok {
		return
	}
	result, err := a.tokenWithdrawals.Create(r.Context(), tokenwithdrawal.CreateRequest{
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), UserID: userID,
		ToAddress: request.ToAddress, Amount: request.Amount,
	})
	if err != nil {
		a.writeTokenWithdrawalError(r.Context(), w, err)
		return
	}
	response, err := a.mapCreatedTokenWithdrawal(result)
	if err != nil {
		logging.WithContext(a.logger, r.Context()).Error("映射 Token 提币响应失败", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "AMOUNT_FORMAT_FAILED", "Token 提币响应格式化失败")
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, response)
}

// decodeTokenWithdrawalRequest 严格读取用户 ID 和单个 Token 提币 JSON 对象。
func decodeTokenWithdrawalRequest(w http.ResponseWriter, r *http.Request) (int64, tokenWithdrawalRequest, bool) {
	userID, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_USER_ID", "用户 ID 无效")
		return 0, tokenWithdrawalRequest{}, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request tokenWithdrawalRequest
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体格式无效")
		return 0, tokenWithdrawalRequest{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "INVALID_BODY", "请求体只能包含一个 JSON 对象")
		return 0, tokenWithdrawalRequest{}, false
	}
	return userID, request, true
}

// mapCreatedTokenWithdrawal 将领域创建结果转换为不暴露 raw_tx 的 API 响应。
func (a *App) mapCreatedTokenWithdrawal(result tokenwithdrawal.CreateResult) (tokenWithdrawalResponse, error) {
	item := result.Withdrawal
	amountText, err := amount.FormatDecimal(item.AmountUnits, a.tokenAsset.Decimals)
	if err != nil {
		return tokenWithdrawalResponse{}, err
	}
	gasETH, err := amount.FormatETH(result.Fee.ReservedFeeWei)
	if err != nil {
		return tokenWithdrawalResponse{}, err
	}
	response := tokenWithdrawalResponse{
		ID: item.ID, UserID: item.UserID, Asset: a.tokenAsset.Symbol, Decimals: a.tokenAsset.Decimals,
		ToAddress: item.ToAddress, Amount: amountText, AmountUnits: item.AmountUnits.String(),
		EstimatedGasWei: result.Fee.ReservedFeeWei.String(), EstimatedGasETH: gasETH,
		GasLimit: result.Fee.GasLimit, MaxFeePerGasWei: result.Fee.MaxFeePerGasWei.String(),
		MaxPriorityFeePerGasWei: result.Fee.MaxPriorityFeePerGasWei.String(), Status: item.Status,
		Created: result.Created, TxHash: item.TxHash, ExplorerURL: a.transactionExplorerURL(item.TxHash),
		Confirmations: item.Confirmations, BlockNumber: item.BlockNumber, ErrorCode: item.ErrorCode,
		ErrorMessage: item.ErrorMessage, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
	if item.Nonce != nil {
		response.Nonce = item.Nonce.String()
	}
	if item.ActualFeeWei != nil {
		response.ActualFeeWei = item.ActualFeeWei.String()
		response.ActualFeeETH, err = amount.FormatETH(item.ActualFeeWei)
		if err != nil {
			return tokenWithdrawalResponse{}, err
		}
	}
	return response, nil
}

// writeTokenWithdrawalError 将 Token 提币领域错误映射为稳定且安全的 HTTP 错误。
func (a *App) writeTokenWithdrawalError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tokenwithdrawal.ErrInvalidRequest):
		writeAPIError(w, http.StatusBadRequest, "INVALID_TOKEN_WITHDRAWAL", err.Error())
	case errors.Is(err, postgres.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "USER_OR_ASSET_NOT_FOUND", "用户、Token 余额或平台钱包不存在")
	case errors.Is(err, postgres.ErrInsufficientBalance):
		writeAPIError(w, http.StatusConflict, "INSUFFICIENT_TOKEN_BALANCE", "Token 可用余额不足")
	case errors.Is(err, postgres.ErrIdempotencyConflict):
		writeAPIError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "幂等标识已用于不同的 Token 提币请求")
	default:
		logging.WithContext(a.logger, ctx).Error("创建 Token 提币失败", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "TOKEN_WITHDRAWAL_UNAVAILABLE", "当前无法创建 Token 提币，请稍后重试")
	}
}
