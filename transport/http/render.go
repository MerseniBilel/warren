package http

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/log"
	"github.com/MerseniBilel/warren/transport"
)

// statusFor is the HTTP column of the error table (warren.md §2.6). It is the
// only place in an HTTP service where a semantic code becomes a status, which
// is what lets domain code return errors.Conflict(...) and never import
// net/http.
func statusFor(code errors.Code) int {
	switch code {
	case errors.CodeInvalid:
		return http.StatusBadRequest
	case errors.CodeUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case errors.CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case errors.CodeNotFound:
		return http.StatusNotFound
	case errors.CodeConflict, errors.CodeContention:
		// Both 409: the caller's write collided with current state. What
		// differs is whether trying again can help, and a client that needs
		// to know reads the code in the body — which is what the code field
		// is on the wire for.
		return http.StatusConflict
	case errors.CodeUnauthenticated:
		return http.StatusUnauthorized
	case errors.CodePermissionDenied:
		return http.StatusForbidden
	case errors.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// errorBody is the wire shape. code is errors.Code verbatim — a closed set of
// eight values, so a client can switch on it — and it is deliberately not RFC
// 9457 problem+json, whose "type" is a URI that errors.Code is not.
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Details       map[string]any `json:"details,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

// WriteError renders err as Warren's error envelope, with the status its
// warren/errors code maps to in warren.md §2.6.
//
// It is exported for EDGE MIDDLEWARE — an authenticator that rejects a forged
// token never reaches a route, so nothing in the framework would otherwise
// render its refusal. Every such middleware was hand-copying this envelope
// out of a golden file, and a field test caught two copies already differing
// in key order. One exported function is the difference between a shape
// Warren owns and a shape every user re-derives:
//
//	func (m authMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
//	    id, err := m.verify(r)
//	    if err != nil {
//	        whttp.WriteError(w, r, errors.Unauthenticated("invalid credential"))
//	        return
//	    }
//	    m.next.ServeHTTP(w, r.WithContext(app.WithIdentity(r.Context(), id)))
//	}
//
// INTERNAL never renders the message and never renders the wrapped cause: a
// fixed "internal error" plus the correlation ID goes to the client, and the
// real thing goes to the log at ERROR. Anything else leaks DSNs and SQL
// statements to the internet — which is exactly the reason a hand-copied
// envelope is a bad idea, since it is the half people leave out.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, err)
}

// writeError renders err through the table above.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	var werr *errors.Error
	code := errors.CodeOf(err)
	if e, ok := asWarrenError(err); ok {
		werr = e
	}

	body := errorPayload{Code: string(code), CorrelationID: log.CorrelationID(ctx)}
	if code == errors.CodeInternal {
		body.Message = "internal error"
		// ErrorContext: see the note in edge.go's recoverer. The correlation
		// ID is in the response body, and it must be on this line too or the
		// two cannot be joined.
		log.FromContext(ctx).ErrorContext(ctx, "request failed", "error", err.Error(), "path", r.URL.Path)
	} else {
		body.Message = werr.Message()
		body.Details = werr.Details()
	}
	if code == errors.CodePermissionDenied || code == errors.CodeUnauthenticated {
		// A denial says as little as possible on the wire, deliberately: the
		// response must not describe the authorization model to a caller who
		// just failed it. That leaves the operator with nothing either — and
		// a scope typo, the most common auth bug there is, then has no
		// diagnosis at all.
		//
		// So the full reason goes to the LOG, at DEBUG, correlated with the
		// response by the same id the body carries. Off in production by
		// default, on when someone is actually debugging, and never on the
		// wire.
		log.FromContext(ctx).DebugContext(ctx, "request denied",
			"code", string(code), "reason", err.Error(), "path", r.URL.Path, "method", r.Method)
	}
	if code == errors.CodeContention {
		// CONTENTION and CONFLICT share 409 deliberately (see statusFor), and
		// the code field is how a client tells them apart. But 409 by itself
		// says "do not retry" in RFC terms, and CONTENTION is precisely the
		// retryable one — app.Retrying retries it, and RetryingOn(p,
		// CodeConflict) is a boot panic to keep the two from being confused.
		//
		// Retry-After carries that distinction to clients that read headers
		// rather than bodies, without moving the status. 0 means "at once":
		// a contended write lost a race, and the next attempt is not rate
		// limited.
		w.Header().Set("Retry-After", "0")
	}
	writeJSON(w, statusFor(code), errorBody{Error: body})
}

// asWarrenError finds the *errors.Error anywhere in the chain. A handler that
// wrapped one with %w still maps to the right status.
func asWarrenError(err error) (*errors.Error, bool) {
	var e *errors.Error
	if stderrors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Encoding the error envelope itself failed; there is nothing left to
		// tell the client but the status.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header()[contentType] = jsonContentType
	w.WriteHeader(status)
	// The trailing newline is what encoding/json.Encoder writes and what
	// every line-oriented tool expects. Without it a curl body runs straight
	// into the next shell prompt, which is felt on every hand-driven request
	// during development — the framework's most-used debugging tool.
	_, _ = w.Write(append(buf, '\n'))
}

// Canonical key and a shared value, assigned straight into the header map:
// see the note in typed.
const contentType = "Content-Type"

var jsonContentType = []string{"application/json; charset=utf-8"}

// bodyPool is why the read is a pooled bytes.Buffer and not
// json.NewDecoder(r.Body) or io.ReadAll: measured, the decoder costs 9
// allocations and 968 B and io.ReadAll costs 2 and 544 B, against 7 and 272 B
// for this. Do not "simplify" it back.
var bodyPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// mediaMatches reports whether a request's Content-Type names want, ignoring
// parameters and case: "application/json; charset=utf-8" matches
// "application/json".
//
// mime.ParseMediaType is not used, deliberately — it allocates a parameter
// map on every request, and this runs on the hot path to compare against one
// fixed string. A malformed header simply does not match, which is the answer
// a parse error would produce anyway.
func mediaMatches(got, want string) bool {
	if i := strings.IndexByte(got, ';'); i >= 0 {
		got = got[:i]
	}
	return strings.EqualFold(strings.TrimSpace(got), want)
}

// typed serves one registered route: guards, then core's pre-built closure —
// decode, bind params, validate, core middleware, handler, encode — then the
// success status. Everything except the body read happens inside the closure
// the Builder froze at boot.
func (s *server) typed(rt transport.HTTPRoute) http.Handler {
	// Bound once, at boot: the codec is frozen into the closure like
	// everything else on this path, so choosing it costs nothing per request.
	invoke := rt.Bind(s.cfg.codec)
	guards := rt.Guards
	success := rt.Success
	limit := s.cfg.maxBodyBytes
	media := s.cfg.codec.Name()
	checkMedia := !s.cfg.anyContentType

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// params is pointer-shaped, so putting it in the interface costs no
		// allocation; the context value costs one.
		ctx := transport.WithParams(r.Context(), params{r: r})

		for _, g := range guards {
			if err := g.Authorize(ctx); err != nil {
				writeError(w, r, err)
				return
			}
		}

		// A body has to be the media type this route decodes. Until this
		// check existed, a form-encoded or text/plain POST was decoded as
		// JSON — which fails, leaving every field zero, and then answered
		// 201. Those two content types plus multipart are exactly the set a
		// browser can send CROSS-ORIGIN with no preflight, so an HTML form
		// on any site could reach a JSON API and have it report success.
		//
		// An ABSENT Content-Type is allowed: a browser always sets one, so
		// omitting it is not reachable from the simple-request shape this
		// closes, and refusing it would break every curl and script that
		// posts a body without the header.
		if ct := r.Header.Get(contentType); checkMedia && ct != "" && !mediaMatches(ct, media) {
			writeJSON(w, http.StatusUnsupportedMediaType, errorBody{Error: errorPayload{
				Code:          string(errors.CodeUnsupportedMedia),
				Message:       "unsupported media type " + ct + "; this route accepts " + media,
				CorrelationID: log.CorrelationID(ctx),
			}})
			return
		}

		buf := bodyPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bodyPool.Put(buf)
		if r.Body != nil {
			if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, limit)); err != nil {
				var tooBig *http.MaxBytesError
				if stderrors.As(err, &tooBig) {
					// 413, not the table's 400. MaxBodyBytes is a TRANSPORT
					// limit, not a domain verdict — the error table maps
					// semantic codes, and refusing a payload before anything
					// semantic has happened is not one of them. A 400 here
					// was also indistinguishable from malformed JSON, which
					// made the option unobservable to a client.
					writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: errorPayload{
						Code: string(errors.CodeInvalid),
						Message: "request body exceeds the " +
							strconv.FormatInt(tooBig.Limit, 10) + "-byte limit",
						CorrelationID: log.CorrelationID(ctx),
					}})
					return
				}
				writeError(w, r, errors.Invalid("body", err))
				return
			}
		}

		out, err := invoke(ctx, buf.Bytes())
		if err != nil {
			writeError(w, r, err)
			return
		}
		if success == http.StatusNoContent {
			// A 204 writes no body and no Content-Type.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Assigning the map entry directly, with a package-level value,
		// skips both the key canonicalisation and the one-element slice
		// Header.Set allocates — 2 allocations on every response. net/http
		// does the same thing for its own headers. Content-Length is left to
		// net/http, which computes it for a response it can buffer.
		w.Header()[contentType] = jsonContentType
		w.WriteHeader(success)
		// Trailing newline, for the same reason writeJSON adds one: every
		// body this framework emits ends with it, so a curl during
		// development does not run into the shell prompt. Consistency across
		// the success and error paths is the point — a client that trims one
		// and not the other is the bug this avoids.
		_, _ = w.Write(append(out, '\n'))
	})
}

// raw serves an escape-hatch route: the edge ring and the route's guards, and
// then the handler, which owns its own body reading, status and encoding.
func (s *server) raw(rr transport.RawRoute, h http.Handler) http.Handler {
	if len(rr.Guards) == 0 {
		return h
	}
	guards := rr.Guards
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Params are seeded for the GUARDS even though a raw route binds
		// nothing into a request struct — "no parameter binding" is about the
		// handler, not about the policy.
		//
		// Without this, the same policy on the same URL shape behaved
		// differently on a raw route: ParamsFromContext returned nil, so a
		// tenant check had nothing to compare. Whoever wrote the policy chose
		// which way that failed, and the natural reading — no param, nothing
		// to compare, not applicable, allow — is a silent cross-tenant bypass
		// on raw routes only. The asymmetry is the bug; the seeding is one
		// allocation on a path that has already decided to be hand-written.
		ctx := transport.WithParams(r.Context(), params{r: r})
		for _, g := range guards {
			if err := g.Authorize(ctx); err != nil {
				writeError(w, r.WithContext(ctx), err)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// methodNotAllowed renders the JSON 405 envelope for a path that exists under
// another verb. Registering this shim costs ServeMux's own free Allow header,
// so the value is computed at boot and written here.
func (s *server) methodNotAllowed(allow string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		// METHOD_NOT_ALLOWED, and emphatically not NOT_FOUND. The path EXISTS
		// — the Allow header above lists the verbs it answers — so telling
		// the client the resource was not found is a lie it acts on: a client
		// switching on error.code concludes the thing is gone and stops
		// asking.
		//
		// It was INVALID until 2026-08-29, on the reasoning that §2.6's table
		// maps the codes HANDLERS raise and a 405 is decided by the adapter
		// before any handler runs, so INVALID was "the honest one of those
		// codes". That was the wrong conclusion from a correct premise: the
		// answer to a code the table lacked was to add it, not to overload
		// one whose meaning is "change the request before retrying". A client
		// could not tell a wrong METHOD from a malformed body, which have
		// nothing in common as remedies — the same defect the 415 carried.
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorPayload{
			Code:          string(errors.CodeMethodNotAllowed),
			Message:       r.Method + " is not allowed on " + r.URL.Path,
			CorrelationID: log.CorrelationID(r.Context()),
		}})
	})
}

// notFound is the catch-all. It also answers a wrong method on "/" itself,
// whose pattern is this one, which is why it carries rootAllow.
// probeMethods is the set the catch-all asks the mux about when deciding
// between 404 and 405. It is the standard request methods: Registrar emits
// five of them, and a raw route or a Handle option may name any.
var probeMethods = [...]string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
	http.MethodConnect, http.MethodTrace,
}

// notFoundOrMethodNotAllowed serves everything the mux did not match: a JSON
// 404 envelope instead of net/http's bare text, or a 405 with Allow when the
// PATH exists and only the method is wrong.
//
// Allow is derived by asking the mux itself — for each standard method, would
// this exact path have matched a real pattern? Anything that answers with a
// pattern other than the catch-all's own "/" is a method this path serves.
//
// That replaces a per-path method-less registration whose only job was to
// render this envelope, and which made a literal segment beside a sibling
// wildcard unregisterable (see build). Three properties come out of asking
// the mux rather than keeping a parallel table:
//
//   - Allow cannot drift from the routes, because it IS the routes.
//   - HEAD is included wherever GET is, because ServeMux serves HEAD from a
//     GET pattern. The old table had to remember to add it.
//   - A path served only through a wildcard reports the wildcard's methods,
//     which is what the request would actually have reached.
//
// It runs only on the miss path, never on a matched route, and it is at most
// nine pure lookups.
func (s *server) notFoundOrMethodNotAllowed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, m := range probeMethods {
			// A shallow copy: Handler reads Method, URL and Host and mutates
			// nothing, so cloning the whole request would be waste on a path
			// that is already an error.
			probe := *r
			probe.Method = m
			if _, pattern := s.mux.Handler(&probe); pattern != "" && pattern != "/" {
				allowed = append(allowed, m)
			}
		}
		if len(allowed) > 0 {
			s.methodNotAllowed(allowHeader(allowed)).ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorPayload{
			Code:          string(errors.CodeNotFound),
			Message:       "no route for " + r.Method + " " + r.URL.Path,
			CorrelationID: log.CorrelationID(r.Context()),
		}})
	})
}

// probe serves a health verdict. It is not a route: it bypasses the edge ring
// so that a probe every two seconds is not a span, an audit line and a rate
// limiter decision.
func probe(verdict func(context.Context) health.Report) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := verdict(r.Context())
		status := http.StatusOK
		if rep.Status != health.StatusUp {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, rep)
	})
}
