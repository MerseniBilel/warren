package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"unicode"

	"github.com/MerseniBilel/warren/errors"
)

// Identity is the authenticated caller, as core can express it: no token, no
// header, no driver type. The edge ring produces it — an authentication
// middleware, or warren/auth in v0.2 — and AuthorizationPolicy and the
// handler consume it.
//
// It is a STRUCT and not an interface, and that is the same decision as the
// ok-bool on IdentityFromContext: absence must not be able to look like
// presence. An interface would reintroduce the typed-nil trap WithTelemetry
// already defends against with a reflect probe — a non-nil interface holding
// a nil pointer reads as identity PRESENT. It would also put a hand-written
// fake in every user's test package, forever, to express "the caller is u-1".
// Identity is DATA, like RequestInfo; Telemetry and AuthorizationPolicy are
// BEHAVIOUR, and that is where interfaces belong.
//
// Nor is it Identity[T]. Each instantiation would get its own context key, so
// a guard seeding one T and a handler reading another would silently see no
// identity at all — and AuthorizationPolicy.Authorize(ctx) error is not
// generic, so a generic identity could only be read by type-switching at
// request time, which is reflective dispatch on the request path. Claims and
// the generic free function Claim[T] serve app-defined claims instead.
//
// A test writes:
//
//	ctx := app.WithIdentity(context.Background(),
//	    app.Identity{Subject: "u-1", Scopes: []string{"orders:write"}})
type Identity struct {
	// Subject is the principal: "sub", a service account name, a user ID.
	// An Identity without one is not an identity — see WithIdentity.
	Subject string

	// Issuer is who vouched for it — "iss", or "" when nobody external did.
	Issuer string

	// Scopes is the OAuth2 "scope" claim, already split. It is a field rather
	// than a Claims entry because it is the one claim OAuth2 and OIDC spell
	// identically, and it is what lets RequireScope live in core without core
	// parsing a token.
	Scopes []string

	// Claims is everything else, verbatim, and may be nil.
	//
	// It is READ-ONLY once seeded, and carried by reference rather than
	// copied: copying per request would cost an allocation proportional to
	// the token, and this map is the verifier's output, not the handler's
	// working state.
	//
	// It is also the reason there is no Audience, ExpiresAt or Tenant field.
	// Expiry and audience are the VERIFIER's business — an identity on the
	// context is by definition already verified, and a field for expiry
	// invites handlers to re-check it badly. Anything v0.2 proves it needs
	// gets promoted then, and promotion is additive.
	Claims map[string]any
}

// HasScope reports whether the identity carries scope.
func (id Identity) HasScope(scope string) bool {
	return slices.Contains(id.Scopes, scope)
}

// LogValue renders the identity for slog WITHOUT its claims.
//
// It is not decoration. Without it, one
//
//	log.FromContext(ctx).InfoContext(ctx, "handled", "identity", id)
//
// dumps the claims map — emails, phone numbers, sometimes a nested token —
// into every log line that touches it. slog.LogValuer is the standard
// library's redaction seam, and six lines here prevent a data-protection
// incident that no amount of documentation prevents.
//
// The scope COUNT is included rather than the scopes: it is the part useful
// in an audit line, and a scope name can carry a tenant or a resource id.
func (id Identity) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("subject", id.Subject),
		slog.String("issuer", id.Issuer),
		slog.Int("scopes", len(id.Scopes)),
	)
}

// String renders the identity for fmt WITHOUT its claims.
//
// LogValue covers slog's top-level attr value and nothing else: slog does not
// call LogValuer on a NESTED value, and fmt never calls it at all. So an
// identity logged through any of these leaked the whole claims map —
//
//	fmt.Errorf("denied for %v", id)      // the 2 a.m. line
//	slog.Any("w", struct{ ID Identity }{id})
//	fmt.Sprintf("%+v", []Identity{id})
//
// — and the first of those can wrap into errors.Internal, which reaches a log
// AND a response body. One String method closes the entire fmt family,
// including %v, %s, %+v and every container that formats its elements.
//
// It still names the caller, deliberately: a redaction that says nothing gets
// replaced by someone printing the fields, which leaks again.
func (id Identity) String() string {
	return "Identity(" + id.Subject + "@" + id.Issuer + ")"
}

var (
	_ slog.LogValuer = Identity{}
	_ fmt.Stringer   = Identity{}
)

type identityKey struct{}

// WithIdentity returns a copy of ctx carrying id. An authentication
// middleware calls it at the edge; a test calls it directly.
//
// An Identity whose Subject is empty is NOT carried, and the context is
// returned unchanged — the same refusal WithTelemetry makes for a nil
// Telemetry. It is the second of the three mechanisms that stop "no identity"
// reading as "identity with an empty subject": a guard bug that produces an
// empty subject makes the request read as unauthenticated, which is
// fail-closed, instead of authenticated-as-nobody, which files rows owned
// by "".
//
// It costs 2 allocations — the interface box and the context node, 112 B
// measured on go1.26.3/darwin-arm64 — charged to the application's own edge
// middleware and only on requests that actually carry a credential. Warren
// installs no identity middleware of its own, which is what keeps
// transport/http's committed request budget where it is.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	if blank(id.Subject) {
		return ctx
	}
	return context.WithValue(ctx, identityKey{}, id)
}

// blank reports whether a subject is empty once whitespace and the invisible
// characters that read as empty are removed.
//
// Comparing against "" alone was not enough: " ", "\t", "\x00", a byte-order
// mark and a zero-width space were all carried, and RequireAuthenticated
// allowed every one of them. A user who trusts the documented refusal would
// file audit rows owned by "\x00". It costs one pass over a short string, on
// the credential path only.
func blank(subject string) bool {
	for _, r := range subject {
		switch r {
		case '\x00', '\ufeff', '\u200b', '\u200c', '\u200d':
			continue // invisible: reads as empty to every human and every log
		}
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// WithoutIdentity returns a copy of ctx carrying NO identity, whatever it
// carried before.
//
// Seeding a zero Identity does not do this — WithIdentity refuses a blank
// subject and returns the context unchanged, so the PREVIOUS identity stays.
// A second authentication stage that failed and "reset" identity to the zero
// value was therefore fail-OPEN: the first stage's caller survived. This is
// the explicit way to say no-one, and it is the direction that matters.
func WithoutIdentity(ctx context.Context) context.Context {
	if _, ok := IdentityFromContext(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, identityKey{}, Identity{})
}

// IdentityFromContext returns the caller's identity and whether there is one.
//
// The ok-bool is the first and strongest of the three anti-ambiguity
// mechanisms, because it is enforced by the compiler rather than by review:
//
//	app.IdentityFromContext(ctx).Subject   // does not compile
//
// A single-return accessor would make that line legal, and it writes rows
// owned by "". This is a deliberate asymmetry with log.CorrelationID, which
// does return a bare string — an absent correlation ID is cosmetic, one log
// line harder to join, while an absent identity is a security decision.
// transport.Params.Path already returns an ok-bool for the same reason.
//
// Reading costs 0 allocations, present or absent.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	if !ok || blank(id.Subject) {
		// The blank case is WithoutIdentity's marker. Checking it here rather
		// than trusting the seeding path means a value that reached the
		// context by any route still cannot present as an authenticated
		// caller with no principal.
		return Identity{}, false
	}
	return id, ok
}

// Claim reads a typed claim out of an identity's Claims map.
//
// It is a generic FREE FUNCTION rather than a method because Go 1.26 has no
// generic methods, and a generic Identity[T] would give every instantiation
// its own context key. It never panics: a missing key, a wrong type and a nil
// map all return the zero value and false, because this runs on the request
// path over a map some verifier produced.
//
// JSON NUMBERS DECODE AS float64. This is the wrong type every JWT user
// reaches for first, and it fails silently:
//
//	Claim[int](id, "exp")      // (0, false) — always, for every token
//	Claim[float64](id, "exp")  // (1735689600, true)
//
// A JSON array is []any, not []string, for the same reason — use Scopes for
// scopes, which is already split. And if your verifier is configured to
// decode numbers as json.Number instead, every Claim[float64] in your
// codebase flips to false at once; pick one and keep it.
//
// An explicit JSON null is indistinguishable from an absent claim: both are
// (zero, false). That is the safe direction — a null claim carries no
// information a handler should act on.
func Claim[T any](id Identity, name string) (T, bool) {
	v, ok := id.Claims[name].(T)
	return v, ok
}

// RequireAuthenticated returns the policy that demands any identity at all.
//
// Attach it per route with transport.Guard, which runs BEFORE decode — so an
// unauthenticated caller's malformed body is a 401, not a 400.
func RequireAuthenticated() AuthorizationPolicy { return scopePolicy{} }

// RequireScope returns the policy that demands an identity carrying every one
// of scopes.
//
// It NEVER merges the two denial codes, because they answer different
// questions and map to different statuses:
//
//	no identity at all         → UNAUTHENTICATED (401): prove who you are
//	an identity, wrong scope   → PERMISSION_DENIED (403): you may not do this
//
// Called with no scopes it is exactly RequireAuthenticated — an empty list
// demands nothing extra, and must not silently allow an anonymous caller,
// which is the shape a variadic helper like this usually fails in.
//
// It is written entirely in core types with no dependency, which is what
// makes it the proof that AuthorizationPolicy is usable before warren/auth
// exists.
func RequireScope(scopes ...string) AuthorizationPolicy {
	return scopePolicy{scopes: scopes}
}

type scopePolicy struct{ scopes []string }

func (p scopePolicy) Authorize(ctx context.Context) error {
	id, ok := IdentityFromContext(ctx)
	if !ok {
		return errors.Unauthenticated("no caller identity on the request")
	}
	for _, want := range p.scopes {
		if !id.HasScope(want) {
			// The scope is NOT named. errors.PermissionDenied prefixes "not
			// allowed to ", so naming it would both read badly and hand an
			// unauthorized caller the exact string to go and obtain — while
			// three methods up, LogValue redacts scope names on the grounds
			// that one can carry a tenant or a resource id. The log line has
			// what an operator needs; the wire does not.
			return errors.PermissionDenied("this action")
		}
	}
	return nil
}
