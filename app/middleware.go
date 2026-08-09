package app

import (
	"context"
	stderrors "errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/MerseniBilel/warren/errors"
)

// RetryPolicy decides whether a failed attempt is retried and how long to
// wait first. The kernel never sees a backoff library.
//
// A concrete one already ships in the core module: broker.ExponentialBackoff.
// It is the ONLY vocabulary — there is no resilience module and there will
// not be one (warren.md §7.3). A circuit breaker guards an OUTBOUND
// dependency and belongs in the adapter that makes the call, behind the port
// your domain declares; returning errors.Unavailable from there is the whole
// integration, because this middleware then retries it and every transport
// maps it.
type RetryPolicy interface {
	// Next reports whether a retry should follow the given completed attempt
	// (1-based) and how long to wait before it. Returning retry == false
	// ends the loop; the handler's last error is returned as-is.
	//
	// Termination is the policy's contract: the middleware imposes no
	// attempt ceiling of its own, so a policy that never returns
	// retry == false retries a persistent failure forever — with a zero
	// delay, as a hot loop. Every real policy bounds its attempts.
	Next(attempt int) (delay time.Duration, retry bool)
}

// AuthorizationPolicy decides whether the identity on the context may
// proceed. The edge ring authenticates and puts identity on the context; this
// policy authorizes.
//
// It is the port an authentication adapter implements — warren/auth, in v0.2 —
// and it is fully usable without one today: write the policy yourself and
// attach it per route with transport.Guard, which runs before decode so an
// unauthorized caller's malformed body is a 403 and not a 400.
//
// The contract: return nil to allow. Return an error from the warren/errors
// vocabulary to deny — errors.PermissionDenied(action) for a known caller
// that may not act, errors.Unauthenticated(reason) when identity is absent —
// and the middleware returns it unchanged, so the adapter maps the right
// status. An error outside the vocabulary maps downstream to INTERNAL, the
// safe default for the unknown.
type AuthorizationPolicy interface {
	Authorize(ctx context.Context) error
}

// Telemetry is the core-ring instrumentation seam Traced and Metered speak
// to. warren/observability implements it over OpenTelemetry and seeds it on
// the context at the edge; the kernel never imports a telemetry SDK. When no
// Telemetry is on the context, Traced and Metered are exact pass-throughs.
type Telemetry interface {
	// Span opens a span named name and returns the derived context and the
	// function that ends the span, recording err (nil on success). The
	// returned context MUST derive from ctx, preserving its values — Metered
	// and HandlerName downstream read through it, and a Span that returns a
	// fresh context silently disables them.
	Span(ctx context.Context, name string) (context.Context, func(err error))

	// Record records one handler invocation: its name, its duration, and its
	// error — nil on success. Implementations key error counters by the
	// warren/errors code.
	Record(name string, d time.Duration, err error)

	// Inject writes the trace context on ctx through set, one key/value pair
	// at a time.
	//
	// The carrier is a FUNCTION rather than a map so an HTTP header, a
	// broker.Message.Headers and a gRPC metadata block are all reachable
	// without an intermediate allocation — and, more importantly, so that
	// core never names a header key. Which keys carry trace context is the
	// W3C spec's business and the implementation's; the kernel's business is
	// only that they travel.
	Inject(ctx context.Context, set func(key, value string))

	// Extract returns a context continuing the trace the key/value pairs get
	// answers describe, or ctx unchanged when there is none. get returns ""
	// for an absent key.
	Extract(ctx context.Context, get func(key string) string) context.Context
}

// RequestInfo describes one inbound request to the telemetry seam, in terms
// core can express: no net/http type, no gRPC type, no OTel type.
//
// Route is the PATTERN — "/users/{id}", not "/users/42". It is the field a
// dashboard groups by and an alert fires on, and the only one whose
// cardinality is bounded by the size of the route table rather than by
// traffic.
type RequestInfo struct {
	Protocol string // "http", "grpc"
	Method   string // "POST", or the gRPC full method
	Route    string // the matched pattern, NOT the concrete path
	Path     string // the concrete path, for a span attribute only
	Scheme   string // "http", "https"
	Host     string
}

// RequestSpan is the optional interface a Telemetry implements to open a
// transport-level SERVER span around a whole request — decode, validation,
// the handler, and encode.
//
// It is what makes a trace answer "which route", "which status" and "how much
// of the latency was transport" rather than only "which handler was slow".
// The handler span nested inside it is where the business time is; this one
// is where the request is.
//
// Optional because the seam must work for a Telemetry that only implements
// the two required methods: an adapter checks and falls back to no span.
type RequestSpan interface {
	// ServerSpan opens the span and returns the derived context and the
	// function that ends it, recording the response status and the error.
	ServerSpan(ctx context.Context, info RequestInfo) (context.Context, func(status int, err error))
}

// HandlerInstrumentation is the optional interface a Telemetry implements to
// decline boot-time composition of Traced and Metered around every route.
//
// It is optional because the common case has no opinion: a Telemetry that
// does not implement it is instrumented. An implementation returning false
// still carries trace context — the transport edge and the broker still use
// Inject and Extract — it just leaves handler middleware to the user.
type HandlerInstrumentation interface {
	InstrumentHandlers() bool
}

// InstrumentsHandlers reports whether t wants boot to compose Traced and
// Metered around each route. A nil t does not.
func InstrumentsHandlers(t Telemetry) bool {
	if t == nil || isTypedNil(t) {
		return false
	}
	if h, ok := t.(HandlerInstrumentation); ok {
		return h.InstrumentHandlers()
	}
	return true
}

type telemetryKey struct{}

type handlerNameKey struct{}

// WithTelemetry returns a copy of ctx carrying t. It is the seeding side of
// the instrumentation seam, called by observability's edge integration; a
// nil t — a typed-nil pointer inside the interface included — is not
// carried, so the never-instrumented path stays a pass-through instead of a
// request-time panic in Span.
//
// The typed-nil probe uses one constant-time reflect nil check. Invariant 7
// forbids type-driven dispatch and container consultation on the request
// path; a nil probe is neither, and the alternative is exactly the
// production panic the invariant exists to prevent.
func WithTelemetry(ctx context.Context, t Telemetry) context.Context {
	if t == nil || isTypedNil(t) {
		return ctx
	}
	return context.WithValue(ctx, telemetryKey{}, t)
}

// IsNilPolicy reports whether a policy is nil, including a non-nil interface
// holding a nil pointer — which would allow every request it guards.
//
// It is exported because transport.Guard must make the same refusal at the
// same moment, and a second copy of the probe is a second thing to get wrong.
func IsNilPolicy(p AuthorizationPolicy) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func isTypedNil(t Telemetry) bool {
	v := reflect.ValueOf(t)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Chan, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// TelemetryFromContext returns the Telemetry carried by ctx, or nil.
func TelemetryFromContext(ctx context.Context) Telemetry {
	// The fused stamp first: an instrumented route carries both values under
	// one key, and WithTelemetry's own key is the seeding path an edge or a
	// test uses.
	if s, ok := ctx.Value(stampKey{}).(*stamp); ok {
		return s.tel
	}
	t, _ := ctx.Value(telemetryKey{}).(Telemetry)
	return t
}

// WithHandlerName returns a copy of ctx carrying the handler's qualified
// name, "<module>.<handler>". The transport adapter seeds it in the route
// table's pre-built closure at boot step 5 — it is the one party that knows
// both names — and Traced and Metered read it back.
func WithHandlerName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, handlerNameKey{}, name)
}

// StampHandlerName returns a function that does what WithHandlerName does,
// with the boxing paid once here instead of on every call.
//
// context.WithValue takes an `any`, so passing a string boxes it — an
// allocation, per request, on every HTTP, gRPC and event route. A route's
// handler name is fixed at boot, so that allocation belongs at boot too.
// Measured on an M3 Pro: 25.4 ns and 2 allocs (64 B) becomes 14.2 ns and 1
// alloc (48 B).
//
// Use WithHandlerName anywhere the name is not fixed in advance; this exists
// for the request path, and the request path alone.
func StampHandlerName(name string) func(context.Context) context.Context {
	boxed := any(name)
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, handlerNameKey{}, boxed)
	}
}

// HandlerName returns the qualified handler name carried by ctx, or "".
func HandlerName(ctx context.Context) string {
	if s, ok := ctx.Value(stampKey{}).(*stamp); ok {
		return s.name
	}
	name, _ := ctx.Value(handlerNameKey{}).(string)
	return name
}

type stampKey struct{}

// stamp is the boot-fixed pair every instrumented route carries: the
// handler's qualified name and the Telemetry to report it to.
type stamp struct {
	name string
	tel  Telemetry
}

// Stamp returns the per-request context stamp for one route: the handler's
// qualified name and the Telemetry, both fixed at boot, stored under ONE key.
//
// One key, not two, because both values are known at boot and an instrumented
// request would otherwise pay two context.WithValue allocations to carry a
// pair that never varies. A nil t stamps the name alone, through
// StampHandlerName, so an uninstrumented service's request path is unchanged.
func Stamp(name string, t Telemetry) func(context.Context) context.Context {
	if t == nil || isTypedNil(t) {
		return StampHandlerName(name)
	}
	boxed := &stamp{name: name, tel: t}
	return func(ctx context.Context) context.Context {
		return context.WithValue(ctx, stampKey{}, boxed)
	}
}

// Retrying re-invokes the handler when the error's OUTERMOST code is
// CodeUnavailable or CodeContention — the two retryable codes. It is what
// optimistic concurrency needs: a stale write is CONTENTION, and re-invoking
// the handler re-reads the aggregate, which is the whole point. The outermost code decides,
// exactly as an adapter's status mapping does, because wrapping is
// recategorization: a handler that wraps an Unavailable inside Internal has
// declared the failure non-retryable, and this middleware agrees. A plain
// %w wrap (fmt.Errorf) leaves the outermost Warren error untouched and
// stays retryable. Every other code returns unretried.
//
// When retries exhaust, or the context is cancelled during a wait, the
// handler's LAST error is returned: it is the freshest and still carries the
// code the adapter maps. A context that is already cancelled still reaches
// the handler once — the handler owns its own context checks; this
// middleware observes cancellation between attempts and during waits. A nil
// policy panics at composition time, like Chain's guards.
func Retrying[Req, Res any](policy RetryPolicy) Middleware[Req, Res] {
	return retrying[Req, Res](policy, retryable)
}

// retrying is the loop both Retrying and RetryingOn use. They differ only in
// which errors they consider retryable, and sharing the loop is what keeps
// the parts that are easy to get wrong — the last error on exhaustion,
// cancellation observed during a wait, and a zero delay still checking the
// context — identical between them.
func retrying[Req, Res any](policy RetryPolicy, shouldRetry func(error) bool) Middleware[Req, Res] {
	if policy == nil {
		panic("app: Retrying composed with a nil policy — construct the policy before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return retryingHandler[Req, Res]{policy: policy, shouldRetry: shouldRetry, next: next}
	}
}

// RetryingOn is Retrying with the retryable codes given explicitly.
//
// Retrying retries CodeUnavailable and nothing else, which is right for a
// dependency that was briefly away. It is NOT right for optimistic
// concurrency: a stale write is CodeConflict, and under contention on one
// aggregate that is the NORMAL outcome, not a failure. A field test measured
// the consequence — 200 buyers against 50 seats sold far fewer than 50 —
// and had to hand-write this middleware to get the right answer. Safe, but
// wrong, and every team would rediscover it.
//
//	app.Chain(h, app.RetryingOn(broker.ExponentialBackoff(5), errors.CodeConflict))
//
// The handler must RE-READ its aggregate on each attempt. Retrying re-invokes
// the handler, not the transaction, so a handler that closed over a stale
// version will conflict for ever — which is why this is a separate name
// rather than a widened default.
//
// Everything else is Retrying's behaviour, including the rule that matters
// most: the OUTERMOST code decides, so a handler that wraps a Conflict inside
// an Internal has declared the failure final and this agrees. Retries are
// only safe on an idempotent handler; see Retrying.
//
// IT DOES NOT INHERIT CodeUnavailable. The codes you name are the whole set,
// so migrating Retrying(p) to RetryingOn(p, errors.CodeConflict) STOPS
// retrying a dependency that was briefly away. Pass both when you want both:
//
//	app.RetryingOn(p, errors.CodeConflict, errors.CodeUnavailable)
//
// At least one code is required. An empty set would retry nothing, silently,
// which is the failure mode a variadic helper like this usually has.
func RetryingOn[Req, Res any](policy RetryPolicy, codes ...errors.Code) Middleware[Req, Res] {
	if len(codes) == 0 {
		panic("app: RetryingOn requires at least one code — with none it would retry nothing, which is app.Chain without it")
	}
	for _, c := range codes {
		if slices.Contains(terminal, c) {
			panic(errTerminalRetry(c))
		}
	}
	return retrying[Req, Res](policy, func(err error) bool {
		var e *errors.Error
		if !stderrors.As(err, &e) {
			return false
		}
		return slices.Contains(codes, e.Code())
	})
}

// retryable reports whether the OUTERMOST Warren code is CodeUnavailable —
// the adapter's own reading of the error, not a chain search: errors.Is
// would find an Unavailable buried under a recategorizing Internal wrap and
// retry a failure the handler declared final.
func retryable(err error) bool {
	var e *errors.Error
	if stderrors.As(err, &e) {
		// The two codes whose definition is "the same request may succeed
		// later unchanged". They retry for different reasons — Unavailable
		// means make the same call again, Contention means start the
		// read-modify-write sequence again — and this middleware does the
		// second by construction, because it re-invokes the HANDLER.
		return e.Code() == errors.CodeUnavailable || e.Code() == errors.CodeContention
	}
	return false
}

// terminal are the codes for which the same request can never succeed
// unchanged, per warren.md §2.6. Retrying one is never correct: it spends
// the whole backoff budget, and a transaction per attempt, to return the
// answer it already had.
var terminal = []errors.Code{
	errors.CodeInvalid, errors.CodeNotFound, errors.CodeConflict,
	errors.CodeUnauthenticated, errors.CodePermissionDenied,
}

// Authorized runs the policy before invoking the handler; a denial
// short-circuits — the handler is never called — and the policy's error
// returns unchanged, code intact. Because it is core-ring, the same check
// applies to HTTP, gRPC, and consumers. A nil policy panics at composition
// time, like Chain's guards.
func Authorized[Req, Res any](policy AuthorizationPolicy) Middleware[Req, Res] {
	// isNilPolicy, not policy == nil: a non-nil interface holding a nil
	// pointer passes the plain check and then ALLOWS every request, which is
	// the typed-nil trap Identity's own doc cites as the reason it is a
	// struct. WithTelemetry has carried a probe for this since it shipped;
	// this had the check without the probe.
	if IsNilPolicy(policy) {
		panic("app: Authorized composed with a nil policy — construct the policy before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return authorizedHandler[Req, Res]{policy: policy, next: next}
	}
}

// UnitOfWork is the transaction seam Transactional speaks to — the shape
// warren/persistence's UnitOfWork already has. It is declared here rather
// than imported so app stays free of every sibling contract: the middleware
// needs one method, and a port with one method costs less than a coupling.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(context.Context) error) error
}

// Transactional wraps Handle in a unit of work, so the aggregate state the
// handler wrote and the outbox rows for the events it raised commit in one
// transaction — or neither does.
//
// A handler that calls uow.Do itself under this middleware is joined, not
// nested: warren.md §10 shows both patterns, and the unit of work's own
// contract makes the inner call join the transaction in scope. The
// transaction's error passes through with its code intact, so a serialization
// failure arrives as CONTENTION (it was UNAVAILABLE until 4a1d152) and
// app.Retrying — composed OUTSIDE this middleware — re-runs the whole
// transaction rather than retrying inside a doomed one.
//
// Composing it the other way round is REFUSED by Chain, with a panic naming
// the fix: one transaction around every attempt has no correct reading, and
// on Postgres it commits a failed attempt's staged writes alongside the next
// one's. A nil unit of work panics at composition time, like Chain's guards.
func Transactional[Req, Res any](uow UnitOfWork) Middleware[Req, Res] {
	if uow == nil {
		panic("app: Transactional composed with a nil unit of work — construct it before boot step 5 composes the route table")
	}
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return transactionalHandler[Req, Res]{uow: uow, next: next}
	}
}

// Traced opens one span per handler invocation, named with the
// "<module>.<handler>" the adapter seeded via WithHandlerName. The Telemetry
// rides the context (WithTelemetry); on a context carrying none, Traced is
// an exact pass-through.
func Traced[Req, Res any]() Middleware[Req, Res] {
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return tracedHandler[Req, Res]{next: next}
	}
}

// Metered records a duration histogram per handler and an error counter
// keyed by the warren/errors code, through the context-carried Telemetry —
// once per invocation, the error path included. On a context carrying no
// Telemetry it is an exact pass-through.
func Metered[Req, Res any]() Middleware[Req, Res] {
	return func(next Handler[Req, Res]) Handler[Req, Res] {
		return meteredHandler[Req, Res]{next: next}
	}
}

// errTerminalRetry refuses a retry that can never succeed.
//
// A field test measured the cost of the one this exists to prevent:
// RetryingOn(p, CodeConflict) turned a guaranteed refusal — five units asked
// for, three in stock — into six database transactions and 1.25s, against
// 7ms for the same refusal returned once. Anyone who knew a SKU could burn a
// worker per request. Composition time is the only place to catch it, because
// at run time the two look identical.
func errTerminalRetry(code errors.Code) string {
	return fmt.Sprintf(
		"app: RetryingOn was composed on %s, which can never succeed on a retry — "+
			"a request refused by a business rule is refused identically on every "+
			"attempt, so this would spend the whole backoff budget and a database "+
			"transaction per attempt to return the same answer.\n\n"+
			"  A stale write is not %s. It is CONTENTION, and app.Retrying already "+
			"retries it:\n\n"+
			"      app.Chain(h, app.Retrying(policy), app.Transactional(uow))\n\n"+
			"  Retrying re-invokes the HANDLER, so your handler must re-read its "+
			"aggregate on each attempt.", code, code)
}

// --- composition identity ---------------------------------------------------
//
// These middleware return NAMED handler types rather than anonymous
// HandlerFunc closures, for one reason: a closure carries no identity, and
// Chain has to be able to see what it is stacking. That is what lets it
// refuse Transactional outside Retrying — a composition with no correct
// reading — at boot instead of letting it corrupt data at run time.
//
// The alternative was matching runtime.FuncForPC names. It works today and
// fails open tomorrow: the ".funcN" suffix is assigned by closure order
// within the file, so adding an unrelated closure above renumbers it and the
// check quietly stops firing.

// Unwrapper is the interface a middleware's handler implements to make itself
// TRANSPARENT to Chain's composition checks.
//
// EVERY middleware Warren ships implements it, so Chain can walk a stack it
// was handed rather than only the one it is building —
// Chain(Chain(h, Retrying(p)), Transactional(uow)) is the same mistake spelled
// in two calls, and the walk catches it.
//
// It is EXPORTED so yours can implement it too, and that is the point rather
// than a courtesy. A field test found four compositions the transaction
// refusal missed, all one cause: this interface was unexported, so a user's
// middleware could not declare itself transparent even when it wanted to.
// A middleware whose handler does not implement Unwrapper is a wall the walk
// stops at, and a Retrying beneath it is invisible to the refusal in an
// enclosing Chain.
//
// One method, and it costs nothing at request time — the walk runs at boot
// step 5, never per request:
//
//	type logging[Req, Res any] struct{ next app.Handler[Req, Res] }
//
//	func (m logging[Req, Res]) Handle(ctx context.Context, r Req) (Res, error) { … }
//	func (m logging[Req, Res]) Unwrap() app.Handler[Req, Res]                  { return m.next }
//
// "Every" is load-bearing for Warren's own, and was not always true. Traced,
// Metered, Authorized and Timeout each returned an anonymous HandlerFunc, so a
// Retrying beneath one of them was invisible and
// Chain(Chain(h, Traced(), Retrying(p)), Transactional(uow)) built the
// corrupting composition without a word. A new middleware here MUST be a
// named type with an Unwrap, not a closure.
type Unwrapper[Req, Res any] interface {
	Handler[Req, Res]
	Unwrap() Handler[Req, Res]
}

// Every middleware Warren ships is walkable, asserted at compile time. Add a
// middleware and this block is the one place that fails until you have given
// it a named handler type — which is the point: the walk failing open is
// silent, and this is not.
var (
	_ Unwrapper[any, any] = retryingHandler[any, any]{}
	_ Unwrapper[any, any] = transactionalHandler[any, any]{}
	_ Unwrapper[any, any] = authorizedHandler[any, any]{}
	_ Unwrapper[any, any] = tracedHandler[any, any]{}
	_ Unwrapper[any, any] = meteredHandler[any, any]{}
	_ Unwrapper[any, any] = timeoutHandler[any, any]{}
)

type retryingHandler[Req, Res any] struct {
	policy      RetryPolicy
	shouldRetry func(error) bool
	next        Handler[Req, Res]
}

// Unwrap makes this middleware transparent to Chain's containsRetrying walk.
// See app.Unwrapper.
func (h retryingHandler[Req, Res]) Unwrap() Handler[Req, Res] { return h.next }

func (h retryingHandler[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	for attempt := 1; ; attempt++ {
		res, err := h.next.Handle(ctx, req)
		if err == nil || !h.shouldRetry(err) {
			return res, err
		}
		delay, retry := h.policy.Next(attempt)
		if !retry {
			return res, err
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return res, err
			}
		} else if ctx.Err() != nil {
			return res, err
		}
	}
}

type authorizedHandler[Req, Res any] struct {
	policy AuthorizationPolicy
	next   Handler[Req, Res]
}

// Unwrap makes this middleware transparent to Chain's containsRetrying walk.
// See app.Unwrapper.
func (h authorizedHandler[Req, Res]) Unwrap() Handler[Req, Res] { return h.next }

func (h authorizedHandler[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	if err := h.policy.Authorize(ctx); err != nil {
		var zero Res
		return zero, err
	}
	return h.next.Handle(ctx, req)
}

type tracedHandler[Req, Res any] struct {
	next Handler[Req, Res]
}

// Unwrap makes this middleware transparent to Chain's containsRetrying walk.
// See app.Unwrapper.
func (h tracedHandler[Req, Res]) Unwrap() Handler[Req, Res] { return h.next }

func (h tracedHandler[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	t := TelemetryFromContext(ctx)
	if t == nil {
		return h.next.Handle(ctx, req)
	}
	sctx, end := t.Span(ctx, HandlerName(ctx))
	res, err := h.next.Handle(sctx, req)
	end(err)
	return res, err
}

type meteredHandler[Req, Res any] struct {
	next Handler[Req, Res]
}

// Unwrap makes this middleware transparent to Chain's containsRetrying walk.
// See app.Unwrapper.
func (h meteredHandler[Req, Res]) Unwrap() Handler[Req, Res] { return h.next }

func (h meteredHandler[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	t := TelemetryFromContext(ctx)
	if t == nil {
		return h.next.Handle(ctx, req)
	}
	start := time.Now()
	res, err := h.next.Handle(ctx, req)
	t.Record(HandlerName(ctx), time.Since(start), err)
	return res, err
}

type transactionalHandler[Req, Res any] struct {
	uow  UnitOfWork
	next Handler[Req, Res]
}

// Unwrap makes this middleware transparent to Chain's containsRetrying walk.
// See app.Unwrapper.
func (h transactionalHandler[Req, Res]) Unwrap() Handler[Req, Res] { return h.next }

func (h transactionalHandler[Req, Res]) Handle(ctx context.Context, req Req) (Res, error) {
	var res Res
	err := h.uow.Do(ctx, func(ctx context.Context) error {
		var err error
		res, err = h.next.Handle(ctx, req)
		return err
	})
	if err != nil {
		var zero Res
		return zero, err
	}
	return res, nil
}
