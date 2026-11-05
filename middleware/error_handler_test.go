package middleware_test

import (
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
	"github.com/lazyboon/xgin/middleware"
)

type testResponse struct {
	err      error
	body     string
	noCommit bool
}

func (r testResponse) Render(c *gin.Context) error {
	if r.err != nil {
		c.Header("Content-Type", "bad/type")
		c.Header("Content-Length", "900")
		return r.err
	}
	if !r.noCommit {
		c.Data(500, "application/json", []byte(r.body))
	}
	return nil
}

func TestRecoveryAndErrorHandler(t *testing.T) {
	logger, b := newLogger()
	r := gin.New()
	r.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.AccessLog(middleware.AccessLogConfig{
		Logger:  middleware.NewSlogAccessLogger(logger),
		Default: middleware.AccessLogPolicy{Start: true, Finish: true},
	}), middleware.ErrorHandler(middleware.ErrorHandlerConfig{Respond: func(c *gin.Context, err error) xgin.IResponse {
		if !strings.Contains(err.Error(), "panic private") {
			t.Fatal(err)
		}
		return testResponse{body: `{"error":"internal"}`}
	}}), middleware.Recovery())
	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		c.Header("Content-Length", "1000")
		c.Header("ETag", "stale")
		panic("panic private")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || w.Body.String() != `{"error":"internal"}` || w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") != "" || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("response %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	logs := entries(t, b)
	if logs[1]["stack"] == nil || logs[1]["level"] != "ERROR" || logs[1]["status"] != float64(500) {
		t.Fatalf("missing diagnostics: %v", logs)
	}
}

func TestErrorResponseFailureDoesNotRecurse(t *testing.T) {
	for _, output := range []xgin.IResponse{testResponse{err: errors.New("render failed")}, testResponse{noCommit: true}, (*testResponse)(nil)} {
		calls := 0
		r := gin.New()
		r.Use(middleware.ErrorHandler(middleware.ErrorHandlerConfig{Respond: func(c *gin.Context, err error) xgin.IResponse { calls++; return output }}), middleware.Recovery())
		r.GET("/", func(c *gin.Context) { panic("private") })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if calls != 1 || w.Code != 500 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "" {
			t.Fatalf("failure fallback %d %v %s", calls, w.Header(), w.Body.String())
		}
	}
}

func TestBrokenPipeAndCommittedResponseAreNotRewritten(t *testing.T) {
	for _, committed := range []bool{false, true} {
		calls := 0
		r := gin.New()
		r.Use(middleware.ErrorHandler(middleware.ErrorHandlerConfig{Respond: func(c *gin.Context, err error) xgin.IResponse { calls++; return testResponse{body: "must not write"} }}), middleware.Recovery())
		r.GET("/", func(c *gin.Context) {
			if committed {
				c.String(201, "already sent")
				panic("private")
			}
			panic(&net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if calls != 0 || committed && (w.Code != 201 || w.Body.String() != "already sent") || !committed && w.Body.Len() != 0 {
			t.Fatalf("rewrote response %d %s", w.Code, w.Body.String())
		}
	}
}

func TestErrorResponderPanicAndBrokenPipeAfterStatus(t *testing.T) {
	for _, broken := range []bool{false, true} {
		calls := 0
		r := gin.New()
		r.Use(middleware.ErrorHandler(middleware.ErrorHandlerConfig{Respond: func(c *gin.Context, err error) xgin.IResponse { calls++; panic("responder panic") }}), middleware.Recovery())
		r.GET("/", func(c *gin.Context) {
			c.Status(500)
			if broken {
				panic(syscall.EPIPE)
			}
			panic("private")
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if broken && calls != 0 || !broken && calls != 1 || w.Body.Len() != 0 || w.Code != 500 {
			t.Fatalf("calls=%d response=%d %s", calls, w.Code, w.Body.String())
		}
	}
}
