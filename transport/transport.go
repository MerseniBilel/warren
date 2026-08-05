// Package transport is the port through which a controller exposes a use
// case over HTTP, gRPC, and events. One Register call, three protocols: that
// is the framework's first claim made concrete.
//
// It is boot-time only. Registration happens at boot step 5 and produces a
// driver-neutral route table of pre-built closures; by the time a request
// arrives the container is never consulted and nothing is resolved.
//
// # Go 1.26 and the shape of registration
//
// warren.md §3.5 registers through generic METHODS on concrete registrars —
// a Go 1.27 feature. Until it ships, registration is generic FREE functions
// with the same names (Get, Post, Method, OnEvent) and the same argument
// order, so the migration is a mechanical rewrite of call sites inside
// Register bodies; Register's own signature never changes. Type arguments
// are usually inferred — transport.Post(r, "/users", c.register) compiles —
// and only need spelling out when the handler's own type does not determine
// Req and Res.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/validate"
)

// Registrar accumulates the routes one controller registers. It is sealed:
// this package holds the only implementation, so no adapter can reimplement
// registration and drift from it — every router decodes, validates, binds
// params, and defaults statuses identically.
type Registrar interface {
	record(e entry)
	moduleName() string
}

// Controller exposes handlers over transports. Register is called once, at
// boot step 5, on an already-instantiated controller — never per request.
type Controller interface {
	Register(r Registrar)
}

// Consumer is a Controller that registers event subscriptions. The two names
// mirror warren.Controllers and warren.Consumers, which differ in lifecycle
// placement — consumers start before servers and stop after them — not in
// what may be registered.
type Consumer = Controller

// Invoker is the route closure: bytes in, bytes out, middleware already
// composed. The DI container is never consulted inside it.
type Invoker func(ctx context.Context, raw []byte) ([]byte, error)

// Codec is the edge's serialisation seam. JSON is the default for HTTP and
// events; the gRPC adapter supplies its own proto codec, which is why a
// route hands out a Bind(Codec) rather than a finished closure.
type Codec interface {
	Name() string
	Decode(data []byte, v any) error
	Encode(v any) ([]byte, error)
}

// JSON returns the standard-library codec. It is the default for HTTP and
// the only codec on the event path.
//
// Unknown members are IGNORED, and that is a decision rather than an
// oversight — warren.md §3.5 records the argument. In short: this same Codec
// decodes events, where a decode failure is INVALID and §2.6 dead-letters
// INVALID without retrying, so a producer adding a field to a payload would
// take out 100% of a consumer's traffic; on the HTTP side leniency is what
// lets a client ship an additive change before the server, which is the
// property §1.3's drain ordering exists to protect; and the gRPC adapter is
// binary proto, whose wire format ignores unknown fields as a matter of
// specification, so a strict JSON default would give one handler two
// acceptance sets across the two transports §3.5 registers it on.
//
// StrictJSON is the opt-in for services whose HTTP clients are first-party.
func JSON() Codec { return jsonCodec{} }

type jsonCodec struct{}

func (jsonCodec) Name() string                    { return "application/json" }
func (jsonCodec) Decode(data []byte, v any) error { return json.Unmarshal(data, v) }
func (jsonCodec) Encode(v any) ([]byte, error)    { return json.Marshal(v) }

// StrictJSON returns a codec that rejects a body carrying a member the target
// type does not declare — so a misspelled field is a 400 naming it, not a 200
// with that field left at its zero value.
//
// It is opt-in, installed per HTTP server with http.Codec, and there is
// deliberately no way to put it on the event path. It costs 3 allocations per
// request more than JSON: json.Decoder has no Reset in encoding/json v1, so
// the reader and the decoder are both per-request. Measured on
// go1.26.3/darwin-arm64; transport/http's allocation budget is asserted
// against the DEFAULT codec, and this is why.
//
// Encoding is identical to JSON's. Strictness is a decode policy.
func StrictJSON() Codec { return strictJSONCodec{} }

type strictJSONCodec struct{}

func (strictJSONCodec) Name() string { return "application/json" }

func (strictJSONCodec) Encode(v any) ([]byte, error) { return json.Marshal(v) }

// Decode rejects unknown members AND trailing data.
//
// The second half is not optional, and it is the reason this is not a
// two-line function. json.Unmarshal refuses a body that carries anything
// after the JSON value; json.Decoder.Decode does NOT — it consumes the first
// value, returns nil, and discards the rest in silence. So the obvious
// implementation of "strict" is laxer than the lenient codec it tightens:
//
//	body                              JSON()   Decoder alone   this
//	{"email":"a"} {"email":"b"}       INVALID  accepted        INVALID
//	{"email":"a"} garbage             INVALID  accepted        INVALID
//	{"email":"a"}\n                   ok       ok              ok
//
// More reports whether another value follows, and it treats trailing
// whitespace as the end — which is what every curl and every editor appends.
func (strictJSONCodec) Decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errTrailingData
	}
	return nil
}

// errTrailingData is a plain error: the invoker wraps whatever Decode returns
// in errors.Invalid("body", err), so the code and the field are already
// right and this only has to say what happened.
var errTrailingData = stderrors.New("unexpected data after the JSON value")

// Params carries path and query parameters. The adapter seeds them; the
// route closure binds them into Req from `param:` and `query:` tags using
// setters computed at registration.
type Params interface {
	Path(name string) (string, bool)
	Query(name string) (string, bool)
}

type paramsKey struct{}

// WithParams returns a copy of ctx carrying p.
func WithParams(ctx context.Context, p Params) context.Context {
	return context.WithValue(ctx, paramsKey{}, p)
}

// ParamsFromContext returns the parameters the adapter seeded, or nil.
func ParamsFromContext(ctx context.Context) Params {
	p, _ := ctx.Value(paramsKey{}).(Params)
	return p
}

// RouteOption configures one route.
type RouteOption struct{ apply func(*routeConfig) }

type routeConfig struct {
	status int
	guards []app.AuthorizationPolicy
	name   string
}

// Status sets the success status an HTTP adapter writes. The defaults are 201
// for Post, 204 for Delete, and 200 for every other verb. It is ignored on the
// error path, where the code's own row in the error table decides.
func Status(code int) RouteOption {
	return RouteOption{apply: func(c *routeConfig) { c.status = code }}
}

// Guard requires the policy to allow the caller BEFORE the request is
// decoded, so an unauthorized caller's malformed body is a 403 and not a
// 400, and unauthenticated input never reaches the decoder. Guards travel as
// data on the route for exactly that reason.
// A nil policy — including a non-nil interface holding a nil pointer — is
// refused HERE, at composition, not on request 1. Without this the boot
// succeeded, the log said "http server listening", and every request to the
// guarded route panicked inside the edge and became a 500. README's headline
// is that every error the framework can detect surfaces at boot, and this one
// is detectable at the call site: app.Authorized and transport.Raw both
// already refuse their nil the same way.
func Guard(p app.AuthorizationPolicy) RouteOption {
	if app.IsNilPolicy(p) {
		panic("transport: Guard given a nil policy — a route guarded by nothing would panic on its first request; construct the policy before Register runs")
	}
	return RouteOption{apply: func(c *routeConfig) { c.guards = append(c.guards, p) }}
}

// Named overrides the "<module>.<handler>" telemetry name.
func Named(name string) RouteOption {
	return RouteOption{apply: func(c *routeConfig) { c.name = name }}
}

// HTTPRoute is one registered HTTP endpoint, described without a driver type.
type HTTPRoute struct {
	Verb     string // "GET", "POST", …
	Pattern  string // "/users/{id}" — net/http 1.22 and chi syntax
	Name     string // "<module>.<handler>"
	Success  int
	Guards   []app.AuthorizationPolicy
	Request  reflect.Type
	Response reflect.Type
	Bind     func(Codec) Invoker
}

// GRPCRoute is one registered gRPC method.
type GRPCRoute struct {
	FullMethod string
	Name       string
	Guards     []app.AuthorizationPolicy
	Request    reflect.Type
	Response   reflect.Type
	Bind       func(Codec) Invoker
}

// EventRoute is one registered subscription. Bind produces a
// broker.MessageHandler: decode, run, discard the response — the disposition
// is the consumer chain's.
type EventRoute struct {
	Topic   string
	Name    string
	Options []broker.SubscribeOption
	Request reflect.Type
	Bind    func(Codec) broker.MessageHandler
}

// RawRoute is one escape-hatch registration: a protocol-native handler for
// what the typed byte-in/byte-out port deliberately does not model.
type RawRoute struct {
	Protocol Protocol
	Pattern  string // the adapter's own syntax — "POST /uploads" for net/http
	Name     string // "<module>.<handler>"
	Guards   []app.AuthorizationPolicy
	Handler  any // opaque to core; the adapter type-asserts it
}

// Protocol names one of the three exposures.
type Protocol uint8

const (
	// ProtocolHTTP is served by warren/transport/http.
	ProtocolHTTP Protocol = iota + 1
	// ProtocolGRPC is served by warren/transport/grpc.
	ProtocolGRPC
	// ProtocolEvent is served by a broker driver.
	ProtocolEvent
)

func (p Protocol) String() string {
	switch p {
	case ProtocolHTTP:
		return "HTTP"
	case ProtocolGRPC:
		return "gRPC"
	case ProtocolEvent:
		return "event"
	default:
		return "unknown"
	}
}

// Table is the finished, driver-neutral description of every registration.
type Table struct {
	http   []HTTPRoute
	grpc   []GRPCRoute
	events []EventRoute
	raw    []RawRoute
	claims map[Protocol]string
	tel    app.Telemetry
}

// HTTP returns the registered HTTP routes.
func (t *Table) HTTP() []HTTPRoute { return t.http }

// GRPC returns the registered gRPC methods.
func (t *Table) GRPC() []GRPCRoute { return t.grpc }

// Events returns the registered subscriptions.
func (t *Table) Events() []EventRoute { return t.events }

// Telemetry returns the instrumentation boot bound, or nil.
//
// It travels on the Table rather than through the container because an
// adapter must be constructible in an application that has no telemetry at
// all: injecting app.Telemetry would make it a REQUIRED dependency and every
// uninstrumented service would fail to resolve it. The Table is already the
// boot-to-adapter channel — provided empty at step 2, filled at step 5, read
// by adapters at step 5b — so it carries this too.
func (t *Table) Telemetry() app.Telemetry { return t.tel }

// Raw returns the escape-hatch registrations, in registration order. An
// adapter serves the entries whose Protocol is its own and type-asserts each
// Handler, failing the boot — naming the route and the concrete type — when
// the assertion fails.
func (t *Table) Raw() []RawRoute { return t.raw }

// Claim records that an adapter is serving a protocol.
func (t *Table) Claim(p Protocol, by string) {
	if t.claims == nil {
		t.claims = map[Protocol]string{}
	}
	t.claims[p] = by
}

// Unserved reports routes registered for a protocol nothing is serving —
// three gRPC methods in an application with no gRPC server is detectable at
// boot, so it fails at boot.
func (t *Table) Unserved() error {
	// A raw route nobody serves 404s exactly like a typed one, so it counts.
	rawFor := map[Protocol]int{}
	for _, r := range t.raw {
		rawFor[r.Protocol]++
	}
	var missing []string
	if n := len(t.http) + rawFor[ProtocolHTTP]; n > 0 && t.claims[ProtocolHTTP] == "" {
		missing = append(missing, fmt.Sprintf("%d HTTP route(s) — add http.Server(...) to warren.New", n))
	}
	if n := len(t.grpc) + rawFor[ProtocolGRPC]; n > 0 && t.claims[ProtocolGRPC] == "" {
		// transport/grpc is deferred to v0.2, so "add grpc.Server(...)" would
		// name a package that does not exist. Say what is true instead.
		missing = append(missing, fmt.Sprintf(
			"%d gRPC method(s) — warren/transport/grpc is not built yet (deferred to v0.2).\n"+
				"      Serve them over HTTP with transport.Get/Post, or drop the transport.Method calls", n))
	}
	if n := len(t.events) + rawFor[ProtocolEvent]; n > 0 && t.claims[ProtocolEvent] == "" {
		missing = append(missing, fmt.Sprintf("%d event subscription(s) — add a broker module to warren.New", n))
	}
	if len(missing) == 0 {
		return nil
	}
	return diagnostic(fmt.Sprintf(
		"✗ registered routes have no adapter serving them\n\n    %s\n\n"+
			"  A route nobody serves is a route that silently 404s in production.",
		strings.Join(missing, "\n    ")))
}

// BuilderOption configures a Builder.
type BuilderOption struct{ apply func(*builderConfig) }

type builderConfig struct {
	validator validate.Validator
	telemetry app.Telemetry
}

// WithValidator sets the validator whose rules are compiled into every route
// closure. The default is validate.Required().
func WithValidator(v validate.Validator) BuilderOption {
	return BuilderOption{apply: func(c *builderConfig) { c.validator = v }}
}

// WithTelemetry binds the instrumentation every route is composed with at
// boot: app.Traced and app.Metered wrap each handler ONCE, here, and the
// request path consults nothing and decides nothing.
//
// This is what makes warren.md §7.1's "one import instruments … handlers"
// true without a user decorating anything. A nil Telemetry composes nothing,
// so an uninstrumented service's route closures are byte-identical to what
// they were before this option existed.
func WithTelemetry(t app.Telemetry) BuilderOption {
	return BuilderOption{apply: func(c *builderConfig) { c.telemetry = t }}
}

// Builder is the accumulator boot step 5 drives: it hands each controller a
// Registrar and freezes the result into a Table.
type Builder struct {
	cfg     builderConfig
	entries []entry
	errs    []error
}

// NewBuilder returns a Builder.
func NewBuilder(opts ...BuilderOption) *Builder {
	b := &Builder{cfg: builderConfig{validator: validate.Required()}}
	for _, opt := range opts {
		opt.apply(&b.cfg)
	}
	return b
}

// For returns the Registrar passed to one module's controllers.
func (b *Builder) For(module string) Registrar { return &registrar{b: b, module: module} }

// Table freezes the registrations into a new Table.
func (b *Builder) Table() (*Table, error) {
	t := &Table{}
	return t, b.Fill(t)
}

// Fill freezes the accumulated registrations into t — the Table the
// bootstrapper provided in the root scope at boot step 2, before the graph was
// validated, so that an adapter could inject it. Any claim already recorded on
// t survives. Every registration problem is reported together, so one boot
// names them all.
func (b *Builder) Fill(t *Table) error {
	if t.claims == nil {
		t.claims = map[Protocol]string{}
	}
	t.http, t.grpc, t.events, t.raw = nil, nil, nil, nil
	t.tel = b.cfg.telemetry
	seenHTTP := map[string]bool{}
	seenGRPC := map[string]bool{}
	seenEvent := map[string]bool{}
	seenRaw := map[string]bool{}

	for _, e := range b.entries {
		if e.rawHandler != nil {
			key := e.protocol.String() + " " + e.pattern
			if seenRaw[key] {
				b.errs = append(b.errs, errDuplicate(key))
				continue
			}
			seenRaw[key] = true
			t.raw = append(t.raw, RawRoute{
				Protocol: e.protocol, Pattern: e.pattern, Name: e.name,
				Guards: e.guards, Handler: e.rawHandler,
			})
			continue
		}
		switch e.protocol {
		case ProtocolHTTP:
			key := e.verb + " " + e.pattern
			if seenHTTP[key] {
				b.errs = append(b.errs, errDuplicate(key))
				continue
			}
			seenHTTP[key] = true
			t.http = append(t.http, HTTPRoute{
				Verb: e.verb, Pattern: e.pattern, Name: e.name, Success: e.status,
				Guards: e.guards, Request: e.req, Response: e.res, Bind: e.bindInvoker,
			})
		case ProtocolGRPC:
			if seenGRPC[e.pattern] {
				b.errs = append(b.errs, errDuplicate(e.pattern))
				continue
			}
			seenGRPC[e.pattern] = true
			t.grpc = append(t.grpc, GRPCRoute{
				FullMethod: e.pattern, Name: e.name, Guards: e.guards,
				Request: e.req, Response: e.res, Bind: e.bindInvoker,
			})
		case ProtocolEvent:
			key := e.pattern + " → " + e.name
			if seenEvent[key] {
				b.errs = append(b.errs, errDuplicate(key))
				continue
			}
			seenEvent[key] = true
			t.events = append(t.events, EventRoute{
				Topic: e.pattern, Name: e.name, Options: e.subOptions,
				Request: e.req, Bind: e.bindHandler,
			})
		}
	}
	if len(b.errs) > 0 {
		return errRegistration(b.errs)
	}
	return nil
}

// entry is one registration, protocol-tagged and already type-erased. A raw
// entry carries rawHandler and nothing else the typed path needs.
type entry struct {
	protocol    Protocol
	verb        string
	pattern     string
	name        string
	status      int
	guards      []app.AuthorizationPolicy
	subOptions  []broker.SubscribeOption
	req, res    reflect.Type
	bindInvoker func(Codec) Invoker
	bindHandler func(Codec) broker.MessageHandler
	rawHandler  any
}

type registrar struct {
	b      *Builder
	module string
}

func (r *registrar) record(e entry)     { r.b.entries = append(r.b.entries, e) }
func (r *registrar) moduleName() string { return r.module }

func (r *registrar) fail(err error) { r.b.errs = append(r.b.errs, err) }

// Get registers an HTTP GET route.
func Get[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolHTTP, "GET", pattern, h, 200, opts...)
}

// Post registers an HTTP POST route. Its default success status is 201.
func Post[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolHTTP, "POST", pattern, h, 201, opts...)
}

// Put registers an HTTP PUT route.
func Put[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolHTTP, "PUT", pattern, h, 200, opts...)
}

// Patch registers an HTTP PATCH route.
func Patch[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolHTTP, "PATCH", pattern, h, 200, opts...)
}

// Delete registers an HTTP DELETE route.
func Delete[Req, Res any](r Registrar, pattern string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolHTTP, "DELETE", pattern, h, 204, opts...)
}

// Raw registers a protocol-native handler — the escape hatch for what the
// typed byte-in/byte-out port deliberately does not model: multipart upload,
// file download, SSE, WebSocket upgrade, NDJSON export.
//
// h is opaque to core: the kernel and the contracts ring never import
// net/http, so the adapter serving p type-asserts it (warren/transport/http
// asserts http.Handler) and fails the boot, naming the route and the concrete
// type, when the assertion fails. It travels through the sealed Registrar so
// that the handler is built by the MODULE's own container, with the module's
// own private providers — which is the whole reason it is not an adapter
// option.
//
// THE PATTERN CARRIES THE METHOD, unlike every other registration here. Get
// and Post name a verb and take a bare path; Raw names none, so its pattern
// is the adapter's own syntax and includes one:
//
//	transport.Post(r, "/uploads", h)          // typed:  path only
//	transport.Raw(r, ProtocolHTTP, "POST /uploads", h)   // raw: method + path
//
// A raw route gets the edge ring, its Guard policies, and the drain. It gets
// no decode, no parameter binding, no validation, no encode, no Status
// default, and no body limit: the handler owns all of it.
func Raw(r Registrar, p Protocol, pattern string, h any, opts ...RouteOption) {
	reg, ok := r.(*registrar)
	if !ok {
		panic("transport: Raw with a foreign Registrar — the interface is sealed")
	}
	if h == nil {
		panic("transport: Raw registered a nil handler for " + pattern)
	}
	if pattern == "" {
		reg.fail(errEmptyPattern("Raw"))
		return
	}
	cfg := routeConfig{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	name := cfg.name
	if name == "" {
		name = reg.moduleName() + "." + rawName(h)
	}
	reg.record(entry{
		protocol:   p,
		pattern:    pattern,
		name:       name,
		guards:     cfg.guards,
		rawHandler: h,
	})
}

// rawName is the handler's own type name — the raw route has no request type
// to fall back on, and no app.Handler to read a method value from.
func rawName(h any) string {
	t := reflect.TypeOf(h)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Name() != "" {
		return t.Name()
	}
	return "raw"
}

// Method registers a gRPC method, named "package.Service/Method".
func Method[Req, Res any](r Registrar, fullMethod string, h app.Handler[Req, Res], opts ...RouteOption) {
	register(r, ProtocolGRPC, "", fullMethod, h, 0, opts...)
}

// OnEvent subscribes a handler to a topic. The options are warren/broker's
// own per-subscription options, forwarded to broker.Pipeline unchanged.
func OnEvent[Req, Res any](r Registrar, topic string, h app.Handler[Req, Res], opts ...broker.SubscribeOption) {
	reg, ok := r.(*registrar)
	if !ok {
		panic("transport: OnEvent with a foreign Registrar — the interface is sealed")
	}
	if h == nil {
		panic("transport: OnEvent registered a nil handler for topic " + topic)
	}
	if topic == "" {
		reg.fail(errEmptyPattern("OnEvent"))
		return
	}
	name := handlerName(reg.moduleName(), h)
	rule, err := planRule[Req](reg.b.cfg.validator)
	if err != nil {
		reg.fail(errCannotValidate(topic, name, err))
		return
	}
	setters, err := paramSetters(reflect.TypeFor[Req]())
	if err != nil {
		reg.fail(err)
		return
	}
	reg.record(entry{
		protocol:   ProtocolEvent,
		pattern:    topic,
		name:       name,
		subOptions: opts,
		req:        reflect.TypeFor[Req](),
		res:        reflect.TypeFor[Res](),
		bindHandler: func(c Codec) broker.MessageHandler {
			invoke := buildInvoker(c, h, rule, setters, name, reg.b.cfg.telemetry)
			return func(ctx context.Context, msg broker.Message) error {
				_, err := invoke(ctx, msg.Payload)
				return err
			}
		},
	})
}

func register[Req, Res any](r Registrar, p Protocol, verb, pattern string, h app.Handler[Req, Res], defaultStatus int, opts ...RouteOption) {
	reg, ok := r.(*registrar)
	if !ok {
		panic("transport: registration with a foreign Registrar — the interface is sealed")
	}
	if h == nil {
		panic("transport: registered a nil handler for " + verb + " " + pattern)
	}
	if pattern == "" {
		reg.fail(errEmptyPattern(verb))
		return
	}
	if p == ProtocolHTTP {
		if err := checkHTTPPattern(verb, pattern); err != nil {
			reg.fail(err)
			return
		}
	}

	cfg := routeConfig{status: defaultStatus}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	name := cfg.name
	if name == "" {
		name = handlerName(reg.moduleName(), h)
	}
	rule, err := planRule[Req](reg.b.cfg.validator)
	if err != nil {
		reg.fail(errCannotValidate(pattern, name, err))
		return
	}
	setters, err := paramSetters(reflect.TypeFor[Req]())
	if err != nil {
		reg.fail(err)
		return
	}
	// HTTP only. A `param:` tag with no matching {wildcard} would bind "" on
	// every HTTP request — the handler looks up the zero value and reports
	// NOT_FOUND with nothing saying why, which is what this check exists to
	// prevent. A gRPC method name is not a path and HAS no wildcards, so the
	// same check refused the canonical Warren handler over the one protocol
	// gRPC exists to share it with:
	//
	//	transport.Get(r, "/users/{id}", h)                    // fine
	//	transport.Method(r, "user.v1.UserService/GetUser", h) // refused
	//
	// OnEvent already exempts itself by never calling this; gRPC was the odd
	// one out. A gRPC adapter fills Req from the protobuf message, so the
	// param setters are simply unused there.
	if p == ProtocolHTTP {
		if err := checkWildcards(pattern, setters); err != nil {
			reg.fail(err)
			return
		}
	}

	reg.record(entry{
		protocol: p,
		verb:     verb,
		pattern:  pattern,
		name:     name,
		status:   cfg.status,
		guards:   cfg.guards,
		req:      reflect.TypeFor[Req](),
		res:      reflect.TypeFor[Res](),
		bindInvoker: func(c Codec) Invoker {
			return buildInvoker(c, h, rule, setters, name, reg.b.cfg.telemetry)
		},
	})
}

// buildInvoker is the erasure: everything that needs Req and Res happens
// here, at boot, inside a generic function. What escapes is a closure over
// bytes.
func buildInvoker[Req, Res any](c Codec, h app.Handler[Req, Res], rule func(*Req) error, setters []setter, name string, tel app.Telemetry) Invoker {
	// Instrumentation is composed HERE, at boot, once per route: this is the
	// single place every typed route and every event route passes through,
	// and it is generic, so Traced and Metered are instantiable. With no
	// Telemetry bound nothing is composed and the closure is what it always
	// was.
	if app.InstrumentsHandlers(tel) {
		h = app.Traced[Req, Res]()(app.Metered[Req, Res]()(h))
	}
	// Boxed once, here, rather than on every request. One key carries the
	// handler name and the Telemetry together — see app.Stamp.
	stamp := app.Stamp(name, tel)
	return func(ctx context.Context, raw []byte) ([]byte, error) {
		var req Req
		if len(raw) > 0 {
			if err := c.Decode(raw, &req); err != nil {
				return nil, errors.Invalid("body", err)
			}
		}
		if len(setters) > 0 {
			if err := bindParams(&req, ParamsFromContext(ctx), setters); err != nil {
				return nil, err
			}
		}
		if rule != nil {
			if err := rule(&req); err != nil {
				return nil, err
			}
		}
		res, err := h.Handle(stamp(ctx), req)
		if err != nil {
			return nil, err
		}
		return c.Encode(res)
	}
}

func planRule[Req any](v validate.Validator) (func(*Req) error, error) {
	if v == nil {
		return nil, nil
	}
	// A non-struct request used to return (nil, nil) — registered with NO
	// validation, and nothing said so. That is the silent skip
	// validate.Required() refuses at plan time, and it belongs at boot with
	// every other wiring mistake rather than in production.
	//
	// validate.PlanFor already refuses a non-struct with its own diagnostic,
	// so this simply stops swallowing it.
	return validate.PlanFor[Req](v)
}

// handlerName renders "<module>.<handler>" — the telemetry name Traced and
// Metered read off the context.
//
// The registered handler is usually an app.Chain, whose outermost value is a
// middleware's anonymous closure; deriving the name from it yields
// "module.1", which would make every span in a service share one name. So a
// closure's name is used only when it is meaningful, and the REQUEST TYPE is
// the fallback: "inventory.ReserveStock" identifies the use case, is stable
// across composition changes, and cannot collide within a module the way an
// anonymous function can. transport.Named overrides both.
func handlerName[Req, Res any](module string, h app.Handler[Req, Res]) string {
	if name := concreteName(h); name != "" {
		return module + "." + name
	}
	if t := reflect.TypeFor[Req](); t.Name() != "" {
		return module + "." + t.Name()
	}
	return module + ".handler"
}

// concreteName is the handler's own name when it has one: a named type, or a
// method value like c.register. Anonymous closures — every composed chain —
// have none.
func concreteName[Req, Res any](h app.Handler[Req, Res]) string {
	if fn, ok := any(h).(app.HandlerFunc[Req, Res]); ok {
		f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
		if f == nil {
			return ""
		}
		name := shortName(f.Name())
		// "func1", "func1.1" — a closure, not a use case.
		if name == "" || strings.HasPrefix(name, "func") || isNumeric(name) {
			return ""
		}
		return name
	}
	t := reflect.TypeOf(h)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Name() != "" {
		return t.Name()
	}
	return ""
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func shortName(full string) string {
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	full = strings.TrimSuffix(full, "-fm")
	return full
}

type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errDuplicate(route string) error {
	return diagnostic(fmt.Sprintf(
		"✗ duplicate route\n\n    %s is registered twice.\n\n"+
			"  One registration would silently shadow the other — remove one, or give\n"+
			"  them distinct patterns.", route))
}

// checkHTTPPattern refuses a typed pattern that carries its own method, or
// that is not a path at all.
//
// Get, Post and the rest already name the verb, and the adapter builds the
// router pattern as "<verb> <pattern>" — so transport.Get(r, "GET /x", h)
// becomes "GET GET /x", which net/http reads as host "GET" and path "/x".
// It boots clean and serves a route nothing can reach. transport.Raw is the
// opposite by design: it names no verb, so its pattern carries one.
func checkHTTPPattern(verb, pattern string) error {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return diagnostic(fmt.Sprintf(
			"✗ HTTP route pattern contains a method\n\n    transport.%s(r, %q, …)\n\n"+
				"  %s already names the method, so the pattern is the path alone:\n\n"+
				"      transport.%s(r, %q, …)\n\n"+
				"  Only transport.Raw takes \"METHOD /path\" — it names no method of\n"+
				"  its own, so the pattern has to carry one.",
			methodFunc(verb), pattern, methodFunc(verb),
			methodFunc(verb), strings.TrimSpace(pattern[i+1:])))
	}
	if pattern[0] != '/' {
		return diagnostic(fmt.Sprintf(
			"✗ HTTP route pattern is not a path\n\n    transport.%s(r, %q, …)\n\n"+
				"  An HTTP pattern starts with \"/\". Wildcards are net/http's:\n"+
				"  \"/users/{id}\", \"/files/{path...}\", \"/exact/{$}\".",
			methodFunc(verb), pattern))
	}
	return nil
}

// methodFunc names the registration function a verb came from, so the
// diagnostic shows the call the user actually wrote.
func methodFunc(verb string) string {
	switch verb {
	case "GET":
		return "Get"
	case "POST":
		return "Post"
	case "PUT":
		return "Put"
	case "PATCH":
		return "Patch"
	case "DELETE":
		return "Delete"
	default:
		return verb
	}
}

func errEmptyPattern(what string) error {
	return diagnostic(fmt.Sprintf(
		"✗ empty route pattern\n\n    %s was registered with an empty pattern.\n\n"+
			"  Give it a path (\"/users\"), a full method name\n"+
			"  (\"user.v1.UserService/Register\"), or a topic.", what))
}

// errCannotValidate names the route whose request type cannot be planned,
// and the two ways out. Without the route, a project with fifty of them is
// told only that "a" request type is wrong.
// errCannotValidate reports that a route's request type cannot be planned.
//
// There are two causes and they do not share a fix. Only one of them is
// about the SHAPE of the request type — and appending that advice to both
// meant a handler which already had a perfectly good struct was told to give
// it a struct, leaving "turn validation off" as the only suggestion left
// standing. An unsupported constraint already arrives as a complete
// diagnostic naming the tokens and offering the playground validator, so
// here it is presented, not re-explained.
func errCannotValidate(pattern, handler string, cause error) error {
	if !stderrors.Is(cause, validate.ErrNotAStruct) {
		return diagnostic(fmt.Sprintf(
			"✗ cannot validate the request for %s\n\n    handler: %s\n\n%v",
			pattern, handler, cause))
	}
	return diagnostic(fmt.Sprintf(
		"✗ cannot validate the request for %s\n\n    handler: %s\n    %v\n\n"+
			"  Give the handler a struct request type — which is what lets a field\n"+
			"  carry `validate:\"required\"` and a param tag — or turn validation off\n"+
			"  for this application:\n\n"+
			"      a := warren.New(...)\n"+
			"      if err := a.Validator(validate.None()); err != nil {\n"+
			"          return err\n"+
			"      }\n"+
			"      return a.Run()",
		pattern, handler, cause))
}

func errRegistration(errs []error) error {
	var b strings.Builder
	b.WriteString("✗ route registration failed\n")
	for _, err := range errs {
		b.WriteString("\n")
		b.WriteString(indent(err.Error()))
		b.WriteString("\n")
	}
	return diagnostic(strings.TrimRight(b.String(), "\n"))
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "\n")
}
