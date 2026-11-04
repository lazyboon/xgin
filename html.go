package xgin

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// htmlRenderer obtains templates through Gin without exposing the engine or
// committing the live response before template execution succeeds.
type htmlRenderer struct {
	context *gin.Context
	name    string
	data    any
	status  int
}

// Render executes a registered template against the finite response buffer.
func (r htmlRenderer) Render(writer http.ResponseWriter) error {
	buffer, ok := writer.(*renderBuffer)
	if !ok {
		return fmt.Errorf("%w: HTML requires the response buffer", ErrInvalidRender)
	}
	copyOf := r.context.Copy()
	copyOf.Writer = buffer
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	copyOf.HTML(status, r.name, r.data)
	if len(copyOf.Errors) > 0 {
		return copyOf.Errors.Last().Err
	}
	return nil
}

// WriteContentType sets the HTML media type without executing a template.
func (r htmlRenderer) WriteContentType(writer http.ResponseWriter) {
	if writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
}
