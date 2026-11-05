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
        middleware.AccessLog(middleware.AccessLogConfig{
            Default: middleware.AccessLogPolicy{Start: true, Finish: true},
        }),
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

### Access logging

`AccessLog` separates **event selection**, **payload capture**, and **output**:

- `Default` controls start/completion events and optional request/response headers and bodies. Its zero value disables all events and capture; set `Start` and/or `Finish` explicitly. Payload capture is off unless explicitly enabled.
- `Resolve` runs before the handler and receives a policy copy. Match `c.Request.Method` and `c.FullPath()` (the route template, such as `/files/:id`, not the concrete URL). Returning an empty policy skips logging and capture. Unmatched routes have an empty template and still use the default policy.
- `ShouldLog` filters each sanitized event before output. To log only failures or slow requests, disable `Start` for that route; a later result cannot retract an already emitted start event.
- `Logger` accepts `IAccessLogger`, independent of Gin, slog, and zap. It defaults to slog. `AccessLoggerFunc` adapts a function, and `NewSlogAccessLogger` adapts a standard logger. Callbacks/loggers must support concurrent requests; they must not retain the Gin or standard context.

```go
engine.Use(middleware.AccessLog(middleware.AccessLogConfig{
    Logger:       middleware.NewSlogAccessLogger(slog.Default()),
    HeaderFormat: middleware.HeaderFormatCompact,
    Default: middleware.AccessLogPolicy{
        Start:  true,
        Finish: true,
        Request: middleware.AccessLogContent{
            MaxBodyBytes: 4096,
            BodyMode:     middleware.RequestBodyBuffered,
            BodyFormat:   middleware.BodyFormatStructured,
        },
        Response: middleware.AccessLogContent{
            MaxBodyBytes: 8192,
        },
    },
    RedactHeaders: []string{"X-Partner-Secret"},
    RedactKeys:    []string{"identity_number"},
    Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
        switch c.FullPath() {
        case "/health", "/ready":
            return middleware.AccessLogPolicy{}
        case "/auth/user/login":
            policy.Request.Body = true
            policy.Response.Headers = true
            // Credentials are masked; token response bodies are not captured.
            policy.Response.Body = false
        case "/files/:id/download", "/files/upload":
            policy.Request.Headers = true
            policy.Response.Headers = true
            policy.Request.Body = false
            policy.Response.Body = false
        case "/connection/gyg/booking":
            if c.Request.Method == http.MethodPost {
                policy.Start = false
                policy.Request.Body = true
                policy.Response.Body = true
            }
        }
        return policy
    },
    ShouldLog: func(record middleware.AccessRecord) bool {
        conditional := record.Route == "/connection/gyg/booking" && record.Method == http.MethodPost
        if !conditional {
            return true
        }
        return record.Status >= 400 || record.Error != nil || record.Duration >= 2*time.Second
    },
}))
```

There is one policy path: `Default` sets the base and `Resolve` may replace it for each request. An empty policy disables all events, both in `Default` and when returned by `Resolve`. There are no implicit event defaults. Zero body limits use 4096 bytes, independently for each direction; limits must be between 1 byte and 1 MiB. Invalid static configuration panics during construction; policies returned by `Resolve` must also use valid limits.

`HeaderFormatCompact` (the default) emits a single value as a string and multiple values as `[]string`. `HeaderFormatMulti` retains `[]string` values for every header; values are never comma-joined. Each event owns detached, sanitized header snapshots.

Bodies are included **only in completion events**. Disabling `Body` or `Finish` disables capture and read-ahead. A result-based filter still requires capture before the result is known. URLs contain paths without query values. Built-in credential masks cannot be disabled; configure `RedactHeaders` and `RedactKeys` for additional secrets. Internal error messages and stacks may contain application secrets; decide whether to retain those in your backend.

#### Request capture modes

Set `Default.Request.BodyMode`, or change it per route in `Resolve`:

| Mode | Behavior |
| --- | --- |
| `RequestBodyPassive` (default) | Observes only bytes consumed by downstream code. Empty DTOs or early returns may leave the body unread. |
| `RequestBodyBuffered` | Reads at most `MaxBodyBytes + 1` before downstream code and restores the consumed prefix and any read error. Complete small bodies are available even if the handler never reads them. |

Buffered mode skips declared oversized bodies without reading ahead. For unknown lengths it reads one extra byte to detect overflow, then replays that prefix followed by the untouched remainder. It preserves `ContentLength`, `GetBody`, forwarded `Close`, and access to read errors; read chunk boundaries may change. The log limit does not reject, truncate, or limit the business request. Normal request-size and timeout controls still belong to the HTTP server/binder. Buffered mode waits for the bounded prefix before invoking downstream middleware, which can trigger `100 Continue`; choose passive mode for handlers that must authorize or process streams before reading. Unsupported payloads (including uploads, compressed bodies, and event streams) are never read ahead. `BodyMode` has no effect on response capture; responses are observed as they are written.

Complete JSON and URL-encoded forms are redacted. Unsupported, oversized, incomplete, or malformed payloads are omitted. Markers distinguish `body not read`, `incomplete body`, and `body capture failed`; no partial raw data or raw transport error is substituted for these markers.

#### Body output formats

Set `BodyFormat` independently on `Default.Request` and `Default.Response`, or override it per route:

| Format | Complete JSON body representation |
| --- | --- |
| `BodyFormatStructured` (default) | `json.RawMessage` containing freshly encoded, sanitized JSON. Objects/arrays/scalars remain JSON values; explicit `null` remains present. |
| `BodyFormatText` | A string containing the same sanitized JSON. Useful when log storage needs a stable text field across routes. |

Structured slog output is directly searchable without decoding an escaped JSON string:

```json
{
  "request_body": {"email": "user@example.com", "password": "[REDACTED]"},
  "response_body": {"code": "OK", "data": {"access_token": "[REDACTED]"}}
}
```

Both formats validate complete JSON, redact it, and enforce the limit again after encoding; large JSON integers retain their precision. Form bodies and omission markers remain strings. Arbitrary JSON body shapes can conflict with strict dynamic field mappings in log indexes; configure those fields as appropriate for your collector, or select `BodyFormatText` consistently. Neither mode logs an unredacted raw-body prefix.

For zap or another backend, implement the single-method interface or use the function adapter. This example belongs in the application; xgin does not import zap:

```go
output := middleware.AccessLoggerFunc(func(_ context.Context, record middleware.AccessRecord) {
    level := zapcore.InfoLevel
    switch record.Level {
    case middleware.AccessLogWarn:
        level = zapcore.WarnLevel
    case middleware.AccessLogError:
        level = zapcore.ErrorLevel
    }
    fields := record.Fields()
    attrs := make([]zap.Field, 0, len(fields))
    for name, value := range fields {
        attrs = append(attrs, zap.Any(name, value))
    }
    logger.Log(level, record.Message(), attrs...)
})
// Pass output as AccessLogConfig.Logger.
```

`AccessRecord.Fields()` returns backend-neutral structured fields and omits disabled payloads. Start events omit status/duration; completion severity is Error for internal errors or status >= 500, Warn for status >= 400, otherwise Info. The backend owns buffering, output failures, flushing, and shutdown; the middleware does not close a shared logger. Records may be retained, but handler errors must not be mutated. Middleware callbacks and output run synchronously on the request goroutine; their panics are not swallowed.

### Upgrading to v1.5.0

`AccessRecord.RequestBody` and `ResponseBody` now use `any` instead of `*string`: nil means disabled, `json.RawMessage` means complete structured JSON, and string means text/form data or an omission marker. Custom loggers should use `record.Fields()` with a JSON-aware encoder (as slog and zap do), rather than converting values to strings or byte slices. Filters that inspect bodies must account for these types. To retain textual JSON output, set `BodyFormatText` for both directions.

### Upgrading to v1.4.0

**v1.4.0 intentionally includes incompatible access-log API changes while retaining the v1 module path.** Pin the previous version until your configuration is updated.

- Pass an `IAccessLogger` as `Logger`; wrap a `*slog.Logger` with `NewSlogAccessLogger`. The transitional `Output` field has been removed.
- Set `Default.Start` and/or `Default.Finish` explicitly. A zero config now logs nothing.
- Set capture flags and limits through `Default.Request` and `Default.Response`; flat capture fields have been removed.
- Replace `Skip` with an empty policy returned by `Resolve`. Replace `GetLogger` with a custom `IAccessLogger` that selects its backend from standard context.
- Header values are compact by default. Select `HeaderFormatMulti` if downstream tools require arrays for single values.

## Migrating from earlier v1 releases

For the v1.4.0 access-log changes, see the upgrade section above. Version v1.2.0 also retained the v1 module path while changing APIs; review the following when upgrading from v1.1.x:

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
