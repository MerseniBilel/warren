// Package validate is the port transport adapters validate decoded requests
// through, plus the standard-library implementation of validate:"required".
//
// The full constraint vocabulary — email, min, oneof — lives in
// warren/validate/playground, its own module wrapping
// go-playground/validator/v10, because the core module is the standard
// library and dig, permanently. Core's Required() enforces presence and
// REFUSES every other token at plan time: a validator that silently ignored
// validate:"required,email" would under-validate in production, which is the
// failure mode warren/config already refuses for configuration.
//
// Validation is planned once, at boot, and runs as a closure per request —
// the transport registrar composes it into the route between decode and the
// handler, which is why handlers never invoke it and no adapter can forget
// to.
package validate

import (
	stderrors "errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/MerseniBilel/warren/errors"
)

// Validator compiles a validation plan for a type at boot and reports every
// constraint it cannot enforce.
type Validator interface {
	// Plan compiles the validate: tags of t, which must be a struct type. It
	// returns an error naming every tag it cannot enforce — a boot failure,
	// never a silent downgrade.
	Plan(t reflect.Type) (Rule, error)
}

// Rule validates one value on the request path. v is a non-nil pointer to
// the type the Rule was planned for. It returns nil, or an *errors.Error
// carrying CodeInvalid with one detail per failing field.
type Rule func(v any) error

// PlanFor plans T once and returns a typed closure — the form the transport
// registrar uses at registration.
func PlanFor[T any](v Validator) (func(*T) error, error) {
	rule, err := v.Plan(reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	return func(t *T) error { return rule(t) }, nil
}

// Required returns the standard-library Validator: presence checking for
// fields tagged validate:"required", and a plan-time refusal of every other
// token.
func Required() Validator { return required{} }

// None returns a Validator that enforces nothing and accepts every tag. It
// is the explicit opt-out for a project using constraints core cannot
// enforce before warren/validate/playground ships: validation becomes the
// handler's business, stated in one visible line of wiring rather than
// silently skipped.
func None() Validator { return none{} }

type none struct{}

func (none) Plan(reflect.Type) (Rule, error) {
	return func(any) error { return nil }, nil
}

type required struct{}

// field is one planned presence check.
type field struct {
	path  string // the json name, dotted for nested fields
	index []int
}

func (required) Plan(t reflect.Type) (Rule, error) {
	if t == nil || t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("validate: Plan requires a struct type, got %v", t)
	}
	var fields []field
	var unsupported []string
	collect(t, nil, nil, &fields, &unsupported)
	if len(unsupported) > 0 {
		return nil, errUnsupported(t, unsupported)
	}
	if len(fields) == 0 {
		return func(any) error { return nil }, nil
	}
	return func(v any) error {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				return errors.Invalid("request", fmt.Errorf("validate: nil %v", t))
			}
			rv = rv.Elem()
		}
		var missing []string
		for _, f := range fields {
			// FieldByIndexErr rather than FieldByIndex: a nil pointer
			// anywhere along the path makes the latter PANIC, and nesting a
			// required field under an optional *Address is ordinary. An
			// unreachable field is not reported — the nil pointer above it
			// either satisfies its own `required` or fails it, and one cause
			// should produce one message.
			fv, err := rv.FieldByIndexErr(f.index)
			if err != nil {
				continue
			}
			if isZero(fv) {
				missing = append(missing, f.path)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		return errMissing(missing)
	}, nil
}

// collect walks the struct, gathering required leaves and every token this
// implementation cannot enforce.
func collect(t reflect.Type, path []string, index []int, out *[]field, unsupported *[]string) {
	for i := range t.NumField() {
		sf := t.Field(i)
		findex := append(append([]int{}, index...), i)

		// An EMBEDDED struct is flattened, exactly as encoding/json
		// promotes its fields to the top level — so `Base.email` is not a
		// key any client ever sends, and an unexported embed is not a hole
		// in the validation. The path does not grow, and the field's own
		// tag is not a leaf rule: `validate:"required"` on an embed is
		// meaningless, since the embedded value is always present.
		if sf.Anonymous {
			et := sf.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && et.NumField() > 0 && hasTags(et) {
				collect(et, path, findex, out, unsupported)
			}
			continue
		}

		if !sf.IsExported() {
			// A tag on a field encoding/json cannot reach is a mistake, and
			// silently ignoring it is the failure this package refuses.
			if sf.Tag.Get("validate") != "" {
				*unsupported = append(*unsupported, "on unexported field "+sf.Name)
			}
			continue
		}

		name := jsonName(sf)
		fpath := append(append([]string{}, path...), name)

		tag := sf.Tag.Get("validate")
		req := false
		for token := range strings.SplitSeq(tag, ",") {
			switch token = strings.TrimSpace(token); token {
			case "":
			case "required":
				req = true
			default:
				*unsupported = append(*unsupported, token)
			}
		}
		if req {
			*out = append(*out, field{path: strings.Join(fpath, "."), index: findex})
		}

		// A slice or map of tagged elements cannot be validated: this
		// implementation walks fields, not elements, and there is no token
		// that could ask it to. REFUSING is the point — silently passing a
		// []LineItem whose SKU is required is the exact failure this
		// package exists to prevent.
		if et, ok := taggedElement(sf.Type); ok {
			*unsupported = append(*unsupported,
				"element validation for "+strings.Join(fpath, ".")+" ("+et.String()+")")
			continue
		}

		// Nested structs nest their path; time.Time and friends are leaves,
		// so only structs with their own tagged fields are descended into.
		// A POINTER to one is descended too — `required` on it proves the
		// pointer is non-nil and says nothing about what is inside.
		ft := sf.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.NumField() > 0 && hasTags(ft) {
			collect(ft, fpath, findex, out, unsupported)
		}
	}
}

// taggedElement reports the element type of a slice, array or map whose
// elements carry validate tags.
func taggedElement(t reflect.Type) (reflect.Type, bool) {
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
	default:
		return nil, false
	}
	et := t.Elem()
	if et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	if et.Kind() == reflect.Struct && et.NumField() > 0 && hasTags(et) {
		return et, true
	}
	return nil, false
}

// hasTags reports whether t or anything beneath it carries a validate: tag —
// the cheap guard that keeps the walk out of stdlib value types.
func hasTags(t reflect.Type) bool {
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Tag.Get("validate") != "" {
			return true
		}
		if sf.Type.Kind() == reflect.Struct && sf.Type != t && hasTags(sf.Type) {
			return true
		}
	}
	return false
}

// jsonName is the key the API client knows the field by: its json name when
// it has one, else the Go field name.
func jsonName(sf reflect.StructField) string {
	tag := sf.Tag.Get("json")
	if tag == "" || tag == "-" {
		return sf.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return sf.Name
	}
	return tag
}

// isZero reports emptiness the way presence checking means it: the zero
// value of the field's kind, with nil counting for the reference kinds.
func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	default:
		return v.IsZero()
	}
}

// errMissing renders one error for the whole request: the message names
// every missing field, and each gets a detail keyed by the name the API
// client knows it by.
func errMissing(missing []string) error {
	err := errors.Invalid(strings.Join(missing, ", "), stderrors.New("required"))
	for _, m := range missing {
		err = err.WithDetail(m, "is required")
	}
	return err
}

// diagnostic carries a rendered multi-line block; the text is the contract.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errUnsupported(t reflect.Type, tokens []string) error {
	return diagnostic(fmt.Sprintf(
		"✗ unsupported validation constraint\n\n"+
			"    %s uses constraints the standard-library validator cannot enforce:\n"+
			"    %s\n\n"+
			"  Core enforces validate:\"required\" only — refusing the rest rather than\n"+
			"  silently under-validating. Two ways forward:\n"+
			"    • install github.com/MerseniBilel/warren/validate/playground and pass\n"+
			"      playground.New() where the validator is configured, or\n"+
			"    • opt out explicitly with validate.None() and validate in the handler.",
		t, strings.Join(tokens, ", ")))
}
