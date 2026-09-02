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
//
// It also returns the exported fields carrying NEITHER tag. On a route with a
// body those are ordinary JSON fields and none of this walk's business; on a
// bodyless one they can never be populated, which is what checkUnbindable
// refuses. Collecting them here rather than walking the type a second time is
// the point: this loop already visits every field and already knows which of
// them a parameter can reach.
func paramSetters(t reflect.Type) ([]setter, []reflect.StructField, []error) {
	if t == nil || t.Kind() != reflect.Struct {
		return nil, nil, nil
	}
	var out []setter
	var untagged []reflect.StructField
	var errs []error
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		if optedOut(sf) {
			// Declared unbindable, deliberately: nothing on the wire fills
			// this field, and nothing should try. It is neither a setter nor
			// a candidate for checkUnbindable.
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
			untagged = append(untagged, sf)
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
		return nil, nil, errs
	}
	return out, untagged, nil
}

// bodyless reports whether an HTTP verb carries no request body.
//
// Written as the COMPLEMENT of the verbs that do, deliberately: openapi's
// hasBody (openapi/emit.go) is the same predicate spelled the same way —
// "POST, PUT, PATCH, and nothing else" — and its comment already states this
// package's premise, that "a GET with a body schema is the document telling a
// client to do something no server will read". Two lists of verbs in two
// modules is a drift waiting to happen; two complements of one list is not.
// A verb added to Registrar tomorrow is bodyless in both places until someone
// deliberately says otherwise, which is the safe default for a refusal.
//
// Raw routes name their own verb and never reach here.
func bodyless(verb string) bool {
	switch verb {
	case "POST", "PUT", "PATCH":
		return false
	}
	return true
}

// checkUnbindable refuses an exported field that nothing can ever populate: no
// `param:`, no `query:`, on a route with no request body. Field test #15
// measured all three spellings of it, and the third is the one the framework
// was already telling people about —
//
//	Author string `quesry:"author"`   // a one-character typo in the KEY
//	Author string                     // no tag at all
//	Author string `json:"author"`     // a body tag on a bodyless verb
//
// each of which boots clean, binds "" for ever, and answers 200 with the
// filter silently dropped. `warren g` already refuses to write the third:
// "A Get carries no body, so a `json:` field here would be unsatisfiable."
//
// This is the same class as checkWildcards, and it earns a refusal for the
// same reason: not "this looks wrong" but "no request populates this". What is
// deliberately NOT refused is the same field on a route WITH a body, where an
// untagged exported field is bound by encoding/json case-insensitively and a
// `db:`/`yaml:`/`bson:` tag beside no binding tag is ordinary and legitimate.
// A check that fired there would be the guessing linter this package has
// declined three times, and it would be switched off within a month.
//
// One honest limit, because the justification overstates without it: the HTTP
// adapter reads a body on every verb, so a client that sends one on a GET
// would in fact populate a `json:` field. RFC 9110 gives content on GET no
// defined semantics, any intermediary may drop it, and Warren's own generator
// already calls such a field unsatisfiable — so this refusal fixes the
// contract rather than merely describing it. It is a boot-behaviour change,
// and that is what it changes.
//
// The REFUSAL is provable; only the HINT guesses, and only within a fixed
// edit distance. That ordering is the whole distinction — a near miss on the
// tag KEY is the likeliest cause and is worth naming, but nothing is refused
// because of it.
func checkUnbindable(verb, pattern, reqType string, untagged []reflect.StructField) []error {
	if !bodyless(verb) {
		return nil
	}
	errs := make([]error, 0, len(untagged))
	for _, sf := range untagged {
		want := suggestedName(sf.Name)
		hint := "  Tag it `query:\"" + want + "\"`, or `param:` with a matching {wildcard}\n" +
			"  in the pattern, or unexport the field.\n\n" +
			"  If nothing on the wire is meant to fill it — a middleware sets it, say —\n" +
			"  declare that: `json:\"-\"` (or `query:\"-\"`, or `param:\"-\"`).\n" +
			"  That is also what stops a client setting it on a route that HAS a body."
		switch {
		case sf.Tag.Get("json") != "":
			// The commonest way to reach this check: a DTO copied from a POST
			// to a GET. Naming the json: tag is the difference between "why is
			// this refused" and "of course — GET has no body".
			hint = fmt.Sprintf(
				"  This field is tagged `json:%q`, and a %s carries no body for it to\n"+
					"  come from. A DTO copied from a POST route is the usual cause.\n"+
					"  `query:%q` is probably what you meant.\n\n%s",
				sf.Tag.Get("json"), verb, want, hint)
		default:
			if key, ok := nearMissTagKey(sf.Tag); ok {
				hint = fmt.Sprintf("  The struct tag has a key `%s`, which binds nothing. Did you mean `%s`?\n\n%s",
					key.got, key.want, hint)
			}
		}
		errs = append(errs, diagnostic(fmt.Sprintf(
			"✗ field can never be bound\n\n    field %s (%s) of %s carries no `param:` or `query:` tag,\n"+
				"    and %s %s has no request body — so nothing populates it.\n\n"+
				"    Every request leaves it at the zero value, which is not an error at\n"+
				"    any layer: the route answers 200 with the field silently ignored.\n\n%s",
			sf.Name, sf.Type, reqType, verb, pattern, hint)))
	}
	return errs
}

// suggestedName lowercases the first rune of a Go field name, which is the
// query parameter a caller would most likely be sending. It is the inverse of
// fieldNameFor and is only ever used inside a hint.
func suggestedName(field string) string {
	if field == "" {
		return "name"
	}
	r := []rune(field)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

type tagKeyMiss struct{ got, want string }

// nearMissTagKey reports a tag key that is a small edit away from "param" or
// "query" — `quesry:"author"` being the case field test #15 measured.
//
// The distance is capped at 2 and the key must be at least four runes, so
// "db", "json" and "yaml" cannot be dragged into a suggestion. Nothing is
// refused on this evidence; it only decorates a failure already proven.
func nearMissTagKey(tag reflect.StructTag) (tagKeyMiss, bool) {
	best := tagKeyMiss{}
	bestD := 3
	for _, got := range tagKeys(tag) {
		if len(got) < 4 {
			continue
		}
		for _, want := range []string{"param", "query"} {
			if got == want {
				continue
			}
			if d := editDistance(got, want); d < bestD {
				best, bestD = tagKeyMiss{got: got, want: want}, d
			}
		}
	}
	return best, bestD < 3
}

// tagKeys lists the keys of a struct tag in the conventional format reflect
// documents: space-separated `key:"value"` pairs. reflect parses this to
// answer Get and Lookup but exposes no way to enumerate it, and enumerating is
// exactly what a "did you mean" needs.
func tagKeys(tag reflect.StructTag) []string {
	var keys []string
	for t := string(tag); t != ""; {
		i := 0
		for i < len(t) && t[i] == ' ' {
			i++
		}
		t = t[i:]
		i = 0
		for i < len(t) && t[i] > ' ' && t[i] != ':' && t[i] != '"' && t[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(t) || t[i] != ':' || t[i+1] != '"' {
			break
		}
		key := t[:i]
		t = t[i+1:]
		j := 1
		for j < len(t) && t[j] != '"' {
			if t[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(t) {
			break
		}
		keys = append(keys, key)
		t = t[j+1:]
	}
	return keys
}

// editDistance is Levenshtein over runes, with one row of state. Tag keys are
// a handful of ASCII characters and this runs once per unbindable field at
// boot, so the naive form is the right one.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// optedOut reports a field the author has declared unbindable on purpose.
//
// Field test #16 found the case the bodyless-route check has no answer for.
// app.Middleware[Req, Res] is func(Handler) Handler, so a middleware may fill
// the request before the handler sees it — Warren's own type invites exactly
// that — and a cross-cutting tenancy middleware setting a Tenant field on the
// DTO is the idiomatic shape. On a GET that field carries no param: and no
// query:, so the check refused it, and all three remedies it offered were
// wrong:
//
//	query:"tenant"    hands the tenant to the caller in the URL — a
//	                  cross-tenant read, recommended by the framework
//	param:"tenant"    the same, plus a wildcard that does not belong
//	unexport it       impossible; the middleware is in another package,
//	                  which is the whole point of a cross-cutting middleware
//
// A refusal with no correct compliance path is a refusal that has to have an
// opt-out. This is it.
//
// THE SPELLING IS json:"-", with query:"-" and param:"-" accepted as
// synonyms for a reader who reaches for the tag family already in play. A
// separate bind:"-" vocabulary was rejected deliberately: json:"-" already
// means "not from the wire" in Go, encoding/json ALREADY honours it on body
// routes, so one spelling covers both route shapes and adds no new word to
// learn. On a body route it is also the live remedy for the mirror-image
// problem — an exported field with no tag is populated from the body by Go
// field name, so {"Tenant":"attacker"} lands unless the field says json:"-".
func optedOut(sf reflect.StructField) bool {
	return sf.Tag.Get("json") == "-" ||
		sf.Tag.Get("param") == "-" ||
		sf.Tag.Get("query") == "-"
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
