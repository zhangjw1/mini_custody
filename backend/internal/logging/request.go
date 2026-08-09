package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
)

type requestIDKey struct{}

// NewRequestID 生成随机请求标识。
func NewRequestID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("生成请求标识失败")
	}
	return hex.EncodeToString(value), nil
}

// WithRequestID 将请求标识写入上下文。
func WithRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// RequestID 从上下文读取请求标识。
func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

// WithContext 将上下文中的请求标识附加到 Logger。
func WithContext(logger *slog.Logger, ctx context.Context) *slog.Logger {
	if requestID := RequestID(ctx); requestID != "" {
		return logger.With("request_id", requestID)
	}
	return logger
}
