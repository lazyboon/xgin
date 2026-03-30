package xgin

import (
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
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
		validator: SharedValidator(getValidatorMaxDepth()),
	}
}

// AutoBind handles multi-source parameter binding and dual-stage validation.
// It follows a "Silent Fill -> Final Bind -> Recursive Validate" pattern
// to ensure all fields (Header/URI/Query/Body) are populated before global validation.
func (c *Context) AutoBind(obj any) error {
	req := c.Request

	// 1. Silent Fill: Populate non-body sources without triggering validation tags.
	if len(c.Params) > 0 {
		m := make(map[string][]string)
		for _, v := range c.Params {
			m[v.Key] = []string{v.Value}
		}
		_ = binding.Uri.BindUri(m, obj)
	}
	_ = binding.Query.Bind(req, obj)
	_ = binding.Header.Bind(req, obj)

	// 2. Final Bind & Base Validation: Bind request body and trigger all 'binding' tags.
	if err := c.ShouldBind(obj); err != nil {
		return err
	}

	// 3. Recursive Business Validation: Execute the custom IValidator interface.
	return c.validator.Validate(obj)
}

// response wraps the rendering process with Before and After lifecycle hooks.
func (c *Context) response(response Response, f func()) {
	hks := loadHookers()

	// Execute the Before-Response hook; allows interception or modification.
	if hks.beforeResponse != nil {
		stop := hks.beforeResponse(c.Context, response)
		if stop {
			return
		}
	}

	// Execute the actual rendering logic
	f()

	// Execute the After-Response hook; ideal for logging or auditing.
	if hks.afterResponse != nil {
		hks.afterResponse(c.Context, response)
	}
}
