package xgin_test

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
)

func TestBearerMalformedAndRenderPanicCause(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	for _, value := range []string{"", "Basic abc", "Bearer", "Bearer abc def", "Bearer =", "Bearer abc=def", "Bearer abc,def", "Bearer 日本"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/", nil)
		c.Request.Header.Set("Authorization", value)
		if v, ok := x.NewContext(c).BearerToken(); ok {
			t.Fatalf("accepted %q => %q", value, v)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Add("Authorization", "Bearer one")
	c.Request.Header.Add("Authorization", "Bearer two")
	if _, ok := x.NewContext(c).Token(); ok {
		t.Fatal("duplicate accepted")
	}
	c.Request.Header.Del("Authorization")
	x.Render(c, xgin.ResponseFunc(func(*gin.Context) error { panic(syscall.EPIPE) }))
	if len(c.Errors) == 0 || !errors.Is(c.Errors.Last().Err, syscall.EPIPE) {
		t.Fatal("panic cause lost")
	}
}

func TestBearerRejectsNonSpaceSeparators(t *testing.T) {
	for _, separator := range []string{"\t", "\u00a0", "\u2003"} {
		t.Run(fmt.Sprintf("U+%04X", []rune(separator)[0]), func(t *testing.T) {
			t.Parallel()
			c := newHTTPContext("GET", "/", "", "")
			c.Request.Header.Set("Authorization", "Bearer"+separator+"abc")
			if token, ok := (xgin.BearerTokenExtractor{}).ExtractToken(c); ok {
				t.Fatalf("accepted separator=%q token=%q", separator, token)
			}
		})
	}
}
