package app

import (
	"net/http"
	"time"

	"github.com/xiaoqi/mini-custody/backend/internal/logging"
)

// responseRecorder 记录 HTTP 响应状态供访问日志使用。
type responseRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader 记录并写出第一个 HTTP 状态码。
func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write 在未显式写状态时记录默认的 200 状态。
func (w *responseRecorder) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(value)
}

// requestContext 为每个请求生成 request_id，并写入上下文、响应头和结构化日志。
func (a *App) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := logging.NewRequestID()
		if err != nil {
			requestID = "request-id-unavailable"
		}
		ctx := logging.WithRequestID(r.Context(), requestID)
		w.Header().Set("X-Request-ID", requestID)
		recorder := &responseRecorder{ResponseWriter: w}
		startedAt := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		logging.WithContext(a.logger, ctx).Info(
			"HTTP 请求处理完成",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	})
}
