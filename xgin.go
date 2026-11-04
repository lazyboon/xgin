// Package xgin adds typed handlers, source-isolated binding and composable
// responses to Gin. Instances are independent and do not modify Gin globals.
package xgin

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// XGin coordinates binding, validation and response rendering for one configuration.
// Create it with New and reuse it across routes. Custom dependencies must be safe
// for concurrent use and must not be modified after registration starts.
type XGin struct {
	binder             IBinder
	validator          IRequestValidator
	requestErrorMapper IRequestErrorMapper
	transformResponse  ResponseTransformer
	tokenExtractor     ITokenExtractor
}

// New validates configuration and builds an independent Gin adapter.
func New(config Config) (*XGin, error) {
	conf, err := config.normalize()
	if err != nil {
		return nil, err
	}
	binder := conf.Binder
	if isNilInterface(binder) {
		binder, err = NewBinder(conf.MaxBodyBytes)
		if err != nil {
			return nil, err
		}
	}
	validator := conf.Validator
	if isNilInterface(validator) {
		validator = NewValidator(conf.ValidatorMaxDepth)
	}
	mapper := conf.RequestErrorMapper
	if isNilInterface(mapper) {
		mapper = DefaultRequestErrorMapper{}
	}
	extractor := conf.TokenExtractor
	if isNilInterface(extractor) {
		extractor = BearerTokenExtractor{}
	}
	return &XGin{
		binder: binder, validator: validator, requestErrorMapper: mapper,
		transformResponse: conf.TransformResponse, tokenExtractor: extractor,
	}, nil
}

// MustNew is New for startup paths where invalid configuration is fatal.
func MustNew(config Config) *XGin {
	instance, err := New(config)
	if err != nil {
		panic(err)
	}
	return instance
}

// Handler compiles a struct DTO when the route is registered and adapts its handler.
// Invalid handlers and DTO declarations panic during registration. Binding errors
// use the configured mapper; business panics belong to Gin recovery middleware.
func (x *XGin) Handler[T any, R IResponse](handler HandlerFunc[T, R]) gin.HandlerFunc {
	if !x.initialized() {
		panic("xgin: Handler requires an initialized instance")
	}
	if handler == nil || reflect.TypeFor[T]().Kind() != reflect.Struct {
		panic("xgin: Handler requires a struct request type and a non-nil handler")
	}
	if preparer, ok := x.binder.(IBinderPreparer); ok {
		if err := preparer.Prepare(new(T)); err != nil {
			panic(fmt.Errorf("xgin: prepare request %s: %w", reflect.TypeFor[T](), err))
		}
	}
	return func(c *gin.Context) {
		if c.IsAborted() {
			return
		}
		defer x.cleanupUploads(c)
		requestContext := x.NewContext(c)
		var req T
		if err := requestContext.AutoBind(&req); err != nil {
			x.Render(c, x.requestErrorMapper.MapRequestError(c, err))
			return
		}
		x.Render(c, handler(requestContext, &req))
	}
}

// Wrap adapts a handler that does not need automatic DTO binding.
func (x *XGin) Wrap[R IResponse](handler func(*Context) R) gin.HandlerFunc {
	if !x.initialized() || handler == nil {
		panic("xgin: Wrap requires an initialized instance and a non-nil handler")
	}
	return func(c *gin.Context) {
		if c.IsAborted() {
			return
		}
		defer x.cleanupUploads(c)
		x.Render(c, handler(x.NewContext(c)))
	}
}

// AutoBind binds HTTP sources and then recursively validates the complete DTO.
// It can also be used by ordinary Gin handlers without a custom Context wrapper.
// Handler and Wrap remove uploaded temporary files after synchronous handling.
// Other callers must arrange MultipartForm.RemoveAll after finishing with uploads.
func (x *XGin) AutoBind(c *gin.Context, target any) error {
	if !x.initialized() {
		return fmt.Errorf("xgin: AutoBind requires an initialized instance")
	}
	if err := x.binder.Bind(c, target); err != nil {
		return err
	}
	return x.validator.Validate(target)
}

// cleanupUploads releases temporary files after binding, rendering or a handler panic.
func (x *XGin) cleanupUploads(c *gin.Context) {
	// Context middleware may copy the request, so net/http's original request
	// does not necessarily reference the multipart form parsed during binding.
	if c.Request != nil && c.Request.MultipartForm != nil {
		if err := c.Request.MultipartForm.RemoveAll(); err != nil {
			_ = c.Error(err)
		}
	}
}

// NewContext captures the request's standard context and attaches this instance.
// Middleware must finish adding standard context values before calling this method.
// Invalid HTTP contexts are programming errors and panic immediately.
func (x *XGin) NewContext(c *gin.Context) *Context {
	if !x.initialized() || c == nil || c.Request == nil {
		panic("xgin: NewContext requires an initialized instance and a request")
	}
	return &Context{Context: c, base: c.Request.Context(), owner: x}
}

// Render writes a single response and terminates the remaining handler chain.
// Errors are attached to Gin; an uncommitted failure restores the original headers
// and leaves status 500 for an outer error middleware. Committed output is never replaced.
func (x *XGin) Render(c *gin.Context, output IResponse) {
	if !x.initialized() || c == nil || c.Writer == nil {
		panic("xgin: Render requires an initialized instance and a valid context")
	}
	c.Abort()
	if c.Writer.Written() {
		_ = c.Error(fmt.Errorf("%w: response already written", ErrInvalidRender))
		return
	}
	originalHeaders := c.Writer.Header().Clone()
	if err := x.writeResponse(c, output); err != nil {
		if !c.Writer.Written() {
			clear(c.Writer.Header())
			maps.Copy(c.Writer.Header(), originalHeaders)
			c.Status(http.StatusInternalServerError)
		}
		_ = c.Error(err)
	}
}

// writeResponse isolates response failures and transforms only default JSON envelopes.
func (x *XGin) writeResponse(c *gin.Context, output IResponse) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &PanicError{kind: ErrRenderPanic, value: recovered, stack: debug.Stack()}
		}
	}()
	if isNilInterface(output) {
		return fmt.Errorf("%w: nil response", ErrInvalidRender)
	}
	// Copy the value before transformation; shared response templates stay unchanged.
	if x.transformResponse != nil {
		switch response := output.(type) {
		case Response:
			output = x.transform(response)
		case *Response:
			output = x.transform(*response)
		}
	}
	if err = output.Render(c); err != nil {
		return err
	}
	if !c.Writer.Written() {
		return fmt.Errorf("%w: response did not commit output", ErrInvalidRender)
	}
	return nil
}

// transform preserves explicit bodies and non-JSON protocol responses.
func (x *XGin) transform(response Response) Response {
	if response.isJSON() && !response.bodyOnly {
		return response.WithBody(x.transformResponse(response))
	}
	return response
}

// initialized rejects an uninitialized instance before entering a request pipeline.
func (x *XGin) initialized() bool {
	return x != nil && !isNilInterface(x.binder) && !isNilInterface(x.validator) && !isNilInterface(x.requestErrorMapper) && !isNilInterface(x.tokenExtractor)
}

// isNilInterface detects both nil interfaces and typed-nil dependencies.
func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
