package xgin

import "github.com/gin-gonic/gin"

// IBinder binds HTTP sources and performs source-local binding-tag validation.
// Implementations must be safe for concurrent use.
type IBinder interface {
	Bind(ctx *gin.Context, target any) error
}

// IBinderPreparer optionally compiles DTO metadata during route registration.
type IBinderPreparer interface{ Prepare(target any) error }

// IRequestValidator validates a DTO after all HTTP sources have been bound.
type IRequestValidator interface{ Validate(value any) error }

// IResponse writes one complete response. A nil error means the response has
// been committed. Implementations must not retain the Gin context; errors after
// commitment can only be logged, never replaced with a second response.
type IResponse interface{ Render(ctx *gin.Context) error }

// IRequestErrorMapper selects the response for binding or validation failures.
// Raw decoder details must not be exposed unless the application explicitly intends it.
type IRequestErrorMapper interface {
	MapRequestError(ctx *gin.Context, err error) IResponse
}

// RequestErrorMapperFunc adapts a function to IRequestErrorMapper.
type RequestErrorMapperFunc func(*gin.Context, error) IResponse

// MapRequestError delegates to the configured function.
func (f RequestErrorMapperFunc) MapRequestError(c *gin.Context, err error) IResponse {
	return f(c, err)
}

// ResponseTransformer replaces the body of a default JSON Response.
// Explicit WithBody/JSON payloads and custom IResponse values bypass it.
// It must be safe for concurrent use and must not modify the supplied response.
type ResponseTransformer func(Response) any

// HandlerFunc receives a request-scoped Context and a complete DTO.
// The response type is inferred when registering Handler[Request](handler).
type HandlerFunc[T any, R IResponse] func(*Context, *T) R

// ITokenExtractor defines where an application obtains its unverified credential.
// Implementations must support concurrent calls and must not retain the Gin context.
type ITokenExtractor interface {
	ExtractToken(ctx *gin.Context) (string, bool)
}

// TokenExtractorFunc adapts a cookie, header or other application-specific extractor.
type TokenExtractorFunc func(*gin.Context) (string, bool)

// ExtractToken delegates to the application-provided extractor.
func (f TokenExtractorFunc) ExtractToken(c *gin.Context) (string, bool) { return f(c) }
