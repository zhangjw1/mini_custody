package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestRequestIDRoundTripAndStructuredLog 验证请求标识可以写入上下文和结构化日志。
func TestRequestIDRoundTripAndStructuredLog(t *testing.T) {
	requestID, err := NewRequestID()
	if err != nil {
		t.Fatalf("NewRequestID() error = %v", err)
	}
	if len(requestID) != 32 {
		t.Fatalf("request ID length = %d, want 32", len(requestID))
	}

	ctx := WithRequestID(context.Background(), requestID)
	if got := RequestID(ctx); got != requestID {
		t.Fatalf("RequestID() = %q, want %q", got, requestID)
	}

	var output bytes.Buffer
	WithContext(New(&output, slog.LevelInfo), ctx).Info("收到请求")
	if got := output.String(); !strings.Contains(got, `"request_id":"`+requestID+`"`) {
		t.Fatalf("structured log does not contain request ID: %s", got)
	}
}
