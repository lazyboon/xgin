package middleware_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin/middleware"
)

func TestRequestID(t *testing.T) {
	for _, trust := range []bool{false, true} {
		for _, incoming := range []string{"trusted-id_1.2", "bad id", strings.Repeat("a", 129), ""} {
			r := gin.New()
			r.Use(middleware.RequestID(middleware.RequestIDConfig{TrustIncoming: trust, Generator: func() string { return "server-id" }}))
			r.GET("/", func(c *gin.Context) { c.String(200, "%s", middleware.RequestIDFromContext(c.Request.Context())) })
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-Request-ID", incoming)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			want := "server-id"
			if trust && incoming == "trusted-id_1.2" {
				want = incoming
			}
			if w.Header().Get("X-Request-ID") != want || w.Body.String() != want {
				t.Fatalf("trust=%v input=%q result=%q", trust, incoming, w.Body.String())
			}
		}
	}
	r := gin.New()
	r.Use(middleware.RequestID(middleware.RequestIDConfig{TrustIncoming: true, Generator: func() string { return "\r\n" }}))
	r.GET("/", func(c *gin.Context) { c.Status(204) })
	req := httptest.NewRequest("GET", "/", nil)
	req.Header["X-Request-Id"] = []string{"one", "two"}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if id := w.Header().Get("X-Request-ID"); len(id) < 20 || strings.ContainsAny(id, "\r\n") {
		t.Fatalf("invalid fallback %q", id)
	}
	ctx := middleware.WithRequestID(context.Background(), "one")
	if middleware.RequestIDFromContext(ctx) != "one" || middleware.RequestIDFromContext(nil) != "" {
		t.Fatal("context lookup")
	}
}
