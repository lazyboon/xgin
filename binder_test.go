package xgin_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	xgin "github.com/lazyboon/xgin"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func newHTTPContext(method, uri, ct, body string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, uri, strings.NewReader(body))
	if ct != "" {
		c.Request.Header.Set("Content-Type", ct)
	}
	return c
}

func binder(t *testing.T, n int64) *xgin.Binder {
	t.Helper()
	b, e := xgin.NewBinder(n)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

type Embedded struct {
	Path string `uri:"id" binding:"required"`
}

func TestBinderSourceIsolation(t *testing.T) {
	type request struct {
		Embedded
		Header string `header:"X-Token" binding:"required"`
		Query  string `form:"q" binding:"required"`
		Body   string `json:"body" binding:"required"`
	}
	var r request
	c := newHTTPContext("POST", "/?q=goodq&body=bad", "application/json", `{"body":"goodbody","Path":"bad","Header":"bad","Query":"bad"}`)
	c.Params = gin.Params{{Key: "id", Value: "goodpath"}}
	c.Request.Header.Set("X-Token", "goodheader")
	if e := binder(t, 1024).Bind(c, &r); e != nil {
		t.Fatal(e)
	}
	if r.Path != "goodpath" || r.Header != "goodheader" || r.Query != "goodq" || r.Body != "goodbody" {
		t.Fatalf("source overwrite: %+v", r)
	}
}

func TestBinderAtomicFailure(t *testing.T) {
	type request struct {
		Path string `uri:"id"`
		Body string `json:"body" binding:"required"`
	}
	r := request{Path: "original", Body: "original"}
	c := newHTTPContext("POST", "/", "application/json", `{}`)
	c.Params = gin.Params{{Key: "id", Value: "new"}}
	if e := binder(t, 1024).Bind(c, &r); e == nil {
		t.Fatal("required not validated")
	}
	if r.Path != "original" || r.Body != "original" {
		t.Fatalf("mutated: %+v", r)
	}
}

func TestBinderContentTypes(t *testing.T) {
	type request struct {
		Value string `json:"value" xml:"value" yaml:"value" toml:"value" binding:"required"`
	}
	for _, tt := range []contentTypeCase{
		{"json_parameter", "application/json; charset=utf-8", `{"value":"yes"}`},
		{"xml_parameter", "application/xml; charset=utf-8", `<request><value>yes</value></request>`},
		{"yaml", "application/yaml", "value: yes"},
		{"toml", "application/toml", `value = "yes"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r request
			if e := binder(t, 1024).Bind(newHTTPContext("POST", "/", tt.ct, tt.body), &r); e != nil {
				t.Fatal(e)
			}
			if r.Value != "yes" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestBinderBodyLimitAndTrailingJSON(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}
	for _, tt := range []bodyLimitCase{
		{"trailing_bytes", `{"value":"ok"}` + strings.Repeat(" ", 64), 20, &http.MaxBytesError{}, 413},
		{"two_documents", `{"value":"ok"}{}`, 1024, xgin.ErrInvalidJSONBody, 400},
		{"trailing_garbage", `{"value":"ok"}bad`, 1024, xgin.ErrInvalidJSONBody, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r request
			c := newHTTPContext("POST", "/", "application/json", tt.body)
			c.Request.ContentLength = -1
			e := binder(t, tt.limit).Bind(c, &r)
			if tt.status == 413 {
				var limit *http.MaxBytesError
				if !errors.As(e, &limit) {
					t.Fatalf("missing body-limit cause: %v", e)
				}
			} else if !errors.Is(e, tt.want) {
				t.Fatalf("error=%v want=%v", e, tt.want)
			}
			if e == nil || xgin.DefaultRequestStatus(e) != tt.status {
				t.Fatalf("err=%v status=%d", e, xgin.DefaultRequestStatus(e))
			}
		})
	}
}

func TestBinderReadOnlyAndUnboundBody(t *testing.T) {
	type request struct {
		ID    string `form:"id"`
		Value string `json:"value"`
	}
	var r request
	c := newHTTPContext("HEAD", "/?id=1", "application/json", `garbage`)
	if e := binder(t, 1).Bind(c, &r); e != nil {
		t.Fatal(e)
	}
	if r.ID != "1" {
		t.Fatal(r)
	}
	type query struct {
		ID string `form:"id"`
	}
	var q query
	c = newHTTPContext("POST", "/?id=1", "application/json", `garbage`)
	if e := binder(t, 1).Bind(c, &q); e != nil {
		t.Fatal(e)
	}
	s, _ := io.ReadAll(c.Request.Body)
	if string(s) != "garbage" {
		t.Fatalf("body consumed %s", s)
	}
}

func TestBinderPointerAndEmbeddedContracts(t *testing.T) {
	type child struct {
		N int `form:"n"`
	}
	type request struct {
		Child *child `form:"child"`
	}
	b := binder(t, 1024)
	var r request
	if e := b.Bind(newHTTPContext("GET", "/?n=3", "", ""), &r); e != nil {
		t.Fatal(e)
	}
	if r.Child == nil || r.Child.N != 3 {
		t.Fatalf("%+v", r)
	}
	var absent request
	if e := b.Bind(newHTTPContext("GET", "/", "", ""), &absent); e != nil {
		t.Fatal(e)
	}
	if absent.Child != nil {
		t.Fatal("absent pointer allocated")
	}
	type ptrEmbed struct{ *Embedded }
	if e := b.Prepare(&ptrEmbed{}); e == nil {
		t.Fatal("anonymous ptr not rejected")
	}
	type conflicting struct {
		ID string `uri:"id" json:"id"`
	}
	if e := b.Prepare(&conflicting{}); e == nil {
		t.Fatal("conflicting source not rejected")
	}
}

func TestBinderMediaTypeNormalization(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}
	for _, ct := range []string{"Application/JSON", "application/json ; charset=utf-8"} {
		t.Run(ct, func(t *testing.T) {
			var r request
			if e := binder(t, 1024).Bind(newHTTPContext("POST", "/", ct, `{"value":"yes"}`), &r); e != nil {
				t.Fatalf("valid MIME type rejected: %v", e)
			}
		})
	}
}

func TestBinderDuplicateContentType(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}
	var r request
	c := newHTTPContext("POST", "/", "application/json", `{"value":"yes"}`)
	c.Request.Header.Add("Content-Type", "application/xml")
	if e := binder(t, 1024).Bind(c, &r); e == nil {
		t.Fatal("ambiguous duplicate Content-Type accepted")
	}
}

type CustomParam struct{ Value string }

func (p *CustomParam) UnmarshalParam(s string) error { p.Value = "decoded:" + s; return nil }

func TestBinderCustomParamStructRequiresNestedTags(t *testing.T) {
	type request struct {
		Value CustomParam `form:"value"`
	}
	var native request
	if e := binding.Query.Bind(newHTTPContext("GET", "/?value=abc", "", "").Request, &native); e != nil {
		t.Fatal(e)
	}
	if native.Value.Value != "decoded:abc" {
		t.Fatal(native)
	}
	var r request
	if e := binder(t, 1024).Bind(newHTTPContext("GET", "/?value=abc", "", ""), &r); e == nil {
		t.Fatal("untagged nested field unexpectedly bypassed source isolation")
	}
}

func TestBinderNormalizedJSONRemainsStrict(t *testing.T) {
	type request struct {
		Value string `json:"value"`
	}
	for _, body := range []string{`{"value":"yes"}{}`, `{"value":"yes"}garbage`} {
		var r request
		c := newHTTPContext("POST", "/", "Application/JSON; Charset=UTF-8", body)
		if e := binder(t, 1024).Bind(c, &r); !errors.Is(e, xgin.ErrInvalidJSONBody) {
			t.Fatalf("normalized type bypassed strict JSON: %v", e)
		}
	}
}

func TestBinderMultipartBoundaryAndHeaderPreserved(t *testing.T) {
	type request struct {
		Value string `form:"value" json:"value" binding:"required"`
	}
	var data bytes.Buffer
	w := multipart.NewWriter(&data)
	if e := w.WriteField("value", "yes"); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	ct := strings.Replace(w.FormDataContentType(), "multipart/form-data", "Multipart/Form-Data", 1)
	var r request
	c := newHTTPContext("POST", "/", ct, data.String())
	if e := binder(t, 4096).Bind(c, &r); e != nil {
		t.Fatal(e)
	}
	if r.Value != "yes" || c.Request.Header.Get("Content-Type") != ct {
		t.Fatalf("value=%q type=%q", r.Value, c.Request.Header.Get("Content-Type"))
	}
}

func TestBinderFormDuplicateContentType(t *testing.T) {
	type request struct {
		Value string `form:"value"`
	}
	var r request
	c := newHTTPContext("POST", "/", "application/x-www-form-urlencoded", "value=yes")
	c.Request.Header.Add("Content-Type", "application/json")
	if e := binder(t, 1024).Bind(c, &r); !errors.Is(e, xgin.ErrUnsupportedContentType) {
		t.Fatalf("duplicate form content type accepted: %v", e)
	}
}

func TestBinderConcurrentPlanCompilation(t *testing.T) {
	b := binder(t, 1024)
	v := xgin.NewValidator(20)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			type request struct {
				ID    string `uri:"id" binding:"required"`
				Value string `json:"value" binding:"required"`
			}
			if e := b.Prepare(&request{}); e != nil {
				errs <- e
				return
			}
			c := newHTTPContext("POST", "/", "Application/JSON", `{"value":"yes"}`)
			c.Params = gin.Params{{Key: "id", Value: "1"}}
			var r request
			if e := b.Bind(c, &r); e != nil {
				errs <- e
				return
			}
			if r.ID != "1" || r.Value != "yes" {
				errs <- fmt.Errorf("unexpected DTO: %+v", r)
				return
			}
			if e := v.Validate(&r); e != nil {
				errs <- e
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

type RecursivePointer *RecursivePointer

func TestBinderRecursivePointerTypeRejected(t *testing.T) {
	type request struct {
		Value RecursivePointer `form:"value"`
	}
	if e := binder(t, 1024).Prepare(&request{}); e == nil {
		t.Fatal("recursive pointer type accepted")
	}
}

func TestBinderNormalizedBSONValidation(t *testing.T) {
	type request struct {
		Value string `codec:"value" bson:"value" binding:"required"`
	}
	data, e := bson.Marshal(bson.M{"value": "yes"})
	if e != nil {
		t.Fatal(e)
	}
	var r request
	if e := binder(t, 1024).Bind(newHTTPContext("POST", "/", "Application/BSON", string(data)), &r); e != nil || r.Value != "yes" {
		t.Fatalf("value=%q err=%v", r.Value, e)
	}
	data, e = bson.Marshal(bson.M{})
	if e != nil {
		t.Fatal(e)
	}
	if e := binder(t, 1024).Bind(newHTTPContext("POST", "/", "Application/BSON", string(data)), &r); e == nil {
		t.Fatal("BSON bypassed required validation")
	}

}

type contentTypeCase struct{ name, ct, body string }

type bodyLimitCase struct {
	name, body string
	limit      int64
	want       error
	status     int
}

type formMergeRequest struct {
	Value string `form:"value" json:"value" binding:"required"`
	Query string `form:"query" binding:"required"`
}

type formMergeCase struct{ name, method, contentType, body, wantValue, wantQuery string }

func TestBinderFormQueryAndBodySelection(t *testing.T) {
	cases := []formMergeCase{
		{"GET uses query", "GET", "application/json", `invalid`, "query-value", "query-only"},
		{"HEAD uses query", "HEAD", "", "", "query-value", "query-only"},
		{"JSON uses body and form query fallback", "POST", "application/json", `{"value":"json-value","Query":"forged"}`, "json-value", "query-only"},
		{"form body wins query", "POST", "application/x-www-form-urlencoded", "value=form-value&query=form-only", "form-value", "form-only"},
	}
	b := binder(t, 1024)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var result formMergeRequest
			if err := b.Bind(newHTTPContext(tc.method, "/?value=query-value&query=query-only", tc.contentType, tc.body), &result); err != nil {
				t.Fatal(err)
			}
			if result.Value != tc.wantValue || result.Query != tc.wantQuery {
				t.Fatalf("request=%+v want value=%q query=%q", result, tc.wantValue, tc.wantQuery)
			}
		})
	}
}

type recursiveFormNode struct {
	Next *recursiveFormNode `form:"next"`
}

type recursiveFormRequest struct {
	Node *recursiveFormNode `form:"node"`
}

func TestBinderRejectsRecursiveNestedForm(t *testing.T) {
	if err := binder(t, 1024).Prepare(&recursiveFormRequest{}); err == nil {
		t.Fatal("recursive nested form accepted")
	}
}
