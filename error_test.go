package xgin_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/lazyboon/xgin"
)

type requestStatusCase struct {
	name   string
	cause  error
	status int
}

func TestDefaultRequestStatus(t *testing.T) {
	cases := []requestStatusCase{
		{"decoder failure", errors.New("private decoder detail"), 400},
		{"missing media type", xgin.ErrMissingContentType, 415},
		{"unsupported media type", xgin.ErrUnsupportedContentType, 415},
		{"body limit", &http.MaxBytesError{Limit: 1}, 413},
		{"invalid target", xgin.ErrInvalidBindTarget, 500},
		{"validation depth", xgin.ErrMaxDepth, 500},
		{"validation panic", xgin.ErrValidationPanic, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := xgin.DefaultRequestStatus(fmt.Errorf("wrapped: %w", tc.cause)); got != tc.status {
				t.Errorf("status=%d want=%d", got, tc.status)
			}
		})
	}
}
