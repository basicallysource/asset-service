package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Middleware wraps a handler. Chain applies them outermost-first.
type Middleware func(http.Handler) http.Handler

// Chain wraps h so that the first middleware listed sees a request first.
func Chain(h http.Handler, middleware ...Middleware) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}

type contextKey int

const requestIDKey contextKey = iota

// RequestIDHeader carries the id in and back out, so a caller's own id shows
// up in this service's logs.
const RequestIDHeader = "X-Request-Id"

// RequestID gives every request an id, reusing the caller's when it sent one.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestIDFrom returns the id given to this request, if any.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Recover turns a panic into a 500 and a log line rather than a dropped
// connection, and keeps one bad request from taking the process with it.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("panic",
						"panic", v,
						"method", r.Method,
						"path", r.URL.Path,
						"request_id", RequestIDFrom(r.Context()),
						"stack", string(debug.Stack()))
					Error(w, http.StatusInternalServerError, CodeInternal, "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Log records one line per request after it completes.
func Log(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.written,
				"duration_ms", time.Since(started).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()))
		})
	}
}

// MaxBytes caps request bodies. It is set here rather than in the upload
// handler so that no route can ever forget it.
func MaxBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recorder remembers what was written so Log can report it.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// sanitizeID keeps a caller-supplied id only if it is short and printable, so
// it cannot smuggle anything into a log line or a response header.
func sanitizeID(id string) string {
	if len(id) == 0 || len(id) > 64 {
		return ""
	}
	if strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		return ""
	}
	return id
}
