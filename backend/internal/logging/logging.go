package logging

import (
	"io"
	"log/slog"
)

func New(output io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
