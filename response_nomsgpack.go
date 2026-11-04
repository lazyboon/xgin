//go:build nomsgpack

package xgin

import (
	"fmt"
	ginrender "github.com/gin-gonic/gin/render"
)

// newMsgPackRenderer reports the format disabled by Gin's nomsgpack build tag.
func newMsgPackRenderer(_ any) (ginrender.Render, error) {
	return nil, fmt.Errorf("%w: MessagePack is disabled by nomsgpack", ErrInvalidRender)
}
