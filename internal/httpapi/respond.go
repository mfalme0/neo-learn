// Package httpapi exposes the Neo Learn REST API.
//
// This layer is responsible for transport concerns only: parsing requests,
// enforcing authentication, mapping domain errors to status codes. All learning
// behaviour lives in internal/learning, which is why an SMS or STK transport
// can be added without touching it.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// errorResponse is the single error shape every endpoint returns, so a client
// never has to branch on which endpoint failed.
type errorResponse struct {
	Error   string            `json:"error"`
	Code    string            `json:"code"`
	Details map[string]string `json:"details,omitempty"`
}

// writeJSON serialises v with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Defence in depth against response splitting if a value is ever
	// reflected into a header.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this cannot be reported as an
		// error response. Log it: it means a handler returned something
		// unserializable.
		slog.Error("write json response", "error", err)
	}
}

// writeError emits an errorResponse.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: message, Code: code})
}

// writeErrorDetails emits an errorResponse with per-field messages, used for
// validation failures.
func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]string) {
	writeJSON(w, status, errorResponse{Error: message, Code: code, Details: details})
}

// decodeJSON reads a JSON request body.
//
// It rejects unknown fields: a client sending a typo like "lessonid" should
// hear about it, not silently get a default.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	const maxBody = 1 << 20 // 1 MiB

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError

		switch {
		case errors.As(err, &syntaxErr):
			return &RequestError{
				Code:    "invalid_json",
				Message: "request body is not valid JSON",
				Details: map[string]string{"offset": syntaxErr.Error()},
			}
		case errors.As(err, &typeErr):
			return &RequestError{
				Code:    "invalid_field_type",
				Message: "a field has the wrong type",
				Details: map[string]string{typeErr.Field: typeErr.Type.String()},
			}
		case errors.Is(err, http.ErrBodyReadAfterClose), err.Error() == "EOF":
			return &RequestError{Code: "empty_body", Message: "request body is required"}
		case isMaxBytesError(err):
			return &RequestError{Code: "body_too_large", Message: "request body exceeds 1 MiB"}
		}

		return &RequestError{Code: "invalid_body", Message: "could not decode request body"}
	}

	// A second value in the body means the client sent something like
	// `{}{}`. That is always a bug worth surfacing.
	if dec.More() {
		return &RequestError{Code: "invalid_body", Message: "request body must contain exactly one JSON object"}
	}

	return nil
}

func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// RequestError is a client-side error carrying a machine-readable code.
type RequestError struct {
	Code    string
	Message string
	Details map[string]string
}

func (e *RequestError) Error() string { return e.Message }

// internalError logs an unexpected failure and returns a generic message.
//
// The underlying error is never returned to the client: database and Redis
// error strings leak schema and connection details.
func internalError(w http.ResponseWriter, r *http.Request, op string, err error) {
	slog.ErrorContext(r.Context(), "request failed",
		"op", op,
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	writeError(w, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
}
