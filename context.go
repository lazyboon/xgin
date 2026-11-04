package xgin

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

type requestIDContextKey struct{}

// Context combines Gin's HTTP helpers with the standard request context.
// Deadline, Done, Err and Value use an immutable snapshot of Request.Context
// captured at construction, regardless of Gin's ContextWithFallback setting.
// These four methods support concurrent calls. Gin/HTTP helpers remain restricted
// to synchronous request handling; never retain Context after the request ends.
// Create it through XGin.NewContext or receive it from a typed handler.
type Context struct {
	*gin.Context
	base  context.Context
	owner *XGin
}

var _ context.Context = (*Context)(nil)

// Deadline forwards the original request deadline.
func (c *Context) Deadline() (time.Time, bool) { return c.base.Deadline() }

// Done forwards request cancellation without allocating a goroutine or channel.
func (c *Context) Done() <-chan struct{} { return c.base.Done() }

// Err returns the original request context's cancellation error.
func (c *Context) Err() error { return c.base.Err() }

// Value reads standard context values, not Gin's separate string-keyed storage.
// Use Gin's Get/GetString methods for values written with Gin's Set method.
func (c *Context) Value(key any) any { return c.base.Value(key) }

// GetValue retrieves a standard context value with its expected type.
// Keys follow context.WithValue's comparable, package-specific key convention.
func (c *Context) GetValue[T any](key any) (T, bool) {
	value, ok := c.base.Value(key).(T)
	return value, ok
}

// RequestID returns the trusted identifier set by the request-ID middleware.
// It never falls back to an untrusted incoming header.
func (c *Context) RequestID() string { return RequestIDFromContext(c.base) }

// Token uses this instance's credential extractor; the default is RFC 6750 Bearer.
// Successful extraction does not authenticate a caller.
func (c *Context) Token() (string, bool) { return c.owner.tokenExtractor.ExtractToken(c.Context) }

// BearerToken explicitly extracts an Authorization Bearer credential, independent
// of Config.TokenExtractor. This method never verifies the credential.
func (c *Context) BearerToken() (string, bool) {
	return (BearerTokenExtractor{}).ExtractToken(c.Context)
}

// AutoBind applies this instance's binder and complete-DTO validation policies.
func (c *Context) AutoBind(target any) error { return c.owner.AutoBind(c.Context, target) }

// Response writes a response through this instance's transformation and error pipeline.
func (c *Context) Response(output IResponse) { c.owner.Render(c.Context, output) }

// WithRequestID returns a standard child context carrying a trusted request ID.
// Validation and trust policy belong to the middleware that creates the identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext returns a request identifier, or an empty string when absent.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}
