package openapi

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

// Emit builds the document from a frozen route table.
//
// It is a PURE FUNCTION of the table and the options, which is what makes the
// golden-file tests possible: a document is compared byte for byte without
// booting anything. Module calls it once at OnStart.
func Emit(t *transport.Table, opts ...Option) (Document, error) {
	cfg := defaults()
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	e := &emitter{components: map[string]Schema{}, componentTypes: map[string]reflect.Type{}, cfg: cfg}

	// THE CONSTRAINT TRAP, and it is the sharpest thing in this package.
	//
	// This emitter derives constraints from `validate:` tags. Under
	// validate.None() every tag is ACCEPTED AND NOTHING IS ENFORCED — that is
	// what None means — so a document publishing `required`, `minLength` and
	// `format` would assert guarantees the service does not make. A client
	// generated from it rejects requests the server would have accepted, and
	// the framework generated the lie.
	//
	// So under None the constraints are dropped and the shape is kept: types,
	// paths and parameters are still true. One refusal says why, in the
	// document and in the log.
	// Compared by TYPE against a fresh validate.None(): `none` is unexported,
	// so there is no value to compare against and no marker interface to
	// assert — and adding either to core for one consumer would be worse than
	// this line.
	v := t.Validator()
	e.unenforced = v == nil || reflect.TypeOf(v) == reflect.TypeOf(validate.None())
	if e.unenforced {
		e.refuse("", "", "the application's validator enforces nothing (validate.None), so `validate:` tags "+
			"are decoration — constraints are omitted rather than published as promises the service does not keep")
	}
	doc := Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:       cfg.title,
			Version:     cfg.version,
			Description: cfg.description,
		},
		Servers: cfg.servers,
		Paths:   map[string]PathItem{},
	}

	for _, route := range t.HTTP() {
		if e.owns(route.Pattern) {
			continue
		}
		e.addTyped(&doc, route)
	}

	// Raw routes are EMITTED, path and method only, each with a refusal.
	//
	// A document smaller than the API is the one error a generated client acts
	// on: omitting POST /uploads does not leave a client without a method for
	// it, it tells the client the endpoint does not exist. An entry with no
	// schema is honest about what the framework knows.
	for _, raw := range t.Raw() {
		if raw.Protocol != transport.ProtocolHTTP || e.owns(raw.Pattern) {
			continue
		}
		e.addRaw(&doc, raw)
	}

	// The components were keyed on full import paths while the walk ran,
	// because only now is the whole set known — and "the shortest suffix that
	// is unique" is not answerable until it is. Resolve the display keys, then
	// rewrite every $ref that points at one.
	if len(e.components) > 0 {
		doc.Components = Components{Schemas: e.resolveComponentNames(&doc)}
	}
	sort.Slice(e.refusals, func(i, j int) bool { return e.refusals[i].Route < e.refusals[j].Route })
	doc.refusals = e.refusals

	if cfg.strict && len(e.refusals) > 0 {
		return doc, errStrict(e.refusals)
	}
	return doc, nil
}

type emitter struct {
	// components is keyed by typeID (a full import path) during the walk, and
	// re-keyed to short display names by resolveComponentNames at the end.
	components map[string]Schema
	// componentTypes is the reflect.Type behind each reflect-derived key.
	// Synthetic components — warren.Error — are absent, which is how
	// resolveComponentNames knows to leave them alone.
	componentTypes map[string]reflect.Type
	refusals       []Refusal
	cfg            config

	// unenforced records that the application's validator checks nothing, so
	// the tags this package reads are decoration. Constraints are omitted
	// rather than published as promises.
	unenforced bool
}

// owns reports a route this package registered itself. Without it every
// document describes its own delivery mechanism, which is both noise and a
// lie about the API's surface.
func (e *emitter) owns(pattern string) bool {
	p := pattern
	if _, after, found := strings.Cut(p, " "); found {
		p = after // a raw pattern carries its method
	}
	return p == e.cfg.specPath || (e.cfg.docsPath != "" && p == e.cfg.docsPath)
}

// resolveComponentNames re-keys the components to their short names and
// rewrites every reference to them, in the components and in the operations.
func (e *emitter) resolveComponentNames(doc *Document) map[string]Schema {
	var synthetic []string
	for id := range e.components {
		if _, derived := e.componentTypes[id]; !derived {
			synthetic = append(synthetic, id)
		}
	}
	names := shortComponentNames(e.componentTypes, synthetic)
	for _, id := range synthetic {
		names[id] = id // warren.Error is a literal, not a type
	}

	out := make(map[string]Schema, len(e.components))
	for id, schema := range e.components {
		out[names[id]] = rewriteRefs(schema, names)
	}
	for _, item := range doc.Paths {
		for verb, op := range item {
			for i := range op.Parameters {
				op.Parameters[i].Schema = rewriteRefs(op.Parameters[i].Schema, names)
			}
			if op.RequestBody != nil {
				rewriteContent(op.RequestBody.Content, names)
			}
			for status, resp := range op.Responses {
				rewriteContent(resp.Content, names)
				op.Responses[status] = resp
			}
			item[verb] = op
		}
	}
	return out
}

func rewriteContent(content map[string]MediaType, names map[string]string) {
	for ct, mt := range content {
		mt.Schema = rewriteRefs(mt.Schema, names)
		content[ct] = mt
	}
}

// rewriteRefs replaces canonical component ids with their display names,
// everywhere a schema can nest one.
func rewriteRefs(s Schema, names map[string]string) Schema {
	if id, ok := strings.CutPrefix(s.Ref, componentRef); ok {
		if name, known := names[id]; known {
			s.Ref = componentRef + name
		}
	}
	if s.Items != nil {
		item := rewriteRefs(*s.Items, names)
		s.Items = &item
	}
	if len(s.Properties) > 0 {
		props := make(map[string]Schema, len(s.Properties))
		for k, v := range s.Properties {
			props[k] = rewriteRefs(v, names)
		}
		s.Properties = props
	}
	return s
}

func (e *emitter) refuse(route, typ, reason string) {
	e.refusals = append(e.refusals, Refusal{Route: route, Type: typ, Reason: reason})
}

func (e *emitter) addTyped(doc *Document, r transport.HTTPRoute) {
	label := r.Verb + " " + r.Pattern
	before := len(e.refusals)

	op := Operation{
		OperationID: operationID(r.Name),
		Responses:   map[string]Response{},
	}

	// Path and query parameters come from the REQUEST type's tags — the same
	// tags transport binds from, so the document cannot promise a parameter
	// the adapter does not bind.
	op.Parameters = e.parametersOf(r.Request, label)

	// A body only for verbs that carry one. A GET with a body schema is the
	// document telling a client to do something no server will read.
	if hasBody(r.Verb) && r.Request != nil && hasBodyFields(r.Request) {
		s := e.schemaFor(r.Request, label)
		op.RequestBody = &RequestBody{
			Required: true,
			Content:  map[string]MediaType{"application/json": {Schema: s}},
		}
	}

	status := strconv.Itoa(r.Success)
	resp := Response{Description: describeStatus(r.Success)}
	if r.Success != 204 && r.Response != nil {
		resp.Content = map[string]MediaType{
			"application/json": {Schema: e.schemaFor(r.Response, label)},
		}
	}
	op.Responses[status] = resp

	// The error envelope, once per operation. Every Warren route can answer
	// it, and a client that cannot parse it has to guess.
	op.Responses["default"] = Response{
		Description: "the framework's error envelope",
		Content:     map[string]MediaType{"application/json": {Schema: e.errorSchema()}},
	}

	if len(r.Guards) > 0 {
		// The document says a route is guarded. It deliberately does NOT say
		// HOW: the policy is application code and describing it would publish
		// the authorization model to callers who just failed it.
		op.Security = []map[string][]any{{"warrenGuard": {}}}
	}

	for _, ref := range e.refusals[before:] {
		op.Undescribed = append(op.Undescribed, ref.Reason)
	}

	path := openAPIPath(r.Pattern)
	item, ok := doc.Paths[path]
	if !ok {
		item = PathItem{}
		doc.Paths[path] = item
	}
	item[strings.ToLower(r.Verb)] = op
}

func (e *emitter) addRaw(doc *Document, r transport.RawRoute) {
	verb, pattern := "get", r.Pattern
	if before, after, found := strings.Cut(r.Pattern, " "); found {
		verb, pattern = strings.ToLower(before), after
	}
	const reason = "a raw route carries no request or response type — its handler owns the whole exchange, " +
		"so no schema can be derived. The route is listed so a client knows it exists."
	e.refuse(strings.ToUpper(verb)+" "+pattern, "", reason)

	path := openAPIPath(pattern)
	item, ok := doc.Paths[path]
	if !ok {
		item = PathItem{}
		doc.Paths[path] = item
	}
	item[verb] = Operation{
		OperationID: operationID(r.Name),
		Responses: map[string]Response{
			"default": {Description: "not described — see x-warren-undescribed"},
		},
		Undescribed: []string{reason},
	}
}

// parametersOf reads param: and query: tags off the request type.
func (e *emitter) parametersOf(t reflect.Type, route string) []Parameter {
	if t == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []Parameter
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		// A field the author declared unbindable — json:"-", or param:/query:
		// spelled "-" — is bound by nothing, so it is a parameter of nothing.
		// Without this the document would publish a parameter literally named
		// "-", which is the invalid-document problem the wildcard checks
		// exist to prevent, arriving through the opt-out that fixes another
		// one. Kept in step with transport's optedOut.
		if f.Tag.Get("json") == "-" || f.Tag.Get("param") == "-" || f.Tag.Get("query") == "-" {
			continue
		}
		for _, in := range []struct{ tag, where string }{{"param", "path"}, {"query", "query"}} {
			name := f.Tag.Get(in.tag)
			if name == "" || name == "-" {
				continue
			}
			s := e.schemaFor(f.Type, route)
			required := e.applyValidate(&s, f.Tag.Get("validate"), route, name)
			if in.where == "path" {
				// A path parameter is always required — the route does not
				// match without it. OpenAPI requires this to be true.
				required = true
			}
			out = append(out, Parameter{Name: name, In: in.where, Required: required, Schema: s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].In != out[j].In {
			return out[i].In < out[j].In
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// errorSchema is the envelope every Warren route can answer with.
func (e *emitter) errorSchema() Schema {
	// A literal key, not a typeID: this schema is built here rather than
	// reflected from a Go type, so resolveComponentNames leaves it untouched
	// and reserves the name against a real type claiming it.
	const name = "warren.Error"
	if _, done := e.components[name]; !done {
		e.components[name] = Schema{
			Type: "object",
			Properties: map[string]Schema{
				"error": {
					Type: "object",
					Properties: map[string]Schema{
						"code":           {Type: "string", Description: "the closed error vocabulary; a client switches on THIS, not on the status"},
						"message":        {Type: "string"},
						"details":        {Type: "object"},
						"correlation_id": {Type: "string"},
					},
					Required: []string{"code", "message"},
				},
			},
			Required: []string{"error"},
		}
	}
	return Schema{Ref: componentRef + name}
}

// hasBody reports whether a verb carries a request body a server will read.
func hasBody(verb string) bool {
	switch strings.ToUpper(verb) {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

// hasBodyFields reports whether the request type has any field that is NOT
// bound from the URL. A request whose every field is a param: or query: has no
// body, and saying otherwise tells a client to send an empty object.
func hasBodyFields(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("param") == "" && f.Tag.Get("query") == "" {
			return true
		}
	}
	return false
}

// openAPIPath converts net/http wildcards to OpenAPI's. Both use {name}; a
// trailing "..." is net/http's own syntax and is not part of the name.
func openAPIPath(p string) string {
	return strings.ReplaceAll(p, "...}", "}")
}

// operationID derives a stable, identifier-safe id from the route's
// "<module>.<handler>" name.
//
// It SANITISES rather than trusting the name. A handler's derived name is
// usually "user.registerUser", but a generic or anonymous handler produces
// something like "user.handler[pkg/path.Req,pkg/path.Res]" — brackets, commas
// and slashes and all. An operationId is what a client generator turns into a
// METHOD NAME, so anything that is not a letter, digit or underscore becomes
// one, and runs are collapsed.
func operationID(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

func describeStatus(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	default:
		return strconv.Itoa(code)
	}
}
