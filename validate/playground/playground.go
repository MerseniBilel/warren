// Package playground implements Warren's validate.Validator over
// go-playground/validator, so the constraints core deliberately refuses —
// email, uuid, min, max, oneof, and the rest of that vocabulary — become
// available by adding one module.
//
// Core ships `required` and nothing else, and REFUSES the rest at boot rather
// than silently under-validating a request. That refusal is the right default
// and it is also why this module exists: `validate:"required,email"` is the
// most ordinary tag in a Go backend, and without this the only alternative is
// validate.None(), which turns validation off for the whole service.
//
// # What it costs
//
// Measured 2026-08-02: 8 third-party modules. A service that keeps
// validate.Required() pays none of them — that is what this being its own
// module buys.
//
// # Usage
//
//	warren.New(...).Validator(playground.New())
//
// Every route's rules are compiled ONCE, at boot step 5, exactly as core's
// are: Plan runs per request type at registration, and the request path runs
// a closure.
package playground

import (
	"reflect"
	"strings"
	"sync"

	pv "github.com/go-playground/validator/v10"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/validate"
)

// Option configures the validator.
type Option struct{ apply func(*config) }

type config struct {
	tagName  string
	register []registration
}

type registration struct {
	tag string
	fn  func(value any) bool
}

// New returns a validate.Validator backed by go-playground/validator.
//
// Pass it where the validator is configured:
//
//	a := warren.New(modules...)
//	a.Validator(playground.New())
//
// It reads the same `validate:` tag core does, so moving a service onto it
// changes no struct — only which constraints boot.
func New(opts ...Option) validate.Validator {
	cfg := config{tagName: "validate"}
	for _, o := range opts {
		o.apply(&cfg)
	}
	v := pv.New(pv.WithRequiredStructEnabled())
	v.SetTagName(cfg.tagName)
	for _, r := range cfg.register {
		fn := r.fn
		_ = v.RegisterValidation(r.tag, func(fl pv.FieldLevel) bool {
			return fn(fl.Field().Interface())
		})
	}
	// The field name a client sees is the `json` name, not the Go name: an
	// API that reports "Email is required" for a field the client sent as
	// "email" is describing the server's internals.
	v.RegisterTagNameFunc(jsonName)
	return &validator{v: v}
}

// TagName reads constraints from a different struct tag. The default is
// "validate", which is what core reads — change it only when migrating from a
// codebase that used another name.
func TagName(name string) Option {
	return Option{apply: func(c *config) { c.tagName = name }}
}

// Register adds a custom constraint, usable as validate:"<tag>". fn receives
// the field's value and reports whether it is acceptable.
//
//	playground.Register("currency", func(v any) bool {
//	    s, ok := v.(string)
//	    return ok && len(s) == 3
//	})
func Register(tag string, fn func(value any) bool) Option {
	return Option{apply: func(c *config) {
		c.register = append(c.register, registration{tag: tag, fn: fn})
	}}
}

type validator struct {
	v    *pv.Validate
	tags sync.Map // token → recognised; boot-time only
}

var _ validate.Validator = (*validator)(nil)

// Plan compiles t's constraints at boot.
//
// go-playground validates by reflection per call rather than by compiling a
// plan, so what this can check at boot is that every tag on the type is one
// the library KNOWS — which is the half that matters, because an unknown tag
// is the failure core refuses to let ship. A tag the library does not
// recognise makes it panic at validation time; catching it here turns a
// request-path panic into a boot diagnostic.
func (val *validator) Plan(t reflect.Type) (validate.Rule, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, errNotAStruct(t)
	}
	if err := val.checkTags(t, nil, map[reflect.Type]bool{}); err != nil {
		return nil, err
	}
	return func(v any) error {
		if err := val.v.Struct(v); err != nil {
			return translate(err)
		}
		return nil
	}, nil
}

// checkTags walks the struct at BOOT and asks the library about every tag, so
// a typo is a boot failure rather than a panic on the first request that
// exercises the field.
func (val *validator) checkTags(t reflect.Type, path []string, seen map[reflect.Type]bool) error {
	if seen[t] {
		return nil // a recursive type is walked once
	}
	seen[t] = true

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		// A fresh slice: append to `path` in place and sibling fields would
		// share and overwrite the backing array, so the reported path would
		// be whichever field was walked last.
		name := append(append([]string{}, path...), jsonNameOf(f))
		if tag := f.Tag.Get("validate"); tag != "" && tag != "-" {
			if bad := val.unknownTokens(tag); len(bad) > 0 {
				return errUnknownConstraint(t, strings.Join(name, "."), bad)
			}
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			if err := val.checkTags(ft, name, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// unknownTokens returns the constraints the library does not know.
//
// The whole token is probed, parameter included: "min=2" is what the user
// wrote, and "min" alone panics because the constraint requires one.
func (val *validator) unknownTokens(tag string) []string {
	var bad []string
	for _, token := range strings.Split(tag, ",") {
		token = strings.TrimSpace(token)
		name, _, _ := strings.Cut(token, "=")
		name = strings.TrimSpace(name)
		switch name {
		case "", "omitempty", "omitnil", "required", "dive", "keys", "endkeys",
			"structonly", "nostructlevel", "isdefault", "required_if",
			"required_unless", "required_with", "required_with_all",
			"required_without", "required_without_all", "excluded_if",
			"excluded_unless", "excluded_with", "excluded_with_all",
			"excluded_without", "excluded_without_all", "-":
			continue
		}
		if !val.probeTag(token) {
			bad = append(bad, name)
		}
	}
	return bad
}

// translate turns go-playground's error into Warren's, keyed by the name the
// CLIENT knows the field by.
//
// The message is deliberately short and mechanical — "must be a valid email
// address" — rather than the library's default sentence, which names the Go
// struct and reads like a stack trace to an API consumer.
func translate(err error) error {
	var invalid *pv.InvalidValidationError
	if asInvalid(err, &invalid) {
		return errors.Internal(err)
	}
	fes, ok := err.(pv.ValidationErrors)
	if !ok || len(fes) == 0 {
		return errors.Invalid("request", err)
	}
	fields := make([]string, 0, len(fes))
	for _, fe := range fes {
		fields = append(fields, fieldPath(fe))
	}
	// One error for the whole request, with a detail per failing field —
	// matching core's shape exactly, so an adapter renders both identically
	// and a client cannot tell which validator is installed.
	out := errors.Invalid(strings.Join(fields, ", "), stringError("invalid"))
	for _, fe := range fes {
		out = out.WithDetail(fieldPath(fe), describe(fe))
	}
	return out
}

type stringError string

func (e stringError) Error() string { return string(e) }
