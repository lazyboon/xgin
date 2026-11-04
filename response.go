package xgin

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	ginrender "github.com/gin-gonic/gin/render"
)

// ContentType selects a built-in response encoder. JSON is the zero value.
// Use Render with a Gin renderer, or implement IResponse, for other formats.
type ContentType uint8

const (
	ContentTypeJSON ContentType = iota
	ContentTypeIndentedJSON
	ContentTypeSecureJSON
	ContentTypeJSONP
	ContentTypeASCIIJSON
	ContentTypePureJSON
	ContentTypeXML
	ContentTypeYAML
	ContentTypeTOML
	ContentTypeMsgPack
	ContentTypeProtoBuf
	ContentTypeString
	ContentTypeRedirect
	ContentTypeHTML
	ContentTypeNone
)

// Response is a reusable response template with value-based With methods.
// JSON encodes Code, Msg and Data unless WithBody supplies an explicit payload.
// Other encoders use Data directly. Metadata and internal errors are never serialized.
// With methods copy headers and error slices before changing them; application data
// remains caller-owned and must not be mutated while a response is being encoded.
type Response struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`

	HTTPStatus  int         `json:"-"`
	Header      http.Header `json:"-"`
	Errors      []error     `json:"-"`
	ContentType ContentType `json:"-"`
	Template    string      `json:"-"`
	Callback    string      `json:"-"`

	bodyOnly bool
}

var _ IResponse = Response{}

// JSON returns a JSON response containing exactly body, without an envelope.
func JSON(status int, body any) Response {
	return Response{HTTPStatus: status, Data: body, bodyOnly: true}
}

// JSONP returns an explicit callback response; no query parameter is read implicitly.
// Callback must be an ASCII identifier or dotted identifier path.
func JSONP(status int, callback string, body any) Response {
	return Response{HTTPStatus: status, Data: body, ContentType: ContentTypeJSONP, Callback: callback, bodyOnly: true}
}

// Text returns literal text; percent signs are never interpreted as formatting verbs.
func Text(status int, text string) Response {
	return Response{HTTPStatus: status, Data: text, ContentType: ContentTypeString}
}

// HTML renders a template registered through Gin's HTML configuration.
func HTML(status int, name string, data any) Response {
	return Response{HTTPStatus: status, Template: name, Data: data, ContentType: ContentTypeHTML}
}

// Redirect returns a validated HTTP redirect; zero status selects 302.
func Redirect(status int, location string) Response {
	return Response{HTTPStatus: status, Data: location, ContentType: ContentTypeRedirect}
}

// Empty returns a status and headers without a response body.
func Empty(status int) Response { return Response{HTTPStatus: status, ContentType: ContentTypeNone} }

// WithStatus returns a copy with an explicit final HTTP status.
func (r Response) WithStatus(status int) Response { r.HTTPStatus = status; return r }

// WithCode returns a copy with a public application code.
func (r Response) WithCode(code string) Response { r.Code = code; return r }

// WithMsg returns a copy with a public message.
func (r Response) WithMsg(msg string) Response { r.Msg = msg; return r }

// WithData returns a copy with caller-owned application data.
func (r Response) WithData(data any) Response { r.Data = data; return r }

// WithBody bypasses the default JSON envelope and instance transformation.
// Use it for third-party JSON contracts or other explicitly shaped payloads.
func (r Response) WithBody(body any) Response { r.Data = body; r.bodyOnly = true; return r }

// WithHeader replaces one header in a detached copy of the header map.
func (r Response) WithHeader(name, value string) Response {
	r.Header = r.Header.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	r.Header.Set(name, value)
	return r
}

// WithHeaders replaces supplied header values without sharing their slices.
func (r Response) WithHeaders(headers http.Header) Response {
	r.Header = r.Header.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	applyRenderHeaders(r.Header, headers)
	return r
}

// WithError appends an internal error without changing the template's error slice.
func (r Response) WithError(err error) Response { return r.WithErrors(err) }

// WithErrors appends non-nil internal errors to a detached slice.
func (r Response) WithErrors(causes ...error) Response {
	r.Errors = append([]error(nil), r.Errors...)
	for _, cause := range causes {
		if !isNilInterface(cause) {
			r.Errors = append(r.Errors, cause)
		}
	}
	return r
}

// WithContentType returns a copy using a built-in encoder.
func (r Response) WithContentType(kind ContentType) Response { r.ContentType = kind; return r }

// WithCallback selects JSONP with an explicit callback name instead of a request query.
// Invalid callback paths fail before any response is committed.
func (r Response) WithCallback(callback string) Response {
	r.Callback = callback
	r.ContentType = ContentTypeJSONP
	return r
}

// WithTemplate selects a template registered on the Gin engine and uses HTML.
func (r Response) WithTemplate(name string) Response {
	r.Template = name
	r.ContentType = ContentTypeHTML
	return r
}

// WithoutCache disables storage by browsers and intermediary caches.
func (r Response) WithoutCache() Response {
	return r.WithHeader("Cache-Control", "no-store").WithHeader("Pragma", "no-cache")
}

// Render encodes the complete response before committing a finite response body.
func (r Response) Render(c *gin.Context) error {
	if c == nil || c.Request == nil || c.Writer == nil {
		return fmt.Errorf("%w: missing response context", ErrInvalidRender)
	}
	for _, cause := range r.Errors {
		if !isNilInterface(cause) {
			_ = c.Error(cause)
		}
	}
	renderer, err := r.renderer(c)
	if err != nil {
		return err
	}
	status := r.HTTPStatus
	if status == 0 && r.ContentType == ContentTypeRedirect {
		status = http.StatusFound
	}
	return (Render{HTTPStatus: status, Header: r.Header, Renderer: renderer}).Render(c)
}

// payload is shared by JSON variants; explicit payloads never gain an envelope.
func (r Response) payload() any {
	if r.bodyOnly {
		return r.Data
	}
	return r
}

// isJSON identifies formats to which instance-level JSON transformation applies.
func (r Response) isJSON() bool {
	switch r.ContentType {
	case ContentTypeJSON, ContentTypeIndentedJSON, ContentTypeSecureJSON, ContentTypeJSONP, ContentTypeASCIIJSON, ContentTypePureJSON:
		return true
	default:
		return false
	}
}

// renderer selects a native Gin renderer without writing to the live connection.
func (r Response) renderer(c *gin.Context) (ginrender.Render, error) {
	switch r.ContentType {
	case ContentTypeJSON:
		return ginrender.JSON{Data: r.payload()}, nil
	case ContentTypeIndentedJSON:
		return ginrender.IndentedJSON{Data: r.payload()}, nil
	case ContentTypeSecureJSON:
		return ginrender.SecureJSON{Prefix: "while(1);", Data: r.payload()}, nil
	case ContentTypeJSONP:
		if !r.validCallback() {
			return nil, fmt.Errorf("%w: invalid JSONP callback path", ErrInvalidRender)
		}
		return ginrender.JsonpJSON{Callback: r.Callback, Data: r.payload()}, nil
	case ContentTypeASCIIJSON:
		return ginrender.AsciiJSON{Data: r.payload()}, nil
	case ContentTypePureJSON:
		return ginrender.PureJSON{Data: r.payload()}, nil
	case ContentTypeXML:
		return ginrender.XML{Data: r.Data}, nil
	case ContentTypeYAML:
		return ginrender.YAML{Data: r.Data}, nil
	case ContentTypeTOML:
		return ginrender.TOML{Data: r.Data}, nil
	case ContentTypeMsgPack:
		return newMsgPackRenderer(r.Data)
	case ContentTypeProtoBuf:
		return ginrender.ProtoBuf{Data: r.Data}, nil
	case ContentTypeString:
		text, ok := r.Data.(string)
		if !ok {
			return nil, fmt.Errorf("%w: text data must be a string", ErrInvalidRender)
		}
		return ginrender.String{Format: "%s", Data: []any{text}}, nil
	case ContentTypeRedirect:
		location, ok := r.Data.(string)
		if !ok || location == "" {
			return nil, fmt.Errorf("%w: redirect location must be a nonempty string", ErrInvalidRender)
		}
		status := r.HTTPStatus
		if status == 0 {
			status = http.StatusFound
		}
		switch status {
		case 300, 301, 302, 303, 305, 307, 308:
		default:
			return nil, fmt.Errorf("%w: invalid redirect status %d", ErrInvalidRender, status)
		}
		return ginrender.Redirect{Code: status, Location: location, Request: c.Request}, nil
	case ContentTypeHTML:
		if r.Template == "" {
			return nil, fmt.Errorf("%w: HTML template is required", ErrInvalidRender)
		}
		return htmlRenderer{context: c, name: r.Template, data: r.Data, status: r.HTTPStatus}, nil
	case ContentTypeNone:
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: unsupported content type %d", ErrInvalidRender, r.ContentType)
	}
}

// validCallback permits function paths without executable expressions or comments.
// An empty callback follows Gin's behavior of returning only the encoded JSON.
func (r Response) validCallback() bool {
	startsSegment := true
	for _, char := range r.Callback {
		if char == '.' {
			if startsSegment {
				return false
			}
			startsSegment = true
			continue
		}
		letter := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == '_' || char == '$'
		digit := char >= '0' && char <= '9'
		if !letter && (!digit || startsSegment) {
			return false
		}
		startsSegment = false
	}
	return r.Callback == "" || !startsSegment
}

// ResponseFunc adapts a custom output function, including streaming, to IResponse.
// It must commit the response before returning nil and must not retain the context.
type ResponseFunc func(*gin.Context) error

// Render invokes the custom response implementation.
func (f ResponseFunc) Render(c *gin.Context) error {
	if f == nil {
		return errors.New("xgin: response function is nil")
	}
	return f(c)
}
