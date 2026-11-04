package xgin_test

import (
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/lazyboon/xgin"
)

func TestRedirectAndHTML(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	w := call(engine(x, xgin.Redirect(0, "/next?q=one")), "GET", "/")
	if w.Code != 302 || w.Header().Get("Location") != "/next?q=one" {
		t.Fatal("redirect", w.Code, w.Header())
	}
	g := engine(x, xgin.HTML(201, "tour", responseData{Name: "<tag>"}))
	g.SetHTMLTemplate(template.Must(template.New("tour").Parse(`<p>{{.Name}}</p>`)))
	w = call(g, "POST", "/")
	if w.Code != 201 || w.Body.String() != `<p>&lt;tag&gt;</p>` {
		t.Fatal("HTML", w.Code, w.Body.String())
	}
	broken := engine(x, xgin.HTML(200, "broken", failData{Name: "ok", Fail: func() (string, error) { return "", errors.New("template secret") }}))
	broken.SetHTMLTemplate(template.Must(template.New("broken").Parse(`prefix {{call .Fail}}`)))
	w = call(broken, "POST", "/")
	if w.Code != 500 || strings.Contains(w.Body.String(), "prefix") || strings.Contains(w.Body.String(), "template secret") {
		t.Fatal("HTML failure committed", w.Code, w.Body.String())
	}
}

type renderFailureCase struct {
	name   string
	output xgin.IResponse
}

func TestRenderFailuresAndHeaders(t *testing.T) {
	adapter := xgin.MustNew(xgin.Config{})
	cases := []renderFailureCase{
		{"renderer returns error after partial encoding", xgin.Render{Renderer: badEncoder{}}},
		{"body forbidden by status", xgin.Response{HTTPStatus: 204, Data: "body"}},
		{"invalid redirect status", xgin.Redirect(304, "/no")},
		{"ambiguous header casing", xgin.Response{Header: http.Header{"X-Test": {"one"}, "x-test": {"two"}}}},
		{"header injection", xgin.Response{Header: http.Header{"X-Test": {"\r\ninjected"}}}},
		{"unsupported format", xgin.Response{ContentType: 255}},
		{"custom response panic", xgin.ResponseFunc(func(*gin.Context) error { panic("private panic") })},
		{"JSON encoding error", xgin.Render{Renderer: render.JSON{Data: make(chan int)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := call(engine(adapter, tc.output), "POST", "/")
			if result.Code != 500 || result.Header().Get("X-Leak") != "" || strings.Contains(result.Body.String(), "private panic") {
				t.Fatalf("status=%d headers=%v body=%s", result.Code, result.Header(), result.Body.String())
			}
		})
	}
}

func TestFilesAndAbort(t *testing.T) {
	path := t.TempDir() + "/tour.txt"
	if e := os.WriteFile(path, []byte("0123456789"), 0600); e != nil {
		t.Fatal(e)
	}
	x := xgin.MustNew(xgin.Config{})
	g := engine(x, xgin.Download(path, "日本.txt"))
	w := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Range", "bytes=2-4")
	g.ServeHTTP(w, request)
	if w.Code != 206 || w.Body.String() != "234" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("file", w.Code, w.Body.String(), w.Header())
	}
	g = gin.New()
	after := false
	g.GET("/", x.Wrap(func(*xgin.Context) xgin.Response { return xgin.Text(200, "one") }), func(c *gin.Context) { after = true; c.String(200, "two") })
	w = call(g, "GET", "/")
	if after || w.Body.String() != "one" {
		t.Fatal("chain not aborted")
	}
}

func TestHeadRedirectUsesGetRepresentationLength(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	g := engine(x, xgin.Redirect(302, "/destination"))
	get, head := call(g, "GET", "/"), call(g, "HEAD", "/")
	if get.Header().Get("Content-Length") != head.Header().Get("Content-Length") || head.Body.Len() != 0 {
		t.Fatalf("GET=%q HEAD=%q", get.Header().Get("Content-Length"), head.Header().Get("Content-Length"))
	}
}

type failData struct {
	Name string
	Fail func() (string, error)
}

type badEncoder struct{}

func (badEncoder) WriteContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/private")
}

func (badEncoder) Render(w http.ResponseWriter) error {
	w.Header().Set("X-Leak", "bad")
	_, _ = w.Write([]byte("partial"))
	return errors.New("encode private")
}
