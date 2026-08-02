package http

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"

	"strings"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// CorrelationHeader is the request header the correlation ID is read from and
// the response header it is written to. A caller that sets it keeps one ID
// across every service a request touches; a caller that does not gets a fresh
// one, and either way the ID is on the context before any user middleware
// runs.
//
// The spelling is net/http's canonical form — "Id", not "ID". Header lookup
// is case-insensitive, so a caller sending "X-Correlation-ID" still matches;
// what the canonical spelling buys is that Get and Set skip
// re-canonicalising the key on every request. Measured: 1 allocation.
const CorrelationHeader = "X-Correlation-Id"

// recoverer is the outermost edge middleware and is not removable. A panic in
// a handler becomes a 500 with the correlation ID, and the stack goes to the
// log — never to the client.
func (s *server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A client that goes away mid-write makes net/http panic with
			// ErrAbortHandler by design; it is not a bug and must not be
			// logged as one.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			ctx := r.Context()
			// ErrorContext, not Error. slog's non-Context methods pass
			// context.Background(), so a log.Handler resolving correlation
			// and trace IDs at emit time sees nothing — and the panic line
			// would be the one record in the service unjoinable to the
			// request that caused it, in the place that link is worth most.
			log.FromContext(ctx).ErrorContext(ctx, "handler panicked",
				"panic", rec,
				"path", r.URL.Path,
				"stack", string(debug.Stack()))
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorPayload{
				Code:          string(errors.CodeInternal),
				Message:       "internal error",
				CorrelationID: log.CorrelationID(ctx),
			}})
		}()
		h.ServeHTTP(w, r)
	})
}

// correlate seeds the correlation ID, so that log.CorrelationID(ctx) answers
// inside a handler and every error envelope carries it. It runs before user
// middleware, which is why Middleware cannot be moved outside it: middleware
// whose log lines have no correlation ID is middleware you cannot trace.
//
// It deliberately does NOT derive a per-request logger. log.With(ctx, ...)
// costs 8 allocations — measured, more than the entire decode-validate-
// handle-encode path — and it charges them to every request, including the
// ones that never log a line. warren.md §2.5 wants log.FromContext(ctx) to
// carry the correlation ID; the way to have that for free is a slog.Handler
// in warren/log that reads it off the context at Handle time, which is where
// slog is designed to do it. Recorded as an open question on this spec.
func (s *server) correlate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if id == "" {
			id = nextCorrelationID()
		}
		w.Header()[CorrelationHeader] = []string{id}
		h.ServeHTTP(w, r.WithContext(log.WithCorrelationID(r.Context(), id)))
	})
}

// correlation IDs are a process-unique random prefix plus a counter. That is
// two allocations per request against crypto/rand's four, it is unique across
// a fleet because the prefix is random per process, and it needs no
// dependency — a UUID library in every service's go.sum to label log lines is
// not a trade this framework makes.
var (
	idPrefix  = randomPrefix()
	idCounter atomic.Uint64
)

func randomPrefix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any supported platform; if it somehow
		// does, a fixed prefix still yields unique-per-process IDs.
		return "warren-"
	}
	return hex.EncodeToString(b[:]) + "-"
}

func nextCorrelationID() string {
	return idPrefix + strconv.FormatUint(idCounter.Add(1), 36)
}

// telemetry continues the CALLER's trace and opens the SERVER span.
//
// Continuation reads the W3C traceparent off the request through the core
// seam, so the handler's span is a child of the caller's rather than the root
// of an orphan trace.
//
// The SERVER span wraps EVERYTHING — decode, validation, the handler, encode,
// and the error rendering — which is what makes a trace answer the questions
// an incident actually asks: which route, which status, how much of the
// latency was transport. Opening it here rather than around the handler is
// also what makes a malformed body, a 404 and a panic visible at all: those
// never reach the handler, so a handler-only span shows nothing.
//
// route is the matched PATTERN, not the concrete path — "/users/{id}", whose
// cardinality is the size of the route table rather than of the traffic. It
// is resolved from Request.Pattern, which net/http sets in place, with no
// clone (Go 1.23).
//
// The whole stage is composed into the edge ring only when a Telemetry is
// bound at boot, and the span only when that Telemetry implements
// app.RequestSpan. An uninstrumented service has no stage at all — not a nil
// check, not a closure.
func (s *server) telemetry(h http.Handler) http.Handler {
	tel := s.tel
	spanner, _ := tel.(app.RequestSpan)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := tel.Extract(r.Context(), r.Header.Get)
		if spanner == nil {
			h.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		ctx, end := spanner.ServerSpan(ctx, app.RequestInfo{
			Protocol: "http",
			Method:   r.Method,
			Route:    routeOf(r),
			Path:     r.URL.Path,
			Scheme:   scheme,
			Host:     r.Host,
		})
		// The status is only knowable by watching the write, so the recorder
		// is allocated on the INSTRUMENTED path only.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r.WithContext(ctx))
		end(rec.status, nil)
	})
}

// routeOf returns the matched pattern with the method stripped — net/http
// reports "POST /users/{id}" and the method is already its own attribute.
// An unmatched request has no pattern, and "" is the honest answer: naming
// the concrete path there is how a 404 flood becomes a cardinality incident.
func routeOf(r *http.Request) string {
	p := r.Pattern
	if _, path, ok := strings.Cut(p, " "); ok {
		return path
	}
	return p
}

// statusRecorder observes the status code. It implements the optional
// interfaces net/http handlers reach for, because swallowing them would break
// SSE and WebSocket upgrade on a raw route the moment telemetry is switched
// on — a failure mode that would look like "tracing broke my streaming".
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := s.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errNotHijackable()
}
