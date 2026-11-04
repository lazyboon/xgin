package xgin

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// PanicError preserves an internal panic and its original stack, never a public message.
type PanicError struct {
	kind  error
	value any
	stack []byte
}

// Error describes the internal panic category and recovered value.
func (e *PanicError) Error() string { return fmt.Sprintf("%v: %v", e.kind, e.value) }

// Unwrap preserves both the panic category and an original error payload.
func (e *PanicError) Unwrap() error {
	if cause, ok := e.value.(error); ok {
		return errors.Join(e.kind, cause)
	}
	return e.kind
}

// StackTrace returns the stack captured at the failure site.
func (e *PanicError) StackTrace() string { return string(e.stack) }

// DefaultRequestErrorMapper returns only an HTTP status, without decoder details.
type DefaultRequestErrorMapper struct{}

var _ IRequestErrorMapper = DefaultRequestErrorMapper{}

// MapRequestError maps a failed request to a bodyless response.
func (DefaultRequestErrorMapper) MapRequestError(_ *gin.Context, err error) IResponse {
	response := Empty(DefaultRequestStatus(err))
	if response.HTTPStatus >= http.StatusInternalServerError {
		return response.WithError(err)
	}
	return response
}

// DefaultRequestStatus classifies binding and validation failures without exposing details.
func DefaultRequestStatus(err error) int {
	var limit *http.MaxBytesError
	switch {
	case errors.Is(err, ErrValidationPanic), errors.Is(err, ErrInvalidBindTarget), errors.Is(err, ErrMaxDepth):
		return http.StatusInternalServerError
	case errors.As(err, &limit):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrMissingContentType), errors.Is(err, ErrUnsupportedContentType):
		return http.StatusUnsupportedMediaType
	default:
		return http.StatusBadRequest
	}
}
