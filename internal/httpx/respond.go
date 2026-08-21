// Package httpx holds the HTTP plumbing every route shares: middleware, and
// one way to write a response.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorBody is the shape of every failure this service returns. Code is stable
// and meant to be branched on; Message is for a human reading a log.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Error codes. Anything a caller might handle differently gets its own.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeTooLarge     = "too_large"
	CodeUnsupported  = "unsupported_type"
	CodeRateLimited  = "rate_limited"
	CodeInternal     = "internal"
)

// JSON writes v as the response body.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already gone, so this can only be recorded.
		slog.Error("write response", "error", err)
	}
}

// Error writes a failure. Messages are safe to show a caller: they say what
// the caller did, never what the service is made of.
func Error(w http.ResponseWriter, status int, code, message string) {
	var body ErrorBody
	body.Error.Code = code
	body.Error.Message = message
	JSON(w, status, body)
}
