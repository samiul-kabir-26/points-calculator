// Package apperr defines errors that carry an HTTP status and a stable,
// machine-readable code for API clients.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Error struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is surfaced as a Retry-After header on 429 responses.
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func New(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func BadRequest(format string, args ...any) *Error {
	return New(http.StatusBadRequest, "invalid_request", format, args...)
}

func NotFound(code, format string, args ...any) *Error {
	return New(http.StatusNotFound, code, format, args...)
}

func Conflict(code, format string, args ...any) *Error {
	return New(http.StatusConflict, code, format, args...)
}

// As extracts an *Error from err, if there is one.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
