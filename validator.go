package xgin

import (
	"errors"
	"reflect"
	"runtime/debug"
	"sync"
)

// ---------------------------------------------------------------------------------------------------------------------

var (
	// ErrMaxDepth reports excessive nesting or consecutive pointer/interface unwrapping.
	ErrMaxDepth = errors.New("xgin: maximum recursion depth exceeded")

	// ErrValidationPanic reports a panic in custom validation and indicates an internal error.
	ErrValidationPanic = errors.New("xgin: validation panicked")
)

var iValidatorType = reflect.TypeFor[IValidator]()

// ---------------------------------------------------------------------------------------------------------------------

// IValidator defines custom rules. Implementations must not mutate the value or its references.
type IValidator interface {
	Validate() error
}

// ---------------------------------------------------------------------------------------------------------------------

// typeMetadata caches validation support and fields requiring recursive traversal.
type typeMetadata struct {
	isValidator bool
	fields      []fieldMetadata
}

// fieldMetadata identifies an exported field to visit.
type fieldMetadata struct {
	index int
}

// ---------------------------------------------------------------------------------------------------------------------

// Validator recursively executes IValidator using an instance-local type cache.
// Type metadata remains cached for the instance lifetime.
// Gin binders are responsible for binding tag validation.
type Validator struct {
	maxDepth int
	cache    sync.Map // Concurrent type metadata cache.
}

// NewValidator creates a validator; nonpositive limits use the default depth.
// Depth bounds object nesting and consecutive pointer/interface unwrapping separately.
func NewValidator(maxDepth int) *Validator {
	if maxDepth <= 0 {
		maxDepth = DefaultValidatorMaxDepth
	}
	return &Validator{
		maxDepth: maxDepth,
	}
}

// Validate checks parents before children and returns the first original rule error.
// Nil values are skipped; validation panics retain diagnostic details in PanicError.
func (v *Validator) Validate(obj any) (err error) {
	if obj == nil {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			// Keep panic details for internal diagnostics; the error mapper controls public output.
			err = &PanicError{kind: ErrValidationPanic, value: r, stack: debug.Stack()}
		}
	}()

	val, ok := obj.(reflect.Value)
	if !ok {
		val = reflect.ValueOf(obj)
	}

	err = v.walk(val, 0)
	return err
}

// walk visits structs, containers, and pointer/interface wrappers.
func (v *Validator) walk(val reflect.Value, depth int) error {
	if !val.IsValid() || !val.CanInterface() {
		return nil
	}

	curr := val
	// Bound unwrapping separately: a pointer/interface cycle never reaches a child walk.
	for steps := 0; curr.Kind() == reflect.Pointer || curr.Kind() == reflect.Interface; steps++ {
		if curr.IsNil() {
			return nil
		}
		if steps >= v.maxDepth {
			return ErrMaxDepth
		}
		curr = curr.Elem()
	}
	if depth > v.maxDepth {
		return ErrMaxDepth
	}

	t := curr.Type()
	meta := v.getMetadata(t)

	// Validate the current object before visiting its children.
	if meta.isValidator {
		if err := v.execute(curr); err != nil {
			return err
		}
	}

	// Recurse into exported fields and container entries.
	switch curr.Kind() {
	case reflect.Struct:
		for i := range meta.fields {
			if err := v.walk(curr.Field(meta.fields[i].index), depth+1); err != nil {
				return err
			}
		}

	case reflect.Slice, reflect.Array:
		for j := 0; j < curr.Len(); j++ {
			if err := v.walk(curr.Index(j), depth+1); err != nil {
				return err
			}
		}

	case reflect.Map:
		iter := curr.MapRange()
		for iter.Next() {
			// Both map keys and values can implement custom validation.
			if err := v.walk(iter.Key(), depth+1); err != nil {
				return err
			}
			if err := v.walk(iter.Value(), depth+1); err != nil {
				return err
			}
		}

	default:
		return nil
	}

	return nil
}

// execute calls the value or pointer implementation of IValidator.
func (v *Validator) execute(val reflect.Value) error {
	// Map entries and other unaddressable values need a shallow copy for pointer receivers.
	// Validation is read-only, so mutations through shared references are prohibited.
	if !val.CanAddr() {
		addressable := reflect.New(val.Type()).Elem()
		addressable.Set(val)
		val = addressable
	}
	var runner IValidator
	// Prefer pointer receivers, which also include value-receiver methods.
	if val.CanAddr() {
		addr := val.Addr()
		if addr.Type().Implements(iValidatorType) {
			runner = addr.Interface().(IValidator)
		}
	}
	// Fall back to the value implementation when needed.
	if runner == nil && val.Type().Implements(iValidatorType) {
		runner = val.Interface().(IValidator)
	}

	if runner != nil {
		return runner.Validate()
	}
	return nil
}

// getMetadata returns cached validation and traversal metadata.
func (v *Validator) getMetadata(t reflect.Type) *typeMetadata {
	if m, ok := v.cache.Load(t); ok {
		return m.(*typeMetadata)
	}

	m := &typeMetadata{}

	// Check both method sets, including named scalar types.
	if t.Implements(iValidatorType) || reflect.PointerTo(t).Implements(iValidatorType) {
		m.isValidator = true
	}

	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			// Unexported fields, including anonymous fields, cannot be safely accessed.
			if f.PkgPath != "" {
				continue
			}

			ft := f.Type
			switch ft.Kind() {
			case reflect.Struct, reflect.Slice, reflect.Array, reflect.Map, reflect.Pointer, reflect.Interface:
				// Containers and wrappers are inspected recursively by walk.
			default:
				if !ft.Implements(iValidatorType) && !reflect.PointerTo(ft).Implements(iValidatorType) {
					continue
				}
			}
			m.fields = append(m.fields, fieldMetadata{index: i})
		}
	}

	v.cache.Store(t, m)
	return m
}

// ---------------------------------------------------------------------------------------------------------------------
