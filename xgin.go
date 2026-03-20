package xgin

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
)

// Global Configuration and Lifecycle Hooks
var (
	// ValidatorMaxDepth defines the recursion limit for the validation engine.
	ValidatorMaxDepth = 20

	// ErrMaxDepth is returned when the validation exceeds ValidatorMaxDepth.
	ErrMaxDepth = errors.New("xgin.validator: maximum recursion depth exceeded")

	// HookerBindingError is triggered on AutoBind or IValidator failures.
	HookerBindingError func(ctx *gin.Context, err error)

	// HookerTransformResponse modifies the Response object before final rendering.
	HookerTransformResponse func(response Response) any

	// HookerBeforeResponse executes before rendering; return true to abort.
	HookerBeforeResponse func(ctx *gin.Context, response Response) (stop bool)

	// HookerAfterResponse executes after the response has been written.
	HookerAfterResponse func(ctx *gin.Context, response Response)
)

// HandlerFunc defines the signature for type-safe generic handlers with request DTO.
type HandlerFunc[T any] func(ctx *Context, req *T) Response

// Handle wraps business logic with automated binding, deep validation, and lifecycle hooks.
func Handle[T any](h HandlerFunc[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := NewContext(c)
		var req T

		// 1. Automated multi-source binding and recursive validation
		if err := ctx.AutoBind(&req); err != nil {
			if HookerBindingError != nil {
				HookerBindingError(c, err)
			} else {
				if !c.IsAborted() {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				}
			}
			return
		}

		// 2. Business logic execution
		r := h(ctx, &req)

		// 3. Response lifecycle management
		ctx.response(r, func() {
			renderResponse(c, r)
		})

		c.Next()
	}
}

// Wrap is a lightweight wrapper for handlers that do not require a request DTO.
func Wrap(handler func(ctx *Context) Response) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := NewContext(c)
		r := handler(ctx)

		ctx.response(r, func() {
			renderResponse(c, r)
		})

		c.Next()
	}
}

// renderResponse dispatches the result to the client based on the specified ContentType.
func renderResponse(c *gin.Context, r Response) {
	// Apply custom HTTP Headers
	for key, val := range r.Header {
		c.Header(key, val)
	}

	// Apply global response transformation if defined
	var finalOutput any = r
	if HookerTransformResponse != nil {
		finalOutput = HookerTransformResponse(r)
	}

	// Rendering Dispatcher
	switch r.ContentType {
	case ContentTypeJSON:
		c.JSON(r.HttpStatus, finalOutput)
	case ContentTypeIndentedJSON:
		c.IndentedJSON(r.HttpStatus, finalOutput)
	case ContentTypeSecureJSON:
		c.SecureJSON(r.HttpStatus, finalOutput)
	case ContentTypeJsonpJSON:
		c.JSONP(r.HttpStatus, finalOutput)
	case ContentTypeAsciiJSON:
		c.AsciiJSON(r.HttpStatus, finalOutput)
	case ContentTypePureJSON:
		c.PureJSON(r.HttpStatus, finalOutput)
	case ContentTypeProtoBuf:
		c.ProtoBuf(r.HttpStatus, finalOutput)
	case ContentTypeTOML:
		c.TOML(r.HttpStatus, finalOutput)
	case ContentTypeXML:
		c.XML(r.HttpStatus, finalOutput)
	case ContentTypeYAML:
		c.YAML(r.HttpStatus, finalOutput)
	case ContentTypeMsgPack:
		c.Render(r.HttpStatus, render.MsgPack{Data: finalOutput})
	case ContentTypeString:
		str := r.Msg
		if s, ok := r.Data.(string); ok {
			str = s
		}
		c.String(r.HttpStatus, str)
	case ContentTypeRedirect:
		if url, ok := r.Data.(string); ok {
			c.Redirect(r.HttpStatus, url)
		}
	case ContentTypeHTML:
		c.HTML(r.HttpStatus, r.HtmlPath, r.Data)
	default:
		c.JSON(r.HttpStatus, finalOutput)
	}
}
