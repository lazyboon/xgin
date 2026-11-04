// Package middleware provides request tracing, access logging, and error handling for Gin.
package middleware

import (
	"context"
	"crypto/rand"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
	"golang.org/x/net/http/httpguts"
)

const maxRequestIDLength = 128

// RequestIDConfig controls request identifier generation and propagation.
// The zero value generates a fresh server identifier in X-Request-ID.
type RequestIDConfig struct {
	Header        string        // Header defaults to X-Request-ID.
	TrustIncoming bool          // TrustIncoming accepts one valid incoming identifier.
	Generator     func() string // Generator must support concurrent calls; nil uses crypto/rand.Text.
}

// RequestID assigns a bounded identifier to the request context and response header.
// Incoming and generated identifiers may contain ASCII letters, digits, '.', '_', and '-'.
// Invalid incoming identifiers are replaced; invalid generator output falls back to crypto/rand.Text.
// Invalid static configuration panics during middleware construction.
func RequestID(config RequestIDConfig) gin.HandlerFunc {
	if config.Header == "" {
		config.Header = "X-Request-ID"
	}
	if !httpguts.ValidHeaderFieldName(config.Header) {
		panic("xgin.middleware: invalid request id header")
	}
	config.Header = http.CanonicalHeaderKey(config.Header)
	if config.Generator == nil {
		config.Generator = rand.Text
	}

	return func(c *gin.Context) {
		var id string
		if config.TrustIncoming {
			values := c.Request.Header.Values(config.Header)
			if len(values) == 1 && validRequestID(values[0]) {
				id = values[0]
			}
		}
		if id == "" {
			id = config.Generator()
			if !validRequestID(id) {
				id = rand.Text()
			}
		}
		c.Request = c.Request.WithContext(WithRequestID(c.Request.Context(), id))
		c.Header(config.Header, id)
		c.Next()
	}
}

// RequestIDFromContext returns the request identifier, or an empty string when absent.
func RequestIDFromContext(ctx context.Context) string { return xgin.RequestIDFromContext(ctx) }

// WithRequestID returns a child context carrying a valid request identifier.
// An invalid identifier panics; the parent follows context.WithValue's non-nil contract.
func WithRequestID(ctx context.Context, id string) context.Context {
	if !validRequestID(id) {
		panic("xgin.middleware: invalid request id")
	}
	return xgin.WithRequestID(ctx, id)
}

// validRequestID restricts identifiers to a bounded, log-safe ASCII alphabet.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLength {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
			ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}
