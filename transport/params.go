package transport

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"

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
//
// Like checkWildcards it returns its failures INDIVIDUALLY, for the same
// reason: the Builder joins every registration failure with errRegistration,
// which indents what it wraps, so a group pre-joined here arrived one indent
// deeper than its siblings and read as nested under one of them.
func paramSetters(t reflect.Type) ([]setter, []error) {
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
		return nil, errs
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

// checkWildcards refuses a route whose `param:` tags and whose pattern's
// wildcards do not describe the same path parameters. Both directions fail:
// a tag with no wildcard, and a wildcard with no tag.
//
// Either way that parameter binds "" on every request — a 400 for ever where
// the field carries validate:"required", an empty identifier reaching the
// handler where it does not — and the published OpenAPI path is invalid,
// because openapi derives parameters from `param:` tags alone. Only PATH
// parameters are checked: a query: tag has no wildcard by definition and
// cannot satisfy one, and Raw routes never reach here.
//
// Failures are returned individually rather than pre-joined; see paramSetters.
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
		hint := fmt.Sprintf("  Add the field, even if the handler ignores it:\n\n      %s string `param:%q`\n\n  or drop {%s} from the pattern.",
			fieldNameFor(w), w, w)
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

// fieldNameFor spells a wildcard as the exported Go field the diagnostic tells
// the user to write. A diagnostic whose whole value is that it can be pasted
// must print code that compiles and reads like Go: the hint used to hardcode
// "ID string `param:\"rest\"`" whatever the wildcard was called.
//
// The rule mirrors `warren g`'s fieldName (cli/internal/generate/generate.go),
// so the field this names is the field the generator would have written. They
// are in different modules and cannot share a function; they must not drift.
//
// net/http requires a wildcard name to be a valid Go identifier, so there is
// nothing else to sanitise.
func fieldNameFor(param string) string {
	if param == "" {
		return "Field"
	}
	r := []rune(param)
	r[0] = unicode.ToUpper(r[0])
	name := string(r)
	switch {
	case name == "Id":
		return "ID"
	case strings.HasSuffix(name, "Id"):
		return strings.TrimSuffix(name, "Id") + "ID"
	}
	return name
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
