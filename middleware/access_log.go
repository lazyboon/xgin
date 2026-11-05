package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// AccessLogContent controls optional headers and bounded body capture.
// A zero MaxBodyBytes uses 4096 bytes; the maximum is 1 MiB.
type AccessLogContent struct {
	Headers      bool
	Body         bool
	MaxBodyBytes int
}

// normalize bounds capture memory independently for each payload direction.
func (c AccessLogContent) normalize() AccessLogContent {
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = defaultLogBodyBytes
	}
	if c.MaxBodyBytes < 1 || c.MaxBodyBytes > maxLogBodyBytes {
		panic("xgin.middleware: log body limit must be between 1 and 1048576")
	}
	return c
}

// AccessLogPolicy controls the events and payloads recorded for one request.
// Bodies are only included in completion events. With Finish disabled, body
// capture is disabled even if Body is true. An empty resolved policy skips logging.
type AccessLogPolicy struct {
	Start    bool
	Finish   bool
	Request  AccessLogContent
	Response AccessLogContent
}

// normalize validates capture limits and fills in their defaults.
func (p AccessLogPolicy) normalize() AccessLogPolicy {
	p.Request = p.Request.normalize()
	p.Response = p.Response.normalize()
	return p
}

// AccessLogConfig controls access records, payload capture and output.
// Callbacks and Logger must support concurrent calls. Gin callbacks must not
// retain the context. Configuration slices are copied during construction.
type AccessLogConfig struct {
	// Logger receives sanitized records. Nil uses slog.Default through an adapter.
	Logger IAccessLogger
	// Default sets the base policy. Its zero value disables logging and capture.
	Default AccessLogPolicy
	// Resolve receives a policy copy before handling the request. Match routes
	// using both Request.Method and FullPath(); unmatched routes have an empty
	// FullPath(). Returned policies must contain valid capture limits.
	Resolve func(*gin.Context, AccessLogPolicy) AccessLogPolicy
	// ShouldLog filters each sanitized event immediately before output. A
	// completion-only condition cannot retroactively suppress a start event;
	// disable Start for routes that should only log errors or slow requests.
	ShouldLog func(AccessRecord) bool
	// HeaderFormat defaults to HeaderFormatCompact.
	HeaderFormat  HeaderFormat
	RedactHeaders []string // Additional sensitive headers; built-in masks cannot be removed.
	RedactKeys    []string // Additional JSON/form keys; matching ignores case, '_' and '-'.
}

type accessLogger struct {
	config  AccessLogConfig
	headers map[string]bool
	keys    map[string]bool
}

// AccessLog observes request start and completion without reading ahead.
// Only complete JSON and URL-encoded bodies are logged. Query values are never
// logged. Invalid static configuration panics during middleware construction.
func AccessLog(config AccessLogConfig) gin.HandlerFunc {
	config.Default = config.Default.normalize()
	if config.HeaderFormat != HeaderFormatMulti && config.HeaderFormat != HeaderFormatCompact {
		panic("xgin.middleware: invalid access log header format")
	}
	if config.Logger == nil {
		config.Logger = NewSlogAccessLogger(nil)
	}
	logger := &accessLogger{
		config:  config,
		headers: defaultRedactHeaders(), keys: defaultRedactKeys(),
	}
	for _, name := range config.RedactHeaders {
		if !httpguts.ValidHeaderFieldName(name) {
			panic("xgin.middleware: invalid sensitive header name")
		}
		logger.headers[strings.ToLower(name)] = true
	}
	for _, key := range config.RedactKeys {
		if normalizeRedactKey(key) == "" {
			panic("xgin.middleware: sensitive body key cannot be empty")
		}
		logger.keys[normalizeRedactKey(key)] = true
	}
	logger.config.RedactHeaders = nil
	logger.config.RedactKeys = nil
	return logger.handle
}

// IStackTracer exposes diagnostic stacks without depending on an error package.
type IStackTracer interface {
	StackTrace() string
}

// handle applies a request-local policy and observes the final response.
func (l *accessLogger) handle(c *gin.Context) {
	startedAt := time.Now()
	policy := l.config.Default
	if l.config.Resolve != nil {
		policy = l.config.Resolve(c, policy).normalize()
	}
	if !policy.Start && !policy.Finish {
		c.Next()
		return
	}
	record := AccessRecord{
		Phase: AccessStarted, Level: AccessLogInfo,
		Method: c.Request.Method, Path: c.Request.URL.Path, Route: c.FullPath(),
		RequestID: RequestIDFromContext(c.Request.Context()),
	}
	var requestHeaders http.Header
	if policy.Request.Headers || policy.Finish && policy.Request.Body {
		requestHeaders = c.Request.Header.Clone()
	}
	requestLength := c.Request.ContentLength
	if policy.Start {
		if policy.Request.Headers {
			record.RequestHeaders = l.headersForLog(requestHeaders)
		}
		l.emit(c, record)
	}

	// Capture must be selected before the handler consumes the streams. Even
	// completion filters such as "errors only" need this bounded observation.
	var requestCapture *bodyCapture
	if policy.Finish && policy.Request.Body && c.Request.Body != nil && capturableBody(requestHeaders) {
		requestCapture = &bodyCapture{limit: policy.Request.MaxBodyBytes}
		c.Request.Body = &captureReadCloser{ReadCloser: c.Request.Body, capture: requestCapture}
	}
	var responseCapture *captureWriter
	if policy.Finish && policy.Response.Body {
		responseCapture = &captureWriter{
			ResponseWriter: c.Writer,
			capture:        bodyCapture{limit: policy.Response.MaxBodyBytes},
		}
		c.Writer = responseCapture
	}
	c.Next()
	if !policy.Finish {
		return
	}

	// Create detached headers for each event: a custom output may retain or
	// modify its record without changing the response or the next event.
	record.Phase = AccessCompleted
	record.Duration = time.Since(startedAt)
	record.Status = c.Writer.Status()
	if policy.Request.Headers {
		record.RequestHeaders = l.headersForLog(requestHeaders)
	}
	if policy.Request.Body {
		complete := requestCapture != nil && (requestCapture.eof ||
			requestLength >= 0 && requestCapture.total == requestLength)
		body := l.bodyForLog(requestCapture, requestHeaders, complete)
		record.RequestBody = &body
	}
	if policy.Response.Headers {
		record.ResponseHeaders = l.headersForLog(c.Writer.Header())
	}
	if responseCapture != nil {
		body := l.bodyForLog(&responseCapture.capture, c.Writer.Header(), true)
		record.ResponseBody = &body
	}
	causes := make([]error, 0, len(c.Errors))
	for _, cause := range c.Errors {
		causes = append(causes, cause.Err)
		var tracer IStackTracer
		if errors.As(cause.Err, &tracer) {
			record.Stacks = append(record.Stacks, tracer.StackTrace())
		} else if stack, ok := cause.Meta.(string); ok {
			record.Stacks = append(record.Stacks, stack)
		}
	}
	record.Error = errors.Join(causes...)
	switch {
	case record.Status >= http.StatusInternalServerError || record.Error != nil:
		record.Level = AccessLogError
	case record.Status >= http.StatusBadRequest:
		record.Level = AccessLogWarn
	}
	l.emit(c, record)
}

// emit passes only sanitized records to the predicate and output.
func (l *accessLogger) emit(c *gin.Context, record AccessRecord) {
	if l.config.ShouldLog == nil || l.config.ShouldLog(record) {
		l.config.Logger.Log(c.Request.Context(), record)
	}
}

// headersForLog preserves multi-value semantics and optionally compacts single values.
func (l *accessLogger) headersForLog(headers http.Header) map[string]any {
	sanitized := l.redactHeaders(headers)
	result := make(map[string]any, len(sanitized))
	for name, values := range sanitized {
		if l.config.HeaderFormat == HeaderFormatCompact && len(values) == 1 {
			result[name] = values[0]
			continue
		}
		result[name] = values
	}
	return result
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
		return l.boundedBody(values.Encode(), capture.limit)
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
	return l.boundedBody(string(encoded), capture.limit)
}

// boundedBody also bounds output expanded by encoding or redaction.
func (l *accessLogger) boundedBody(body string, limit int) string {
	if len(body) > limit {
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
