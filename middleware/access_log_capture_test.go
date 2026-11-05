package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
	"github.com/lazyboon/xgin/middleware"
)

type bufferedBodyCase struct {
	name           string
	payload        string
	declaredLength int64
	limit          int
	prefetched     int
	marker         string
}

func TestBufferedRequestPreservesPayloadAndLimits(t *testing.T) {
	cases := []bufferedBodyCase{
		{name: "known empty object", payload: "{}", declaredLength: 2, limit: 64, prefetched: 2},
		{name: "unknown length", payload: "{}", declaredLength: -1, limit: 64, prefetched: 2},
		{name: "exact limit", payload: "{}", declaredLength: -1, limit: 2, prefetched: 2},
		{name: "single byte limit", payload: "1", declaredLength: -1, limit: 1, prefetched: 1},
		{
			name: "declared oversized", payload: "{} ", declaredLength: 3, limit: 2,
			marker: "[omitted: body exceeds log limit]",
		},
		{
			name: "unknown oversized", payload: "{}   ", declaredLength: -1, limit: 2, prefetched: 3,
			marker: "[omitted: body exceeds log limit]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger := new(recordBuffer)
			original := &trackedBody{reader: strings.NewReader(tc.payload)}
			router := gin.New()
			router.Use(middleware.AccessLog(middleware.AccessLogConfig{
				Logger: logger,
				Default: middleware.AccessLogPolicy{
					Finish: true,
					Request: middleware.AccessLogContent{
						Body: true, BodyMode: middleware.RequestBodyBuffered, MaxBodyBytes: tc.limit,
					},
				},
			}))
			router.POST("/", func(c *gin.Context) {
				if original.bytesRead != tc.prefetched || c.Request.ContentLength != tc.declaredLength {
					t.Fatalf("prefetched=%d length=%d", original.bytesRead, c.Request.ContentLength)
				}
				// Reading GetBody must remain independent of the restored live stream.
				duplicate, err := c.Request.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				copyData, copyErr := io.ReadAll(duplicate)
				_ = duplicate.Close()
				body, readErr := io.ReadAll(c.Request.Body)
				if copyErr != nil || readErr != nil || string(body) != tc.payload || string(copyData) != tc.payload {
					t.Fatalf("replayed=%q duplicate=%q errors=%v/%v", body, copyData, readErr, copyErr)
				}
				if err := c.Request.Body.Close(); err != nil {
					t.Fatal(err)
				}
				c.Status(204)
			})
			request := httptest.NewRequest("POST", "/", nil)
			request.Body = original
			request.ContentLength = tc.declaredLength
			request.Header.Set("Content-Type", "application/json")
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(tc.payload)), nil
			}
			router.ServeHTTP(httptest.NewRecorder(), request)
			if !original.closed || original.bytesRead != len(tc.payload) {
				t.Fatal("original body was not fully consumed and closed")
			}
			body := logger.snapshot()[0].RequestBody
			if tc.marker != "" {
				if body != tc.marker {
					t.Fatalf("marker=%v want=%s", body, tc.marker)
				}
				return
			}
			if raw, ok := body.(json.RawMessage); !ok || string(raw) != strings.TrimSpace(tc.payload) {
				t.Fatalf("structured body=%T %v", body, body)
			}
		})
	}
}

type emptyLogRequest struct{}

func TestRequestCaptureModesWithEmptyDTO(t *testing.T) {
	logger := new(recordBuffer)
	adapter := xgin.MustNew(xgin.Config{})
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
		Default: middleware.AccessLogPolicy{
			Finish:  true,
			Request: middleware.AccessLogContent{Body: true},
		},
		Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
			if c.Param("mode") == "buffered" {
				policy.Request.BodyMode = middleware.RequestBodyBuffered
			}
			return policy
		},
	}))
	router.POST("/:mode", adapter.Handler[emptyLogRequest](func(*xgin.Context, *emptyLogRequest) xgin.Response {
		return xgin.Response{Code: "OK"}
	}))
	for _, mode := range []string{"passive", "buffered"} {
		request := httptest.NewRequest("POST", "/"+mode, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(httptest.NewRecorder(), request)
	}
	records := logger.snapshot()
	if len(records) != 2 || records[0].RequestBody != "[omitted: body not read]" {
		t.Fatalf("passive empty DTO=%+v", records)
	}
	if raw, ok := records[1].RequestBody.(json.RawMessage); !ok || string(raw) != "{}" {
		t.Fatalf("buffered empty DTO=%+v", records[1])
	}
}

// interruptedReader reports a one-shot error with prefix bytes, then recovers.
type interruptedReader struct {
	prefix *strings.Reader
	tail   *strings.Reader
	err    error
}

func (r *interruptedReader) Read(data []byte) (int, error) {
	if r.prefix.Len() > 0 {
		n, err := r.prefix.Read(data)
		if r.prefix.Len() == 0 {
			err = r.err
			r.err = nil
		}
		return n, err
	}
	return r.tail.Read(data)
}

func TestBufferedRequestReplaysReadAndCloseErrors(t *testing.T) {
	logger := new(recordBuffer)
	readErr := context.Canceled
	closeErr := errors.New("close failed")
	original := &trackedBody{
		reader: &interruptedReader{
			prefix: strings.NewReader(`{"safe":1}`), tail: strings.NewReader("tail"), err: readErr,
		},
		closeErr: closeErr,
	}
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
		Default: middleware.AccessLogPolicy{
			Finish:  true,
			Request: middleware.AccessLogContent{Body: true, BodyMode: middleware.RequestBodyBuffered},
		},
	}))
	router.POST("/", func(c *gin.Context) {
		// A zero-length read must not consume the saved error.
		if n, err := c.Request.Body.Read(nil); n != 0 || err != nil {
			t.Fatalf("empty read=%d/%v", n, err)
		}
		prefix, err := io.ReadAll(c.Request.Body)
		if string(prefix) != `{"safe":1}` || !errors.Is(err, readErr) {
			t.Fatalf("prefix/error changed: %q/%v", prefix, err)
		}
		tail, err := io.ReadAll(c.Request.Body)
		if string(tail) != "tail" || err != nil {
			t.Fatalf("recovery changed: %q/%v", tail, err)
		}
		if err := c.Request.Body.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("close error changed: %v", err)
		}
		c.Status(204)
	})
	request := httptest.NewRequest("POST", "/", nil)
	request.Body = original
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	if !original.closed || logger.snapshot()[0].RequestBody != "[omitted: body capture failed]" {
		t.Fatal("read failure was hidden")
	}
}

func TestBufferedRequestSkipsUnsupportedPayloads(t *testing.T) {
	logger := new(recordBuffer)
	original := &trackedBody{reader: strings.NewReader("private binary")}
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
		Default: middleware.AccessLogPolicy{
			Finish:  true,
			Request: middleware.AccessLogContent{Body: true, BodyMode: middleware.RequestBodyBuffered},
		},
	}))
	router.POST("/", func(c *gin.Context) {
		if original.reads != 0 || c.Request.Body != original {
			t.Fatal("unsupported payload read ahead or wrapped")
		}
		c.Status(204)
	})
	request := httptest.NewRequest("POST", "/", nil)
	request.Body = original
	request.Header.Set("Content-Type", "application/octet-stream")
	router.ServeHTTP(httptest.NewRecorder(), request)
	if logger.snapshot()[0].RequestBody != "[omitted: unsupported payload]" {
		t.Fatal("unsupported payload logged")
	}
}

type bodyFormatCase struct {
	name    string
	payload string
}

func TestJSONBodyFormats(t *testing.T) {
	cases := []bodyFormatCase{
		{name: "object", payload: `{"password":"hidden","id":9007199254740993}`},
		{name: "array", payload: `[{"password":"hidden"}]`},
		{name: "null", payload: `null`},
		{name: "number", payload: `9007199254740993`},
		{name: "boolean", payload: `true`},
		{name: "string", payload: `"a value"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, format := range []middleware.BodyFormat{middleware.BodyFormatStructured, middleware.BodyFormatText} {
				logger, buffer := newLogger()
				router := gin.New()
				router.Use(middleware.AccessLog(middleware.AccessLogConfig{
					Logger: middleware.NewSlogAccessLogger(logger),
					Default: middleware.AccessLogPolicy{
						Finish: true,
						Request: middleware.AccessLogContent{
							Body: true, BodyMode: middleware.RequestBodyBuffered, BodyFormat: format,
						},
						Response: middleware.AccessLogContent{Body: true, BodyFormat: format},
					},
				}))
				router.POST("/", func(c *gin.Context) {
					c.Data(http.StatusOK, "application/json", []byte(tc.payload))
				})
				request := httptest.NewRequest("POST", "/", strings.NewReader(tc.payload))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(httptest.NewRecorder(), request)
				if strings.Contains(buffer.snapshot(), "hidden") {
					t.Fatal("credential leaked")
				}
				var event map[string]json.RawMessage
				if err := json.Unmarshal([]byte(buffer.snapshot()), &event); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{"request_body", "response_body"} {
					value, exists := event[field]
					if !exists {
						t.Fatalf("%s missing for %s", field, tc.name)
					}
					encoded := string(value)
					if format == middleware.BodyFormatText {
						if err := json.Unmarshal(value, &encoded); err != nil {
							t.Fatalf("%s was not text: %s", field, value)
						}
					}
					if !json.Valid([]byte(encoded)) || strings.Contains(encoded, "hidden") {
						t.Fatalf("unsafe or invalid %s: %s", field, encoded)
					}
					if strings.Contains(tc.payload, "9007199254740993") && !strings.Contains(encoded, "9007199254740993") {
						t.Fatalf("large integer precision lost: %s", encoded)
					}
					if tc.name == "object" && format == middleware.BodyFormatStructured && value[0] != '{' {
						t.Fatalf("object encoded as a string: %s", value)
					}
				}
			}
		})
	}
}

func TestBufferedInvalidJSONIsNeverLoggedRaw(t *testing.T) {
	cases := []bodyFormatCase{
		{name: "incomplete JSON", payload: `{"password":"hidden"`},
		{name: "multiple documents", payload: `{}{"password":"hidden"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger := new(recordBuffer)
			router := gin.New()
			router.Use(middleware.AccessLog(middleware.AccessLogConfig{
				Logger: logger,
				Default: middleware.AccessLogPolicy{
					Finish:  true,
					Request: middleware.AccessLogContent{Body: true, BodyMode: middleware.RequestBodyBuffered},
				},
			}))
			router.POST("/", func(c *gin.Context) { c.Status(204) })
			request := httptest.NewRequest("POST", "/", strings.NewReader(tc.payload))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(httptest.NewRecorder(), request)
			if logger.snapshot()[0].RequestBody != "[omitted: invalid json]" {
				t.Fatal("invalid payload was logged")
			}
		})
	}
}

func TestJSONRedactionExpansionRespectsLimit(t *testing.T) {
	const payload = `{"pwd":1}`
	logger := new(recordBuffer)
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: logger,
		Default: middleware.AccessLogPolicy{
			Finish: true,
			Request: middleware.AccessLogContent{
				Body: true, BodyMode: middleware.RequestBodyBuffered, MaxBodyBytes: len(payload),
			},
		},
	}))
	router.POST("/", func(c *gin.Context) { c.Status(204) })
	request := httptest.NewRequest("POST", "/", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	if logger.snapshot()[0].RequestBody != "[omitted: redacted body exceeds log limit]" {
		t.Fatal("expanded redacted JSON exceeded the limit")
	}
}
