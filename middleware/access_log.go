package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

const (
	defaultLogBodyBytes = 4096
	maxLogBodyBytes     = 1 << 20
)

// AccessLogConfig controls structured request logs and optional bounded payload capture.
// Callbacks must support concurrent calls and must not retain the Gin context.
// Only complete JSON and URL-encoded forms are logged; other payloads are omitted.
type AccessLogConfig struct {
	Logger          *slog.Logger
	GetLogger       func(*gin.Context) *slog.Logger // Non-nil results take precedence over Logger.
	Skip            func(*gin.Context) bool
	RequestHeaders  bool
	ResponseHeaders bool
	RequestBody     bool
	ResponseBody    bool
	MaxBodyBytes    int      // Zero uses 4096; the allowed range is 1 through 1 MiB.
	RedactHeaders   []string // Additional sensitive header names; defaults cannot be removed.
	RedactKeys      []string // Additional sensitive JSON/form keys; matching ignores case, '_' and '-'.
}

type accessLogger struct {
	config  AccessLogConfig
	headers map[string]bool
	keys    map[string]bool
}

// AccessLog records request start and completion using slog.
// Header and body capture are disabled by default. URLs include only the path, never the query.
// Request bodies are observed as the application reads them; logging never reads ahead.
// Invalid limits or redaction names panic during construction. Configuration slices are copied.
func AccessLog(config AccessLogConfig) gin.HandlerFunc {
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultLogBodyBytes
	}
	if config.MaxBodyBytes < 1 || config.MaxBodyBytes > maxLogBodyBytes {
		panic("xgin.middleware: log body limit must be between 1 and 1048576")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	logger := &accessLogger{
		config:  config,
		headers: defaultRedactHeaders(),
		keys:    defaultRedactKeys(),
	}
	for _, name := range config.RedactHeaders {
		if !httpguts.ValidHeaderFieldName(name) {
			panic("xgin.middleware: invalid sensitive header name")
		}
		logger.headers[strings.ToLower(name)] = true
	}
	for _, key := range config.RedactKeys {
		if key == "" {
			panic("xgin.middleware: sensitive body key cannot be empty")
		}
		logger.keys[normalizeRedactKey(key)] = true
	}
	logger.config.RedactHeaders = nil
	logger.config.RedactKeys = nil
	return logger.handle
}

// IStackTracer exposes diagnostic stacks without depending on a particular error type.
type IStackTracer interface {
	StackTrace() string
}

// handle observes the request lifecycle without changing payload consumption.
func (l *accessLogger) handle(c *gin.Context) {
	if l.config.Skip != nil && l.config.Skip(c) {
		c.Next()
		return
	}
	startedAt := time.Now()
	logger := l.config.Logger
	if l.config.GetLogger != nil {
		if selected := l.config.GetLogger(c); selected != nil {
			logger = selected
		}
	}
	method, path := c.Request.Method, c.Request.URL.Path
	var requestHeaders http.Header
	if l.config.RequestHeaders || l.config.RequestBody {
		requestHeaders = c.Request.Header.Clone()
	}
	requestLength := c.Request.ContentLength
	attrs := []slog.Attr{
		slog.String("method", method),
		slog.String("path", path),
		slog.String("request_id", RequestIDFromContext(c.Request.Context())),
	}
	if l.config.RequestHeaders {
		attrs = append(attrs, slog.Any("request_headers", l.redactHeaders(requestHeaders)))
	}
	logger.LogAttrs(c.Request.Context(), slog.LevelInfo, "http request started", attrs...)

	var requestCapture *bodyCapture
	if l.config.RequestBody && c.Request.Body != nil && capturableBody(requestHeaders) {
		requestCapture = &bodyCapture{limit: l.config.MaxBodyBytes}
		c.Request.Body = &captureReadCloser{ReadCloser: c.Request.Body, capture: requestCapture}
	}
	var responseCapture *captureWriter
	if l.config.ResponseBody {
		responseCapture = &captureWriter{
			ResponseWriter: c.Writer,
			capture:        bodyCapture{limit: l.config.MaxBodyBytes},
		}
		c.Writer = responseCapture
	}

	c.Next()

	status := c.Writer.Status()
	attrs = append(attrs,
		slog.String("route", c.FullPath()),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(time.Since(startedAt))/float64(time.Millisecond)),
	)
	if l.config.RequestBody {
		complete := requestCapture != nil && (requestCapture.eof || requestLength >= 0 && requestCapture.total == requestLength)
		attrs = append(attrs, slog.String("request_body", l.bodyForLog(requestCapture, requestHeaders, complete)))
	}
	if l.config.ResponseHeaders {
		attrs = append(attrs, slog.Any("response_headers", l.redactHeaders(c.Writer.Header())))
	}
	if responseCapture != nil {
		attrs = append(attrs, slog.String("response_body", l.bodyForLog(&responseCapture.capture, c.Writer.Header(), true)))
	}
	causes := make([]error, 0, len(c.Errors))
	var stacks []string
	for _, cause := range c.Errors {
		causes = append(causes, cause.Err)
		var tracer IStackTracer
		if errors.As(cause.Err, &tracer) {
			stacks = append(stacks, tracer.StackTrace())
		} else if stack, ok := cause.Meta.(string); ok {
			stacks = append(stacks, stack)
		}
	}
	if len(causes) > 0 {
		attrs = append(attrs, slog.Any("error", errors.Join(causes...)))
	}
	if len(stacks) > 0 {
		attrs = append(attrs, slog.Any("stack", stacks))
	}
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError || len(causes) > 0 {
		level = slog.LevelError
	} else if status >= http.StatusBadRequest {
		level = slog.LevelWarn
	}
	logger.LogAttrs(c.Request.Context(), level, "http request completed", attrs...)
}

// redactHeaders returns a detached, sanitized header snapshot.
func (l *accessLogger) redactHeaders(headers http.Header) http.Header {
	result := headers.Clone()
	for name := range result {
		if l.headers[strings.ToLower(name)] {
			result[name] = []string{"[REDACTED]"}
		}
	}
	return result
}

// bodyForLog emits only complete supported bodies after structured redaction.
func (l *accessLogger) bodyForLog(capture *bodyCapture, headers http.Header, complete bool) string {
	if capture == nil || capture.disabled || !capturableBody(headers) {
		return "[omitted: unsupported payload]"
	}
	if capture.truncated {
		return "[omitted: body exceeds log limit]"
	}
	if capture.failed || !complete {
		return "[omitted: incomplete body]"
	}
	if len(capture.data) == 0 {
		return ""
	}
	mediaType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(capture.data))
		if err != nil {
			return "[omitted: invalid form]"
		}
		for key := range values {
			if l.keys[normalizeRedactKey(key)] {
				values[key] = []string{"[REDACTED]"}
			}
		}
		return l.boundedBody(values.Encode())
	}

	decoder := json.NewDecoder(bytes.NewReader(capture.data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "[omitted: invalid json]"
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return "[omitted: invalid json]"
	}
	l.redactJSON(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "[omitted: invalid json]"
	}
	return l.boundedBody(string(encoded))
}

// boundedBody also bounds output expanded by encoding or redaction.
func (l *accessLogger) boundedBody(body string) string {
	if len(body) > l.config.MaxBodyBytes {
		return "[omitted: redacted body exceeds log limit]"
	}
	return body
}

// redactJSON traverses decoded JSON and masks sensitive field values.
func (l *accessLogger) redactJSON(value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if l.keys[normalizeRedactKey(key)] {
				value[key] = "[REDACTED]"
			} else {
				l.redactJSON(child)
			}
		}
	case []any:
		for _, child := range value {
			l.redactJSON(child)
		}
	}
}

// bodyCapture retains a bounded prefix and tracks whether it is complete.
type bodyCapture struct {
	data      []byte
	limit     int
	total     int64
	truncated bool
	eof       bool
	failed    bool
	disabled  bool
}

// append observes a byte or string prefix without converting the entire input.
// Slicing happens before append, so capture never copies more than its byte limit.
func (b *bodyCapture) append[T ~string | ~[]byte](data T) {
	b.total += int64(len(data))
	if b.disabled {
		return
	}
	remaining := b.limit - len(b.data)
	if len(data) > remaining {
		b.truncated = true
		data = data[:remaining]
	}
	if len(data) > 0 && b.data == nil {
		b.data = make([]byte, 0, b.limit)
	}
	b.data = append(b.data, data...)
}

// disable discards buffered data when a response becomes unsuitable for capture.
func (b *bodyCapture) disable() {
	b.disabled = true
	b.data = nil
}

// captureReadCloser preserves the original stream's read errors and Close behavior.
type captureReadCloser struct {
	io.ReadCloser
	capture *bodyCapture
}

// Read preserves stream results while observing only bytes actually consumed.
func (r *captureReadCloser) Read(data []byte) (int, error) {
	n, err := r.ReadCloser.Read(data)
	if n > 0 {
		r.capture.append(data[:n])
	}
	if err == io.EOF {
		r.capture.eof = true
	} else if err != nil {
		r.capture.failed = true
	}
	return n, err
}

// captureWriter observes successful writes and preserves Gin transport behavior.
type captureWriter struct {
	gin.ResponseWriter
	capture bodyCapture
}

// Write captures only bytes accepted by the underlying response writer.
func (w *captureWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.record(data[:n])
	}
	if err != nil {
		w.capture.failed = true
	}
	return n, err
}

// WriteString preserves the native string-write path and bounded capture.
func (w *captureWriter) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		w.record(data[:n])
	}
	if err != nil {
		w.capture.failed = true
	}
	return n, err
}

// ReadFrom prevents io.Copy from bypassing capture or recursively calling itself.
func (w *captureWriter) ReadFrom(reader io.Reader) (int64, error) {
	// Hide ReaderFrom so io.Copy always passes through this writer's bounded Write.
	return io.Copy(writerOnly{Writer: w}, reader)
}

// Flush forwards streaming flushes without retaining the full response.
func (w *captureWriter) Flush() {
	w.ResponseWriter.Flush()
}

// Hijack disables capture before transferring connection ownership.
func (w *captureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.capture.disable()
	return w.ResponseWriter.Hijack()
}

// Unwrap exposes optional transport capabilities to http.ResponseController.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// record stops capture for binary, compressed, streaming or attachment output.
func (w *captureWriter) record[T ~string | ~[]byte](data T) {
	if !capturableBody(w.Header()) {
		w.capture.disable()
		return
	}
	w.capture.append(data)
}

// writerOnly hides ReaderFrom from io.Copy to keep bounded Write interception.
type writerOnly struct {
	io.Writer
}

// defaultRedactHeaders lists credentials that header capture always masks.
func defaultRedactHeaders() map[string]bool {
	names := []string{
		"authorization", "proxy-authorization", "cookie", "set-cookie", "www-authenticate",
		"x-api-key", "api-key", "x-auth-token", "x-access-token", "x-csrf-token", "x-xsrf-token",
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result
}

// defaultRedactKeys lists normalized JSON/form credential names to mask.
func defaultRedactKeys() map[string]bool {
	names := []string{
		"password", "passwd", "pwd", "passphrase", "oldpassword", "newpassword", "confirmpassword",
		"passwordconfirmation", "secret", "clientsecret", "token", "accesstoken", "refreshtoken",
		"idtoken", "authorization", "apikey", "privatekey", "credential", "credentials",
		"session", "sessionid", "sessiontoken", "jwt", "otp", "verificationcode",
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result
}

// normalizeRedactKey ignores case and common separator differences.
func normalizeRedactKey(key string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
}

// capturableBody permits structured redaction and excludes unsafe output kinds.
func capturableBody(headers http.Header) bool {
	if headers.Get("Content-Disposition") != "" || headers.Get("Content-Encoding") != "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/x-www-form-urlencoded"
}
