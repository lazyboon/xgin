package xgin

import (
	"errors"
	"net/http"
	"sync"

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

	hooksMu sync.RWMutex
)

type hookers struct {
	bindingError      func(ctx *gin.Context, err error)
	transformResponse func(response Response) any
	beforeResponse    func(ctx *gin.Context, response Response) (stop bool)
	afterResponse     func(ctx *gin.Context, response Response)
}

func loadHookers() hookers {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return hookers{
		bindingError:      HookerBindingError,
		transformResponse: HookerTransformResponse,
		beforeResponse:    HookerBeforeResponse,
		afterResponse:     HookerAfterResponse,
	}
}

func getValidatorMaxDepth() int {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return ValidatorMaxDepth
}

func SetValidatorMaxDepth(depth int) {
	if depth <= 0 {
		depth = 20
	}
	hooksMu.Lock()
	ValidatorMaxDepth = depth
	hooksMu.Unlock()
}

func SetHookerBindingError(h func(ctx *gin.Context, err error)) {
	hooksMu.Lock()
	HookerBindingError = h
	hooksMu.Unlock()
}

func SetHookerTransformResponse(h func(response Response) any) {
	hooksMu.Lock()
	HookerTransformResponse = h
	hooksMu.Unlock()
}

func SetHookerBeforeResponse(h func(ctx *gin.Context, response Response) (stop bool)) {
	hooksMu.Lock()
	HookerBeforeResponse = h
	hooksMu.Unlock()
}

func SetHookerAfterResponse(h func(ctx *gin.Context, response Response)) {
	hooksMu.Lock()
	HookerAfterResponse = h
	hooksMu.Unlock()
}

// HandlerFunc defines the signature for type-safe generic handlers with request DTO.
type HandlerFunc[T any] func(ctx *Context, req *T) Response

// Handle wraps business logic with automated binding, deep validation, and lifecycle hooks.
func Handle[T any](h HandlerFunc[T]) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := NewContext(c)
		var req T
		hks := loadHookers()

		// 1. Automated multi-source binding and recursive validation
		if err := ctx.AutoBind(&req); err != nil {
			if hks.bindingError != nil {
				hks.bindingError(c, err)
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
	status := normalizeStatus(r)

	// Apply custom HTTP Headers
	for key, val := range r.Header {
		c.Header(key, val)
	}

	// Apply global response transformation if defined
	var finalOutput any = r
	hks := loadHookers()
	if hks.transformResponse != nil {
		finalOutput = hks.transformResponse(r)
	}

	// Rendering Dispatcher
	switch r.ContentType {
	case ContentTypeJSON:
		c.JSON(status, finalOutput)
	case ContentTypeIndentedJSON:
		c.IndentedJSON(status, finalOutput)
	case ContentTypeSecureJSON:
		c.SecureJSON(status, finalOutput)
	case ContentTypeJsonpJSON:
		c.JSONP(status, finalOutput)
	case ContentTypeAsciiJSON:
		c.AsciiJSON(status, finalOutput)
	case ContentTypePureJSON:
		c.PureJSON(status, finalOutput)
	case ContentTypeProtoBuf:
		c.ProtoBuf(status, finalOutput)
	case ContentTypeTOML:
		c.TOML(status, finalOutput)
	case ContentTypeXML:
		c.XML(status, finalOutput)
	case ContentTypeYAML:
		c.YAML(status, finalOutput)
	case ContentTypeMsgPack:
		c.Render(status, render.MsgPack{Data: finalOutput})
	case ContentTypeString:
		str := r.Msg
		if s, ok := r.Data.(string); ok {
			str = s
		}
		c.String(status, str)
	case ContentTypeRedirect:
		if url, ok := r.Data.(string); ok {
			c.Redirect(status, url)
		}
	case ContentTypeHTML:
		c.HTML(status, r.HtmlPath, r.Data)
	default:
		c.JSON(status, finalOutput)
	}
}

func normalizeStatus(r Response) int {
	status := r.HttpStatus
	if status >= 100 && status <= 999 {
		return status
	}
	if r.ContentType == ContentTypeRedirect {
		return http.StatusFound
	}
	return http.StatusOK
}
