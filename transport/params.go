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
// Only PATH parameters are checked. A query: tag has no wildcard by
// definition, and a pattern with a wildcard nothing binds is legitimate —
// a route may ignore a segment it matches on.
func checkWildcards(pattern string, setters []setter) error {
	var missing []setter
	for _, s := range setters {
		if s.query {
			continue
		}
		if !hasWildcard(pattern, s.name) {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var errs []error
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
	return errRegistration(errs)
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
