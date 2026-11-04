# xgin

[![Go Reference](https://pkg.go.dev/badge/github.com/lazyboon/xgin.svg)](https://pkg.go.dev/github.com/lazyboon/xgin)

Typed handlers, source-isolated request binding, recursive validation, and composable responses for [Gin](https://github.com/gin-gonic/gin). Each adapter owns its configuration; xgin does not change Gin's global settings.

## Getting started

Requires **Go 1.27.3 or later**, including generic method support.

```sh
go get github.com/lazyboon/xgin
```

```go
package main

import (
    "github.com/gin-gonic/gin"
    "github.com/lazyboon/xgin"
    "github.com/lazyboon/xgin/middleware"
)

type GreetingRequest struct {
    Name string `form:"name" binding:"required"`
}

func main() {
    adapter := xgin.MustNew(xgin.Config{})
    router := gin.New()
    router.Use(
        middleware.RequestID(middleware.RequestIDConfig{}),
        middleware.AccessLog(middleware.AccessLogConfig{}),
        middleware.ErrorHandler(middleware.ErrorHandlerConfig{}),
        middleware.Recovery(),
    )
    router.GET("/hello", adapter.Handler[GreetingRequest](
        func(ctx *xgin.Context, req *GreetingRequest) xgin.Response {
            return xgin.Response{Code: "OK", Msg: "success"}.
                WithData("Hello, " + req.Name).
                WithHeader("X-Request-ID", ctx.RequestID())
        },
    ))
    if err := router.Run(":8080"); err != nil {
        panic(err)
    }
}
```

`GET /hello?name=Gin` returns `{"code":"OK","msg":"success","data":"Hello, Gin"}`.

`Handler[T]` requires a struct request type, prepares its binding plan at registration, then binds, validates, and invokes the typed handler. Invalid DTO declarations and nil handlers panic at registration. The response type is inferred and may be any `IResponse` implementation. Use `Wrap` for a handler without automatic DTO binding.

`Context` embeds Gin's context and implements `context.Context`. Its `Deadline`, `Done`, `Err`, and `Value` methods use the standard request context captured when the wrapper is created. `GetValue[T]` provides typed lookup; Gin's `Set`/`Get` storage remains separate. Finish adding standard context values in middleware before entering the typed handler. Only standard context operations support concurrent calls; Gin helpers and response writes belong to synchronous request handling.

`Token()` uses the configured `ITokenExtractor`; the default extracts one well-formed Bearer credential. `BearerToken()` always uses that default. Extraction does not authenticate the caller.

## Binding and validation

```go
type UpdateRequest struct {
    ID    string                `uri:"id" binding:"required"`
    Trace string                `header:"X-Trace" binding:"required"`
    Name  string                `form:"name" json:"name" binding:"required"`
    File  *multipart.FileHeader `form:"file"`
}
```

Import `mime/multipart` for upload fields. Sources follow these rules:

| Tag | Source |
| --- | --- |
| `uri` | Gin route parameters |
| `header` | HTTP headers |
| `form` | Query on GET/HEAD; URL-encoded or multipart form on writes; query fallback on other writes |
| `json`, `xml`, `yaml`, `toml`, `codec`, `bson` | Explicitly declared body encodings on writes |
| `form` on a standard `multipart.FileHeader` shape | Real multipart file parts only |

A field may combine body encoding tags and `form`: GET/HEAD read its query value; writes select the body decoder. Form writes retain Gin's body/query merging rules. URI and header fields cannot also declare another source. `query`, `msgpack`, and `protobuf` request tags are rejected; use `form` and `codec` where appropriate. All body fields must share at least one supported encoding.

Only explicitly tagged top-level fields bind. Embedded value structs may flatten; embedded pointers are rejected. Named nested string-mapped structs require matching source tags on their exported children, and recursive form/URI/header layouts are rejected. Gin binding-tag validation runs on each source separately, so cross-source rules belong in `IValidator`:

```go
func (req *UpdateRequest) Validate() error {
    if req.Name == req.ID {
        return errors.New("name must differ from ID")
    }
    return nil
}
```

Import `errors` for this rule. The default validator calls parent rules before children and visits exported fields, slice/array entries, and both map keys and values. Pointer-receiver rules work for map entries too. Nil values are skipped; cycles and excessive nesting return `ErrMaxDepth`. Rules must not mutate their receiver or referenced data. Validation returns the first rule error; traversal order between map entries is unspecified.

Uploads support standard `multipart.FileHeader`, pointers, and their direct slices or fixed arrays. Arrays require an exact file count. Use `*multipart.FileHeader` with `binding:"required"` to require presence. Query/form metadata cannot forge a file header. Named nested upload fields, defined lookalikes, and extra wrappers are rejected. `Handler` and `Wrap` remove multipart temporary files on completion, including binding failures and panics. With direct `AutoBind`, call `Request.MultipartForm.RemoveAll()` after finishing with uploads. Copy uploads to application-owned storage before returning if they must persist.

The default binder buffers only bodies needed by the DTO, up to 1 MiB by default, including multipart overhead. GET/HEAD ignore the body. JSON must contain exactly one complete document. Relevant write requests require an accepted, unambiguous `Content-Type`; MIME case and parameters are normalized. A binding failure leaves the target unchanged; use a fresh DTO for each request. Custom validation runs after binding and does not roll back a successfully bound DTO.

## Responses

`Response` defaults to HTTP 200 and a JSON `code`/`msg`/`data` envelope. Its `With...` methods return copies; header maps and error slices detach when changed. Payload data stays caller-owned and must remain unchanged during encoding.

```go
success := xgin.Response{Code: "OK", Msg: "success"}
output := success.WithStatus(http.StatusCreated).WithData(result)
```

`Config.TransformResponse` shapes default JSON envelopes, including JSON variants. It receives a copy and must support concurrent calls without mutating it:

```go
adapter := xgin.MustNew(xgin.Config{
    TransformResponse: func(response xgin.Response) any {
        return gin.H{"payload": response.Data, "message": response.Msg}
    },
})
```

For third-party JSON contracts, return `xgin.JSON(status, body)` or `response.WithBody(body)`: both bypass the envelope and transformer. Custom `IResponse` values and non-JSON formats also bypass transformation.

Other outputs include `Text`, `HTML`, `Redirect`, `Empty`, `File`, and `Download`. `WithContentType` selects JSON variants, XML, YAML, TOML, MessagePack, or protobuf; non-JSON encoders use `Data` directly. Protobuf output requires a protobuf message. The `nomsgpack` build tag disables MessagePack binding/rendering. `JSONP` takes an explicit ASCII identifier or dotted callback path and never trusts a query callback automatically.

`Render` adapts a finite Gin renderer. Finite responses encode completely before committing, validate status/headers, and compute their content length; HEAD sends the equivalent headers without a body. HTML uses templates registered on the Gin engine. `File`/`Download` stream application-authorized regular files with Range, HEAD, and conditional-request support. Authorize the file path in application code.

Implement `IResponse.Render(*gin.Context) error` or use `ResponseFunc` for other protocols and streaming. A successful implementation must commit its output. Never retain the Gin context after returning. Rendering ends the remaining route handler chain. Uncommitted failures restore existing headers, record the error, and leave status 500 for outer error middleware; committed output is never replaced. `WithError` records internal causes without serializing them.

## Configuration and errors

Use `New(Config)` for checked construction or `MustNew(Config)` during startup. Zero configuration selects a 1 MiB request-body limit, validation depth 20, the default binder/validator, a status-only request error mapper, and Bearer extraction. Default validation depth configuration accepts 0–128; 0 selects 20. Negative body limits are invalid. Custom binders and validators own their corresponding limit policies. Typed-nil dependencies select defaults.

Replace `IBinder`, optional `IBinderPreparer`, `IRequestValidator`, `IRequestErrorMapper`, or `ITokenExtractor` through `Config`. Dependencies and callbacks must support concurrent use; do not modify their configuration after route registration begins. Binder plans and validator metadata remain cached for the instance lifetime, so use a **bounded set of DTO types** rather than generating new reflection types for each request.

The default request mapper returns an empty response:

| Failure | Status |
| --- | --- |
| Binding, decoding, or ordinary custom rule error | 400 |
| Body limit exceeded | 413 |
| Missing/unsupported body content type | 415 |
| Invalid target, validation depth, or validation panic | 500 |

Use `RequestErrorMapperFunc` to choose a public error format. `errors.Is`/`errors.As` preserve categories and wrapped causes; `PanicError` retains internal diagnostics and a stack. Never expose raw decoder or panic diagnostics unless that is an intentional application policy. Handler panics belong to recovery middleware.

## Middleware

Register in the order shown above: **RequestID → AccessLog → ErrorHandler → Recovery → routes**. This lets the logger see final errors/statuses and lets the error handler select a response after recovery. Configure `ErrorHandlerConfig.Respond` to customize uncommitted internal errors; its default is an empty 500. Error handling preserves already committed responses and broken connections.

Request IDs are generated by default. Incoming IDs are accepted only with `TrustIncoming: true` and valid syntax; apply that setting at a trusted boundary. `Context.RequestID()` reads only the trusted standard context value.

Access logs use `log/slog` and record request start/completion. Header/body capture is off by default. Optional capture is bounded (4096 bytes by default; maximum 1 MiB), observes request bytes only as the application reads them, and redacts credential headers and sensitive JSON/form keys. Configure `RedactHeaders` and `RedactKeys` for application-specific secrets. Incomplete, malformed, oversized, compressed, binary, attachment, and streaming bodies are omitted. URLs log the path without query values. Internal errors/stacks remain server-side; custom error messages can still contain sensitive details.

## Migrating from earlier v1 releases

This update retains the v1 module path but changes APIs. Review these changes before upgrading:

- Keep the `github.com/lazyboon/xgin` import path and upgrade the Go toolchain.
- Create an adapter with `New`/`MustNew`; register typed `Handler[T]` or `Wrap` handlers returning `IResponse`.
- Move global hooks into instance configuration and middleware. The current API has no `globalhookers` API.
- Use explicit source tags; query uses `form`. Put cross-source validation in `IValidator`, and account for registration-time errors for unsupported layouts.
- Use `Response` templates and `With...` methods. Use `JSON`/`WithBody` for explicit external payloads, and configure envelope transforms per instance.
- Choose request-error mapping and install middleware in the documented order. Standard context values and Gin storage have separate lookup APIs.

## Development

Default tests use local `httptest` fixtures and require no database or external service.

```sh
go test -race ./...
go test -tags nomsgpack ./...
go vet ./...
```

See [LICENSE](LICENSE) for licensing terms.
