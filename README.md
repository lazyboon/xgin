# xgin

`xgin` is a pragmatic extension toolkit for [gin](https://github.com/gin-gonic/gin).
It keeps Gin lightweight while adding typed handlers, multi-source binding, recursive business validation, and production-friendly middleware.

## Install

```bash
go get github.com/lazyboon/xgin
```

## Core Features

- Generic handler wrapper: `xgin.Handle(func(ctx *xgin.Context, req *Req) xgin.Response)`
- Multi-source auto binding: URI + Query + Header + Body in a single flow (`AutoBind`)
- Recursive business validation with `IValidator`
- Unified chain-style response builder with multiple content types
- Global lifecycle hook points for bind errors and response processing

## Quick Start

```go
package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
)

type LoginReq struct {
	User string `json:"user" binding:"required"`
	Pass string `json:"pass" binding:"required"`
}

func main() {
	r := gin.New()

	r.POST("/login", xgin.Handle(func(ctx *xgin.Context, req *LoginReq) xgin.Response {
		return xgin.Response{}.
			WithHttpStatus(http.StatusOK).
			WithCode(0).
			WithMsg("ok").
			WithData(gin.H{"user": req.User})
	}))

	_ = r.Run(":8080")
}
```

## Validator Features

Validation in `xgin` runs in two stages:

1. Gin tag validation (`binding:"required"`, etc.)
2. Recursive custom business validation via `Validate() error` (`IValidator`)

### Recursive Coverage

- Nested structs
- Slices and arrays (`[]T`, `[N]T`)
- Maps (`map[K]V`, validates both keys and values)
- Pointer receiver and value receiver validators
- Built-in recursion depth guard (default: `20`)

### Example

```go
type Item struct {
	Price int `json:"price" binding:"required"`
}

func (i Item) Validate() error {
	if i.Price <= 0 {
		return errors.New("price must be positive")
	}
	return nil
}

type CreateOrderReq struct {
	UserID string `uri:"user_id" binding:"required"`
	Token  string `header:"X-Token" binding:"required"`
	Page   int    `form:"page"`
	Items  []Item `json:"items" binding:"required"`
}

func (r *CreateOrderReq) Validate() error {
	if len(r.Items) == 0 {
		return errors.New("items cannot be empty")
	}
	return nil
}
```

Inside `xgin.Handle(...)`, `AutoBind(&req)` performs:

- Silent fill for URI/Query/Header
- Body bind with Gin tag validation
- Recursive business validation (`IValidator`)

### Depth Configuration

```go
xgin.SetValidatorMaxDepth(64)
```

## Middleware Features

### 1) Metadata Middleware

`middleware.NewMetadata(...)` injects runtime metadata into response headers:

- Request ID (default: `X-Request-ID`)
- Receive time (default: `X-Receive-Time`)
- Response time (default: `X-Response-Time`)
- Custom static headers

```go
r.Use(middleware.NewMetadata(&middleware.MetadataOptions{
	NeedRequestID:    true,
	NeedReceiveTime:  true,
	NeedResponseTime: true,
	Custom: map[string]string{
		"X-Service": "order-api",
	},
}))
```

### 2) AccessLog Middleware

`middleware.NewAccessLog(handler, ...)` provides structured access logging:

- Optional capture of request/response headers and bodies
- Route-level overrides via `SpecificPath`
- Path skip list via `SkipPaths`
- Safe request body capture limit to reduce OOM risk
- Request ID pass-through or auto-generation
- Binary response detection to avoid log pollution

```go
r.Use(middleware.NewAccessLog(func(e *middleware.Entity) {
	// send to zap/slog/ELK
}, &middleware.AccessLogOptions{
	BaseOption: &middleware.AccessLogBaseOptions{
		RequestHeader:  true,
		RequestBody:    true,
		ResponseHeader: true,
		ResponseBody:   true,
		MaxBodyLength:  4096,
	},
	SkipPaths: []string{"GET:/ping"},
	SpecificPath: map[string]*middleware.AccessLogBaseOptions{
		"POST:/login": {
			RequestHeader:  true,
			RequestBody:    false,
			ResponseHeader: true,
			ResponseBody:   false,
			MaxBodyLength:  1024,
		},
	},
}))
```

### 3) Recovery Middleware

`middleware.NewRecovery(...)` provides panic recovery for production:

- Broken pipe / connection reset detection
- Configurable stack depth and frame skip
- Sensitive header masking
- Pluggable logging callback
- Custom panic response handler

```go
r.Use(middleware.NewRecovery(&middleware.RecoveryOptions{
	LogCallback: func(c *gin.Context, msg string) {
		// write msg to logger
	},
	Handler: func(c *gin.Context, err any) {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	},
	MaxStack:  50,
	StackSkip: 3,
}))
```

## Hookers

You can customize xgin lifecycle globally:

```go
xgin.SetHookerBindingError(func(ctx *gin.Context, err error) {
	ctx.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
})

xgin.SetHookerTransformResponse(func(r xgin.Response) any {
	return gin.H{"code": r.Code, "msg": r.Msg, "data": r.Data}
})

xgin.SetHookerBeforeResponse(func(ctx *gin.Context, r xgin.Response) bool {
	return false // return true to abort render
})

xgin.SetHookerAfterResponse(func(ctx *gin.Context, r xgin.Response) {
	// audit/log
})
```

## Response Content Types

`Response.WithContentType(...)` supports:

- JSON / IndentedJSON / SecureJSON / JSONP / AsciiJSON / PureJSON
- MsgPack / ProtoBuf / TOML / XML / YAML
- String / Redirect / HTML

## Development

```bash
go test ./...
go vet ./...
go build ./...
```

## License

MIT
