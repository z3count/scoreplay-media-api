package handler

import (
	"encoding/json"
	"net/http"
)

// writeJSON serializes data as JSON and writes it to the response with the
// given HTTP status code.
//
// Sets Content-Type to application/json. If JSON marshaling fails (which should
// not happen with well-formed structs), it falls back to a plain-text 500 error.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// writeError writes a standardized JSON error response.
//
// All error responses follow the same envelope:
//
//	{"error": {"code": "ERROR_CODE", "message": "human-readable message"}}
//
// This consistency makes it easy for API clients to parse errors programmatically
// (using the code) and display them to users (using the message).
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
