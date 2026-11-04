package xgin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/lazyboon/xgin"
	mw "github.com/lazyboon/xgin/middleware"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type emptyRequest struct{}

type responseData struct {
	Name string `json:"name" xml:"name" yaml:"name" toml:"name"`
}

func engine(x *xgin.XGin, output xgin.IResponse) *gin.Engine {
	g := gin.New()
	g.Use(mw.ErrorHandler(mw.ErrorHandlerConfig{Respond: func(*gin.Context, error) xgin.IResponse { return xgin.JSON(500, gin.H{"error": "private failure"}) }}), mw.Recovery())
	g.Any("/", x.Handler(func(c *xgin.Context, _ *emptyRequest) xgin.IResponse { return output }))
	return g
}

func call(g *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestEnvelopeAndCopySemantics(t *testing.T) {
	original := xgin.Response{Code: "OK", Msg: "success", Header: http.Header{"Set-Cookie": {"a=1"}}, Errors: make([]error, 0, 3)}
	changed := original.WithData(responseData{Name: "tour"}).WithHeader("Set-Cookie", "b=2").WithErrors(nil, errors.New("private cause"))
	if original.Header.Get("Set-Cookie") != "a=1" || len(original.Errors) != 0 {
		t.Fatal("template mutated")
	}
	headers := http.Header{"X-Test": {"before"}}
	response := changed.WithHeaders(headers)
	headers["X-Test"][0] = "after"
	if response.Header.Get("X-Test") != "before" {
		t.Fatal("header slice shared")
	}
	w := call(engine(xgin.MustNew(xgin.Config{}), response), "POST", "/")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "HTTPStatus") {
		t.Fatalf("bad envelope %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if len(body) != 3 || body["code"] != "OK" {
		t.Fatal(body)
	}
}

func TestRawAndTransformerIsolation(t *testing.T) {
	shared := xgin.Response{Code: "OK", Msg: "success", Data: responseData{Name: "plain"}}
	a := xgin.MustNew(xgin.Config{TransformResponse: func(r xgin.Response) any { return gin.H{"a": r.Data} }})
	b := xgin.MustNew(xgin.Config{TransformResponse: func(r xgin.Response) any { return gin.H{"b": r.Data} }})
	first, second := engine(a, shared), engine(b, shared)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			g, key := first, "a"
			if i%2 != 0 {
				g, key = second, "b"
			}
			w := call(g, "POST", "/")
			var result map[string]any
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result[key] == nil {
				t.Errorf("instance leak %s", w.Body.String())
			}
		})
	}
	wg.Wait()
	w := call(engine(a, xgin.JSON(201, gin.H{"external": true})), "POST", "/")
	if w.Code != 201 || w.Body.String() != `{"external":true}` {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call(engine(a, shared.WithBody(gin.H{"explicit": true})), "POST", "/")
	if w.Body.String() != `{"explicit":true}` {
		t.Fatal(w.Body.String())
	}
}

func TestFiniteFormats(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	for _, kind := range []xgin.ContentType{xgin.ContentTypeJSON, xgin.ContentTypeIndentedJSON, xgin.ContentTypeSecureJSON, xgin.ContentTypeJSONP, xgin.ContentTypeASCIIJSON, xgin.ContentTypePureJSON, xgin.ContentTypeXML, xgin.ContentTypeYAML, xgin.ContentTypeTOML, xgin.ContentTypeMsgPack} {
		if kind == xgin.ContentTypeMsgPack && binding.Default("POST", "application/msgpack").Name() != "msgpack" {
			continue
		}
		r := xgin.Response{Code: "OK", Data: responseData{Name: "日本<tour>"}}.WithContentType(kind)
		w := call(engine(x, r), "POST", "/")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("format %d: %d %s", kind, w.Code, w.Body.String())
		}
	}
	w := call(engine(x, xgin.Response{Data: wrapperspb.String("hello"), ContentType: xgin.ContentTypeProtoBuf}), "POST", "/")
	if w.Code != 200 || w.Body.Len() == 0 {
		t.Fatal("protobuf", w.Code, w.Body.String())
	}
	w = call(engine(x, xgin.Text(202, "100% %s %d")), "POST", "/")
	if w.Code != 202 || w.Body.String() != "100% %s %d" {
		t.Fatal("text formatted", w.Code, w.Body.String())
	}
	w = call(engine(x, xgin.Empty(204)), "POST", "/")
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatal("empty")
	}
	g := engine(x, xgin.JSON(200, gin.H{"hello": "world"}))
	get, head := call(g, "GET", "/"), call(g, "HEAD", "/")
	if head.Body.Len() != 0 || get.Header().Get("Content-Length") != head.Header().Get("Content-Length") {
		t.Fatal("head length")
	}
}

func TestTypedNilErrorIsNotAttached(t *testing.T) {
	var cause *nilCause
	output := xgin.Response{Code: "OK"}.WithErrors(cause, nil)
	if len(output.Errors) != 0 {
		t.Fatal("typed nil retained")
	}
	x := xgin.MustNew(xgin.Config{})
	w := call(engine(x, output), "POST", "/")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestJSONPDoesNotTrustExecutableQueryCallback(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	output := xgin.Response{Code: "OK", ContentType: xgin.ContentTypeJSONP}
	w := call(engine(x, output), "GET", "/?callback=alert%281%29%2F%2F")
	if strings.Contains(w.Body.String(), "alert(1)//") {
		t.Fatal("unvalidated query became JavaScript", w.Body.String())
	}
}

func TestJSONPExplicitCallbackValidation(t *testing.T) {
	x := xgin.MustNew(xgin.Config{})
	for _, callback := range []string{"client.receive", "jQuery_123", "$callbacks.handle0"} {
		w := call(engine(x, xgin.JSONP(200, callback, gin.H{"value": 1})), "GET", "/?callback=evil")
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), callback+"(") {
			t.Fatal(callback, w.Code, w.Body.String())
		}
	}
	for _, callback := range []string{"alert(1)//", "x;alert(1)", "a..b", ".a", "a.", "1a", "a[0]", "a\n.b"} {
		w := call(engine(x, xgin.JSONP(200, callback, gin.H{"value": 1})), "GET", "/")
		if w.Code != 500 || strings.Contains(w.Body.String(), callback) {
			t.Fatal("callback accepted", callback, w.Code, w.Body.String())
		}
	}
}

func ExampleJSON() {
	adapter := xgin.MustNew(xgin.Config{TransformResponse: func(r xgin.Response) any {
		return gin.H{"payload": r.Data}
	}})
	// An explicit third-party JSON contract bypasses the envelope and transformer.
	result := call(engine(adapter, xgin.JSON(http.StatusCreated, gin.H{"accepted": true})), "POST", "/")
	fmt.Println(result.Code)
	fmt.Println(result.Body.String())
	// Output:
	// 201
	// {"accepted":true}
}

type nilCause struct{}

func (*nilCause) Error() string { panic("nil cause dereferenced") }
