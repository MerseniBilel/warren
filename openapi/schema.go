package openapi

import (
	"encoding"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"reflect"
	"strconv"
	"strings"
)

// schemaFor derives a schema from a Go type, recording a refusal for anything
// it cannot describe rather than guessing.
//
// It reads the SAME tags the transport adapter binds and validates against, so
// the document cannot describe an API the service does not serve. That is the
// whole design: there is no second source of truth to drift from.
func (e *emitter) schemaFor(t reflect.Type, route string) Schema {
	if t == nil {
		return Schema{}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if s, ok := e.wireSchema(t, route); ok {
		return s
	}

	switch t.Kind() {
	case reflect.String:
		return Schema{Type: "string"}
	case reflect.Bool:
		return Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return Schema{Type: "number"}
	case reflect.Slice, reflect.Array:
		item := e.schemaFor(t.Elem(), route)
		return Schema{Type: "array", Items: &item}
	case reflect.Map:
		// An object with unconstrained keys. OpenAPI can say "object" and no
		// more, which is honest.
		return Schema{Type: "object"}
	case reflect.Struct:
		return e.structSchema(t, route)
	case reflect.Interface:
		// `any` is the one field kind that genuinely carries no information.
		// It is emitted as an unconstrained schema AND refused, because a
		// client author reading `{}` cannot tell "anything" from "we did not
		// look".
		e.refuse(route, t.String(), "a field of interface type carries no schema — a client cannot know what to send")
		return Schema{}
	default:
		e.refuse(route, t.String(), "no OpenAPI type corresponds to Go kind "+t.Kind().String())
		return Schema{}
	}
}

// The four interfaces encoding/json consults before its own type-dependent
// encoding, in the order go doc encoding/json.Marshal specifies them.
//
// The ORDER is load-bearing and it is the reverse of the obvious one. A type
// implementing MarshalText AND MarshalJSON is encoded by MarshalJSON —
// measured on go1.27.0, and specified: the Marshaler rules are listed above
// the TextMarshaler ones. Checking TextMarshaler first would describe such a
// type as a string when its wire form is whatever MarshalJSON returns, which
// is precisely the confident-wrong-answer this ladder exists to stop.
var (
	jsonMarshalerToType = reflect.TypeFor[jsonv2.MarshalerTo]()
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	textAppenderType    = reflect.TypeFor[encoding.TextAppender]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
)

// knownWireSchemas is the CLOSED table of types whose JSON form this package
// asserts from measurement rather than derives.
//
// A row is an amendment to this table, in a diff, with the marshalled output
// quoted. Both rows below were measured on go1.27.0:
//
//	{"t":"0001-01-01T00:00:00Z","u":"00000000-0000-0000-0000-000000000000"}
//
// Without the table, both would still be described correctly as strings by
// the TextMarshaler rung — the table exists to add the `format`, which is the
// part a client generator turns into a real date or UUID type.
var knownWireSchemas = map[string]Schema{
	"time.Time": {Type: "string", Format: "date-time"},
	"uuid.UUID": {Type: "string", Format: "uuid"},
}

// wireSchema answers for a type that encodes itself, rather than by the shape
// of its fields.
//
// Before this existed, time.Time reached structSchema, which walked its three
// unexported fields, found none, and published `{"type":"object"}` for a value
// that is a string on the wire — with no refusal, because nothing here refused.
// Go 1.27's stdlib uuid.UUID would have been `{"type":"array"}` for the same
// reason: it is [16]byte with a MarshalText.
//
// The pointer type is checked as well as the value type. json.Marshal calls a
// pointer-receiver marshaller only where the value is addressable, which this
// package cannot know from a reflect.Type alone — so it errs toward refusing,
// because a refusal says "we did not look" and a wrong schema says something
// false with confidence.
func (e *emitter) wireSchema(t reflect.Type, route string) (Schema, bool) {
	if s, ok := knownWireSchemas[typeID(t)]; ok {
		return s, true
	}
	if implementsAny(t, jsonMarshalerToType, jsonMarshalerType) {
		e.refuse(route, t.String(), "the type chooses its own JSON with MarshalJSON, so its wire form is bytes "+
			"returned by a method — reflection can read the method's existence and never its output")
		return Schema{}, true
	}
	if implementsAny(t, textAppenderType, textMarshalerType) {
		// Not a guess: encoding/json is specified to encode a TextAppender's
		// or TextMarshaler's output AS A JSON STRING, whatever the Go kind
		// underneath. A format is added only from the table above.
		return Schema{Type: "string"}, true
	}
	return Schema{}, false
}

func implementsAny(t reflect.Type, ifaces ...reflect.Type) bool {
	ptr := reflect.PointerTo(t)
	for _, i := range ifaces {
		if t.Implements(i) || ptr.Implements(i) {
			return true
		}
	}
	return false
}

// structSchema walks the exported fields, honouring json:, and applies the
// validate: constraints.
func (e *emitter) structSchema(t reflect.Type, route string) Schema {
	if t.Name() != "" && t.PkgPath() != "" {
		// Named struct: register it in components and reference it, so a type
		// used by three routes appears once.
		// Keyed on the CANONICAL id — the full import path — while the
		// document is being built. The short, readable key is computed once
		// at the end, when the whole set of types is known and "shortest
		// suffix that is unique" is answerable. Keying on the display name
		// here is what made two BookViews one schema.
		id := typeID(t)
		if _, done := e.components[id]; !done {
			e.componentTypes[id] = t
			// Placed BEFORE the walk so a self-referential type terminates.
			e.components[id] = Schema{Type: "object"}
			e.components[id] = e.inlineStruct(t, route)
		}
		return Schema{Ref: componentRef + id}
	}
	return e.inlineStruct(t, route)
}

func (e *emitter) inlineStruct(t reflect.Type, route string) Schema {
	out := Schema{Type: "object", Properties: map[string]Schema{}}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			// Embedded: promote its properties, which is what encoding/json
			// does on the wire.
			inner := e.schemaFor(f.Type, route)
			for _, k := range sortedKeys(inner.Properties) {
				out.Properties[k] = inner.Properties[k]
			}
			out.Required = append(out.Required, inner.Required...)
			continue
		}
		name, omit := jsonName(f)
		if omit {
			continue
		}
		// A param: or query: field is NOT a body property — it is bound from
		// the URL, and putting it in the body schema would tell a client to
		// send it twice.
		if f.Tag.Get("param") != "" || f.Tag.Get("query") != "" {
			continue
		}
		s := e.schemaFor(f.Type, route)
		required := e.applyValidate(&s, f.Tag.Get("validate"), route, name)
		out.Properties[name] = s
		if required {
			out.Required = append(out.Required, name)
		}
	}
	sortStrings(out.Required)
	return out
}

// applyValidate maps the validate: vocabulary onto the schema and reports
// whether the field is required.
//
// Only the tags with an unambiguous JSON Schema meaning are mapped. A tag this
// does not know is IGNORED rather than guessed at — a wrong constraint in a
// published document is worse than a missing one, because a client generator
// enforces it.
//
// Ignoring is right; ignoring SILENTLY was not. doc.go promises that what this
// package cannot describe it refuses out loud, and an unmapped token is
// exactly that: the service enforces `isbn13`, a bad ISBN is a 400 naming the
// field, and the published schema said `{"type":"string"}` with nothing to
// say it was thinner than the server. Each unmapped token is now a Refusal.
//
// The table is deliberately NOT extended to cover them. go-playground's
// `isbn13` accepts hyphens and spaces, so any `pattern` invented here would be
// a constraint the generated client enforces and the server does not — the
// failure this comment's first paragraph exists to prevent.
func (e *emitter) applyValidate(s *Schema, tag, route, field string) (required bool) {
	if e.unenforced {
		// The shape stays true — types, names, paths. Only the PROMISES go.
		// No per-token refusals either: one refusal already says the whole
		// vocabulary is decoration here, and repeating it per field would
		// bury it.
		return false
	}
	required, unmapped := applyValidate(s, tag)
	for _, token := range unmapped {
		e.refuse(route, field, "the service enforces `"+token+"` and no JSON Schema keyword expresses it, "+
			"so the published schema is less strict than the server")
	}
	return required
}

func applyValidate(s *Schema, tag string) (required bool, unmapped []string) {
	for rule := range strings.SplitSeq(tag, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		key, value, _ := strings.Cut(rule, "=")
		switch key {
		case "required":
			required = true
		case "email":
			s.Format = "email"
		case "url", "uri":
			s.Format = "uri"
		case "uuid", "uuid4":
			s.Format = "uuid"
		case "min":
			applyBound(s, value, true)
		case "max":
			applyBound(s, value, false)
		case "len":
			if n, err := strconv.Atoi(value); err == nil && s.Type == "string" {
				s.MinLength, s.MaxLength = &n, &n
			}
		case "oneof":
			if s.Type == "string" {
				s.Enum = strings.Fields(value)
			}
		default:
			unmapped = append(unmapped, key)
		}
	}
	return required, unmapped
}

// applyBound puts min/max on the right keyword: length for strings and arrays,
// value for numbers. Putting minLength on an integer would be a constraint a
// client generator enforces and a server never checks.
func applyBound(s *Schema, value string, lower bool) {
	switch s.Type {
	case "string", "array":
		n, err := strconv.Atoi(value)
		if err != nil {
			return
		}
		if lower {
			s.MinLength = &n
		} else {
			s.MaxLength = &n
		}
	case "integer", "number":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return
		}
		if lower {
			s.Minimum = &f
		} else {
			s.Maximum = &f
		}
	}
}

// jsonName resolves the wire name of a field, honouring `json:"-"`.
func jsonName(f reflect.StructField) (name string, omit bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	n, _, _ := strings.Cut(tag, ",")
	if n == "" {
		n = f.Name
	}
	return n, false
}

// componentRef is the prefix every schema reference carries.
const componentRef = "#/components/schemas/"

// typeID is a named type's collision-free identity: its full import path and
// name. Components are keyed on it WHILE the document is built.
//
// The key users see is computed from the whole set afterwards, by
// shortComponentNames. Splitting the two is the fix for the defect that was
// here: componentName kept only the LAST path element, so
// catalog/application.BookView and lending/application.BookView both keyed on
// "application.BookView" — and since warren g module names every feature's
// use-case package `application`, every Warren service shares one namespace by
// construction. The second type was never walked; both routes referenced the
// first. Its doc comment claimed the property the code did not have.
func typeID(t reflect.Type) string {
	if t.PkgPath() == "" {
		return t.Name()
	}
	return t.PkgPath() + "." + t.Name()
}

// shortComponentNames gives each type the SHORTEST run of trailing import-path
// segments that tells it apart from every other type in this document.
//
//	one BookView   →  application.BookView
//	two BookViews  →  catalog.application.BookView
//	                  lending.application.BookView
//
// A total function of the types the emitter actually reached, so collision is
// impossible by construction rather than unlikely. Keying on the full import
// path would also be collision-proof and would name every generated client's
// type ExampleComLibraryInternalModulesCatalogApplicationBookView.
//
// The cost, stated: adding a colliding type in a second feature RENAMES the
// first feature's key. That is unavoidable under any shortest-unique scheme,
// and it is a rename visible in the document diff rather than a silent lie
// about which fields an endpoint returns.
//
// reserved holds keys already spoken for by synthetic components such as
// warren.Error, which have no reflect.Type and never move.
func shortComponentNames(types map[string]reflect.Type, reserved []string) map[string]string {
	out := make(map[string]string, len(types))
	taken := make(map[string]bool, len(reserved))
	for _, r := range reserved {
		taken[r] = true
	}
	remaining := sortedKeys(types)

	// Bounded by the longest import path, NOT by "a round that resolved
	// nothing". A round resolving nothing is the NORMAL first step of a
	// collision — two BookViews both answer "application.BookView" at depth 1
	// and neither can be taken — so stopping there sent every colliding type
	// to the fallback and published the full import path as its key.
	deepest := 1
	for _, t := range types {
		if n := len(strings.Split(t.PkgPath(), "/")); n > deepest {
			deepest = n
		}
	}

	for depth := 1; len(remaining) > 0 && depth <= deepest; depth++ {
		count := map[string]int{}
		cand := make(map[string]string, len(remaining))
		for _, id := range remaining {
			c := suffixName(types[id], depth)
			cand[id] = c
			count[c]++
		}
		var next []string
		for _, id := range remaining {
			// Unique among what is still unresolved, and not already spoken
			// for by a type that resolved at a shallower depth.
			if c := cand[id]; count[c] == 1 && !taken[c] {
				out[id], taken[c] = c, true
				continue
			}
			next = append(next, id)
		}
		remaining = next
	}

	// Nothing should reach here: at full depth every candidate is the whole
	// import path, which typeID makes unique per type. The fallback is the
	// canonical id, so a key is still emitted and still unique.
	for _, id := range remaining {
		out[id] = sanitizeKey(id)
	}
	return out
}

// suffixName renders a type as its last `depth` path segments plus its name.
func suffixName(t reflect.Type, depth int) string {
	pkg := t.PkgPath()
	if pkg == "" {
		return sanitizeKey(t.Name())
	}
	seg := strings.Split(pkg, "/")
	if depth < len(seg) {
		seg = seg[len(seg)-depth:]
	}
	return sanitizeKey(strings.Join(seg, ".") + "." + t.Name())
}

// sanitizeKey holds a component key inside the ^[a-zA-Z0-9._-]+$ that OpenAPI
// requires of a component name. A generic instantiation is the realistic
// source of anything else: "pkg.Page[pkg.Book]" carries brackets and a dot.
func sanitizeKey(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
