package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

const maxBodyBytes = 1 << 20

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders err as a JSON error. Errors that aren't *apperr.Error, or a
// store error with an obvious HTTP meaning, are logged and reported as a 500.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	ae, ok := apperr.As(err)
	if !ok {
		switch {
		case errors.Is(err, store.ErrNotFound):
			ae = apperr.NotFound("not_found", "resource not found")
		case errors.Is(err, store.ErrConflict):
			ae = apperr.Conflict("already_exists", "a resource with that slug already exists")
		default:
			s.log.ErrorContext(r.Context(), "internal error", "err", err, "path", r.URL.Path)
			ae = apperr.New(http.StatusInternalServerError, "internal_error", "internal server error")
		}
	}
	if ae.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ae.RetryAfter.Seconds()))))
	}
	writeJSON(w, ae.Status, errorBody{Error: errorDetail{
		Code: ae.Code, Message: ae.Message, RequestID: requestIDFrom(r.Context()),
	}})
}

// decodeJSON decodes a single JSON object from the body into dst, rejecting unknown fields.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return apperr.BadRequest("request body is empty")
		case errors.As(err, &maxErr):
			return apperr.New(http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds %d bytes", maxBodyBytes)
		default:
			return apperr.BadRequest("malformed JSON body: %v", err)
		}
	}
	if dec.More() {
		return apperr.BadRequest("request body must contain a single JSON object")
	}
	return nil
}

type page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

const (
	defaultPageSize = 50
	maxPageSize     = 500
)

func parseLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPageSize {
		return 0, apperr.BadRequest("limit must be an integer between 1 and %d", maxPageSize)
	}
	return n, nil
}

func parseTime(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.BadRequest("%s must be an RFC 3339 timestamp", name)
	}
	return &t, nil
}

func queryPtr(r *http.Request, name string) *string {
	if v := r.URL.Query().Get(name); v != "" {
		return &v
	}
	return nil
}

func parseOrder(r *http.Request) (ascending bool, err error) {
	switch r.URL.Query().Get("order") {
	case "", "desc":
		return false, nil
	case "asc":
		return true, nil
	}
	return false, apperr.BadRequest("order must be asc or desc")
}

func invalidCursor() error { return apperr.BadRequest("cursor is invalid") }

func cursorString(v any) *string {
	s := fmt.Sprint(v)
	return &s
}
