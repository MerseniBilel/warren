package http

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync/atomic"

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
			log.FromContext(ctx).Error("handler panicked",
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

// telemetry continues the CALLER's trace: it reads the W3C traceparent off
// the request through the core seam and seeds the context, so the handler's
// span is a child of the caller's rather than the root of an orphan trace.
//
// It is composed into the edge ring only when a Telemetry is bound at boot.
// An uninstrumented service has no such stage and pays nothing — not a nil
// check, not a closure.
func (s *server) telemetry(h http.Handler) http.Handler {
	tel := s.tel
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := tel.Extract(r.Context(), r.Header.Get)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
