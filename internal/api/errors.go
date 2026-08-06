package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// errorBody is the single error shape every endpoint returns. Message names
// the cause; an empty list is never used to stand in for a failure.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, format string, args ...any) {
	writeJSON(w, status, errorBody{Error: code, Message: fmt.Sprintf(format, args...)})
}

// storeErr maps a store failure to a status. ErrNotFound is a 404; anything
// else is a real 500 naming the operation that failed.
func storeErr(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "%s: not found", op)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeErr(w, http.StatusConflict, "conflict", "%s: conflicting concurrent update", op)
		return
	}
	writeErr(w, http.StatusInternalServerError, "store_error", "%s: %v", op, err)
}

// pathID parses an int64 path value, writing a 400 and reporting false when it
// is not one.
func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "path parameter %q must be a positive integer, got %q", name, raw)
		return 0, false
	}
	return id, true
}

// intParam reads a bounded integer query parameter.
func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be an integer, got %q", name, raw)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("query parameter %q must be between %d and %d, got %d", name, min, max, n)
	}
	return n, nil
}

// int64Param reads an int64 query parameter with a floor.
func int64Param(r *http.Request, name string, def, min int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be an integer, got %q", name, raw)
	}
	if n < min {
		return 0, fmt.Errorf("query parameter %q must be >= %d, got %d", name, min, n)
	}
	return n, nil
}
