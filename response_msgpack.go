//go:build !nomsgpack

package xgin

import ginrender "github.com/gin-gonic/gin/render"

// newMsgPackRenderer keeps Gin's optional MessagePack build support intact.
func newMsgPackRenderer(data any) (ginrender.Render, error) {
	return ginrender.MsgPack{Data: data}, nil
}
