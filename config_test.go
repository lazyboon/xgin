package xgin_test

import (
	"testing"

	"github.com/lazyboon/xgin"
)

type invalidConfigCase struct {
	name   string
	config xgin.Config
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []invalidConfigCase{
		{name: "negative body limit", config: xgin.Config{MaxBodyBytes: -1}},
		{name: "negative validation depth", config: xgin.Config{ValidatorMaxDepth: -1}},
		{name: "excessive validation depth", config: xgin.Config{ValidatorMaxDepth: xgin.ValidatorMaxDepthLimit + 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := xgin.New(tc.config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestNewTreatsTypedNilDependenciesAsDefaults(t *testing.T) {
	var binder *xgin.Binder
	var validator *xgin.Validator
	if _, err := xgin.New(xgin.Config{Binder: binder, Validator: validator}); err != nil {
		t.Fatal(err)
	}
}
