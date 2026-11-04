package xgin_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lazyboon/xgin/middleware"

	"github.com/gin-gonic/gin"
	"github.com/lazyboon/xgin"
)

func TestDefaultValidationPanicRetainsDiagnostics(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	g := gin.New()
	var causes int
	g.Use(func(c *gin.Context) {
		c.Next()
		causes = len(c.Errors)
		if causes > 0 {
			var p *xgin.PanicError
			if !errors.As(c.Errors.Last().Err, &p) || p.StackTrace() == "" {
				t.Error("panic diagnostics missing")
			}
		}
	})
	g.POST("/", x.Handler(func(*xgin.Context, *panicReq) xgin.Response { return xgin.Text(200, "unexpected") }))
	w := call(g, "POST", "/")
	if w.Code != 500 || causes == 0 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("bad validation panic", w.Code, causes, w.Body.String())
	}
}

type FullRequest struct {
	ID    string `uri:"id"`
	Value string `json:"value"`
}

func (r *FullRequest) Validate() error {
	if r.ID != "1" || r.Value != "yes" {
		return errBusiness
	}
	return nil
}

type CustomRender struct{ Code int }

func (r CustomRender) Render(c *gin.Context) error {
	c.Status(r.Code)
	c.Writer.WriteHeaderNow()
	return nil
}

func TestGenericHandlerCompleteDTO(t *testing.T) {
	x, e := xgin.New(xgin.Config{})
	if e != nil {
		t.Fatal(e)
	}
	h := x.Handler(func(c *xgin.Context, r *FullRequest) CustomRender { return CustomRender{Code: 201} })
	engine := gin.New()
	engine.POST("/:id", h)
	w := httptest.NewRecorder()
	r := newHTTPContext("POST", "/1", "application/json", `{"value":"yes"}`).Request
	engine.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
}

type NilRender struct{}

func (*NilRender) Render(*gin.Context) error { panic("typed nil invoked") }

type FailingRender struct{}

var errAfterWrite = errors.New("after write")

func (FailingRender) Render(c *gin.Context) error { c.String(202, "first"); return errAfterWrite }

func TestTypedNilAndErrorAfterResponse(t *testing.T) {
	x, e := xgin.New(xgin.Config{})
	if e != nil {
		t.Fatal(e)
	}
	t.Run("typed_nil", func(t *testing.T) {
		c := newHTTPContext("GET", "/", "", "")
		x.Render(c, (*NilRender)(nil))
		if c.Writer.Status() != 500 || len(c.Errors) != 1 || !errors.Is(c.Errors[0].Err, xgin.ErrInvalidRender) {
			t.Fatalf("status=%d errors=%v", c.Writer.Status(), c.Errors)
		}
	})
	t.Run("after_write", func(t *testing.T) {
		c := newHTTPContext("GET", "/", "", "")
		x.Render(c, FailingRender{})
		if c.Writer.Status() != 202 || len(c.Errors) != 1 || !errors.Is(c.Errors[0].Err, errAfterWrite) {
			t.Fatalf("status=%d errors=%v", c.Writer.Status(), c.Errors)
		}
	})
}

func init() { gin.SetMode(gin.ReleaseMode) }

type uploadCleanupCase struct {
	name         string
	manual       bool
	panicHandler bool
	invalid      bool
}

type cleanupUpload struct {
	File *multipart.FileHeader `form:"file" binding:"required"`
	Name string                `form:"name" binding:"required"`
}

// Exercise cleanup with a real HTTP server: request-ID middleware replaces the
// request, and the server's original request cannot own the parsed multipart form.
func TestUploadTemporaryCleanup(t *testing.T) {
	cases := []uploadCleanupCase{
		{name: "typed success"}, {name: "typed bind error", invalid: true}, {name: "typed panic", panicHandler: true},
		{name: "manual success", manual: true}, {name: "manual bind error", manual: true, invalid: true}, {name: "manual panic", manual: true, panicHandler: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("TMPDIR", directory)
			x := xgin.MustNew(xgin.Config{MaxBodyBytes: 4096})
			router := gin.New()
			router.MaxMultipartMemory = 1
			router.Use(middleware.RequestID(middleware.RequestIDConfig{}), middleware.ErrorHandler(middleware.ErrorHandlerConfig{}), middleware.Recovery())
			handle := func(c *xgin.Context, req *cleanupUpload) xgin.Response {
				file, err := req.File.Open()
				if err != nil {
					t.Error(err)
					return xgin.Empty(500)
				}
				disk, ok := file.(*os.File)
				if !ok {
					t.Error("upload did not spill to a temporary file")
				} else if _, err := os.Stat(disk.Name()); err != nil {
					t.Error(err)
				}
				if err := file.Close(); err != nil {
					t.Error(err)
				}
				if tc.panicHandler {
					panic("business failure")
				}
				return xgin.Empty(204)
			}
			if tc.manual {
				router.POST("/", x.Wrap(func(c *xgin.Context) xgin.Response {
					var req cleanupUpload
					if err := c.AutoBind(&req); err != nil {
						return xgin.Empty(xgin.DefaultRequestStatus(err))
					}
					return handle(c, &req)
				}))
			} else {
				router.POST("/", x.Handler(handle))
			}
			server := httptest.NewServer(router)
			defer server.Close()
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if !tc.invalid {
				if err := writer.WriteField("name", "sample"); err != nil {
					t.Fatal(err)
				}
			}
			part, err := writer.CreateFormFile("file", "sample.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(part, "hello"); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			response, err := http.Post(server.URL, writer.FormDataContentType(), &body)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Error(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Error(err)
			}
			server.Close()
			want := 204
			if tc.invalid {
				want = 400
			}
			if tc.panicHandler {
				want = 500
			}
			if response.StatusCode != want {
				t.Errorf("status=%d want=%d", response.StatusCode, want)
			}
			remaining, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 0 {
				t.Errorf("retained %d multipart temporary files", len(remaining))
			}
		})
	}
}

type exampleGreeting struct {
	Name string `form:"name" binding:"required"`
}

func ExampleXGin_Handler() {
	adapter := xgin.MustNew(xgin.Config{})
	router := gin.New()
	router.GET("/hello", adapter.Handler[exampleGreeting](func(c *xgin.Context, req *exampleGreeting) xgin.Response {
		return xgin.Response{Code: "OK", Msg: "success"}.WithData("Hello, " + req.Name)
	}))
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest("GET", "/hello?name=Gin", nil))
	fmt.Println(result.Code)
	fmt.Println(result.Body.String())
	// Output:
	// 200
	// {"code":"OK","msg":"success","data":"Hello, Gin"}
}

func TestHandlerUsesRequestErrorMapper(t *testing.T) {
	calls := 0
	adapter := xgin.MustNew(xgin.Config{
		RequestErrorMapper: xgin.RequestErrorMapperFunc(func(c *gin.Context, err error) xgin.IResponse {
			calls++
			if err == nil {
				t.Error("mapper received no binding error")
			}
			return xgin.JSON(http.StatusUnprocessableEntity, gin.H{"code": "INVALID_INPUT"})
		}),
	})
	router := gin.New()
	router.GET("/", adapter.Handler[exampleGreeting](func(*xgin.Context, *exampleGreeting) xgin.Response {
		t.Error("invalid request reached handler")
		return xgin.Empty(204)
	}))
	result := call(router, "GET", "/")
	if calls != 1 || result.Code != 422 || result.Body.String() != `{"code":"INVALID_INPUT"}` {
		t.Fatalf("calls=%d status=%d body=%s", calls, result.Code, result.Body.String())
	}
}

type panicReq struct{}

func (*panicReq) Validate() error { panic("validator private failure") }
