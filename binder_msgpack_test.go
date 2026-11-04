//go:build !nomsgpack

package xgin_test

import (
	"testing"

	"github.com/ugorji/go/codec"
)

func TestBinderMessagePack(t *testing.T) {
	type request struct {
		Value string `codec:"value" binding:"required"`
	}
	var data []byte
	if err := codec.NewEncoderBytes(&data, &codec.MsgpackHandle{}).Encode(map[string]string{"value": "yes"}); err != nil {
		t.Fatal(err)
	}
	var got request
	if err := binder(t, 1024).Bind(newHTTPContext("POST", "/", "Application/Msgpack", string(data)), &got); err != nil || got.Value != "yes" {
		t.Fatalf("request=%+v error=%v", got, err)
	}
}
