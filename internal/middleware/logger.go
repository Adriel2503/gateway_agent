package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger logs HTTP requests: method, path, status, duration.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wr := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wr, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wr.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
