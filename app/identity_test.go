package app_test

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/broker"
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

// --- field test #6 ----------------------------------------------------------

// TestFmtVerbsDoNotLeakClaims — field test #6, defect B3. LogValue protects
// slog's top-level attr value and NOTHING else: slog does not call LogValuer
// on a nested value, and fmt never calls it at all. A tester logged an
// identity fifteen ways and got the claims map back TEN times, including
// through the line a Go developer writes at 2 a.m.:
//
//	fmt.Errorf("denied for %v", id)
//
// The worst of them wraps into errors.Internal, which can reach a log AND a
// response body. One String method closes the entire fmt family.
func TestFmtVerbsDoNotLeakClaims(t *testing.T) {
	t.Parallel()

	const secret = "SUPERSECRET-ssn"
	id := app.Identity{
		Subject: "alice",
		Issuer:  "https://issuer.example",
		Scopes:  []string{"docs:read"},
		Claims:  map[string]any{"ssn": secret, "refresh_token": "tok-" + secret},
	}

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", id)},
		{"%s", sprintfS(id)},
		{"%+v", fmt.Sprintf("%+v", id)},
		{"pointer %v", fmt.Sprintf("%v", &id)},
		{"wrapped in an error", fmt.Errorf("denied for %v", id).Error()},
		{"inside a slice", fmt.Sprintf("%v", []app.Identity{id})},
		{"inside a map", fmt.Sprintf("%v", map[string]app.Identity{"caller": id})},
	} {
		if strings.Contains(tc.got, secret) {
			t.Errorf("%s leaked a claim value: %s", tc.name, tc.got)
		}
		// It must still be USEFUL — a redaction that says nothing gets
		// replaced by %+v on the struct's fields, which leaks again.
		if !strings.Contains(tc.got, "alice") {
			t.Errorf("%s does not identify the caller: %s", tc.name, tc.got)
		}
	}
}

// TestWithIdentityRefusesABlankSubject — field test #6. WithIdentity compared
// against "" only, so " ", "\t", "\x00" and "" were all carried, and
// RequireAuthenticated returned nil for every one. A user who trusts the
// documented refusal files audit rows owned by "\x00".
func TestWithIdentityRefusesABlankSubject(t *testing.T) {
	t.Parallel()

	// A byte-order mark and a zero-width space, written as escapes so the
	// source file itself stays clean.
	for _, subject := range []string{" ", "\t", "\n", "  \t ", "\x00", "\x00\x00", "\ufeff", "\u200b"} {
		ctx := context.Background()
		got := app.WithIdentity(ctx, app.Identity{Subject: subject})
		if got != ctx {
			t.Errorf("WithIdentity carried a blank subject %q", subject)
		}
		if _, ok := app.IdentityFromContext(got); ok {
			t.Errorf("a blank subject %q reads back as present", subject)
		}
		if err := app.RequireAuthenticated().Authorize(got); err == nil {
			t.Errorf("RequireAuthenticated allowed a caller whose subject is %q", subject)
		}
	}
}

// TestWithoutIdentityClearsIt — field test #6. There was no way to clear an
// identity, and seeding a zero Identity over an existing one left the FIRST
// in place: a second authentication stage that fails and "resets" identity to
// zero was fail-OPEN. That is the one hole in the absence-never-looks-like-
// presence story, and it is the direction that matters.
func TestWithoutIdentityClearsIt(t *testing.T) {
	t.Parallel()

	ctx := app.WithIdentity(context.Background(), app.Identity{Subject: "alice"})
	if _, ok := app.IdentityFromContext(ctx); !ok {
		t.Fatal("setup: alice is not present")
	}

	cleared := app.WithoutIdentity(ctx)
	if got, ok := app.IdentityFromContext(cleared); ok {
		t.Errorf("WithoutIdentity left %q on the context", got.Subject)
	}
	if err := app.RequireAuthenticated().Authorize(cleared); err == nil {
		t.Error("a cleared context still authenticates")
	}
	// The original is untouched — contexts are values.
	if _, ok := app.IdentityFromContext(ctx); !ok {
		t.Error("WithoutIdentity mutated the parent context")
	}
}

// TestAuthorizedRejectsATypedNilPolicy — field test #6. Authorized catches a
// nil interface but not a non-nil interface holding a nil pointer, so such a
// policy ALLOWED every request. identity.go's own doc cites this exact trap
// as why WithTelemetry carries a reflect probe; Authorized did not have one.
func TestAuthorizedRejectsATypedNilPolicy(t *testing.T) {
	t.Parallel()

	// A nil POINTER in a non-nil interface. staticcheck will tell you
	// `policy == nil` is never true here — that is the trap stated as a
	// compiler-adjacent fact: the plain nil check every guard writes cannot
	// see this value, and the policy underneath allows everything.
	var typed *allowAll
	var policy app.AuthorizationPolicy = typed

	defer func() {
		if recover() == nil {
			t.Error("Authorized accepted a typed-nil policy — every request would be allowed")
		}
	}()
	_ = app.Authorized[string, string](policy)
}

// sprintfS exercises the %s verb through a variable, so staticcheck does not
// rewrite the very call under test into the String() it is meant to reach.
func sprintfS(v any) string { return fmt.Sprintf("%s", v) }

type allowAll struct{}

func (*allowAll) Authorize(context.Context) error { return nil }

// TestDeniedResponseDoesNotNameTheScope — field test #6, defect B7. The body
// read "not allowed to scope docs:admin", handing a caller who just failed
// authorization the exact scope to go and obtain — while LogValue three
// methods up redacts scope names because one can carry a tenant or a
// resource id.
func TestDeniedResponseDoesNotNameTheScope(t *testing.T) {
	t.Parallel()

	ctx := app.WithIdentity(context.Background(), app.Identity{Subject: "u-1", Scopes: []string{"docs:read"}})
	err := app.RequireScope("docs:admin", "tenant:acme").Authorize(ctx)
	if err == nil {
		t.Fatal("a caller without the scope was allowed")
	}
	for _, leak := range []string{"docs:admin", "tenant:acme"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the denial names a scope the caller does not hold (%q): %v", leak, err)
		}
	}
}

// TestClaimOnJSONNumbers pins the sharp edge the doc now warns about: every
// JSON number arrives as float64, so Claim[int] never works — and failing
// silently is exactly why it needs a test as well as a sentence.
func TestClaimOnJSONNumbers(t *testing.T) {
	t.Parallel()

	// What encoding/json produces for {"exp": 1735689600, "roles": ["a"]}.
	id := app.Identity{Subject: "u-1", Claims: map[string]any{
		"exp":   float64(1735689600),
		"roles": []any{"a", "b"},
	}}

	if _, ok := app.Claim[int](id, "exp"); ok {
		t.Error("Claim[int] on a JSON number succeeded — the doc says it never can")
	}
	if v, ok := app.Claim[float64](id, "exp"); !ok || v != 1735689600 {
		t.Errorf("Claim[float64] = %v, %v, want the value", v, ok)
	}
	if _, ok := app.Claim[[]string](id, "roles"); ok {
		t.Error("Claim[[]string] on a JSON array succeeded — it is []any")
	}
	if v, ok := app.Claim[[]any](id, "roles"); !ok || len(v) != 2 {
		t.Errorf("Claim[[]any] = %v, %v", v, ok)
	}
}

// --- resilience ruling, 2026-08-05 -----------------------------------------

// TestTimeoutBoundsEachAttemptWhenInsideRetrying and its sibling below are
// the pin that makes the dropped spec's open question 4 — "does Timeout bound
// the attempt or the sequence?" — permanently answered rather than merely
// written down. The answer is Chain's argument order, and the two orderings
// produce materially different systems.
func TestTimeoutBoundsEachAttemptWhenInsideRetrying(t *testing.T) {
	t.Parallel()

	var deadlines []time.Time
	h := app.HandlerFunc[string, string](func(ctx context.Context, _ string) (string, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("no deadline reached the handler")
		}
		deadlines = append(deadlines, d)
		return "", werrors.Unavailable("downstream", stderrors.New("nope"))
	})

	// Timeout INSIDE Retrying: composed last, so it wraps the handler and
	// each attempt gets its own budget.
	chained := app.Chain(h,
		app.Retrying[string, string](broker.ExponentialBackoff(3)),
		app.Timeout[string, string](time.Hour),
	)
	_, _ = chained.Handle(context.Background(), "x")

	if len(deadlines) < 2 {
		t.Fatalf("handler ran %d times, want the retries", len(deadlines))
	}
	// A fresh budget per attempt means the deadlines MOVE.
	if deadlines[0].Equal(deadlines[len(deadlines)-1]) {
		t.Error("every attempt shared one deadline; Timeout inside Retrying must bound each attempt")
	}
}

func TestTimeoutBoundsTheSequenceWhenOutsideRetrying(t *testing.T) {
	t.Parallel()

	var deadlines []time.Time
	h := app.HandlerFunc[string, string](func(ctx context.Context, _ string) (string, error) {
		d, _ := ctx.Deadline()
		deadlines = append(deadlines, d)
		return "", werrors.Unavailable("downstream", stderrors.New("nope"))
	})

	// Timeout OUTSIDE Retrying: composed first, so one budget covers them all.
	chained := app.Chain(h,
		app.Timeout[string, string](time.Hour),
		app.Retrying[string, string](broker.ExponentialBackoff(3)),
	)
	_, _ = chained.Handle(context.Background(), "x")

	if len(deadlines) < 2 {
		t.Fatalf("handler ran %d times, want the retries", len(deadlines))
	}
	if !deadlines[0].Equal(deadlines[len(deadlines)-1]) {
		t.Error("attempts got different deadlines; Timeout outside Retrying must bound the whole sequence")
	}
}

// TestTimeoutRespectsAShorterCallerDeadline — the caller knows more. A
// request context already bounded at 100ms must not be extended to an hour.
func TestTimeoutRespectsAShorterCallerDeadline(t *testing.T) {
	t.Parallel()

	caller, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var got time.Time
	h := app.HandlerFunc[string, string](func(ctx context.Context, _ string) (string, error) {
		got, _ = ctx.Deadline()
		return "ok", nil
	})
	if _, err := app.Timeout[string, string](time.Hour)(h).Handle(caller, "x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if time.Until(got) > time.Minute {
		t.Errorf("the caller's 50ms deadline was extended to %v", time.Until(got))
	}
}

// TestTimeoutIsAPassThroughOnSuccess — it must not change what a handler
// returns, and it must not allocate its way onto the request path.
func TestTimeoutIsAPassThroughOnSuccess(t *testing.T) {
	t.Parallel()

	h := app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
		return "value", nil
	})
	got, err := app.Timeout[string, string](time.Hour)(h).Handle(context.Background(), "x")
	if err != nil || got != "value" {
		t.Errorf("Handle = %q, %v, want the handler's own result", got, err)
	}
}

// TestTimeoutDoesNotInterruptAHandlerThatIgnoresContext pins the honesty the
// doc claims: a deadline is a signal, not a kill. A handler that never looks
// at ctx runs to completion, exactly as transport/http's ShutdownTimeout
// already documents for the drain. No sleep — a channel handshake.
func TestTimeoutDoesNotInterruptAHandlerThatIgnoresContext(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	finished := make(chan struct{})
	h := app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
		<-release // ignores ctx entirely
		close(finished)
		return "done", nil
	})

	done := make(chan string, 1)
	go func() {
		v, _ := app.Timeout[string, string](time.Nanosecond)(h).Handle(context.Background(), "x")
		done <- v
	}()

	close(release)
	<-finished
	if v := <-done; v != "done" {
		t.Errorf("Handle = %q; Timeout must not interrupt a handler that ignores its context", v)
	}
}
