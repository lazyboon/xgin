package xgin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
)

type valueKey struct{}

func TestContextStandardContractAndExtraction(t *testing.T) {
	base, cancel := context.WithTimeout(context.WithValue(context.Background(), valueKey{}, 42), time.Minute)
	defer cancel()
	base = xgin.WithRequestID(base, "trusted-id")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil).WithContext(base)
	c.Request.Header.Set("Authorization", "bEaReR abc.DEF-123=")
	x := xgin.MustNew(xgin.Config{})
	ctx := x.NewContext(c)
	if ctx.RequestID() != "trusted-id" || ctx.Value(valueKey{}) != 42 {
		t.Fatal("metadata missing")
	}
	if value, ok := ctx.GetValue[int](valueKey{}); !ok || value != 42 {
		t.Fatal("typed value")
	}
	deadline, ok := ctx.Deadline()
	original, _ := base.Deadline()
	if !ok || !deadline.Equal(original) {
		t.Fatal("deadline")
	}
	if token, ok := ctx.Token(); !ok || token != "abc.DEF-123=" {
		t.Fatal("token", token, ok)
	}
	// Gin string storage and stdlib context values have intentionally separate owners.
	ctx.Set("gin-only", 7)
	if ctx.GetInt("gin-only") != 7 || ctx.Value("gin-only") != nil {
		t.Fatal("context value domains")
	}
	c.Request = c.Request.WithContext(context.Background())
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("base context changed")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancellation lost")
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			for range 100 {
				ctx.Value(valueKey{})
				ctx.Deadline()
				ctx.Done()
				ctx.Err()
			}
		})
	}
	wg.Wait()
	customized := xgin.MustNew(xgin.Config{TokenExtractor: xgin.TokenExtractorFunc(func(c *gin.Context) (string, bool) { v, e := c.Cookie("credential"); return v, e == nil })})
	c.Request.AddCookie(&http.Cookie{Name: "credential", Value: "cookie-value"})
	other := customized.NewContext(c)
	if token, ok := other.Token(); !ok || token != "cookie-value" {
		t.Fatal("custom extraction")
	}
}

func TestManualAutoBind(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	ctx := xgin.MustNew(xgin.Config{}).NewContext(c)
	var value emptyRequest
	if err := ctx.AutoBind(&value); err != nil {
		t.Fatal(err)
	}
	ctx.Response(xgin.Text(200, "manual"))
	if !c.Writer.Written() {
		t.Fatal("manual response")
	}
}

func TestContextCancelCauseAndDerivedChildren(t *testing.T) {
	cause := errors.New("request stopped")
	base, cancel := context.WithCancelCause(context.Background())
	g, _ := gin.CreateTestContext(httptest.NewRecorder())
	g.Request = httptest.NewRequest("GET", "/", nil).WithContext(base)
	wrapped := xgin.MustNew(xgin.Config{}).NewContext(g)
	child, stop := context.WithCancel(wrapped)
	defer stop()
	cancel(cause)
	if wrapped.Err() != context.Canceled || context.Cause(wrapped) != cause || context.Cause(child) != cause {
		t.Fatal("stdlib cancellation chain changed")
	}
}
