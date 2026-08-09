package logging

import (
	"io"
	"log/slog"
)

// New 创建输出 JSON 结构化日志的 Logger。
func New(output io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
