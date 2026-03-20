package xgin

import (
	"github.com/gin-gonic/gin"
)

// Context wraps gin.Context to provide extended functionality
type Context struct {
	*gin.Context
	validator *Validator
}

// NewContext creates a new xgin Context from a gin Context
func NewContext(c *gin.Context) *Context {
	return &Context{
		Context:   c,
		validator: SharedValidator(ValidatorMaxDepth),
	}
}

// AutoBind performs a multi-source binding and recursive validation.
// It sequentially binds URI, Header, and Request Body parameters.
func (c *Context) AutoBind(obj any) error {
	// 1. Bind URI Path Parameters
	if len(c.Params) > 0 {
		if err := c.ShouldBindUri(obj); err != nil {
			return err
		}
	}

	// 2. Bind Request Headers
	// Note: ShouldBindHeader is strict; ensure the DTO uses `header` tags.
	if err := c.ShouldBindHeader(obj); err != nil {
		return err
	}

	// 3. Bind Request Body (JSON, XML, Form, etc. based on Content-Type)
	if err := c.ShouldBind(obj); err != nil {
		return err
	}

	// 4. Execute Recursive Validation Engine
	// This triggers the IValidator interface and deep-scans nested structs/slices.
	return c.validator.Validate(obj)
}

// response wraps the rendering process with Before and After lifecycle hooks.
func (c *Context) response(response Response, f func()) {
	// Execute the Before-Response hook; allows interception or modification.
	if HookerBeforeResponse != nil {
		stop := HookerBeforeResponse(c.Context, response)
		if stop {
			return
		}
	}

	// Execute the actual rendering logic
	f()

	// Execute the After-Response hook; ideal for logging or auditing.
	if HookerAfterResponse != nil {
		HookerAfterResponse(c.Context, response)
	}
}
