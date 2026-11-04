package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// PanicError preserves an internal panic and its stack for server logs.
// Applications must not send its Error or StackTrace output to clients.
type PanicError struct {
	value any
	stack []byte
}

// Error describes the panic for internal diagnostics.
func (e *PanicError) Error() string {
	return fmt.Sprintf("xgin.middleware: panic: %v", e.value)
}

// Unwrap preserves an error value used as the panic payload.
func (e *PanicError) Unwrap() error {
	err, _ := e.value.(error)
	return err
}

// StackTrace returns the stack captured when the panic was recovered.
func (e *PanicError) StackTrace() string {
	return string(e.stack)
}

// Recovery records handler panics without committing an error response.
// Install ErrorHandler outside it to select the public error representation.
// Gin retains its special broken connection handling, which does not write another response.
func Recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, value any) {
		_ = c.Error(&PanicError{value: value, stack: debug.Stack()})
		if !c.Writer.Written() {
			c.Status(http.StatusInternalServerError)
		}
		c.Abort()
	})
}
