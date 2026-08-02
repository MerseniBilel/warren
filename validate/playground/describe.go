package playground

import (
	stderrors "errors"
	"reflect"
	"strings"

	pv "github.com/go-playground/validator/v10"
)

// describe renders one field failure as a short, mechanical sentence a client
// can show a user.
//
// go-playground's own Error() reads "Key: 'RegisterUser.Email' Error:Field
// validation for 'Email' failed on the 'email' tag" — which names the Go
// struct and reads like a stack trace to whoever is filling in a form. These
// say what to do instead.
func describe(fe pv.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "url", "uri":
		return "must be a valid URL"
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "min":
		return "must be at least " + fe.Param() + lengthUnit(fe)
	case "max":
		return "must be at most " + fe.Param() + lengthUnit(fe)
	case "len":
		return "must be exactly " + fe.Param() + lengthUnit(fe)
	case "gt":
		return "must be greater than " + fe.Param()
	case "gte":
		return "must be " + fe.Param() + " or more"
	case "lt":
		return "must be less than " + fe.Param()
	case "lte":
		return "must be " + fe.Param() + " or less"
	case "oneof":
		return "must be one of: " + strings.Join(strings.Fields(fe.Param()), ", ")
	case "eqfield":
		return "must match " + fe.Param()
	case "alphanum":
		return "must be letters and digits only"
	case "numeric":
		return "must be a number"
	case "e164":
		return "must be a phone number in E.164 format"
	default:
		if fe.Param() != "" {
			return "must satisfy " + fe.Tag() + "=" + fe.Param()
		}
		return "must satisfy " + fe.Tag()
	}
}

// lengthUnit distinguishes "at least 3 characters" from "at least 3", because
// min on a string means length and on a number means value.
func lengthUnit(fe pv.FieldError) string {
	switch fe.Kind() {
	case reflect.String:
		return " characters"
	case reflect.Slice, reflect.Array, reflect.Map:
		return " items"
	default:
		return ""
	}
}

// fieldPath is the dotted name the CLIENT knows the field by — json names all
// the way down, with the request type's own name stripped.
func fieldPath(fe pv.FieldError) string {
	ns := fe.Namespace()
	if _, rest, ok := strings.Cut(ns, "."); ok {
		return rest
	}
	return fe.Field()
}

// jsonName is registered with the library so every reported field carries the
// name the API uses, not the Go field name.
func jsonName(f reflect.StructField) string { return jsonNameOf(f) }

func jsonNameOf(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return f.Name
	}
	return name
}

func asInvalid(err error, target **pv.InvalidValidationError) bool {
	return stderrors.As(err, target)
}

// probeTag reports whether v recognises a constraint, PROBING the library
// rather than consulting a list — the library publishes no list, and a
// hardcoded one would rot the first time go-playground added a tag.
//
// The probe is the library's own behaviour: an unknown tag makes it PANIC, so
// running one throwaway validation inside a recover answers the question. Two
// details are load-bearing and both were bugs first:
//
//   - it probes the WHOLE token, "min=2" and not "min", because a constraint
//     that requires a parameter panics without one and would be misreported
//     as unknown;
//   - it probes the CALLER'S validator, not a package-level one, or every
//     constraint added with Register would be misreported as unknown.
//
// Results are cached per validator: the set of tags in an application is
// small and fixed at boot, and this never runs on the request path.
func (val *validator) probeTag(token string) (ok bool) {
	if cached, hit := val.tags.Load(token); hit {
		return cached.(bool)
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
		val.tags.Store(token, ok)
	}()
	// Var reports a validation FAILURE as an error and an UNKNOWN TAG as a
	// panic, so reaching the return at all is the answer.
	_ = val.v.Var("x", token)
	return true
}
