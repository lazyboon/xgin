package middleware

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// MaxCaptureLimit defines the maximum buffer size (1MB) to prevent OOM attacks.
const MaxCaptureLimit = 1 << 20

// ---------------------------------------------------------------------------------------------------------------------

// Entity represents the structured access log entry.
type Entity struct {
	Method    string          `json:"method,omitempty"`
	Path      string          `json:"path,omitempty"`
	ClientIP  string          `json:"client_ip,omitempty"`
	Request   *RequestEntity  `json:"request,omitempty"`
	Response  *ResponseEntity `json:"response,omitempty"`
	Latency   time.Duration   `json:"latency,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
}

// RequestEntity holds captured request metadata and body.
type RequestEntity struct {
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// ResponseEntity holds captured response metadata and body.
type ResponseEntity struct {
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
	Status int         `json:"status,omitempty"`
}

// ---------------------------------------------------------------------------------------------------------------------

// AccessLogBaseOptions defines capturing policies for a single request.
type AccessLogBaseOptions struct {
	RequestHeader  bool
	RequestBody    bool
	ResponseHeader bool
	ResponseBody   bool
	MaxBodyLength  int // Maximum bytes to record in the log entity
}

// AccessLogOptions handles global and path-specific logging policies.
type AccessLogOptions struct {
	BaseOption   *AccessLogBaseOptions
	SkipPaths    []string                         // Format: "GET:/ping"
	SpecificPath map[string]*AccessLogBaseOptions // Key: "POST:/login"
}

// NewAccessLog initializes the access logging middleware with the provided handler and options.
func NewAccessLog(handler func(entity *Entity), opts ...*AccessLogOptions) gin.HandlerFunc {
	if handler == nil {
		panic("access log handler must not be nil")
	}

	conf := &AccessLogOptions{
		BaseOption:   &AccessLogBaseOptions{MaxBodyLength: 4096},
		SpecificPath: make(map[string]*AccessLogBaseOptions),
	}

	skipSet := make(map[string]struct{})
	engine := &accessLogEngine{handler: handler, conf: conf, skipSet: skipSet}

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if opt.BaseOption != nil {
			conf.BaseOption = opt.BaseOption
		}
		for _, p := range opt.SkipPaths {
			if key, err := engine.safeNormalizeKey(p); err == nil {
				skipSet[key] = struct{}{}
			}
		}
		for k, v := range opt.SpecificPath {
			if key, err := engine.safeNormalizeKey(k); err == nil {
				conf.SpecificPath[key] = v
			}
		}
	}

	return engine.Handle
}

// ---------------------------------------------------------------------------------------------------------------------

type accessLogEngine struct {
	handler func(entity *Entity)
	conf    *AccessLogOptions
	skipSet map[string]struct{}
}

// Handle implements the gin.HandlerFunc logic.
func (e *accessLogEngine) Handle(c *gin.Context) {
	start := time.Now()

	// 1. Route Matching: Prefer FullPath for parameterized routes like /user/:id
	routePath := c.FullPath()
	if routePath == "" {
		routePath = c.Request.URL.Path
	}
	key := e.normalizeKey(c.Request.Method, routePath)

	if _, ok := e.skipSet[key]; ok {
		c.Next()
		return
	}

	opt := e.conf.BaseOption
	if v, ok := e.conf.SpecificPath[key]; ok {
		opt = v
	}

	// 2. Traceability: Extract or generate Request ID
	requestID := ""
	if v, ok := c.Get("X-Request-ID"); ok && v != nil {
		requestID = fmt.Sprintf("%v", v)
	}
	if requestID == "" {
		requestID = c.GetHeader("X-Request-ID")
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}

	// 3. Request Capture: Safely read body with overflow protection
	var reqEntity *RequestEntity
	if opt.RequestHeader || opt.RequestBody {
		reqEntity = &RequestEntity{}
		if opt.RequestHeader {
			reqEntity.Header = c.Request.Header.Clone()
		}

		if opt.RequestBody && c.Request.Body != nil {
			// Read Max+1 bytes to detect if body exceeds safety limit
			body, err := io.ReadAll(io.LimitReader(c.Request.Body, MaxCaptureLimit+1))
			if err == nil {
				if len(body) <= MaxCaptureLimit {
					// Restore Body and metadata for subsequent handlers/retries
					c.Request.Body = io.NopCloser(bytes.NewReader(body))
					c.Request.ContentLength = int64(len(body))
					c.Request.GetBody = func() (io.ReadCloser, error) {
						return io.NopCloser(bytes.NewReader(body)), nil
					}
					reqEntity.Body = e.truncate(body, opt.MaxBodyLength)
				} else {
					reqEntity.Body = []byte("[body too large]")
					// Restore original body if too large for capture
					c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
				}
			}
		}
	}

	// 4. Response Interception
	w := &bodyWriter{
		ResponseWriter: c.Writer,
		engine:         e,
		maxLen:         opt.MaxBodyLength,
	}
	c.Writer = w

	c.Next()

	// 5. Response Capture
	var resEntity *ResponseEntity
	if opt.ResponseHeader || opt.ResponseBody {
		resEntity = &ResponseEntity{
			Status: w.Status(),
			Header: w.Header().Clone(),
		}
		if opt.ResponseBody && w.buf != nil && w.buf.Len() > 0 {
			if !w.isBinary {
				resEntity.Body = append([]byte(nil), w.buf.Bytes()...)
			} else {
				resEntity.Body = []byte("[binary content]")
			}
		}
	}

	// 6. Safe Callback: Protect against log handler panics
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[AccessLog Panic] id=%s path=%s err=%v\n", requestID, c.Request.URL.Path, r)
		}
	}()

	e.handler(&Entity{
		Method:    c.Request.Method,
		Path:      c.Request.URL.RequestURI(),
		ClientIP:  c.ClientIP(),
		Request:   reqEntity,
		Response:  resEntity,
		Latency:   time.Since(start),
		RequestID: requestID,
	})
}

func (e *accessLogEngine) truncate(src []byte, max int) []byte {
	if max <= 0 || len(src) <= max {
		return append([]byte(nil), src...)
	}
	dst := make([]byte, max+15)
	copy(dst, src[:max])
	copy(dst[max:], "... [truncated]")
	return dst
}

func (e *accessLogEngine) normalizeKey(m, p string) string {
	return strings.ToUpper(strings.TrimSpace(m)) + ":" + strings.TrimSpace(p)
}

func (e *accessLogEngine) safeNormalizeKey(s string) (string, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("invalid path format")
	}
	return e.normalizeKey(parts[0], parts[1]), nil
}

func (e *accessLogEngine) isBinary(ct string) bool {
	if ct == "" {
		return false // Assume text for debug friendliness if Content-Type is missing
	}
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "image/") ||
		strings.Contains(ct, "video/") ||
		strings.Contains(ct, "audio/") ||
		strings.Contains(ct, "octet-stream") ||
		strings.Contains(ct, "zip") ||
		strings.Contains(ct, "protobuf") ||
		strings.Contains(ct, "event-stream")
}

// ---------------------------------------------------------------------------------------------------------------------

type bodyWriter struct {
	gin.ResponseWriter
	engine *accessLogEngine

	buf       *bytes.Buffer
	maxLen    int
	isBinary  bool
	checkedCT bool
}

// checkCT caches the binary status based on Content-Type to avoid redundant lookups.
func (w *bodyWriter) checkCT() {
	if w.checkedCT {
		return
	}
	w.isBinary = w.engine.isBinary(w.Header().Get("Content-Type"))
	w.checkedCT = true
}

func (w *bodyWriter) capture(p []byte) {
	if w.buf == nil {
		w.buf = bytes.NewBuffer(make([]byte, 0, 512))
	}
	if w.maxLen > 0 && w.buf.Len() < w.maxLen {
		remain := w.maxLen - w.buf.Len()
		if len(p) > remain {
			w.buf.Write(p[:remain])
			w.buf.WriteString("... [truncated]")
		} else {
			w.buf.Write(p)
		}
	} else if w.maxLen == 0 {
		w.buf.Write(p)
	}
}

func (w *bodyWriter) WriteHeader(code int) {
	w.checkCT()
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodyWriter) Write(p []byte) (int, error) {
	w.checkCT()
	if !w.isBinary {
		w.capture(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *bodyWriter) WriteString(s string) (int, error) {
	w.checkCT()
	if !w.isBinary {
		w.capture([]byte(s))
	}
	if ws, ok := w.ResponseWriter.(interface {
		WriteString(string) (int, error)
	}); ok {
		return ws.WriteString(s)
	}
	return w.ResponseWriter.Write([]byte(s))
}

// ReadFrom solves the zero-copy bypass and infinite recursion issues.
func (w *bodyWriter) ReadFrom(r io.Reader) (int64, error) {
	w.checkCT()

	if w.isBinary {
		if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
			return rf.ReadFrom(r)
		}
		return io.Copy(w.ResponseWriter, r)
	}

	// Use TeeReader to capture data while it's piped to the actual writer.
	// We use an anonymous struct to hide ReadFrom to prevent io.Copy from recursing.
	tee := io.TeeReader(r, writerFunc(func(p []byte) (int, error) {
		w.capture(p)
		return len(p), nil
	}))

	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, tee)
}

func (w *bodyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *bodyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *bodyWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) {
	return f(p)
}
