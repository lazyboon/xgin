package middleware_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin/middleware"
)

func init() { gin.SetMode(gin.ReleaseMode) }

type logBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func (b *logBuffer) snapshot() string {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.String()
}

func newLogger() (*slog.Logger, *logBuffer) {
	b := new(logBuffer)
	return slog.New(slog.NewJSONHandler(b, nil)), b
}

func entries(t *testing.T, b *logBuffer) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.snapshot()), "\n") {
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatal(err)
		}
		result = append(result, item)
	}
	return result
}

type trackedBody struct {
	reader   io.Reader
	reads    int
	closed   bool
	closeErr error
}

func (r *trackedBody) Read(p []byte) (int, error) { r.reads++; return r.reader.Read(p) }

func (r *trackedBody) Close() error { r.closed = true; return r.closeErr }

type failingReader struct {
	sent bool
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, r.err
	}
	r.sent = true
	return copy(p, `{"password":"hidden"`), r.err
}

func TestCapturePreservesRequestStream(t *testing.T) {
	logger, b := newLogger()
	sentinel := errors.New("read failure")
	closeErr := errors.New("close failure")
	body := &trackedBody{reader: &failingReader{err: sentinel}, closeErr: closeErr}
	r := gin.New()
	r.Use(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, RequestBody: true}))
	r.POST("/", func(c *gin.Context) {
		if body.reads != 0 {
			t.Fatal("logging read ahead")
		}
		data, err := io.ReadAll(c.Request.Body)
		if !errors.Is(err, sentinel) || string(data) != `{"password":"hidden"` {
			t.Fatalf("stream changed: %q %v", data, err)
		}
		if err := c.Request.Body.Close(); !errors.Is(err, closeErr) {
			t.Fatal("close error changed")
		}
		c.Status(204)
	})
	req := httptest.NewRequest("POST", "/", nil)
	req.Body = body
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)
	if !body.closed || strings.Contains(b.snapshot(), "hidden") {
		t.Fatalf("unsafe capture: %s", b.snapshot())
	}
}

func TestStructuredCaptureAndRedaction(t *testing.T) {
	logger, b := newLogger()
	headers := []string{"X-Custom-Secret"}
	keys := []string{"private_value"}
	config := middleware.AccessLogConfig{Logger: logger, RequestHeaders: true, ResponseHeaders: true, RequestBody: true, ResponseBody: true, RedactHeaders: headers, RedactKeys: keys}
	log := middleware.AccessLog(config)
	headers[0] = "Changed"
	keys[0] = "changed"
	r := gin.New()
	r.Use(middleware.RequestID(middleware.RequestIDConfig{}), log)
	r.POST("/private", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			t.Fatal(err)
		}
		c.Header("Content-Type", "application/json")
		c.Header("Set-Cookie", "session=secretcookie")
		if _, err := c.Writer.WriteString(`{"safe":42,"access_token":"secretresponse","nested":[{"privateValue":"customsecret"}]}`); err != nil {
			t.Fatal(err)
		}
	})
	req := httptest.NewRequest("POST", "/private?token=secretquery", strings.NewReader(`{"password":"secretpassword","private_value":"secretprivate","safe":"yes"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secretbearer")
	req.Header.Set("X-Custom-Secret", "secretheader")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	for _, secret := range []string{"secretresponse", "customsecret", "secretpassword", "secretprivate", "secretquery", "secretbearer", "secretheader", "secretcookie"} {
		if strings.Contains(b.snapshot(), secret) {
			t.Fatalf("leaked %q: %s", secret, b.snapshot())
		}
	}
	items := entries(t, b)
	if len(items) != 2 || items[0]["msg"] != "http request started" || items[1]["msg"] != "http request completed" {
		t.Fatalf("events: %v", items)
	}
	if !strings.Contains(items[1]["response_body"].(string), `"safe":42`) || !strings.Contains(items[1]["request_body"].(string), `"safe":"yes"`) {
		t.Fatalf("payload missing: %v", items)
	}
	if !strings.Contains(w.Body.String(), "secretresponse") {
		t.Fatal("response changed")
	}
}

type payloadCase struct {
	name, contentType, body, disposition string
	limit                                int
	partial                              bool
}

func TestUnsafePayloadsOmitted(t *testing.T) {
	cases := []payloadCase{
		{name: "invalid json", contentType: "application/json", body: `{"password":"secretpayload"`},
		{name: "truncated json", contentType: "application/json", body: `{"password":"secretpayload"}`, limit: 10},
		{name: "partial read", contentType: "application/json", body: `{"password":"secretpayload"}`, partial: true},
		{name: "binary", contentType: "application/octet-stream", body: "secretpayload"},
		{name: "sse", contentType: "text/event-stream", body: "secretpayload"},
		{name: "attachment", contentType: "application/json", body: `{"safe":"secretpayload"}`, disposition: "attachment; filename=x.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, b := newLogger()
			r := gin.New()
			r.Use(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, RequestBody: true, ResponseBody: true, MaxBodyBytes: tc.limit}))
			r.POST("/", func(c *gin.Context) {
				if tc.partial {
					_, _ = c.Request.Body.Read(make([]byte, 3))
				} else {
					_, _ = io.Copy(io.Discard, c.Request.Body)
				}
				c.Header("Content-Type", tc.contentType)
				if tc.disposition != "" {
					c.Header("Content-Disposition", tc.disposition)
				}
				_, _ = c.Writer.WriteString(tc.body)
			})
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			if tc.disposition != "" {
				req.Header.Set("Content-Disposition", tc.disposition)
			}
			r.ServeHTTP(httptest.NewRecorder(), req)
			if strings.Contains(b.snapshot(), "secretpayload") {
				t.Fatalf("unsafe body: %s", b.snapshot())
			}
		})
	}
}

func TestFormRedactionAndReaderFrom(t *testing.T) {
	logger, b := newLogger()
	r := gin.New()
	r.Use(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, RequestBody: true, ResponseBody: true}))
	r.POST("/", func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Header("Content-Type", "application/x-www-form-urlencoded")
		readerFrom, ok := c.Writer.(io.ReaderFrom)
		if !ok {
			t.Fatal("ReaderFrom lost")
		}
		_, err := readerFrom.ReadFrom(strings.NewReader("password=secretform&safe=visible"))
		if err != nil {
			t.Fatal(err)
		}
		c.Writer.Flush()
	})
	req := httptest.NewRequest("POST", "/", strings.NewReader("access_token=secretrequest&safe=yes"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !w.Flushed || strings.Contains(b.snapshot(), "secretform") || strings.Contains(b.snapshot(), "secretrequest") || !strings.Contains(b.snapshot(), "safe=visible") {
		t.Fatalf("capture: %s", b.snapshot())
	}
}

func TestDefaultNoCaptureSkipAndConcurrentRequests(t *testing.T) {
	logger, b := newLogger()
	r := gin.New()
	r.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, Skip: func(c *gin.Context) bool { return c.Request.URL.Path == "/skip" }}))
	r.GET("/:name", func(c *gin.Context) { c.JSON(200, gin.H{"password": "secretpayload"}) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/skip", nil))
	if b.snapshot() != "" {
		t.Fatal("skip logged")
	}
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/run", nil))
			if w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	wg.Wait()
	if got := len(entries(t, b)); got != 80 {
		t.Fatalf("events %d", got)
	}
	if strings.Contains(b.snapshot(), "secretpayload") || strings.Contains(b.snapshot(), "response_body") {
		t.Fatal("default payload capture enabled")
	}
}

func TestConfigurationRejectsInvalidValues(t *testing.T) {
	constructors := []func(){
		func() { middleware.RequestID(middleware.RequestIDConfig{Header: "bad header"}) },
		func() { middleware.AccessLog(middleware.AccessLogConfig{MaxBodyBytes: -1}) },
		func() { middleware.AccessLog(middleware.AccessLogConfig{MaxBodyBytes: 1<<20 + 1}) },
		func() { middleware.AccessLog(middleware.AccessLogConfig{RedactHeaders: []string{"bad header"}}) },
	}
	for _, construct := range constructors {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("invalid config accepted")
				}
			}()
			construct()
		}()
	}
}

type IUnwrapper interface{ Unwrap() http.ResponseWriter }

type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (r *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	r.hijacked = true
	client, server := net.Pipe()
	_ = client.Close()
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func TestWriterUnwrapAndHijack(t *testing.T) {
	logger, b := newLogger()
	r := gin.New()
	r.Use(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, ResponseBody: true}))
	r.GET("/", func(c *gin.Context) {
		if unwrapper, ok := c.Writer.(IUnwrapper); !ok || unwrapper.Unwrap() == nil {
			t.Fatal("Unwrap lost")
		}
		conn, _, err := c.Writer.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	})
	w := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !w.hijacked {
		t.Fatal("Hijack not forwarded")
	}
	if strings.Contains(b.snapshot(), "secret") {
		t.Fatal("hijacked payload captured")
	}
}

type captureBoundaryCase struct {
	name        string
	stringFirst bool
	extra       string
}

func TestCaptureMixedWritesAtLimit(t *testing.T) {
	const limit = 64
	body := `{"message":"` + strings.Repeat("x", limit-len(`{"message":"`)-len(`"}`)) + `"}`
	cases := []captureBoundaryCase{
		{name: "string then bytes", stringFirst: true},
		{name: "bytes then string"},
		{name: "string then bytes overflow", stringFirst: true, extra: " "},
		{name: "bytes then string overflow", extra: " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, buffer := newLogger()
			router := gin.New()
			router.Use(middleware.AccessLog(middleware.AccessLogConfig{Logger: logger, ResponseBody: true, MaxBodyBytes: limit}))
			router.GET("/", func(c *gin.Context) {
				c.Header("Content-Type", "application/json")
				if tc.stringFirst {
					_, _ = c.Writer.WriteString(body[:32])
					_, _ = c.Writer.Write([]byte(body[32:]))
				} else {
					_, _ = c.Writer.Write([]byte(body[:32]))
					_, _ = c.Writer.WriteString(body[32:])
				}
				if tc.extra != "" {
					_, _ = c.Writer.WriteString(tc.extra)
				}
			})
			result := httptest.NewRecorder()
			router.ServeHTTP(result, httptest.NewRequest("GET", "/", nil))
			if result.Body.String() != body+tc.extra {
				t.Fatalf("forwarded body=%q", result.Body.String())
			}
			events := entries(t, buffer)
			if len(events) != 2 {
				t.Fatalf("events=%d want=2", len(events))
			}
			got := events[1]["response_body"]
			want := body
			if tc.extra != "" {
				want = "[omitted: body exceeds log limit]"
			}
			if got != want {
				t.Fatalf("capture=%q want=%q", got, want)
			}
		})
	}
}

// recordBuffer retains snapshots to verify independence across requests/events.
type recordBuffer struct {
	sync.Mutex
	records []middleware.AccessRecord
}

func (b *recordBuffer) Log(_ context.Context, record middleware.AccessRecord) {
	b.Lock()
	defer b.Unlock()
	b.records = append(b.records, record)
}

func (b *recordBuffer) snapshot() []middleware.AccessRecord {
	b.Lock()
	defer b.Unlock()
	return append([]middleware.AccessRecord(nil), b.records...)
}

func TestAccessLogRoutePolicies(t *testing.T) {
	output := new(recordBuffer)
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Output:  output,
		Default: middleware.AccessLogPolicy{Start: true, Finish: true},
		Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
			switch c.FullPath() {
			case "/health":
				return middleware.AccessLogPolicy{}
			case "/items/:id":
				if c.Request.Method == http.MethodPost {
					policy.Start = false
					policy.Request.Body = true
					policy.Response.Body = true
				}
			case "/start":
				policy.Finish = false
				policy.Request.Body = true
				policy.Response.Body = true
			}
			return policy
		},
	}))
	handler := func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.JSON(200, gin.H{"safe": true})
	}
	router.GET("/health", handler)
	router.GET("/items/:id", handler)
	router.POST("/items/:id", handler)
	router.POST("/start", func(c *gin.Context) {
		if _, wrapped := c.Request.Body.(*trackedBody); !wrapped {
			t.Error("start-only policy wrapped the request body")
		}
		handler(c)
	})
	paths := []string{"/health", "/items/1", "/items/2", "/start", "/missing"}
	methods := []string{"GET", "GET", "POST", "POST", "GET"}
	for i, path := range paths {
		req := httptest.NewRequest(methods[i], path, nil)
		req.Body = &trackedBody{reader: strings.NewReader(`{"safe":"request"}`)}
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	records := output.snapshot()
	if len(records) != 6 {
		t.Fatalf("records=%d want=6", len(records))
	}
	if records[0].RequestBody != nil || records[1].ResponseBody != nil {
		t.Fatal("method-specific policy leaked to GET")
	}
	post := records[2]
	if post.Phase != middleware.AccessCompleted || post.Route != "/items/:id" ||
		post.RequestBody == nil || post.ResponseBody == nil {
		t.Fatalf("POST record=%+v", post)
	}
	if records[3].Phase != middleware.AccessStarted || records[3].RequestBody != nil {
		t.Fatal("start-only policy captured a body")
	}
	if records[5].Route != "" || records[5].Status != 404 {
		t.Fatalf("unmatched route=%+v", records[5])
	}
}

func TestAccessLogCompletionFilter(t *testing.T) {
	output := new(recordBuffer)
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Output: output,
		Default: middleware.AccessLogPolicy{
			Finish:   true,
			Request:  middleware.AccessLogContent{Body: true},
			Response: middleware.AccessLogContent{Body: true},
		},
		ShouldLog: func(record middleware.AccessRecord) bool {
			// The predicate receives sanitized bodies, including for discarded events.
			if record.RequestBody == nil || strings.Contains(*record.RequestBody, "hidden") {
				t.Error("predicate received an unsafe or missing request body")
			}
			return record.Status >= 400 || record.Error != nil
		},
	}))
	router.POST("/:kind", func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		switch c.Param("kind") {
		case "error":
			c.JSON(400, gin.H{"password": "hidden"})
		case "internal":
			_ = c.Error(errors.New("failure"))
			c.JSON(200, gin.H{})
		default:
			c.JSON(200, gin.H{})
		}
	})
	for _, kind := range []string{"ok", "error", "internal"} {
		req := httptest.NewRequest("POST", "/"+kind, strings.NewReader(`{"password":"hidden"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	records := output.snapshot()
	if len(records) != 2 || records[0].Level != middleware.AccessLogWarn ||
		records[1].Level != middleware.AccessLogError || records[1].Error == nil {
		t.Fatalf("filtered records=%+v", records)
	}
	if strings.Contains(*records[0].ResponseBody, "hidden") {
		t.Fatal("output received an unredacted body")
	}
}

func TestAccessLogCompactHeadersAndOutputPrecedence(t *testing.T) {
	output := new(recordBuffer)
	logger, buffer := newLogger()
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Output: output, Logger: logger,
		GetLogger: func(*gin.Context) *slog.Logger {
			t.Error("legacy logger selected despite Output")
			return logger
		},
		HeaderFormat: middleware.HeaderFormatCompact,
		Default: middleware.AccessLogPolicy{
			Start: true, Finish: true,
			Request:  middleware.AccessLogContent{Headers: true},
			Response: middleware.AccessLogContent{Headers: true},
		},
	}))
	router.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.Writer.Header().Add("X-Multi", "first")
		c.Writer.Header().Add("X-Multi", "second")
		c.Header("Set-Cookie", "secret")
		c.Status(204)
	})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Single", "one")
	req.Header.Set("Authorization", "secret")
	router.ServeHTTP(httptest.NewRecorder(), req)
	records := output.snapshot()
	if buffer.snapshot() != "" || len(records) != 2 {
		t.Fatal("multiple outputs selected")
	}
	first, second := records[0], records[1]
	if first.RequestHeaders["X-Single"] != "one" || second.ResponseHeaders["Set-Cookie"] != "[REDACTED]" {
		t.Fatalf("compact headers=%v %v", first.RequestHeaders, second.ResponseHeaders)
	}
	values, ok := second.ResponseHeaders["X-Multi"].([]string)
	if !ok || len(values) != 2 {
		t.Fatal("multiple header values were flattened")
	}
	first.RequestHeaders["X-Single"] = "changed"
	if second.RequestHeaders["X-Single"] != "one" || req.Header.Get("Authorization") != "secret" {
		t.Fatal("event or request headers were aliased")
	}
}

func TestAccessLogIndependentCaptureLimits(t *testing.T) {
	output := new(recordBuffer)
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Output: output,
		Default: middleware.AccessLogPolicy{
			Finish:   true,
			Request:  middleware.AccessLogContent{Body: true, MaxBodyBytes: 8},
			Response: middleware.AccessLogContent{Body: true, MaxBodyBytes: 128},
		},
	}))
	router.POST("/", func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.JSON(200, gin.H{"safe": "response"})
	})
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"safe":"request"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
	record := output.snapshot()[0]
	if *record.RequestBody != "[omitted: body exceeds log limit]" ||
		!strings.Contains(*record.ResponseBody, "response") {
		t.Fatalf("independent capture=%+v", record)
	}
}

func TestAccessLogNewConfigurationValidation(t *testing.T) {
	cases := []middleware.AccessLogConfig{
		{HeaderFormat: middleware.HeaderFormat(99)},
		{Default: middleware.AccessLogPolicy{Finish: true, Request: middleware.AccessLogContent{MaxBodyBytes: -1}}},
		{Default: middleware.AccessLogPolicy{Finish: true, Response: middleware.AccessLogContent{MaxBodyBytes: 1<<20 + 1}}},
		{RedactKeys: []string{"_-"}},
	}
	for i, config := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid config accepted")
				}
			}()
			middleware.AccessLog(config)
		})
	}
}

func TestAccessLogConcurrentPolicyIsolation(t *testing.T) {
	output := new(recordBuffer)
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Output:  output,
		Default: middleware.AccessLogPolicy{Finish: true},
		Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
			policy.Response.Body = c.Param("id") == "capture"
			return policy
		},
	}))
	router.GET("/:id", func(c *gin.Context) { c.JSON(200, gin.H{"safe": true}) })
	var group sync.WaitGroup
	for range 20 {
		for _, path := range []string{"/capture", "/omit"} {
			group.Go(func() {
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
			})
		}
	}
	group.Wait()
	records := output.snapshot()
	if len(records) != 40 {
		t.Fatalf("records=%d", len(records))
	}
	for _, record := range records {
		if (record.ResponseBody != nil) != (record.Path == "/capture") {
			t.Fatalf("policy crossed requests: %+v", record)
		}
	}
}

func TestAccessLogLegacyLoggerSelectionAndSkipPrecedence(t *testing.T) {
	fallback, fallbackBuffer := newLogger()
	selected, selectedBuffer := newLogger()
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: fallback,
		GetLogger: func(c *gin.Context) *slog.Logger {
			if c.Param("id") == "selected" {
				return selected
			}
			return nil
		},
		Skip: func(c *gin.Context) bool { return c.Param("id") == "skip" },
		Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
			if c.Param("id") == "skip" {
				t.Error("Resolve called on skipped request")
			}
			return policy
		},
	}))
	router.GET("/:id", func(c *gin.Context) { c.Status(204) })
	for _, path := range []string{"/fallback", "/selected", "/skip"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if len(entries(t, fallbackBuffer)) != 2 || len(entries(t, selectedBuffer)) != 2 {
		t.Fatal("legacy logger selection changed")
	}
}
