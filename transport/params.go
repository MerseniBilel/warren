package transport

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/MerseniBilel/warren/errors"
)

// setter binds one path or query parameter into a field. Setters are
// computed once, at registration, from `param:` and `query:` tags — the
// request path only walks the list.
type setter struct {
	name  string
	query bool // false = path parameter
	index []int
	set   func(reflect.Value, string) error
}

// paramSetters plans the parameter binding for T, or reports the fields it
// cannot bind. An unsupported kind is a registration error, not a silent
// skip: a path parameter that never arrives is a bug that would surface as a
// zero value on request 1.
func paramSetters(t reflect.Type) ([]setter, error) {
	if t == nil || t.Kind() != reflect.Struct {
		return nil, nil
	}
	var out []setter
	var errs []error
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, query := sf.Tag.Get("param"), false
		if name == "" {
			if q := sf.Tag.Get("query"); q != "" {
				name, query = q, true
			}
		}
		if name == "" {
			// A tag nested inside a struct field would never be bound, and
			// silently ignoring it is the failure this package refuses
			// everywhere else: it would surface as a zero value on request 1.
			if sf.Type.Kind() == reflect.Struct && hasParamTag(sf.Type) {
				errs = append(errs, diagnostic(fmt.Sprintf(
					"✗ nested parameter tag\n\n    field %s (%s) contains param:/query: tags, but parameters bind to\n    top-level fields only.\n\n  Move the tagged field up to the request struct, or drop the tag.",
					sf.Name, sf.Type)))
			}
			continue
		}
		set, err := setterFor(sf.Type)
		if err != nil {
			errs = append(errs, diagnostic(fmt.Sprintf(
				"✗ unsupported parameter type\n\n    field %s (%s) is tagged %q but %s cannot be bound from a\n    string parameter.\n\n  Use a string, bool, an integer or float kind, or a type implementing\n  encoding.TextUnmarshaler.",
				sf.Name, sf.Type, name, sf.Type)))
			continue
		}
		out = append(out, setter{name: name, query: query, index: []int{i}, set: set})
	}
	if len(errs) > 0 {
		return nil, errRegistration(errs)
	}
	return out, nil
}

// hasParamTag reports whether t or anything beneath it carries a param: or
// query: tag.
func hasParamTag(t reflect.Type) bool {
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Tag.Get("param") != "" || sf.Tag.Get("query") != "" {
			return true
		}
		if sf.Type.Kind() == reflect.Struct && sf.Type != t && hasParamTag(sf.Type) {
			return true
		}
	}
	return false
}

func setterFor(t reflect.Type) (func(reflect.Value, string) error, error) {
	if reflect.PointerTo(t).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
		return func(v reflect.Value, s string) error {
			return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s))
		}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return func(v reflect.Value, s string) error { v.SetString(s); return nil }, nil
	case reflect.Bool:
		return func(v reflect.Value, s string) error {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return err
			}
			v.SetBool(b)
			return nil
		}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(v reflect.Value, s string) error {
			n, err := strconv.ParseInt(s, 10, v.Type().Bits())
			if err != nil {
				return err
			}
			v.SetInt(n)
			return nil
		}, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return func(v reflect.Value, s string) error {
			n, err := strconv.ParseUint(s, 10, v.Type().Bits())
			if err != nil {
				return err
			}
			v.SetUint(n)
			return nil
		}, nil
	case reflect.Float32, reflect.Float64:
		return func(v reflect.Value, s string) error {
			f, err := strconv.ParseFloat(s, v.Type().Bits())
			if err != nil {
				return err
			}
			v.SetFloat(f)
			return nil
		}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %s", t.Kind())
	}
}

// bindParams applies the planned setters. A missing parameter leaves the
// field alone — the body may have set it, and validation decides whether it
// was required. A malformed one is INVALID, which is a 400 and never a
// retry.
func bindParams(req any, p Params, setters []setter) error {
	if p == nil {
		return nil
	}
	v := reflect.ValueOf(req).Elem()
	for _, s := range setters {
		var raw string
		var ok bool
		if s.query {
			raw, ok = p.Query(s.name)
		} else {
			raw, ok = p.Path(s.name)
		}
		if !ok {
			continue
		}
		if err := s.set(v.FieldByIndex(s.index), raw); err != nil {
			return errors.Invalid(s.name, err)
		}
	}
	return nil
}

// checkWildcards refuses a `param:` tag the route pattern has no wildcard
// for. Both facts are known here, at registration, and the alternative is a
// field that binds "" on every request: the handler looks up the zero value
// and returns NOT_FOUND, so the service boots, serves, and is wrong.
//
// Renaming a path segment without renaming the tag is the ordinary way to
// reach it, and nothing in the response says the parameter never arrived.
//
// BOTH directions are checked, and the second sentence of this comment used
// to deny the second one: "a pattern with a wildcard nothing binds is
// legitimate — a route may ignore a segment it matches on." It is not
// legitimate, and field test #14 measured why. A {id} no field binds boots
// green and then fails every request — 400 for ever where the field carries
// validate:"required", and an empty identifier reaching the handler where it
// does not — while the published OpenAPI path is invalid either way, because
// OpenAPI 3.1 requires a path parameter for every template expression and
// openapi derives parameters from `param:` tags alone. A route that ignores a
// segment it matches on is therefore serving a path its own published
// contract does not mention.
//
// The remedy is one struct field, and it costs nothing at request time —
// bindParams walks a precomputed index either way — so the diagnostic names it
// rather than offering an opt-out.
//
// Only PATH parameters are checked. A query: tag has no wildcard by
// definition, and cannot satisfy one.
// It returns the failures INDIVIDUALLY rather than pre-joined. errRegistration
// indents what it wraps, and the Builder joins every registration failure with
// the same function at boot — so a pre-joined group arrived one indent deeper
// than its siblings and read as nested under one of them. Returning a slice
// makes each failure a sibling, which is what
// TestEveryJoinedFailureLeadsWithItsOwnHeadline asserts and what the forward
// check had quietly been getting wrong too, for want of a second failure to
// stand beside.
func checkWildcards(pattern, reqType string, setters []setter) []error {
	var missing []setter
	for _, s := range setters {
		if s.query {
			continue
		}
		if !hasWildcard(pattern, s.name) {
			missing = append(missing, s)
		}
	}
	// The reverse: a wildcard the request type binds nothing to.
	var unbound []string
	for _, w := range wildcards(pattern) {
		bound := false
		for _, s := range setters {
			if !s.query && s.name == w {
				bound = true
				break
			}
		}
		if !bound {
			unbound = append(unbound, w)
		}
	}

	if len(missing) == 0 && len(unbound) == 0 {
		return nil
	}

	var errs []error //nolint:prealloc // two loops append to it under different conditions
	present := wildcards(pattern)
	for _, s := range missing {
		hint := "  Add it to the pattern, or drop the tag."
		switch {
		case len(present) == 1:
			hint = fmt.Sprintf("  The pattern declares {%s}. Did you mean `param:%q`?", present[0], present[0])
		case len(present) > 1:
			hint = fmt.Sprintf("  The pattern declares {%s}. Use one of those, or add {%s}.",
				strings.Join(present, "}, {"), s.name)
		}
		errs = append(errs, diagnostic(fmt.Sprintf(
			"✗ no matching path wildcard\n\n    field tagged `param:%q` has no {%s} in the route pattern\n    %s\n\n"+
				"    It would bind \"\" on every request — the handler would look up the\n"+
				"    zero value and report NOT_FOUND, with nothing saying why.\n\n%s",
			s.name, s.name, pattern, hint)))
	}

	for _, w := range unbound {
		// The near miss is the likely cause, and the forward half already
		// prints this class of hint: a field tagged for a name the pattern
		// does not declare, beside a wildcard nothing declares a field for.
		hint := fmt.Sprintf("  Add the field, even if the handler ignores it:\n\n      ID string `param:%q`\n\n  or drop {%s} from the pattern.", w, w)
		var tagged []string
		for _, s := range setters {
			if !s.query {
				tagged = append(tagged, s.name)
			}
		}
		if len(tagged) > 0 {
			hint = fmt.Sprintf("  %s carries `param:` for %s and nothing for {%s}.\n%s",
				reqType, "{"+strings.Join(tagged, "}, {")+"}", w, hint)
		}
		errs = append(errs, diagnostic(fmt.Sprintf(
			"✗ path wildcard nothing binds\n\n    pattern %s declares {%s}, and no field of %s carries `param:%q`\n\n"+
				"    Every request binds \"\" for it: with validate:\"required\" the route\n"+
				"    400s for ever, without it the handler receives an empty identifier\n"+
				"    and reports NOT_FOUND. The published OpenAPI path is invalid either\n"+
				"    way — a templated segment with no parameter.\n\n%s",
			pattern, w, reqType, w, hint)))
	}
	return errs
}

// hasWildcard reports whether pattern declares {name} or {name...}.
func hasWildcard(pattern, name string) bool {
	for _, w := range wildcards(pattern) {
		if w == name {
			return true
		}
	}
	return false
}

// wildcards lists the names a pattern declares, in order, with the trailing
// "..." of a multi-segment wildcard stripped — {rest...} binds "rest", so
// that is the name a tag must carry.
func wildcards(pattern string) []string {
	var out []string
	for rest := pattern; ; {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out
		}
		rest = rest[open+1:]
		close := strings.IndexByte(rest, '}')
		if close < 0 {
			return out // an unbalanced brace is the router's error to report
		}
		out = append(out, strings.TrimSuffix(rest[:close], "..."))
		rest = rest[close+1:]
	}
}
