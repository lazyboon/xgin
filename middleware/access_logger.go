package middleware

import (
	"context"
	"log/slog"
	"time"
)

// HeaderFormat controls header values in sanitized access records.
type HeaderFormat uint

const (
	HeaderFormatCompact HeaderFormat = iota // Single values become strings; multiple values remain []string.
	HeaderFormatMulti                       // Every value remains a []string, including single values.
)

// AccessPhase identifies the point in a request's lifecycle.
type AccessPhase string

const (
	AccessStarted   AccessPhase = "Started"
	AccessCompleted AccessPhase = "Completed"
)

// AccessLogLevel classifies access records independently of any logging backend.
type AccessLogLevel string

const (
	AccessLogInfo  AccessLogLevel = "Info"
	AccessLogWarn  AccessLogLevel = "Warn"
	AccessLogError AccessLogLevel = "Error"
)

// AccessRecord is a sanitized snapshot passed to filters and logging backends.
// Status, Duration, response fields, errors and bodies are only set on completion.
// Header values follow HeaderFormat. Bodies contain json.RawMessage for
// structured JSON, strings for text/forms/omission markers, or nil when disabled;
// an omitted-body marker explains why enabled capture could not safely log data.
// Backends may retain records, but must not mutate errors supplied by the handler.
// Error messages and stacks are diagnostics and may contain application secrets.
type AccessRecord struct {
	Phase           AccessPhase
	Level           AccessLogLevel
	Method          string
	Path            string
	Route           string
	RequestID       string
	Status          int
	Duration        time.Duration
	RequestHeaders  map[string]any
	ResponseHeaders map[string]any
	RequestBody     any
	ResponseBody    any
	Error           error
	Stacks          []string
}

// Message returns the standard message for this lifecycle event.
func (r AccessRecord) Message() string {
	if r.Phase == AccessStarted {
		return "http request started"
	}
	return "http request completed"
}

// Fields returns backend-neutral structured fields, excluding disabled payloads.
// Start records omit response metadata; all records include method, path and ID.
// The result shares sanitized payload snapshots with this record.
func (r AccessRecord) Fields() map[string]any {
	fields := map[string]any{
		"method":     r.Method,
		"path":       r.Path,
		"request_id": r.RequestID,
	}
	if r.Phase == AccessCompleted {
		fields["route"] = r.Route
		fields["status"] = r.Status
		fields["duration_ms"] = float64(r.Duration) / float64(time.Millisecond)
	}
	if r.RequestHeaders != nil {
		fields["request_headers"] = r.RequestHeaders
	}
	if r.ResponseHeaders != nil {
		fields["response_headers"] = r.ResponseHeaders
	}
	if r.RequestBody != nil {
		fields["request_body"] = r.RequestBody
	}
	if r.ResponseBody != nil {
		fields["response_body"] = r.ResponseBody
	}
	if r.Error != nil {
		fields["error"] = r.Error.Error()
	}
	if len(r.Stacks) > 0 {
		fields["stack"] = r.Stacks
	}
	return fields
}

// IAccessLogger writes sanitized access records. Implementations must support
// concurrent requests and own their output failures, buffering and lifecycle.
// Log must not retain ctx; copy any needed context values before returning.
type IAccessLogger interface {
	Log(ctx context.Context, record AccessRecord)
}

// AccessLoggerFunc adapts a function to IAccessLogger. It must be non-nil.
type AccessLoggerFunc func(context.Context, AccessRecord)

// Log forwards the event to the wrapped function.
func (f AccessLoggerFunc) Log(ctx context.Context, record AccessRecord) {
	f(ctx, record)
}

// SlogAccessLogger writes access records using the standard structured logger.
// Create it with NewSlogAccessLogger; it is safe for concurrent use.
type SlogAccessLogger struct {
	logger *slog.Logger
}

// NewSlogAccessLogger wraps logger. Nil selects slog.Default at construction.
func NewSlogAccessLogger(logger *slog.Logger) *SlogAccessLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogAccessLogger{logger: logger}
}

// Log preserves record severity and forwards request context to the slog handler.
func (l *SlogAccessLogger) Log(ctx context.Context, record AccessRecord) {
	level := slog.LevelInfo
	switch record.Level {
	case AccessLogWarn:
		level = slog.LevelWarn
	case AccessLogError:
		level = slog.LevelError
	case AccessLogInfo:
	}
	if !l.logger.Enabled(ctx, level) {
		return
	}
	fields := record.Fields()
	attrs := make([]slog.Attr, 0, len(fields))
	for name, value := range fields {
		attrs = append(attrs, slog.Any(name, value))
	}
	l.logger.LogAttrs(ctx, level, record.Message(), attrs...)
}
