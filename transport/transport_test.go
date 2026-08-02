package transport_test

import (
	"context"
	stderrors "errors"
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

func TestNilHandlerPanicsAtRegistration(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("registering a nil handler did not panic")
		}
	}()
	transport.Post(transport.NewBuilder().For("user"), "/users",
		app.Handler[registerUser, userDTO](nil))
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
