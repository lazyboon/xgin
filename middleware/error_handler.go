package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime/debug"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/lazyboon/xgin"
)

// ErrorHandlerConfig selects the public response for an uncommitted internal failure.
type ErrorHandlerConfig struct {
	// Respond must support concurrent calls and return a complete response.
	// Nil uses an empty HTTP 500 response. The error is for internal diagnostics only.
	Respond func(*gin.Context, error) xgin.IResponse
}

// ErrorHandler writes a final response only when a recorded error has an uncommitted 5xx status.
// Register it outside Recovery. Responses already sent and broken connections are left alone.
// A failing Respond or response renderer is recorded and falls back to an empty HTTP 500.
func ErrorHandler(config ErrorHandlerConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Writer.Written() || c.Writer.Status() < http.StatusInternalServerError || len(c.Errors) == 0 {
			return
		}

		c.Abort()
		clearContentHeaders(c.Writer.Header())
		causes := make([]error, 0, len(c.Errors))
		for _, cause := range c.Errors {
			if errors.Is(cause.Err, syscall.EPIPE) || errors.Is(cause.Err, syscall.ECONNRESET) || errors.Is(cause.Err, http.ErrAbortHandler) {
				return
			}
			causes = append(causes, cause.Err)
		}
		if err := writeErrorResponse(c, config.Respond, errors.Join(causes...)); err != nil {
			_ = c.Error(err)
			if !c.Writer.Written() {
				clearContentHeaders(c.Writer.Header())
				c.Status(http.StatusInternalServerError)
				c.Writer.WriteHeaderNow()
			}
		}
	}
}

// writeErrorResponse isolates callback failures without recursively handling them.
func writeErrorResponse(c *gin.Context, respond func(*gin.Context, error) xgin.IResponse, cause error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = &PanicError{value: value, stack: debug.Stack()}
		}
	}()
	if respond == nil {
		c.Status(http.StatusInternalServerError)
		c.Writer.WriteHeaderNow()
		return nil
	}
	output := respond(c, cause)
	if nilResponse(output) {
		return errors.New("xgin.middleware: error responder returned nil")
	}
	if err := output.Render(c); err != nil {
		return fmt.Errorf("xgin.middleware: render error response: %w", err)
	}
	if !c.Writer.Written() {
		return errors.New("xgin.middleware: error response did not commit")
	}
	return nil
}

// nilResponse detects nil and typed-nil response implementations.
func nilResponse(output xgin.IResponse) bool {
	if output == nil {
		return true
	}
	value := reflect.ValueOf(output)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// clearContentHeaders removes representation metadata before a fallback response.
func clearContentHeaders(headers http.Header) {
	for _, name := range []string{
		"Content-Type", "Content-Length", "Content-Encoding", "Content-Disposition",
		"Content-Range", "Transfer-Encoding", "Trailer", "ETag", "Last-Modified", "Accept-Ranges",
	} {
		headers.Del(name)
	}
}
