package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin/middleware"
)

type accessSeverityCase struct {
	name  string
	level middleware.AccessLogLevel
	want  string
}

func TestSlogAccessLogger(t *testing.T) {
	cases := []accessSeverityCase{
		{name: "info", level: middleware.AccessLogInfo, want: "INFO"},
		{name: "warning", level: middleware.AccessLogWarn, want: "WARN"},
		{name: "error", level: middleware.AccessLogError, want: "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, buffer := newLogger()
			body := ""
			record := middleware.AccessRecord{
				Phase: middleware.AccessCompleted, Level: tc.level,
				Method: "POST", Path: "/items/1", Route: "/items/:id", RequestID: "request-1",
				Status: 400, Duration: 1500 * time.Microsecond,
				RequestBody: &body, Error: errors.New("internal failure"), Stacks: []string{"stack"},
			}
			middleware.NewSlogAccessLogger(logger).Log(context.Background(), record)
			events := entries(t, buffer)
			if len(events) != 1 || events[0]["level"] != tc.want || events[0]["duration_ms"] != 1.5 ||
				events[0]["error"] != "internal failure" || events[0]["request_body"] != "" {
				t.Fatalf("record=%v", events)
			}
			if _, exists := events[0]["response_body"]; exists {
				t.Fatal("disabled body included")
			}
		})
	}
}

func TestSlogAccessLoggerDisabledLevelAndDefault(t *testing.T) {
	buffer := new(logBuffer)
	logger := slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{Level: slog.LevelError}))
	middleware.NewSlogAccessLogger(logger).Log(context.Background(), middleware.AccessRecord{
		Phase: middleware.AccessStarted, Level: middleware.AccessLogInfo,
	})
	if buffer.snapshot() != "" {
		t.Fatal("disabled level emitted")
	}
	if middleware.NewSlogAccessLogger(nil) == nil {
		t.Fatal("nil fallback logger")
	}
}

func TestAccessRecordStartFields(t *testing.T) {
	record := middleware.AccessRecord{Phase: middleware.AccessStarted, Method: "GET", Path: "/"}
	fields := record.Fields()
	for _, name := range []string{"route", "status", "duration_ms", "request_body", "response_headers", "error"} {
		if _, exists := fields[name]; exists {
			t.Fatalf("start event contains %s", name)
		}
	}
	if record.Message() != "http request started" {
		t.Fatal("incorrect start message")
	}
}

func ExampleAccessLog() {
	router := gin.New()
	router.Use(middleware.AccessLog(middleware.AccessLogConfig{
		Logger: middleware.AccessLoggerFunc(func(_ context.Context, record middleware.AccessRecord) {
			fmt.Println(record.Phase, record.Method, record.Route, record.Status)
		}),
		Default: middleware.AccessLogPolicy{Finish: true},
		Resolve: func(c *gin.Context, policy middleware.AccessLogPolicy) middleware.AccessLogPolicy {
			if c.FullPath() == "/health" {
				return middleware.AccessLogPolicy{}
			}
			return policy
		},
	}))
	router.GET("/health", func(c *gin.Context) { c.Status(200) })
	router.GET("/items/:id", func(c *gin.Context) { c.Status(204) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/items/1", nil))
	// Output: Completed GET /items/:id 204
}
