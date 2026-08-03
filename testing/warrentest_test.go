package warrentest_test

import (
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
	"github.com/MerseniBilel/warren/domain"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	warrentest "github.com/MerseniBilel/warren/testing"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

var _ = flag.Bool("update", false, "rewrite golden files")

// --- a small module under test --------------------------------------------

type userID string

func (id userID) String() string { return string(id) }

type registered struct {
	User  userID
	Email string
	At    time.Time
}

func (e registered) EventName() string     { return "user.registered" }
func (e registered) OccurredAt() time.Time { return e.At }
func (e registered) AggregateID() string   { return e.User.String() }

type repo interface {
	Taken(email string) bool
}

type fakeRepo struct{ taken bool }

func (f fakeRepo) Taken(string) bool { return f.taken }

type registerUser struct{ Email string }
type userDTO struct{ ID string }

type handler struct {
	users repo
	pub   broker.Publisher
}

func (h *handler) Handle(ctx context.Context, cmd registerUser) (userDTO, error) {
	if h.users.Taken(cmd.Email) {
		return userDTO{}, werrors.Conflict("user already exists")
	}
	ev := registered{User: "u-1", Email: cmd.Email, At: time.Unix(1, 0)}
	if err := h.pub.Publish(ctx, ev.EventName(), broker.Message{
		ID: "evt-1", Type: ev.EventName(), Key: ev.AggregateID(),
		Payload: []byte(`{"User":"u-1","Email":"` + cmd.Email + `"}`),
	}); err != nil {
		return userDTO{}, err
	}
	return userDTO{ID: "u-1"}, nil
}

func userModule() warren.Module {
	return warren.NewModule("user",
		warren.Providers(
			func() repo { return fakeRepo{} },
			func(r repo, p broker.Publisher) app.Handler[registerUser, userDTO] {
				return &handler{users: r, pub: p}
			},
		),
		warren.Consumers(func(h app.Handler[registerUser, userDTO], hr health.Registry) *probe {
			_ = hr.Register(health.NewCheck("user", func(context.Context) error { return nil }))
			return &probe{}
		}),
	)
}

// probe stands in for a consumer: it exists to be constructed at boot and to
// register its health check. It registers no subscription, but everything
// listed in warren.Consumers must still be a transport.Controller.
type probe struct{}

func (*probe) Register(transport.Registrar) {}

// --- the harness ----------------------------------------------------------

func TestInvokeDrivesTheHandler(t *testing.T) {
	t.Parallel()

	a := warrentest.NewModuleTest(t, userModule(), warrentest.WithMemoryBroker())
	res, err := warrentest.Invoke[registerUser, userDTO](context.Background(), a, registerUser{Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ID != "u-1" {
		t.Errorf("res = %+v", res)
	}
}

func TestReplaceSubstitutesAFake(t *testing.T) {
	t.Parallel()

	a := warrentest.NewModuleTest(t, userModule(),
		warrentest.WithMemoryBroker(),
		warrentest.Replace[repo](fakeRepo{taken: true}),
	)
	_, err := warrentest.Invoke[registerUser, userDTO](context.Background(), a, registerUser{Email: "bob@example.com"})
	if !werrors.Is(err, werrors.CodeConflict) {
		t.Errorf("err = %v, want the fake's CONFLICT — the substitution did not take", err)
	}
}

func TestReplaceThatMatchesNothingFailsTheBoot(t *testing.T) {
	t.Parallel()

	// A typo'd fake is never silently ignored: a fake you would trust and
	// never get is worse than no fake.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	a := warren.New(userModule())
	if err := a.Substitute(warren.Substitute[*strings.Reader](strings.NewReader(""))); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("an unmatched substitution booted silently")
	}
	if !strings.Contains(err.Error(), "substitution matched no provider") {
		t.Errorf("diagnostic:\n%v", err)
	}
}

func TestPublishedAndAssertPublished(t *testing.T) {
	t.Parallel()

	a := warrentest.NewModuleTest(t, userModule(), warrentest.WithMemoryBroker())
	if _, err := warrentest.Invoke[registerUser, userDTO](context.Background(), a, registerUser{Email: "bob@example.com"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	got := warrentest.AssertPublished[registered](t, a)
	if got.Email != "bob@example.com" {
		t.Errorf("event = %+v, want the decoded payload", got)
	}
	if msgs := warrentest.Published(a, "user.registered"); len(msgs) != 1 {
		t.Errorf("Published = %d messages, want 1", len(msgs))
	}
}

func TestAssertPublishedNamesWhatWasPublished(t *testing.T) {
	t.Parallel()

	// The failure message must be actionable: what was expected, and what
	// actually reached the broker.
	a := warrentest.NewModuleTest(t, userModule(), warrentest.WithMemoryBroker())
	fake := &recordingT{}
	func() {
		defer func() { _ = recover() }()
		warrentest.AssertPublished[registered](fake, a)
	}()
	if !strings.Contains(fake.msg, "user.registered") || !strings.Contains(fake.msg, "Published topics") {
		t.Errorf("failure message is not actionable:\n%s", fake.msg)
	}
}

// recordingT captures a Fatalf instead of failing the real test.
type recordingT struct {
	testing.TB
	msg string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Fatalf(format string, args ...any) {
	r.msg = fmt.Sprintf(format, args...)
	panic("fatal")
}

func TestBootFailureSurfacesWarrensDiagnostic(t *testing.T) {
	t.Parallel()

	broken := warren.NewModule("broken",
		warren.Consumers(func(repo) *probe { return &probe{} }),
	)
	a := warren.New(broken)
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("a module with an unresolvable dependency booted")
	}
	if !strings.Contains(err.Error(), "✗ cannot resolve dependency") {
		t.Errorf("boot failure is not Warren's diagnostic:\n%v", err)
	}
	_ = stderrors.New
	_ = domain.Event(registered{})
}

// TestInvokeInReachesEveryBootedModule covers the case a real service hits
// on its first end-to-end test: three features on one graph, and a use case
// to drive in each.
//
// Invoke alone cannot do it — it resolves from the one module
// NewModuleTest was pointed at, and InModule fixes that choice at boot for
// every call. Without InvokeIn, every multi-feature test hand-writes the
// same generic wrapper over App.Warren().Invoke.
func TestInvokeInReachesEveryBootedModule(t *testing.T) {
	t.Parallel()

	a := warrentest.NewModuleTest(t, moduleA(), warrentest.WithModules(moduleB()))

	got, err := warrentest.InvokeIn[echoReq, echoRes](context.Background(), a, "a", echoReq{Text: "from a"})
	if err != nil {
		t.Fatalf("InvokeIn a: %v", err)
	}
	if got.Text != "a: from a" {
		t.Errorf("got %+v", got)
	}

	// The other module, from the same booted app — which is the whole point.
	got, err = warrentest.InvokeIn[echoReq, echoRes](context.Background(), a, "b", echoReq{Text: "from b"})
	if err != nil {
		t.Fatalf("InvokeIn b: %v", err)
	}
	if got.Text != "b: from b" {
		t.Errorf("got %+v", got)
	}

	// And a module that is not booted names itself in the failure.
	if _, err = warrentest.InvokeIn[echoReq, echoRes](context.Background(), a, "nope", echoReq{}); err == nil {
		t.Fatal("invoking into an unbooted module succeeded")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the diagnostic does not name the module:\n%v", err)
	}
}

type echoReq struct{ Text string }
type echoRes struct{ Text string }

type echoHandler struct{ prefix string }

func (h echoHandler) Handle(_ context.Context, r echoReq) (echoRes, error) {
	return echoRes{Text: h.prefix + ": " + r.Text}, nil
}

func moduleA() warren.Module {
	return warren.NewModule("a", warren.Providers(
		func() app.Handler[echoReq, echoRes] { return echoHandler{prefix: "a"} },
	))
}

func moduleB() warren.Module {
	return warren.NewModule("b", warren.Providers(
		func() app.Handler[echoReq, echoRes] { return echoHandler{prefix: "b"} },
	))
}

// --- a module whose routes carry a tag the core validator refuses ---------

type reserveStock struct {
	OrderID  string `json:"order_id" validate:"required,min=3"`
	Quantity int    `json:"quantity" validate:"gte=1,lte=1000"`
}

type reservation struct{ ID string }

type stockHandler struct{}

func (stockHandler) Handle(context.Context, reserveStock) (reservation, error) {
	return reservation{ID: "r-1"}, nil
}

// stockController registers a route whose request type carries min=/gte=/lte=
// — constraints the standard-library validator plans and refuses.
type stockController struct {
	h app.Handler[reserveStock, reservation]
}

func (c *stockController) Register(r transport.Registrar) {
	transport.Post(r, "/stock/reservations", c.h)
}

func stockModule() warren.Module {
	return warren.NewModule("stock",
		warren.Providers(func() app.Handler[reserveStock, reservation] { return stockHandler{} }),
		warren.Controllers(func(h app.Handler[reserveStock, reservation]) *stockController {
			return &stockController{h: h}
		}),
	)
}

// permissive is a stand-in for validate/playground: a Validator that plans
// every tag. Core cannot import the playground module (it is a separate
// module, and core is stdlib + dig), so the SEAM is what this tests — that a
// Validator reaching the boot from the graph is the one routes are compiled
// against.
type permissive struct{ planned int }

func (p *permissive) Plan(reflect.Type) (validate.Rule, error) {
	p.planned++
	return func(any) error { return nil }, nil
}

// TestAValidatorFromTheGraphIsUsed — a module test could not use
// validate/playground at all. Core's validator refuses min=/gte=, the boot
// diagnostic tells you to install the playground module, and then every
// module test in that module failed to boot: NewModuleTest calls Start
// itself, and the validator was reachable only through App.Validator, which
// the harness never calls. Two shipped v0.1 features were mutually
// exclusive, and Warren's own diagnostic walked you into it.
func TestAValidatorFromTheGraphIsUsed(t *testing.T) {
	t.Parallel()

	v := &permissive{}
	a := warrentest.NewModuleTest(t, stockModule(), warrentest.WithValidator(v))

	if v.planned == 0 {
		t.Error("the validator from the graph planned nothing — the route was compiled against a different one")
	}
	got, err := warrentest.Invoke[reserveStock, reservation](t.Context(), a, reserveStock{OrderID: "o-1", Quantity: 2})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got.ID != "r-1" {
		t.Errorf("Invoke = %+v, want the handler's reservation", got)
	}
}

// TestWithoutAValidatorTheCoreOneStillRefuses — the fix must not silently
// downgrade: a module carrying tags core cannot enforce, booted with no
// validator, must still fail loudly.
func TestWithoutAValidatorTheCoreOneStillRefuses(t *testing.T) {
	t.Parallel()

	// Driven through warren.App directly: NewModuleTest turns a boot failure
	// into t.Fatalf, which is right for a user and untestable from here.
	a := warren.New(stockModule(), claimsModule())
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("a module whose tags core cannot enforce booted with no validator")
	}
	if !strings.Contains(err.Error(), "unsupported validation constraint") {
		t.Errorf("diagnostic was:\n%v\nwant it to name the unsupported constraint", err)
	}
}

// claimsModule mirrors what NewModuleTest installs so a route registered in
// a test app is not refused for having no adapter.
func claimsModule() warren.Module {
	return warren.NewModule("claims",
		warren.Providers(func(tbl *transport.Table) *claims {
			for _, p := range []transport.Protocol{
				transport.ProtocolHTTP, transport.ProtocolGRPC, transport.ProtocolEvent,
			} {
				tbl.Claim(p, "test")
			}
			return &claims{}
		}),
		warren.Eager[*claims](),
	)
}

type claims struct{}
