package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxBodyBytes = 64 << 10

// Error is a client-facing error with an HTTP status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// DecodeJSON decodes exactly one JSON object into dst.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) *Error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &Error{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &Error{http.StatusBadRequest, "invalid JSON body"}
	}

	// Ensure the body contains a single JSON object (EOF expected)
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &Error{http.StatusBadRequest, "body must contain a single JSON object"}
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}
