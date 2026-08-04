package app_test

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
	"github.com/MerseniBilel/warren/transport"
)

// The identity seam exists so that "no caller" can never be mistaken for "a
// caller whose subject happens to be empty". Every test below is about that
// one property, or about the two error codes it makes reachable.

func TestIdentityIsAbsentOnABareContext(t *testing.T) {
	t.Parallel()

	got, ok := app.IdentityFromContext(context.Background())
	if ok {
		t.Errorf("IdentityFromContext found an identity on a bare context: %+v", got)
	}
	if got.Subject != "" || got.Issuer != "" || got.Scopes != nil || got.Claims != nil {
		t.Errorf("the absent identity is not the zero value: %+v", got)
	}
}

// TestIdentityIsNotComparable pins a property that looks like an accident and
// is not: Identity holds a slice and a map, so `==` does not compile on it.
// That is what keeps a later field addition source-compatible — no user can
// have built a dependency on equality for a new field to break — and it is
// the reason Claims can absorb whatever v0.2's verifier turns out to need.
//
// The assertion is the compile error itself, so this test documents rather
// than executes. If Identity ever becomes comparable, this comment is the
// warning that the additive-evolution guarantee went with it.
func TestIdentityIsNotComparable(t *testing.T) {
	t.Parallel()

	// Uncommenting the next line must not compile:
	//   _ = app.Identity{} == app.Identity{}
	var id app.Identity
	if reflect.TypeOf(id).Comparable() {
		t.Error("Identity became comparable; adding a field is now a potential breaking change")
	}
}

// TestIdentityIsAbsentUnderARealRequestContext — a request context is not
// bare. It carries a logger, a correlation ID, params and a telemetry stamp,
// and the lookup has to walk past all of them and still say no.
func TestIdentityIsAbsentUnderARealRequestContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctx = log.WithCorrelationID(ctx, "abc-1")
	ctx = log.WithLogger(ctx, slog.Default())
	ctx = transport.WithParams(ctx, fakeParams{})
	ctx = context.WithValue(ctx, struct{ k string }{"noise"}, 1)

	if _, ok := app.IdentityFromContext(ctx); ok {
		t.Error("a request context with no identity reported one")
	}
}

// TestAnEmptySubjectIsAbsence is the mechanism, not a nicety. A guard bug
// that produces an identity with no principal must make the request read as
// UNAUTHENTICATED — fail-closed — rather than authenticated-as-nobody, which
// would file rows owned by "".
func TestAnEmptySubjectIsAbsence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	got := app.WithIdentity(ctx, app.Identity{Scopes: []string{"admin"}})
	if got != ctx {
		t.Error("WithIdentity carried an identity with no subject; it must return the context unchanged")
	}
	if _, ok := app.IdentityFromContext(got); ok {
		t.Error("an identity with an empty subject reads back as present")
	}
}

func TestIdentityRoundTrips(t *testing.T) {
	t.Parallel()

	claims := map[string]any{"email": "bob@example.com", "tier": 3}
	in := app.Identity{
		Subject: "u-1",
		Issuer:  "https://issuer.example",
		Scopes:  []string{"orders:read", "orders:write"},
		Claims:  claims,
	}
	out, ok := app.IdentityFromContext(app.WithIdentity(context.Background(), in))
	if !ok {
		t.Fatal("the identity did not read back")
	}
	if out.Subject != in.Subject || out.Issuer != in.Issuer {
		t.Errorf("out = %+v, want %+v", out, in)
	}
	// Carried by reference: copying per request would cost an allocation
	// proportional to the token, and the map is the verifier's output.
	if len(out.Claims) != len(claims) || out.Claims["tier"] != 3 {
		t.Errorf("claims = %v, want %v", out.Claims, claims)
	}
	if !out.HasScope("orders:write") {
		t.Error("HasScope false for a scope the identity holds")
	}
	if out.HasScope("admin") {
		t.Error("HasScope true for a scope the identity does not hold")
	}
	if (app.Identity{Subject: "u-2"}).HasScope("anything") {
		t.Error("HasScope true on a nil scope slice")
	}
}

// TestRequireScopeNeverMergesTheTwoCodes — 401 and 403 answer different
// questions, and this is the distinction every framework in the survey either
// gets wrong or leaves to the user. These are also the FIRST policies in the
// repository able to produce either code: until now two rows of warren.md
// §2.6 were untested by construction.
func TestRequireScopeNeverMergesTheTwoCodes(t *testing.T) {
	t.Parallel()

	policy := app.RequireScope("orders:write")

	// No identity at all: the caller has not proved who they are.
	err := policy.Authorize(context.Background())
	if !werrors.Is(err, werrors.CodeUnauthenticated) {
		t.Errorf("no identity = %v, want UNAUTHENTICATED (401)", err)
	}

	// A known caller without the scope: they proved who they are, and may not.
	withOther := app.WithIdentity(context.Background(),
		app.Identity{Subject: "u-1", Scopes: []string{"orders:read"}})
	err = policy.Authorize(withOther)
	if !werrors.Is(err, werrors.CodePermissionDenied) {
		t.Errorf("wrong scope = %v, want PERMISSION_DENIED (403)", err)
	}

	// And the allowed case.
	withScope := app.WithIdentity(context.Background(),
		app.Identity{Subject: "u-1", Scopes: []string{"orders:read", "orders:write"}})
	if err := policy.Authorize(withScope); err != nil {
		t.Errorf("a caller holding the scope was denied: %v", err)
	}
}

func TestRequireAuthenticated(t *testing.T) {
	t.Parallel()

	policy := app.RequireAuthenticated()
	if err := policy.Authorize(context.Background()); !werrors.Is(err, werrors.CodeUnauthenticated) {
		t.Errorf("no identity = %v, want UNAUTHENTICATED", err)
	}
	ctx := app.WithIdentity(context.Background(), app.Identity{Subject: "u-1"})
	if err := policy.Authorize(ctx); err != nil {
		t.Errorf("an authenticated caller was denied: %v", err)
	}
}

// TestRequireScopeWithNoScopesIsRequireAuthenticated — an empty scope list
// must not silently allow everyone, which is the shape this kind of variadic
// helper usually fails in.
func TestRequireScopeWithNoScopesIsRequireAuthenticated(t *testing.T) {
	t.Parallel()

	policy := app.RequireScope()
	if err := policy.Authorize(context.Background()); !werrors.Is(err, werrors.CodeUnauthenticated) {
		t.Errorf("no identity = %v, want UNAUTHENTICATED", err)
	}
	ctx := app.WithIdentity(context.Background(), app.Identity{Subject: "u-1"})
	if err := policy.Authorize(ctx); err != nil {
		t.Errorf("RequireScope() denied an authenticated caller: %v", err)
	}
}

// TestLogValueNeverLeaksClaims — without this, one
// log.FromContext(ctx).InfoContext(ctx, "handled", "identity", id) dumps
// emails, phone numbers and sometimes a nested token into every log line.
func TestLogValueNeverLeaksClaims(t *testing.T) {
	t.Parallel()

	id := app.Identity{
		Subject: "u-1",
		Issuer:  "https://issuer.example",
		Scopes:  []string{"a", "b"},
		Claims: map[string]any{
			"ssn":          "123-45-6789",
			"access_token": "eyJhbGciOiJIUzI1NiJ9.secret",
		},
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("handled", "identity", id)
	out := buf.String()

	for _, secret := range []string{"123-45-6789", "eyJhbGciOiJIUzI1NiJ9.secret", "ssn", "access_token"} {
		if strings.Contains(out, secret) {
			t.Errorf("the log line leaked %q:\n%s", secret, out)
		}
	}
	// What it SHOULD say: who, from where, and how many scopes.
	for _, want := range []string{"u-1", "issuer.example", "2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, out)
		}
	}
}

func TestClaim(t *testing.T) {
	t.Parallel()

	id := app.Identity{Subject: "u-1", Claims: map[string]any{"tier": 3, "email": "bob@example.com"}}

	if v, ok := app.Claim[int](id, "tier"); !ok || v != 3 {
		t.Errorf("Claim[int](tier) = %v, %v, want 3, true", v, ok)
	}
	if v, ok := app.Claim[string](id, "email"); !ok || v != "bob@example.com" {
		t.Errorf("Claim[string](email) = %q, %v", v, ok)
	}
	// A missing key, a wrong type, and a nil map: all (zero, false), never a
	// panic — this runs on the request path with a map the verifier produced.
	if v, ok := app.Claim[string](id, "absent"); ok || v != "" {
		t.Errorf("a missing claim = %q, %v, want zero, false", v, ok)
	}
	if v, ok := app.Claim[string](id, "tier"); ok || v != "" {
		t.Errorf("a wrong-typed claim = %q, %v, want zero, false", v, ok)
	}
	if v, ok := app.Claim[string](app.Identity{Subject: "u"}, "any"); ok || v != "" {
		t.Errorf("a claim from a nil map = %q, %v, want zero, false", v, ok)
	}
}

// TestIdentityAllocations is the committed cost, in the style of
// transport/http's budget test.
//
// The strings are RUNTIME values on purpose. With compile-time constants the
// interface box becomes a static symbol and this measures a fake 1
// allocation — a number production never sees.
func TestIdentityAllocations(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation budgets are measured in the full run")
	}
	subject := "u-" + strconv.Itoa(len(t.Name()))
	id := app.Identity{Subject: subject}
	ctx := context.Background()
	withID := app.WithIdentity(ctx, id)

	if got := testing.AllocsPerRun(200, func() { sinkCtx = app.WithIdentity(ctx, id) }); got > 2 {
		t.Errorf("WithIdentity allocates %v, budget 2 (the box and the context node)", got)
	}
	// Reading is free, and the ABSENT case must be free too: it is the one
	// every unguarded request on a guarded service takes.
	if got := testing.AllocsPerRun(200, func() { sinkID, sinkOK = app.IdentityFromContext(withID) }); got != 0 {
		t.Errorf("IdentityFromContext (present) allocates %v, want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() { sinkID, sinkOK = app.IdentityFromContext(ctx) }); got != 0 {
		t.Errorf("IdentityFromContext (absent) allocates %v, want 0", got)
	}
}

var (
	sinkCtx context.Context
	sinkID  app.Identity
	sinkOK  bool
)

// TestIdentityIsPerRequestUnderRace — route closures are built once at boot
// and shared; the identity is not. Two callers through one closure must never
// see each other's.
func TestIdentityIsPerRequestUnderRace(t *testing.T) {
	t.Parallel()

	const n = 32
	var wg sync.WaitGroup
	bad := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := "u-" + strconv.Itoa(i)
			ctx := app.WithIdentity(context.Background(), app.Identity{Subject: want})
			got, ok := app.IdentityFromContext(ctx)
			if !ok || got.Subject != want {
				bad[i] = got.Subject
			}
		}()
	}
	wg.Wait()
	for i, b := range bad {
		if b != "" {
			t.Errorf("worker %d saw subject %q", i, b)
		}
	}
}

type fakeParams struct{}

func (fakeParams) Path(string) (string, bool)  { return "", false }
func (fakeParams) Query(string) (string, bool) { return "", false }
