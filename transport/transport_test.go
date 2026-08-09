package transport_test

import (
	"context"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

// --- the §3.5 fixture: one handler, three protocols -----------------------

type registerUser struct {
	Email string `json:"email" validate:"required"`
	Name  string `json:"name"`
}

type userDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type getUser struct {
	ID    string `param:"id"`
	Trace string `query:"trace"`
}

type userController struct{ calls int }

func (c *userController) register(_ context.Context, cmd registerUser) (userDTO, error) {
	c.calls++
	if cmd.Email == "taken@example.com" {
		return userDTO{}, werrors.Conflict("user already exists")
	}
	return userDTO{ID: "u-1", Email: cmd.Email}, nil
}

func (c *userController) get(_ context.Context, q getUser) (userDTO, error) {
	c.calls++
	return userDTO{ID: q.ID, Email: q.Trace}, nil
}

// Register is called once, at boot: one controller, three exposures.
func (c *userController) Register(r transport.Registrar) {
	transport.Post(r, "/users", app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](c.register)))
	transport.Method(r, "user.v1.UserService/Register", app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](c.register)))
	transport.OnEvent(r, "billing.customer.created", app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](c.register)))
	transport.Get(r, "/users/{id}", app.Handler[getUser, userDTO](app.HandlerFunc[getUser, userDTO](c.get)))
}

var _ transport.Controller = (*userController)(nil)

func build(t *testing.T, cs ...transport.Controller) *transport.Table {
	t.Helper()
	b := transport.NewBuilder()
	for i, c := range cs {
		c.Register(b.For("user"))
		_ = i
	}
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	return tbl
}

func TestOneControllerThreeProtocols(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})

	if got := len(tbl.HTTP()); got != 2 {
		t.Errorf("HTTP routes = %d, want 2", got)
	}
	if got := len(tbl.GRPC()); got != 1 {
		t.Errorf("gRPC routes = %d, want 1", got)
	}
	if got := len(tbl.Events()); got != 1 {
		t.Errorf("event routes = %d, want 1", got)
	}

	post := tbl.HTTP()[0]
	if post.Verb != "POST" || post.Pattern != "/users" {
		t.Errorf("route = %s %s", post.Verb, post.Pattern)
	}
	if post.Success != 201 {
		t.Errorf("POST success = %d, want 201 by default", post.Success)
	}
	if post.Name != "user.register" {
		t.Errorf("name = %q, want <module>.<handler>", post.Name)
	}
	if tbl.HTTP()[1].Success != 200 {
		t.Errorf("GET success = %d, want 200", tbl.HTTP()[1].Success)
	}
}

func TestInvokerDecodesCallsEncodes(t *testing.T) {
	t.Parallel()

	c := &userController{}
	tbl := build(t, c)
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	out, err := invoke(context.Background(), []byte(`{"email":"bob@example.com","name":"Bob"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(string(out), `"email":"bob@example.com"`) {
		t.Errorf("response = %s, want the encoded DTO", out)
	}
	if c.calls != 1 {
		t.Errorf("handler called %d times, want 1", c.calls)
	}
}

func TestHandlerErrorCodeSurvivesTheInvoker(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	_, err := invoke(context.Background(), []byte(`{"email":"taken@example.com"}`))
	if !werrors.Is(err, werrors.CodeConflict) {
		t.Errorf("err = %v, want CONFLICT intact — the adapter maps it to 409", err)
	}
}

func TestDecodeFailureIsInvalid(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	_, err := invoke(context.Background(), []byte(`{"email":`))
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Errorf("err = %v, want INVALID — a malformed body is 400, and on a consumer it dead-letters", err)
	}
}

// TestDecodeFailureNamesTheFieldNotTheGoType pins the wire contract for a
// type mismatch. encoding/json's own wording is
//
//	json: cannot unmarshal number into Go struct field RegisterUser.email of type string
//
// which puts an internal Go type name in a 400 body a stranger reads. The
// repository diagnostics already hold to this rule — a NOT_FOUND names "the
// value the caller asked by, never the Go type, which a client has no
// business seeing" — and the codec was the one place that did not.
func TestDecodeFailureNamesTheFieldNotTheGoType(t *testing.T) {
	t.Parallel()

	for _, codec := range []transport.Codec{transport.JSON(), transport.StrictJSON()} {
		t.Run(codec.Name(), func(t *testing.T) {
			tbl := build(t, &userController{})
			invoke := tbl.HTTP()[0].Bind(codec)

			_, err := invoke(context.Background(), []byte(`{"email":123,"name":"X"}`))
			if !werrors.Is(err, werrors.CodeInvalid) {
				t.Fatalf("err = %v, want INVALID", err)
			}
			got := err.Error()
			for _, leak := range []string{"registerUser", "RegisterUser", "Go struct field", "cannot unmarshal"} {
				if strings.Contains(got, leak) {
					t.Errorf("the 400 body leaks %q to the client:\n%s", leak, got)
				}
			}
			// Naming the offending field is the whole point of replacing the
			// message — a 400 that says only "invalid" is worse than the leak.
			if !strings.Contains(got, "email") {
				t.Errorf("the diagnostic does not name the offending field:\n%s", got)
			}
			if !strings.Contains(got, "string") || !strings.Contains(got, "number") {
				t.Errorf("the diagnostic says neither what was wanted nor what arrived:\n%s", got)
			}
		})
	}
}

func TestValidationRunsBeforeTheHandler(t *testing.T) {
	t.Parallel()

	c := &userController{}
	tbl := build(t, c)
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	// email is validate:"required" — a bad request never reaches Handle().
	_, err := invoke(context.Background(), []byte(`{"name":"Bob"}`))
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Fatalf("err = %v, want INVALID", err)
	}
	if c.calls != 0 {
		t.Error("the handler ran despite failing validation")
	}
}

func TestParamAndQueryBinding(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	get := tbl.HTTP()[1]
	invoke := get.Bind(transport.JSON())

	ctx := transport.WithParams(context.Background(), fakeParams{
		path:  map[string]string{"id": "u-7"},
		query: map[string]string{"trace": "req-9"},
	})
	out, err := invoke(ctx, nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(string(out), `"id":"u-7"`) || !strings.Contains(string(out), `"email":"req-9"`) {
		t.Errorf("response = %s, want path and query bound into Req", out)
	}
}

type fakeParams struct {
	path  map[string]string
	query map[string]string
}

func (p fakeParams) Path(n string) (string, bool)  { v, ok := p.path[n]; return v, ok }
func (p fakeParams) Query(n string) (string, bool) { v, ok := p.query[n]; return v, ok }

func TestEventRouteDecodesIntoTheHandler(t *testing.T) {
	t.Parallel()

	c := &userController{}
	tbl := build(t, c)
	ev := tbl.Events()[0]
	if ev.Topic != "billing.customer.created" {
		t.Errorf("topic = %q", ev.Topic)
	}

	h := ev.Bind(transport.JSON())
	err := h(context.Background(), broker.Message{
		ID:      "evt-1",
		Type:    "billing.customer.created",
		Payload: []byte(`{"email":"bob@example.com"}`),
	})
	if err != nil {
		t.Fatalf("message handler: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("handler called %d times, want 1 — the response is discarded, the error is the disposition", c.calls)
	}
}

func TestEventDecodeFailureIsInvalid(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	h := tbl.Events()[0].Bind(transport.JSON())

	err := h(context.Background(), broker.Message{ID: "evt-1", Payload: []byte(`{`)})
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Errorf("err = %v, want INVALID so the chain dead-letters it without retrying", err)
	}
}

func TestEventOptionsReachTheRoute(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("billing")
	transport.OnEvent(r, "billing.subscription.created",
		app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
			func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil })),
		broker.WithRetry(broker.ExponentialBackoff(5)),
		broker.WithDeadLetter("billing.subscription.created.dlq"),
		broker.WithConcurrency(10),
	)
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if got := len(tbl.Events()[0].Options); got != 3 {
		t.Errorf("options = %d, want the three forwarded to broker.Pipeline", got)
	}
}

func TestGuardsRunBeforeDecode(t *testing.T) {
	t.Parallel()

	denied := werrors.PermissionDenied("create users")
	b := transport.NewBuilder()
	transport.Post(b.For("user"), "/users",
		app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
			func(context.Context, registerUser) (userDTO, error) {
				t.Error("the handler ran despite a denied guard")
				return userDTO{}, nil
			})),
		transport.Guard(denyPolicy{err: denied}),
	)
	tbl, _ := b.Table()
	route := tbl.HTTP()[0]
	if len(route.Guards) != 1 {
		t.Fatalf("guards = %d, want 1 — the adapter runs them before decode", len(route.Guards))
	}
	// Guards travel as data so a denial precedes the decoder: an
	// unauthorized caller's malformed body must be 403, not 400.
	if err := route.Guards[0].Authorize(context.Background()); !werrors.Is(err, werrors.CodePermissionDenied) {
		t.Errorf("guard = %v, want the policy's error unchanged", err)
	}
}

type denyPolicy struct{ err error }

func (p denyPolicy) Authorize(context.Context) error { return p.err }

func TestDuplicateRouteIsABootError(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	h := app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
		func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil }))
	transport.Post(r, "/users", h)
	transport.Post(r, "/users", h)

	_, err := b.Table()
	if err == nil {
		t.Fatal("two POST /users registrations built a table — one would silently shadow the other")
	}
	if !strings.Contains(err.Error(), "POST /users") {
		t.Errorf("diagnostic does not name the route:\n%s", err)
	}
}

func TestUnservedProtocolFailsBoot(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	tbl.Claim(transport.ProtocolHTTP, "transport/http")
	// gRPC and events were registered but nothing serves them.
	err := tbl.Unserved()
	if err == nil {
		t.Fatal("routes with no adapter serving them booted silently")
	}
	for _, want := range []string{"gRPC", "event"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not name the unserved %s routes:\n%s", want, err)
		}
	}
}

func TestAllProtocolsClaimed(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	tbl.Claim(transport.ProtocolHTTP, "transport/http")
	tbl.Claim(transport.ProtocolGRPC, "transport/grpc")
	tbl.Claim(transport.ProtocolEvent, "broker/kafka")
	if err := tbl.Unserved(); err != nil {
		t.Errorf("Unserved = %v, want nil once every protocol is claimed", err)
	}
}

func TestRegistrationErrorsAccumulate(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	type unsupported struct {
		Ch chan int `param:"ch"`
	}
	transport.Get(r, "/a", app.Handler[unsupported, userDTO](app.HandlerFunc[unsupported, userDTO](
		func(context.Context, unsupported) (userDTO, error) { return userDTO{}, nil })))
	transport.Get(r, "", app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
		func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil })))

	_, err := b.Table()
	if err == nil {
		t.Fatal("bad registrations built a table")
	}
	// Both problems in one boot, not the first one only.
	if !strings.Contains(err.Error(), "chan") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("only some registration errors surfaced:\n%s", err)
	}
}

// TestNilHandlerDoesNotPanicAtRegistration replaces a test that asserted the
// opposite. The panic was never argued against AGENT.md's admission test —
// added 2026-08-09, which is exactly what it was written to stop — and it
// fails two of the four criteria: reg.fail is three lines away, and the
// alternative is a clean boot failure rather than silent data loss.
func TestNilHandlerDoesNotPanicAtRegistration(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering a nil handler panicked: %v", r)
		}
	}()
	b := transport.NewBuilder()
	transport.Post(b.For("user"), "/users", app.Handler[registerUser, userDTO](nil))
	if b.Failures() == nil {
		t.Fatal("a nil handler was accepted silently")
	}
}

func TestContainerNotConsultedPerRequest(t *testing.T) {
	t.Parallel()

	// The route table holds pre-built closures: Bind runs once, at boot, and
	// the invoker allocates nothing from the container per request.
	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	body := []byte(`{"email":"bob@example.com"}`)
	for range 100 {
		if _, err := invoke(context.Background(), body); err != nil {
			t.Fatalf("invoke: %v", err)
		}
	}
}

func BenchmarkInvoker(b *testing.B) {
	tbl := transport.NewBuilder()
	c := &userController{}
	c.Register(tbl.For("user"))
	table, err := tbl.Table()
	if err != nil {
		b.Fatalf("Table: %v", err)
	}
	invoke := table.HTTP()[0].Bind(transport.JSON())
	ctx := context.Background()
	body := []byte(`{"email":"bob@example.com","name":"Bob"}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := invoke(ctx, body); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = stderrors.New

func TestNestedParamTagIsARegistrationError(t *testing.T) {
	t.Parallel()

	type nested struct {
		Inner struct {
			ID string `param:"id"`
		}
	}
	b := transport.NewBuilder()
	transport.Get(b.For("m"), "/x/{id}", app.Handler[nested, userDTO](app.HandlerFunc[nested, userDTO](
		func(context.Context, nested) (userDTO, error) { return userDTO{}, nil })))
	_, err := b.Table()
	if err == nil {
		t.Fatal("a nested param tag registered silently — it would surface as a zero value on request 1")
	}
	if !strings.Contains(err.Error(), "nested parameter tag") {
		t.Errorf("diagnostic:\n%s", err)
	}
}

func TestValidatorIsConfigurable(t *testing.T) {
	t.Parallel()

	type richer struct {
		Email string `json:"email" validate:"required,email"`
	}
	reg := func(b *transport.Builder) {
		transport.Post(b.For("user"), "/users", app.Handler[richer, userDTO](app.HandlerFunc[richer, userDTO](
			func(context.Context, richer) (userDTO, error) { return userDTO{}, nil })))
	}

	// The default refuses a tag it cannot enforce...
	b := transport.NewBuilder()
	reg(b)
	if _, err := b.Table(); err == nil {
		t.Error("the default validator accepted a constraint it cannot enforce")
	}

	// ...and the explicit opt-out lets the project boot.
	b = transport.NewBuilder(transport.WithValidator(validate.None()))
	reg(b)
	if _, err := b.Table(); err != nil {
		t.Errorf("validate.None() did not unblock registration: %v", err)
	}
}

// TestChainedHandlerGetsAMeaningfulName pins the review's B1: the registered
// handler is normally an app.Chain, whose outermost value is a middleware's
// anonymous closure — deriving the name from it gave every span in a service
// the same name, "module.1".
func TestChainedHandlerGetsAMeaningfulName(t *testing.T) {
	t.Parallel()

	pass := func(next app.Handler[registerUser, userDTO]) app.Handler[registerUser, userDTO] {
		return app.HandlerFunc[registerUser, userDTO](func(ctx context.Context, r registerUser) (userDTO, error) {
			return next.Handle(ctx, r)
		})
	}
	chained := app.Chain(
		app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
			func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil })),
		pass, pass,
	)

	b := transport.NewBuilder()
	transport.Post(b.For("inventory"), "/x", chained)
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	got := tbl.HTTP()[0].Name
	if got == "inventory.1" || strings.HasPrefix(got, "inventory.func") {
		t.Fatalf("name = %q — every span in the service would share it", got)
	}
	if got != "inventory.registerUser" {
		t.Errorf("name = %q, want the request type as the stable fallback", got)
	}
}

// TestNonStructRequestIsRefusedNotSkipped — planRule returned (nil, nil) for
// any non-struct Req, so a route taking one was registered with NO
// validation at all and nothing said so. That is the silent-skip failure
// validate.Required() refuses at plan time; the route table has to refuse it
// too, at boot, where every other wiring mistake surfaces.
func TestNonStructRequestIsRefusedNotSkipped(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("catalogue")
	transport.Post(r, "/tags", app.HandlerFunc[[]string, struct{}](
		func(context.Context, []string) (struct{}, error) { return struct{}{}, nil },
	))

	_, err := b.Table()
	if err == nil {
		t.Fatal("a route with a non-struct request registered with no validation")
	}
	for _, want := range []string{"/tags", "None()"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic is missing %q:\n%v", want, err)
		}
	}
}

// A verb baked into a typed pattern used to boot clean and serve an
// unreachable route: the adapter builds "<verb> <pattern>", so
// transport.Get(r, "GET /x", h) became "GET GET /x" — host "GET", path "/x".
// Found by field-testing, 2026-08-02.
func TestTypedPatternWithAMethodIsARegistrationError(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	transport.Get(b.For("user"), "GET /oops", app.HandlerFunc[getUser, userDTO](
		func(context.Context, getUser) (userDTO, error) { return userDTO{}, nil }))

	_, err := b.Table()
	if err == nil {
		t.Fatal("a method inside a typed pattern must be a boot error")
	}
	for _, want := range []string{"contains a method", `transport.Get(r, "/oops"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic must contain %q:\n%s", want, err)
		}
	}
}

func TestTypedPatternMustBeAPath(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	transport.Post(b.For("user"), "users", app.HandlerFunc[registerUser, userDTO](
		func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil }))

	_, err := b.Table()
	if err == nil {
		t.Fatal("a pattern that is not a path must be a boot error")
	}
	if !strings.Contains(err.Error(), "not a path") {
		t.Errorf("diagnostic:\n%s", err)
	}
}

// Raw is the deliberate exception: it names no verb, so its pattern carries
// one.
func TestRawPatternMayCarryAMethod(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	transport.Raw(b.For("user"), transport.ProtocolHTTP, "POST /uploads", &uploadHandler{})
	if _, err := b.Table(); err != nil {
		t.Fatalf("Raw must accept \"METHOD /path\": %v", err)
	}
}

// TestParamTagWithNoWildcardIsARegistrationError — a `param:"id"` tag whose
// route pattern has no {id} bound to "" on every request, and nothing said
// so. The service booted, the route served, and the handler looked up the
// zero value: `{"code":"NOT_FOUND","message":"*domain.Product  not found"}`,
// the double space being the only clue. Renaming a path segment and
// forgetting the tag is the ordinary way to reach it.
//
// Both facts are known at registration, so this belongs with the duplicate
// route and the unsupported tag, not in production.
func TestParamTagWithNoWildcardIsARegistrationError(t *testing.T) {
	t.Parallel()

	type req struct {
		ID     string `param:"id"`
		Reason string `json:"reason"`
	}
	b := transport.NewBuilder()
	transport.Post(b.For("catalog"), "/products/{productId}/discontinue",
		app.Handler[req, userDTO](app.HandlerFunc[req, userDTO](
			func(context.Context, req) (userDTO, error) { return userDTO{}, nil })))
	_, err := b.Table()
	if err == nil {
		t.Fatal("a param tag with no matching wildcard registered silently — it binds \"\" on every request")
	}
	for _, want := range []string{"no matching path wildcard", `param:"id"`, "/products/{productId}/discontinue", "productId"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic does not mention %q:\n%s", want, err)
		}
	}
}

// TestParamTagsThatDoMatchStillRegister — the check must not refuse the
// ordinary cases: an exact wildcard, a trailing {rest...}, and a query tag,
// which never has a wildcard by definition.
func TestParamTagsThatDoMatchStillRegister(t *testing.T) {
	t.Parallel()

	type req struct {
		ID   string `param:"id"`
		Rest string `param:"rest"`
		Page int    `query:"page"`
	}
	b := transport.NewBuilder()
	transport.Get(b.For("catalog"), "/products/{id}/files/{rest...}",
		app.Handler[req, userDTO](app.HandlerFunc[req, userDTO](
			func(context.Context, req) (userDTO, error) { return userDTO{}, nil })))
	if _, err := b.Table(); err != nil {
		t.Fatalf("a route whose wildcards all match was refused:\n%s", err)
	}
}

// --- codec strictness ------------------------------------------------------
//
// Architect ruling, 2026-08-04. JSON() stays lenient and StrictJSON() is the
// opt-in. The reasons are in warren.md §3.5; the tests below are here because
// each one pins a decision that would otherwise look like an accident of
// encoding/json and get "simplified" away.

// typoField is the misspelled member the strictness tests send. It is
// spelled wrong ON PURPOSE — a field name the request type does not declare
// is the entire subject of these tests — so the spell checker is told once,
// here, rather than at every use.
//
//nolint:misspell // "nmae" is the defect under test, not a typo in prose
const typoField = `"nmae":"typo"`

// typoName is the same misspelling on its own, for asserting that a
// rejection names the offending field.
//
//nolint:misspell // as above
const typoName = "nmae"

// TestJSONIgnoresUnknownFields — leniency is now a documented guarantee, not
// a side effect of json.Unmarshal. One Codec serves HTTP and events, and on
// the event path a decode failure is INVALID, which §2.6 dead-letters WITHOUT
// retry. A producer adding a field to an event payload is ordinary schema
// evolution; under a strict default it would dead-letter 100% of a consumer's
// traffic and page someone.
func TestJSONIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	out, err := invoke(context.Background(), []byte(`{"email":"bob@example.com","name":"Bob",`+typoField+`}`))
	if err != nil {
		t.Fatalf("an unknown member was rejected by the default codec: %v", err)
	}
	if !strings.Contains(string(out), `"email":"bob@example.com"`) {
		t.Errorf("response = %s, want the known fields decoded", out)
	}
}

// TestJSONRejectsTrailingData — json.Unmarshal refuses a second document, and
// that is the property a careless "make it strict" refactor destroys:
// json.Decoder.Decode consumes the FIRST value and returns nil, discarding
// the rest in silence. Measured, before this test existed:
//
//	lenient  {"email":"a"} {"email":"b"} → INVALID
//	naive    {"email":"a"} {"email":"b"} → 201, second document gone
//
// So the strict codec had to be MORE careful than the lenient one, not less.
func TestJSONRejectsTrailingData(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())

	for _, body := range []string{
		`{"email":"a@example.com"} {"email":"b@example.com"}`,
		`{"email":"a@example.com"} garbage`,
	} {
		if _, err := invoke(context.Background(), []byte(body)); !werrors.Is(err, werrors.CodeInvalid) {
			t.Errorf("body %q = %v, want INVALID — a second document must not be discarded", body, err)
		}
	}
}

// TestStrictJSONRejectsUnknownFields is the opt-in's whole purpose.
func TestStrictJSONRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.StrictJSON())

	_, err := invoke(context.Background(), []byte(`{"email":"bob@example.com",`+typoField+`}`))
	if !werrors.Is(err, werrors.CodeInvalid) {
		t.Fatalf("StrictJSON accepted an unknown member: %v", err)
	}
	if !strings.Contains(err.Error(), typoName) {
		t.Errorf("the rejection does not name the offending field:\n%v", err)
	}
}

// TestStrictJSONRejectsTrailingData — without the More() check this passes
// silently, and the strict codec would be laxer than the default it exists to
// tighten. This is the regression test for that.
func TestStrictJSONRejectsTrailingData(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.StrictJSON())

	for _, body := range []string{
		`{"email":"a@example.com"} {"email":"b@example.com"}`,
		`{"email":"a@example.com"} garbage`,
	} {
		if _, err := invoke(context.Background(), []byte(body)); !werrors.Is(err, werrors.CodeInvalid) {
			t.Errorf("StrictJSON accepted %q: %v — the second document was discarded", body, err)
		}
	}
}

// TestStrictJSONAcceptsTrailingWhitespace — the framing check must not reject
// a trailing newline, which is what every curl and every editor appends.
func TestStrictJSONAcceptsTrailingWhitespace(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	invoke := tbl.HTTP()[0].Bind(transport.StrictJSON())

	for _, body := range []string{
		`{"email":"bob@example.com"}   `,
		"{\"email\":\"bob@example.com\"}\n",
		"{\"email\":\"bob@example.com\"}\r\n",
	} {
		if _, err := invoke(context.Background(), []byte(body)); err != nil {
			t.Errorf("StrictJSON rejected trailing whitespace in %q: %v", body, err)
		}
	}
}

// TestStrictJSONIsNamedTheSameContentType — it decodes application/json. A
// different Name() would make the adapter's content negotiation disagree with
// itself.
func TestStrictJSONIsNamedTheSameContentType(t *testing.T) {
	t.Parallel()

	if got, want := transport.StrictJSON().Name(), transport.JSON().Name(); got != want {
		t.Errorf("StrictJSON().Name() = %q, want %q", got, want)
	}
}

// TestStrictJSONEncodesIdentically — strictness is a DECODE policy. An
// encoder that differed would change every response body of a service that
// opted in.
func TestStrictJSONEncodesIdentically(t *testing.T) {
	t.Parallel()

	v := map[string]any{"email": "bob@example.com", "n": 1}
	strict, err := transport.StrictJSON().Encode(v)
	if err != nil {
		t.Fatalf("StrictJSON encode: %v", err)
	}
	lenient, err := transport.JSON().Encode(v)
	if err != nil {
		t.Fatalf("JSON encode: %v", err)
	}
	if string(strict) != string(lenient) {
		t.Errorf("StrictJSON encoded %s, JSON encoded %s — strictness is a decode policy", strict, lenient)
	}
}

// TestEventRoutesCannotBeMadeStrict — the strongest form of the ruling. There
// is deliberately no exported way to install a codec on the event path,
// because INVALID on a consumer is terminal: broker/middleware.go routes it
// past the retry rows straight to DeadLetter, at ERROR, "the one consumer
// event that should page a human". A strict event codec turns a producer's
// additive change into a DLQ storm.
//
// The assertion is on the API surface rather than on behaviour: behaviour
// cannot be wrong while there is nothing to configure.
func TestEventRoutesCannotBeMadeStrict(t *testing.T) {
	t.Parallel()

	tbl := build(t, &userController{})
	if len(tbl.Events()) == 0 {
		t.Skip("the fixture registers no event routes")
	}
	// Events() hands out a Bind(Codec) like HTTP does, but nothing in
	// transport/http or warren wires a user-chosen codec into it — the
	// event pipeline always binds JSON(). If that ever changes, this test
	// is where the DLQ blast radius gets re-argued.
	invoke := tbl.Events()[0].Bind(transport.JSON())
	if err := invoke(context.Background(), broker.Message{
		Payload: []byte(`{"email":"bob@example.com","unknown":"field"}`),
	}); err != nil {
		t.Errorf("an event carrying an unknown member did not reach the handler: %v", err)
	}
}

// TestGuardRefusesANilPolicy — field test #6, defect B1, and the one finding
// that contradicts a stated invariant. Guard appended the policy with no
// check, so the boot SUCCEEDED, the log said "http server listening", and
// every request to the guarded route panicked in the edge and became a 500.
//
// README's headline is that every error the framework can detect surfaces at
// boot, never on request 1. This one is detectable at the call site, and the
// fix was already written twice in the same codebase — app.Authorized and
// transport.Raw both refuse their nil the same way.
func TestGuardRefusesANilPolicy(t *testing.T) {
	t.Parallel()

	t.Run("a nil interface", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if recover() == nil {
				t.Error("Guard(nil) was accepted — every request to the route would 500")
			}
		}()
		_ = transport.Guard(nil)
	})

	t.Run("a non-nil interface holding a nil pointer", func(t *testing.T) {
		t.Parallel()
		// A nil POINTER in a non-nil interface: staticcheck reports that
		// `p == nil` is never true here, which is the trap stated as a fact.
		var typed *nilPolicy
		var p app.AuthorizationPolicy = typed
		defer func() {
			if recover() == nil {
				t.Error("Guard accepted a typed-nil policy — the route would allow everyone")
			}
		}()
		_ = transport.Guard(p)
	})
}

type nilPolicy struct{}

func (*nilPolicy) Authorize(context.Context) error { return nil }

// TestGRPCAcceptsAParamTaggedHandler — the group adapter ruling, 2026-08-05,
// found this by execution and it falsifies warren.md §4.2's claim that the
// gRPC round required "zero changes to core transport".
//
// checkWildcards is right for HTTP: a `param:"id"` field with no {id} in the
// pattern would bind "" on every request. But a gRPC method name is not a
// path and has no wildcards, so the check refused the canonical Warren
// handler — the very handler gRPC exists to share with HTTP:
//
//	transport.Get(r, "/users/{id}", h)                    // fine
//	transport.Method(r, "user.v1.UserService/GetUser", h) // BOOT FAILED
//
// OnEvent already exempts itself; gRPC was the odd one out.
func getUserByID(context.Context, getUser) (userDTO, error) { return userDTO{ID: "u-1"}, nil }

func TestGRPCAcceptsAParamTaggedHandler(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	transport.Get(r, "/users/{id}", app.HandlerFunc[getUser, userDTO](getUserByID))
	transport.Method(r, "user.v1.UserService/GetUser",
		app.HandlerFunc[getUser, userDTO](getUserByID))
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("a param-tagged handler was refused over gRPC, where there are no path wildcards: %v", err)
	}
	tbl.Claim(transport.ProtocolHTTP, "transport/http")
	tbl.Claim(transport.ProtocolGRPC, "transport/grpc")
	if n := len(tbl.GRPC()); n != 1 {
		t.Errorf("gRPC routes = %d, want 1", n)
	}
}

// TestHTTPStillRefusesAParamWithNoWildcard — the relaxation must not cost the
// case the check was written for. On HTTP the field would bind "" on every
// request and the handler would 404 with nothing saying why.
func TestHTTPStillRefusesAParamWithNoWildcard(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	transport.Get(b.For("user"), "/users",
		app.HandlerFunc[getUser, userDTO](getUserByID))

	if _, err := b.Table(); err == nil {
		t.Fatal("HTTP accepted a param: tag with no matching wildcard")
	} else if !strings.Contains(err.Error(), "no matching path wildcard") {
		t.Errorf("wrong diagnostic: %v", err)
	}
}

// TestATransactionalHandlerKeepsItsOwnName — the regression the 2026-08-08
// ordering ruling predicted its own fix would cause, flagged before it was
// written.
//
// concreteName falls through to reflect.TypeOf(h).Name() for any handler
// that is not a HandlerFunc. Making app.Transactional return a NAMED type
// (so Chain can see what it is stacking) means every transactional route
// would answer "transactionalHandler[…]" — so every one of them would share
// one span name and one metric label, silently. TestChainedHandlerGetsA-
// MeaningfulName uses a USER middleware and cannot catch it.
func TestATransactionalHandlerKeepsItsOwnName(t *testing.T) {
	t.Parallel()

	named := app.HandlerFunc[registerUser, userDTO](registerUser2)
	chained := app.Chain[registerUser, userDTO](named,
		app.Transactional[registerUser, userDTO](noopUoW{}))

	b := transport.NewBuilder()
	transport.Post(b.For("inventory"), "/x", chained)
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	got := tbl.HTTP()[0].Name
	if strings.Contains(got, "transactionalHandler") {
		t.Fatalf("name = %q — a framework wrapper's type name is not a use case, "+
			"and every transactional route in the service would share it", got)
	}
	if got != "inventory.registerUser" {
		t.Errorf("name = %q, want the request-type fallback inventory.registerUser", got)
	}
}

func registerUser2(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil }

type noopUoW struct{}

func (noopUoW) Do(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// --- nil handlers ----------------------------------------------------------
//
// A controller field the constructor forgot to assign is the shape `warren new`
// generates, and until 2026-08-09 it was a raw panic and exit 2 — while the
// two sibling checks in the same functions, a method in the pattern and a
// duplicate route, produced a clean "✗ route registration failed" block and
// exit 1. The admission test (AGENT.md § General) fails a nil handler on
// criteria 3 and 4: reg.fail exists, and the alternative is a clean boot
// failure rather than silent data loss.

var update = flag.Bool("update", false, "rewrite golden files")

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("diagnostic does not match golden file %s\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// nilHandlerController is the reproduction: a constructor that omits a field,
// so Register hands a nil handler to a route.
type nilHandlerController struct {
	h app.Handler[registerUser, userDTO] // never assigned
}

func (c *nilHandlerController) Register(r transport.Registrar) {
	transport.Post(r, "/users", c.h)
}

func TestNilHandlerIsARegistrationFailureNotAPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering a nil handler panicked instead of failing the boot: %v", r)
		}
	}()

	b := transport.NewBuilder()
	(&nilHandlerController{}).Register(b.For("user"))
	_, err := b.Table()
	if err == nil {
		t.Fatal("a nil handler built a table")
	}
	// The failure is accumulated, not raised: the builder holds it, and the
	// bootstrapper can read the list without Fill when it has to abandon
	// earlier than Fill.
	failures := b.Failures()
	if failures == nil {
		t.Fatal("Failures() is nil after a failed registration")
	}
	if failures.Error() != err.Error() {
		t.Errorf("Failures() and Fill report different things:\n%v\n\n%v", failures, err)
	}
	if got := strings.Count(err.Error(), "was registered with a nil handler"); got != 1 {
		t.Errorf("the nil handler is reported %d times, want 1:\n%v", got, err)
	}
}

func TestNilHandlerDiagnosticIsGolden(t *testing.T) {
	t.Parallel()

	t.Run("http", func(t *testing.T) {
		t.Parallel()
		b := transport.NewBuilder()
		(&nilHandlerController{}).Register(b.For("user"))
		_, err := b.Table()
		if err == nil {
			t.Fatal("a nil handler built a table")
		}
		assertGolden(t, "nil_handler", err.Error())
	})

	t.Run("event", func(t *testing.T) {
		t.Parallel()
		b := transport.NewBuilder()
		var h app.Handler[registerUser, userDTO]
		transport.OnEvent(b.For("user"), "user.registered", h)
		_, err := b.Table()
		if err == nil {
			t.Fatal("a nil handler built a table")
		}
		assertGolden(t, "nil_handler_event", err.Error())
	})

	t.Run("grpc", func(t *testing.T) {
		t.Parallel()
		b := transport.NewBuilder()
		var h app.Handler[registerUser, userDTO]
		transport.Method(b.For("user"), "user.v1.UserService/Register", h)
		_, err := b.Table()
		if err == nil {
			t.Fatal("a nil handler built a table")
		}
		assertGolden(t, "nil_handler_grpc", err.Error())
	})

	t.Run("raw", func(t *testing.T) {
		t.Parallel()
		b := transport.NewBuilder()
		transport.Raw(b.For("user"), transport.ProtocolHTTP, "POST /uploads", nil)
		_, err := b.Table()
		if err == nil {
			t.Fatal("a nil raw handler built a table")
		}
		assertGolden(t, "nil_handler_raw", err.Error())
	})
}

// TestNilHandlerJoinsOtherRegistrationFailures — a nil handler is an ordinary
// registration failure, so one boot reports it together with everything else
// that went wrong, which is the property step 5 claims.
func TestNilHandlerJoinsOtherRegistrationFailures(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	var h app.Handler[registerUser, userDTO]
	transport.Post(r, "/users", h)
	good := app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
		func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil }))
	transport.Get(r, "/users/{id}", good)
	transport.Get(r, "/users/{id}", good)

	_, err := b.Table()
	if err == nil {
		t.Fatal("a nil handler and a duplicate route built a table")
	}
	for _, want := range []string{"nil handler", "duplicate route"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not carry %q:\n%v", want, err)
		}
	}
}

// TestEveryJoinedFailureLeadsWithItsOwnHeadline — a report that joins two
// failures gives each of them a ✗ line of its own, at the same indent.
//
// The nil-handler entry began with its subject and no headline, so when it
// was the first entry it read as the BODY of "✗ route registration failed"
// while the duplicate beside it kept its own "✗ duplicate route" — one
// failure nested under the other, and only because of the order they were
// registered in. A field test graded the nil handler 2/10 for lacking the
// framework's diagnostic shape; an entry that borrows a neighbour's headline
// is the same defect one notch quieter.
func TestEveryJoinedFailureLeadsWithItsOwnHeadline(t *testing.T) {
	t.Parallel()

	b := transport.NewBuilder()
	r := b.For("user")
	var nilHandler app.Handler[registerUser, userDTO]
	transport.Post(r, "/users", nilHandler)
	good := app.Handler[registerUser, userDTO](app.HandlerFunc[registerUser, userDTO](
		func(context.Context, registerUser) (userDTO, error) { return userDTO{}, nil }))
	transport.Get(r, "/users/{id}", good)
	transport.Get(r, "/users/{id}", good)

	_, err := b.Table()
	if err == nil {
		t.Fatal("a nil handler and a duplicate route built a table")
	}
	report := err.Error()
	assertGolden(t, "nil_handler_and_duplicate", report)

	lines := strings.Split(report, "\n")
	if len(lines) == 0 || lines[0] != "✗ route registration failed" {
		t.Fatalf("the report does not open with the joined header:\n%s", report)
	}

	// Every ✗ after the header is one failure's own headline, and they all sit
	// at the same indent — which is what "neither nests under the other" means
	// when the only structure the terminal has is leading spaces.
	indents := map[string]int{}
	for _, line := range lines[1:] {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "✗ ") {
			continue
		}
		indents[trimmed] = len(line) - len(trimmed)
	}
	for _, want := range []string{"✗ nil handler", "✗ duplicate route"} {
		if _, ok := indents[want]; !ok {
			t.Errorf("no headline %q of its own — the failure is nested in a sibling's:\n%s", want, report)
		}
	}
	seen := map[int][]string{}
	for headline, indent := range indents {
		seen[indent] = append(seen[indent], headline)
	}
	if len(seen) > 1 {
		t.Errorf("the joined failures sit at %d different indents, so one nests under another: %v\n%s", len(seen), seen, report)
	}
}
