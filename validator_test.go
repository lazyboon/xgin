package xgin_test

import (
	"errors"
	"reflect"
	"testing"

	xgin "github.com/lazyboon/xgin"
)

var errBusiness = errors.New("business")

type Rule string

func (r *Rule) Validate() error {
	if *r == "bad" {
		return errBusiness
	}
	return nil
}

type BadParent struct{ Child Rule }

func (p *BadParent) Validate() error { return errBusiness }

type PanicRule struct{}

func (*PanicRule) Validate() error { panic("secret") }

func TestValidatorContainers(t *testing.T) {
	for _, tt := range []validationContainerCase{
		{"map_value", map[string]Rule{"a": "bad"}}, {"map_key", map[Rule]string{"bad": "a"}}, {"slice", []Rule{"bad"}}, {"array", [1]Rule{"bad"}}, {"interface", interfaceRule{Rule("bad")}}, {"pointer", pointerRule{new(Rule("bad"))}}, {"root_value", Rule("bad")}, {"reflect_value", reflect.ValueOf(Rule("bad"))}, {"parent", &BadParent{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if e := xgin.NewValidator(20).Validate(tt.value); !errors.Is(e, errBusiness) {
				t.Fatalf("%v", e)
			}
		})
	}
}

func TestValidatorCycleAndNil(t *testing.T) {
	type node struct{ Next *node }
	n := &node{}
	n.Next = n
	var iface any
	iface = &iface
	m := map[string]any{}
	m["self"] = m
	s := make([]any, 1)
	s[0] = s
	for _, tt := range []validationCycleCase{{"pointer_cycle", n}, {"interface_cycle", iface}, {"map_cycle", m}, {"slice_cycle", s}} {
		t.Run(tt.name, func(t *testing.T) {
			if e := xgin.NewValidator(4).Validate(tt.value); !errors.Is(e, xgin.ErrMaxDepth) {
				t.Fatal(e)
			}
		})
	}
	for _, tt := range []validationNilCase{{"nil", nil}, {"typed_nil", (*PanicRule)(nil)}, {"invalid_reflect", reflect.Value{}}} {
		t.Run(tt.name, func(t *testing.T) {
			if e := xgin.NewValidator(4).Validate(tt.value); e != nil {
				t.Fatal(e)
			}
		})
	}
	e := xgin.NewValidator(4).Validate(&PanicRule{})
	var panicErr *xgin.PanicError
	if !errors.Is(e, xgin.ErrValidationPanic) || !errors.As(e, &panicErr) || panicErr.StackTrace() == "" {
		t.Fatalf("panic diagnostics: %v", e)
	}
}

func TestValidatorDefinedPointerCycle(t *testing.T) {
	var p RecursivePointer
	p = RecursivePointer(&p)
	if e := xgin.NewValidator(4).Validate(p); !errors.Is(e, xgin.ErrMaxDepth) {
		t.Fatalf("defined pointer cycle: %v", e)
	}
}

type interfaceRule struct{ V any }

type pointerRule struct{ V *Rule }

type validationContainerCase struct {
	name  string
	value any
}

type validationCycleCase struct {
	name  string
	value any
}

type validationNilCase struct {
	name  string
	value any
}

// One nested object is one depth step; pointer unwrapping has its own limit.
type depthLeaf struct{ Value Rule }

type depthParent struct{ Child depthLeaf }

func TestValidatorDepthBoundary(t *testing.T) {
	validator := xgin.NewValidator(1)
	if err := validator.Validate(&depthLeaf{Value: "good"}); err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(&depthParent{Child: depthLeaf{Value: "bad"}}); !errors.Is(err, xgin.ErrMaxDepth) {
		t.Fatalf("got %v want maximum-depth error", err)
	}
}
