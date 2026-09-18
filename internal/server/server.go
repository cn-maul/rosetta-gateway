package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request_id"

func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Middleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := generateRequestID()
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		r = r.WithContext(ctx)

		w.Header().Set("X-Request-Id", reqID)

		wrapped := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
			"remote", r.RemoteAddr,
		)
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

func RequestSizeLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func AdminAuth(next http.Handler, adminToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			if token == adminToken {
				next.ServeHTTP(w, r)
				return
			}
		}

		http.Error(w, `{"error":{"message":"unauthorized","type":"authentication_error"}}`, http.StatusUnauthorized)
	})
}

func Recovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered",
					"error", fmt.Sprintf("%v", rec),
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				http.Error(w, `{"error":{"message":"internal error","type":"internal_error"}}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
