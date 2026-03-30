package xgin

import (
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
)

// IValidator defines the interface for custom business logic validation.
type IValidator interface {
	Validate() error
}

var (
	iValidatorType    = reflect.TypeOf((*IValidator)(nil)).Elem()
	validatorInstance *Validator
	validatorDepth    int
	validatorMu       sync.RWMutex
)

// typeMetadata caches the structure and validation capabilities of a type.
type typeMetadata struct {
	isValidator bool
	kind        reflect.Kind
	fields      []fieldMetadata
}

// fieldMetadata describes relevant metadata for struct fields.
type fieldMetadata struct {
	index    int
	isStruct bool
	isSlice  bool
	isPtr    bool
	canValid bool
}

// Validator handles recursive validation with metadata caching.
type Validator struct {
	maxDepth int
	cache    sync.Map // concurrent-safe metadata cache
	errLog   io.Writer
}

// NewValidator creates a new validator instance with a specified depth limit.
func NewValidator(maxDepth int) *Validator {
	if maxDepth <= 0 {
		maxDepth = 20
	}
	return &Validator{
		maxDepth: maxDepth,
		errLog:   os.Stderr,
	}
}

// SharedValidator returns a global singleton validator.
func SharedValidator(maxDepth int) *Validator {
	if maxDepth <= 0 {
		maxDepth = 20
	}

	validatorMu.RLock()
	curr := validatorInstance
	currDepth := validatorDepth
	validatorMu.RUnlock()
	if curr != nil && currDepth == maxDepth {
		return curr
	}

	validatorMu.Lock()
	defer validatorMu.Unlock()
	if validatorInstance == nil || validatorDepth != maxDepth {
		validatorInstance = NewValidator(maxDepth)
		validatorDepth = maxDepth
	}
	return validatorInstance
}

// Validate triggers the recursive validation process for the given object.
func (v *Validator) Validate(obj any) (err error) {
	if obj == nil {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintf(v.errLog, "[xgin.Validator Panic] %v\n", r)
			err = fmt.Errorf("xgin.validator: panic recovered: %v", r)
		}
	}()

	val, ok := obj.(reflect.Value)
	if !ok {
		val = reflect.ValueOf(obj)
	}

	err = v.walk(val, 0)
	return err
}

// walk recursively traverses the object to execute IValidator implementations.
func (v *Validator) walk(val reflect.Value, depth int) error {
	if depth > v.maxDepth {
		return ErrMaxDepth
	}

	curr := val
	// Unpack pointers until the base value is reached
	for curr.Kind() == reflect.Pointer {
		if curr.IsNil() {
			return nil
		}
		curr = curr.Elem()
	}

	t := curr.Type()
	meta := v.getMetadata(t)

	// 1. Execute custom validation logic if supported
	if meta.isValidator {
		if err := v.execute(curr); err != nil {
			return err
		}
	}

	// 2. Recursive traversal for container types
	switch curr.Kind() {
	case reflect.Struct:
		for i := range meta.fields {
			f := &meta.fields[i]
			fieldVal := curr.Field(f.index)
			if f.isPtr && fieldVal.IsNil() {
				continue
			}
			// Only walk into fields that are structs, slices, or have a Validate method
			if f.isStruct || f.isSlice || f.canValid {
				if err := v.walk(fieldVal, depth+1); err != nil {
					return err
				}
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
			// Validate both keys and values
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

// execute attempts to call the Validate method on the value or its pointer.
func (v *Validator) execute(val reflect.Value) error {
	var runner IValidator
	// Try pointer receiver first
	if val.CanAddr() {
		addr := val.Addr()
		if addr.Type().Implements(iValidatorType) {
			runner = addr.Interface().(IValidator)
		}
	}
	// Fallback to value receiver
	if runner == nil && val.Type().Implements(iValidatorType) {
		runner = val.Interface().(IValidator)
	}

	if runner != nil {
		return runner.Validate()
	}
	return nil
}

// getMetadata retrieves or generates type metadata for efficient traversal.
func (v *Validator) getMetadata(t reflect.Type) *typeMetadata {
	if m, ok := v.cache.Load(t); ok {
		return m.(*typeMetadata)
	}

	m := &typeMetadata{kind: t.Kind()}

	// Check if type or its pointer implements IValidator
	if t.Implements(iValidatorType) || reflect.PointerTo(t).Implements(iValidatorType) {
		m.isValidator = true
	}

	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			// Skip unexported fields (unless they are anonymous/embedded)
			if f.PkgPath != "" && !f.Anonymous {
				continue
			}

			ft := f.Type
			isPtr := ft.Kind() == reflect.Pointer
			if isPtr {
				ft = ft.Elem()
			}

			m.fields = append(m.fields, fieldMetadata{
				index:    i,
				isPtr:    isPtr,
				isStruct: ft.Kind() == reflect.Struct,
				isSlice:  ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array,
				canValid: f.Type.Implements(iValidatorType) || reflect.PointerTo(f.Type).Implements(iValidatorType),
			})
		}
	}

	v.cache.Store(t, m)
	return m
}
