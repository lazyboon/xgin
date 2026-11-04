package xgin_test

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	xgin "github.com/lazyboon/xgin"
)

type SingleFile struct {
	File *multipart.FileHeader `form:"file"`
}

type RequiredFile struct {
	File *multipart.FileHeader `form:"file" binding:"required"`
}

type ValueFile struct {
	File multipart.FileHeader `form:"file" binding:"required"`
}

type ManyFiles struct {
	Files []*multipart.FileHeader `form:"file" binding:"required"`
}

type ArrayFiles struct {
	Files [2]multipart.FileHeader `form:"file" binding:"required"`
}

type ValueFiles struct {
	Files []multipart.FileHeader `form:"file"`
}

type PointerArrayFiles struct {
	Files [2]*multipart.FileHeader `form:"file"`
}

type MixedFiles struct {
	Name string                `form:"name" binding:"required"`
	File *multipart.FileHeader `form:"file" binding:"required"`
}

type EmbeddedFiles struct{ SingleFile }

type DefinedFile multipart.FileHeader
type UnsupportedFile struct {
	File *DefinedFile `form:"file"`
}

type NestedFile struct {
	Nested SingleFile `form:"nested"`
}

type NestedBodyFile struct {
	Nested SingleFile `json:"nested"`
}

func uploadContext(t *testing.T, count int) *gin.Context {
	t.Helper()
	var data bytes.Buffer
	writer := multipart.NewWriter(&data)
	if e := writer.WriteField("name", "normal"); e != nil {
		t.Fatal(e)
	}
	if e := writer.WriteField("Filename", "forged.txt"); e != nil {
		t.Fatal(e)
	}
	if e := writer.WriteField("file", `{"Filename":"forged.txt","Size":999}`); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < count; i++ {
		part, e := writer.CreateFormFile("file", "real.txt")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(part, "hello"); e != nil {
			t.Fatal(e)
		}
	}
	if e := writer.Close(); e != nil {
		t.Fatal(e)
	}
	c := newHTTPContext("POST", "/?Filename=forged.txt&Size=999&Header=bad&name=query", writer.FormDataContentType(), data.String())
	t.Cleanup(func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	})
	return c
}

func TestFileSingleAndEmbedded(t *testing.T) {
	b := binder(t, 4096)
	var r SingleFile
	if e := b.Prepare(&r); e != nil {
		t.Fatal(e)
	}
	if e := b.Bind(uploadContext(t, 1), &r); e != nil {
		t.Fatal(e)
	}
	if r.File == nil || r.File.Filename != "real.txt" || r.File.Size != 5 {
		t.Fatalf("file=%+v", r.File)
	}
	f, e := r.File.Open()
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(f)
	_ = f.Close()
	if e != nil || string(body) != "hello" {
		t.Fatalf("content %s err %v", body, e)
	}
	var embedded EmbeddedFiles
	if e := b.Bind(uploadContext(t, 1), &embedded); e != nil || embedded.File == nil {
		t.Fatalf("embedded=%+v err=%v", embedded, e)
	}
}

func TestFileCollections(t *testing.T) {
	b := binder(t, 4096)
	var many ManyFiles
	if e := b.Bind(uploadContext(t, 2), &many); e != nil || len(many.Files) != 2 {
		t.Fatalf("many=%+v err=%v", many, e)
	}
	var array ArrayFiles
	if e := b.Bind(uploadContext(t, 2), &array); e != nil || array.Files[1].Size != 5 {
		t.Fatalf("array=%+v err=%v", array, e)
	}
	if e := b.Bind(uploadContext(t, 1), &array); !errors.Is(e, binding.ErrMultiFileHeaderLenInvalid) {
		t.Fatalf("array count: %v", e)
	}
	var vals ValueFiles
	if e := b.Bind(uploadContext(t, 2), &vals); e != nil || len(vals.Files) != 2 || vals.Files[1].Size != 5 {
		t.Fatalf("value files=%+v err=%v", vals, e)
	}
	var ptrs PointerArrayFiles
	if e := b.Bind(uploadContext(t, 2), &ptrs); e != nil || ptrs.Files[1] == nil {
		t.Fatalf("pointer array=%+v err=%v", ptrs, e)
	}
}

func TestFileMissingAndForgery(t *testing.T) {
	b := binder(t, 4096)
	contexts := []*gin.Context{newHTTPContext("GET", "/?Filename=forged.txt&Size=999", "", ""), newHTTPContext("HEAD", "/?Filename=forged.txt", "", ""), newHTTPContext("POST", "/?Filename=forged.txt", "application/json", `{"File":{"Filename":"forged.txt"}}`), newHTTPContext("POST", "/", "application/x-www-form-urlencoded", "Filename=forged.txt&Size=999"), uploadContext(t, 0)}
	for _, c := range contexts {
		var optional SingleFile
		if e := b.Bind(c, &optional); e != nil || optional.File != nil {
			t.Fatalf("method=%s type=%s optional=%+v err=%v", c.Request.Method, c.ContentType(), optional, e)
		}
		var required RequiredFile
		if e := b.Bind(c, &required); e == nil {
			t.Fatalf("required accepted absent file for %s %s", c.Request.Method, c.ContentType())
		}
	}
	var value ValueFile
	if e := b.Bind(uploadContext(t, 1), &value); e != nil || value.File.Size != 5 {
		t.Fatalf("value=%+v err=%v", value, e)
	}
	// Gin preserves its native required-on-struct behavior; use a pointer for presence.
	if e := b.Bind(uploadContext(t, 0), &value); e != nil {
		t.Fatalf("native value file semantics changed: %v", e)
	}
}

func TestFileMixedLimitAndAtomicity(t *testing.T) {
	b := binder(t, 4096)
	var mixed MixedFiles
	if e := b.Bind(uploadContext(t, 1), &mixed); e != nil || mixed.Name != "normal" || mixed.File == nil {
		t.Fatalf("mixed=%+v err=%v", mixed, e)
	}
	original := &multipart.FileHeader{Filename: "original"}
	r := MixedFiles{Name: "original", File: original}
	c := uploadContext(t, 0)
	if e := b.Bind(c, &r); e == nil {
		t.Fatal("required absent accepted")
	}
	if r.Name != "original" || r.File != original {
		t.Fatalf("target mutated: %+v", r)
	}
	var single SingleFile
	e := binder(t, 64).Bind(uploadContext(t, 1), &single)
	if e == nil || xgin.DefaultRequestStatus(e) != 413 {
		t.Fatalf("file body limit err=%v", e)
	}
}

func TestFileUnsupportedTypes(t *testing.T) {
	b := binder(t, 4096)
	for _, r := range []any{&UnsupportedFile{}, &NestedFile{}, &NestedBodyFile{}} {
		if e := b.Prepare(r); e == nil {
			t.Fatalf("unsupported file shape accepted: %T", r)
		}
	}
}

func TestFileHandlerTemporaryCleanup(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	x, e := xgin.New(xgin.Config{MaxBodyBytes: 4096})
	if e != nil {
		t.Fatal(e)
	}
	engine := gin.New()
	engine.MaxMultipartMemory = 1
	var temporary string
	engine.POST("/", x.Handler(func(c *xgin.Context, r *RequiredFile) CustomRender {
		f, e := r.File.Open()
		if e != nil {
			t.Error(e)
			return CustomRender{500}
		}
		disk, ok := f.(*os.File)
		if !ok {
			t.Error("file did not spill to disk")
		} else {
			temporary = disk.Name()
		}
		_ = f.Close()
		return CustomRender{201}
	}))
	c := uploadContext(t, 1)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, c.Request)
	if w.Code != 201 || temporary == "" {
		t.Fatalf("status=%d temporary=%q", w.Code, temporary)
	}
	if _, e := os.Stat(temporary); !os.IsNotExist(e) {
		t.Fatalf("temporary file retained: %v", e)
	}
}

type StandardFileAlias = multipart.FileHeader
type AliasedFile struct {
	File *StandardFileAlias `form:"file"`
}

type DefinedFiles []*multipart.FileHeader
type DefinedFileSlice struct {
	Files DefinedFiles `form:"file"`
}

type WrappedFilePointer struct {
	File **multipart.FileHeader `form:"file"`
}

type JSONFile struct {
	File *multipart.FileHeader `json:"file"`
}

func TestFileAliasesAndRejectedWrappers(t *testing.T) {
	b := binder(t, 4096)
	var r AliasedFile
	if e := b.Bind(uploadContext(t, 1), &r); e != nil || r.File == nil {
		t.Fatalf("alias=%+v err=%v", r, e)
	}
	for _, bad := range []any{&DefinedFileSlice{}, &WrappedFilePointer{}, &JSONFile{}} {
		if e := b.Prepare(bad); e == nil {
			t.Fatalf("unsupported file shape accepted: %T", bad)
		}
	}
	var many ManyFiles
	if e := b.Bind(uploadContext(t, 0), &many); e == nil {
		t.Fatal("required missing file slice accepted")
	}
	var array ArrayFiles
	if e := b.Bind(uploadContext(t, 0), &array); e == nil {
		t.Fatal("required missing file array accepted")
	}
}
