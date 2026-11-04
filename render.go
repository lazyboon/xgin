package xgin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	ginrender "github.com/gin-gonic/gin/render"
	"golang.org/x/net/http/httpguts"
)

// Rendering errors are public sentinels; wrapped causes remain available through errors.Is/As.
var (
	ErrInvalidRender = errors.New("xgin: invalid render")

	ErrRenderPanic = errors.New("xgin: render panicked")
)

// Render adapts any finite Gin renderer, buffering encoding before commitment.
// A nil Renderer commits only the status and headers. File and streaming outputs
// should use FileResponse or a custom IResponse instead.
type Render struct {
	HTTPStatus int              // Zero uses the renderer status, or 200 if none is selected.
	Header     http.Header      // Additional headers; framing is managed by this implementation.
	Renderer   ginrender.Render // Nil commits a bodyless response.
}

var _ IResponse = Render{}

// Render encodes a finite body, validates metadata and commits exactly once.
func (r Render) Render(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return fmt.Errorf("%w: missing request context", ErrInvalidRender)
	}
	if c.Writer == nil {
		return fmt.Errorf("%w: missing response writer", ErrInvalidRender)
	}
	if c.Writer.Written() {
		return fmt.Errorf("%w: response already written", ErrInvalidRender)
	}
	if err := validateRenderHeaders(r.Header); err != nil {
		return err
	}

	buffer := renderBuffer{
		ResponseWriter: c.Writer,
		header:         make(http.Header),
		defaultStatus:  r.HTTPStatus,
	}
	applyRenderHeaders(buffer.header, r.Header)

	if r.Renderer != nil {
		if isNilInterface(r.Renderer) {
			return fmt.Errorf("%w: typed nil renderer", ErrInvalidRender)
		}
		if err := r.rendererFor(c.Request).Render(&buffer); err != nil {
			return err
		}
	}

	status, err := r.status(buffer.status)
	if err != nil {
		return err
	}

	if !bodyAllowedForStatus(status) && buffer.Len() > 0 {
		return fmt.Errorf(
			"%w: status %d does not allow a response body",
			ErrInvalidRender,
			status,
		)
	}

	buffer.header.Del("Transfer-Encoding")

	if bodyAllowedForStatus(status) {
		buffer.header.Set("Content-Length", strconv.Itoa(buffer.Len()))
	} else {
		buffer.header.Del("Content-Length")
	}

	if err = validateProducedHeaders(buffer.header); err != nil {
		return err
	}

	applyRenderHeaders(c.Writer.Header(), buffer.header)

	c.Status(status)
	c.Writer.WriteHeaderNow()

	if c.Request.Method == http.MethodHead || !bodyAllowedForStatus(status) || buffer.Len() == 0 {
		return nil
	}

	_, err = c.Writer.Write(buffer.Bytes())
	return err
}

// rendererFor supplies the current request to redirect renderers without modifying them.
func (r Render) rendererFor(request *http.Request) ginrender.Render {
	switch renderer := r.Renderer.(type) {
	case ginrender.Redirect:
		renderer.Request = r.redirectRequest(renderer.Request, request)
		return renderer
	case *ginrender.Redirect:
		copyOf := *renderer
		copyOf.Request = r.redirectRequest(copyOf.Request, request)
		return copyOf
	default:
		return r.Renderer
	}
}

// redirectRequest computes a HEAD representation as GET before discarding its body.
// This keeps an emitted Content-Length equal to the equivalent GET response.
func (r Render) redirectRequest(selected, current *http.Request) *http.Request {
	if selected == nil {
		selected = current
	}
	if current.Method == http.MethodHead && selected.Method == http.MethodHead {
		copyOf := new(http.Request)
		*copyOf = *selected
		copyOf.Method = http.MethodGet
		return copyOf
	}
	return selected
}

// status resolves explicit and renderer statuses and rejects conflicting final statuses.
func (r Render) status(renderedStatus int) (int, error) {
	if r.HTTPStatus != 0 && renderedStatus != 0 && r.HTTPStatus != renderedStatus {
		return 0, fmt.Errorf(
			"%w: response status %d conflicts with renderer status %d",
			ErrInvalidRender,
			r.HTTPStatus,
			renderedStatus,
		)
	}

	status := r.HTTPStatus
	if status == 0 {
		status = renderedStatus
	}
	if status == 0 {
		status = http.StatusOK
	}

	if status < http.StatusOK || status > 599 {
		return 0, fmt.Errorf("%w: invalid status %d", ErrInvalidRender, status)
	}

	return status, nil
}

// FileResponse streams an authorized regular file with Range, HEAD and conditional
// request support. Name selects attachment disposition; Path must be authorized by
// the application. ServeContent-generated failures are reflected in the HTTP status.
type FileResponse struct {
	Path   string      // Server path authorized by the application.
	Name   string      // Optional attachment filename; it does not change Path.
	Header http.Header // Extra headers; HTTP manages framing and content length.
}

var _ IResponse = FileResponse{}

// File returns a streamed, application-authorized file response.
func File(path string) FileResponse { return FileResponse{Path: path} }

// Download streams a file as an attachment using the supplied download name.
func Download(path, name string) FileResponse { return FileResponse{Path: path, Name: name} }

// Render opens and streams a file without retaining its content in memory.
func (r FileResponse) Render(c *gin.Context) (err error) {
	if c == nil || c.Request == nil {
		return fmt.Errorf("%w: missing request context", ErrInvalidRender)
	}
	if c.Writer == nil {
		return fmt.Errorf("%w: missing response writer", ErrInvalidRender)
	}
	if c.Writer.Written() {
		return fmt.Errorf("%w: response already written", ErrInvalidRender)
	}
	if err = validateRenderHeaders(r.Header); err != nil {
		return err
	}

	file, err := os.Open(r.Path)
	if err != nil {
		return fmt.Errorf("xgin: open render file: %w", err)
	}

	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("xgin: close render file: %w", closeErr))
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("xgin: stat render file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: render file is not a regular file", ErrInvalidRender)
	}

	applyRenderHeaders(c.Writer.Header(), r.Header)

	if r.Name != "" {
		c.Header(
			"Content-Disposition",
			mime.FormatMediaType(
				"attachment",
				map[string]string{"filename": r.Name},
			),
		)
	}

	writer := fileRenderWriter{
		ResponseWriter: c.Writer,
	}

	http.ServeContent(&writer, c.Request, info.Name(), info.ModTime(), file)

	if writer.err == nil {
		c.Writer.WriteHeaderNow()
	}

	return writer.err
}

// renderBuffer implements both HTTP and Gin writers without committing the live connection.
// Gin-specific methods allow HTML execution to use the same finite-response pipeline.
type renderBuffer struct {
	gin.ResponseWriter
	bytes.Buffer
	header        http.Header
	status        int
	defaultStatus int
	committed     bool
}

// Header returns the private header map used during encoding.
func (b *renderBuffer) Header() http.Header {
	return b.header
}

// WriteHeader records the first requested status without writing to the connection.
func (b *renderBuffer) WriteHeader(status int) {
	if !b.committed && b.status == 0 {
		b.status = status
	}
}

// Write buffers bytes and records a simulated commitment for Gin's renderer API.
func (b *renderBuffer) Write(data []byte) (int, error) {
	b.WriteHeaderNow()
	return b.Buffer.Write(data)
}

// WriteString buffers literal text without forwarding it to the live connection.
func (b *renderBuffer) WriteString(data string) (int, error) {
	b.WriteHeaderNow()
	return b.Buffer.WriteString(data)
}

// WriteHeaderNow marks the private response as committed, not the real connection.
func (b *renderBuffer) WriteHeaderNow() {
	b.committed = true
}

// Status returns the renderer status, the explicit outer status, or Gin's default.
func (b *renderBuffer) Status() int {
	if b.status == 0 {
		if b.defaultStatus != 0 {
			return b.defaultStatus
		}
		return http.StatusOK
	}
	return b.status
}

// Size follows Gin's -1 convention until a simulated commitment has occurred.
func (b *renderBuffer) Size() int {
	if !b.committed {
		return -1
	}
	return b.Len()
}

// Written reports simulated commitment to Gin's rendering methods.
func (b *renderBuffer) Written() bool { return b.committed }

// Flush never flushes the live connection during finite response encoding.
func (b *renderBuffer) Flush() { b.WriteHeaderNow() }

// Hijack is unavailable during finite response encoding.
func (b *renderBuffer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// Pusher is unavailable until the real response has been committed.
func (b *renderBuffer) Pusher() http.Pusher { return nil }

// fileRenderWriter records stream write failures and intentionally hides ReaderFrom.
type fileRenderWriter struct {
	http.ResponseWriter
	err error
}

// Write forwards bytes and preserves the first transport error.
func (w *fileRenderWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)

	if w.err == nil {
		w.err = err
	}

	return n, err
}

// bodyAllowedForStatus excludes final statuses with no response body.
func bodyAllowedForStatus(status int) bool {
	switch status {
	case http.StatusNoContent,
		http.StatusResetContent,
		http.StatusNotModified:
		return false

	default:
		return true
	}
}

// validateRenderHeaders reserves framing headers for the response implementation.
func validateRenderHeaders(headers http.Header) error {
	if err := validateHeaderSyntax(headers); err != nil {
		return err
	}

	for name := range headers {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Transfer-Encoding") {
			return fmt.Errorf(
				"%w: reserved render header %q",
				ErrInvalidRender,
				name,
			)
		}
	}

	return nil
}

// validateProducedHeaders checks renderer-generated headers before commitment.
func validateProducedHeaders(headers http.Header) error {
	return validateHeaderSyntax(headers)
}

// validateHeaderSyntax rejects invalid bytes and ambiguously cased duplicate names.
func validateHeaderSyntax(headers http.Header) error {
	seen := make(map[string]struct{}, len(headers))
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("%w: duplicate header name %q", ErrInvalidRender, name)
		}
		seen[canonical] = struct{}{}
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf(
				"%w: invalid header name",
				ErrInvalidRender,
			)
		}

		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf(
					"%w: invalid header value",
					ErrInvalidRender,
				)
			}
		}
	}

	return nil
}

// applyRenderHeaders copies header slices to prevent cross-request aliases.
func applyRenderHeaders(target, source http.Header) {
	for name, values := range source {
		target[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}
