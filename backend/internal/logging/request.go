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

func NewRequestID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate request ID")
	}
	return hex.EncodeToString(value), nil
}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func RequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func WithContext(logger *slog.Logger, ctx context.Context) *slog.Logger {
	if requestID := RequestID(ctx); requestID != "" {
		return logger.With("request_id", requestID)
	}
	return logger
}
