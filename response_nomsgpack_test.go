//go:build nomsgpack

package xgin_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/lazyboon/xgin"
)

func TestMessagePackDisabledBeforeCommit(t *testing.T) {
	context := newHTTPContext("GET", "/", "", "")
	adapter := xgin.MustNew(xgin.Config{})
	adapter.Render(context, xgin.Response{ContentType: xgin.ContentTypeMsgPack, Data: "disabled"})
	if context.Writer.Written() || context.Writer.Status() != http.StatusInternalServerError {
		t.Fatalf("written=%v status=%d", context.Writer.Written(), context.Writer.Status())
	}
	if len(context.Errors) != 1 || !errors.Is(context.Errors.Last().Err, xgin.ErrInvalidRender) {
		t.Fatalf("errors=%v", context.Errors)
	}
}
