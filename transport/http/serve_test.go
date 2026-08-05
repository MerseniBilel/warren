package http_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/health"
	wlog "github.com/MerseniBilel/warren/log"
	"github.com/MerseniBilel/warren/transport"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

// --- health probes --------------------------------------------------------

func TestLivenessIsAlwaysUpAndRunsNoChecks(t *testing.T) {
	t.Parallel()

	var ran bool
	m := warren.NewModule("checks",
		warren.Providers(func(reg health.Registry) *checkHolder {
			_ = reg.Register(health.NewCheck("db", func(context.Context) error {
				ran = true
				return errors.Unavailable("postgres", context.DeadlineExceeded)
			}))
			return &checkHolder{}
		}),
		warren.Eager[*checkHolder](),
	)
	base := serve(t, []warren.Module{m, userModule()})

	res, body := do(t, "GET", base+"/healthz", "")
	if res.StatusCode != 200 {
		t.Errorf("liveness = %d, want 200 — a database blip must not restart every replica", res.StatusCode)
	}
	if ran {
		t.Error("liveness ran a check; it must run none")
	}
	if !strings.Contains(body, `"status"`) {
		t.Errorf("body = %s", body)
	}
}

type checkHolder struct{}

func TestReadinessReportsAFailingCheck(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("checks",
		warren.Providers(func(reg health.Registry) *checkHolder {
			_ = reg.Register(health.NewCheck("db", func(context.Context) error {
				return errors.Unavailable("postgres", context.DeadlineExceeded)
			}))
			return &checkHolder{}
		}),
		warren.Eager[*checkHolder](),
	)
	base := serve(t, []warren.Module{m, userModule()})

	res, body := do(t, "GET", base+"/readyz", "")
	if res.StatusCode != 503 {
		t.Errorf("readiness = %d, want 503 when a critical check fails", res.StatusCode)
	}
	if !strings.Contains(body, "db") {
		t.Errorf("body = %s — /readyz must name the failing check", body)
	}
}

// The probes bypass the edge ring so that a probe every two seconds is not a
// span, an audit line and a rate-limiter decision.
func TestProbesBypassEdgeMiddleware(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	spy := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.Path)
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	base := serve(t, []warren.Module{userModule()}, whttp.Middleware(spy))

	do(t, "GET", base+"/healthz", "")
	do(t, "GET", base+"/readyz", "")
	do(t, "GET", base+"/users/u-1", "")

	mu.Lock()
	defer mu.Unlock()
	for _, p := range seen {
		if p == "/healthz" || p == "/readyz" {
			t.Errorf("edge middleware ran for %s", p)
		}
	}
	if len(seen) != 1 || seen[0] != "/users/u-1" {
		t.Errorf("middleware saw %v, want only the route", seen)
	}
}

// --- the edge ring --------------------------------------------------------

func TestCorrelationIDIsMintedAndEchoed(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, _ := do(t, "GET", base+"/users/u-1", "")
	if res.Header.Get(whttp.CorrelationHeader) == "" {
		t.Error("no correlation ID on the response")
	}
}

func TestCorrelationIDFromTheCallerIsKept(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	req, _ := http.NewRequest("GET", base+"/users/u-1", nil)
	// Sent in the spelling most callers use; header lookup is
	// case-insensitive, so it must still be found.
	req.Header.Set("X-Correlation-ID", "trace-me")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get(whttp.CorrelationHeader); got != "trace-me" {
		t.Errorf("correlation ID = %q, want the caller's — one ID across every service", got)
	}
}

func TestMiddlewareRunsInArgumentOrderAfterCorrelation(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var order []string
	mark := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}
	}
	corr := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			// User middleware must be able to see the correlation ID; that is
			// why it cannot be moved outside the seeding.
			if id := warrenCorrelationID(r); id == "" {
				order = append(order, "NO-ID")
			}
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	base := serve(t, []warren.Module{userModule()}, whttp.Middleware(mark("first"), mark("second"), corr))
	do(t, "GET", base+"/users/u-1", "")

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("middleware order = %v, want [first second]", order)
	}
}

// --- guards ---------------------------------------------------------------

type denyPolicy struct{}

func (denyPolicy) Authorize(context.Context) error { return errors.PermissionDenied("users:read") }

type guardedController struct{ reached bool }

func (c *guardedController) get(context.Context, getUser) (userDTO, error) {
	c.reached = true
	return userDTO{}, nil
}

func (c *guardedController) Register(r transport.Registrar) {
	transport.Get(r, "/guarded/{id}", app.HandlerFunc[getUser, userDTO](c.get),
		transport.Guard(denyPolicy{}))
}

func TestGuardRunsBeforeDecode(t *testing.T) {
	t.Parallel()

	c := &guardedController{}
	m := warren.NewModule("guarded", warren.Controllers(func() *guardedController { return c }))
	base := serve(t, []warren.Module{m})

	// A malformed body behind a guard is a 403, not a 400: unauthenticated
	// input never reaches the decoder.
	res, _ := do(t, "GET", base+"/guarded/u-1", `{not json`)
	if res.StatusCode != 403 {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if c.reached {
		t.Error("the handler ran behind a denying guard")
	}
}

// --- raw routes -----------------------------------------------------------

type uploader struct{ got string }

func (u *uploader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A raw handler owns its own body reading — the whole point of the
	// escape hatch.
	buf := make([]byte, 32)
	n, _ := r.Body.Read(buf)
	u.got = string(buf[:n])
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("stored"))
}

type rawController struct{ up *uploader }

func (c *rawController) Register(r transport.Registrar) {
	transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", c.up)
}

func TestRawRouteIsServedWithoutDecodeOrEncode(t *testing.T) {
	t.Parallel()

	up := &uploader{}
	m := warren.NewModule("files", warren.Controllers(func() *rawController { return &rawController{up: up} }))
	base := serve(t, []warren.Module{m})

	res, body := do(t, "POST", base+"/uploads", "not json at all")
	if res.StatusCode != 202 {
		t.Errorf("status = %d, want the handler's own 202", res.StatusCode)
	}
	if body != "stored" {
		t.Errorf("body = %q — a raw route encodes nothing", body)
	}
	if up.got != "not json at all" {
		t.Errorf("handler read %q", up.got)
	}
	if res.Header.Get(whttp.CorrelationHeader) == "" {
		t.Error("a raw route still gets the edge ring")
	}
}

type notAHandler struct{}

type badRawController struct{}

func (c *badRawController) Register(r transport.Registrar) {
	transport.Raw(r, transport.ProtocolHTTP, "POST /uploads", notAHandler{})
}

func TestRawRouteThatIsNotAnHTTPHandlerFailsTheBoot(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("files", warren.Controllers(func() *badRawController { return &badRawController{} }))
	a := warren.New(m, whttp.Server())
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("a raw route that is not an http.Handler must fail the boot")
	}
	for _, want := range []string{"POST /uploads", "notAHandler", "http.HandlerFunc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic must contain %q:\n%s", want, err)
		}
	}
}

// --- Handle ---------------------------------------------------------------

func TestHandleServesADependencyFreeHandler(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	base := serve(t, []warren.Module{userModule()}, whttp.Handle("GET /ping", h))

	res, body := do(t, "GET", base+"/ping", "")
	if res.StatusCode != 200 || body != "pong" {
		t.Errorf("status = %d, body = %q", res.StatusCode, body)
	}
}

// --- boot ------------------------------------------------------------------

func TestConflictingPatternsFailTheBootNotAPanic(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("conflict",
		warren.Controllers(func() *conflictController { return &conflictController{} }))
	a := warren.New(m, whttp.Server())

	// net/http.ServeMux PANICS on overlapping patterns. A panic reaching the
	// user is a bug; this must be a diagnostic.
	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("two wildcards on the same path booted")
	}
	if !strings.Contains(err.Error(), "conflicting HTTP route patterns") {
		t.Errorf("diagnostic:\n%s", err)
	}
}

type conflictController struct{}

// getByUID exists so the second route's `param:` tag matches its own
// wildcard. Reusing getUser (param:"id") on /users/{uid} makes THAT the
// first failure — correctly, since it would bind "" on every request — and
// this test is about the pattern conflict underneath it.
type getByUID struct {
	UID string `param:"uid"`
}

func (c *conflictController) Register(r transport.Registrar) {
	transport.Get(r, "/users/{id}", app.HandlerFunc[getUser, userDTO](
		func(context.Context, getUser) (userDTO, error) { return userDTO{}, nil }))
	transport.Get(r, "/users/{uid}", app.HandlerFunc[getByUID, userDTO](
		func(context.Context, getByUID) (userDTO, error) { return userDTO{}, nil }))
}

// --- shutdown --------------------------------------------------------------

type blockingController struct {
	entered chan struct{}
	release chan struct{}
}

func (c *blockingController) hold(ctx context.Context, _ getUser) (userDTO, error) {
	close(c.entered)
	<-c.release
	return userDTO{ID: "finished"}, nil
}

func (c *blockingController) Register(r transport.Registrar) {
	transport.Get(r, "/slow/{id}", app.HandlerFunc[getUser, userDTO](c.hold))
}

// The ordering warren.md §1.3 promises and most hand-rolled Go services get
// backwards: readiness closes FIRST, the load balancer is given DrainDelay to
// observe it, and only then does the listener stop — with in-flight requests
// still finishing.
func TestDrainFinishesInFlightRequestsAfterReadinessCloses(t *testing.T) {
	t.Parallel()

	c := &blockingController{entered: make(chan struct{}), release: make(chan struct{})}
	m := warren.NewModule("slow", warren.Controllers(func() *blockingController { return c }))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()
	a := warren.New(m, whttp.Server(whttp.Listener(ln), whttp.DrainDelay(2*time.Second)))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}

	// One request in flight, blocked inside the handler.
	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		res, err := http.Get(base + "/slow/u-1")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		buf := make([]byte, 64)
		n, _ := res.Body.Read(buf)
		done <- result{status: res.StatusCode, body: string(buf[:n])}
	}()
	<-c.entered

	stopped := make(chan error, 1)
	go func() { stopped <- a.Stop(context.Background()) }()

	// Readiness must close before the listener does.
	waitFor(t, "readiness to close", func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == 503
	})

	// And during DrainDelay the listener is still accepting — which is the
	// whole point: the balancer has not yet noticed the 503.
	res, err := http.Get(base + "/users/u-1")
	if err != nil {
		t.Fatalf("the listener stopped before DrainDelay elapsed: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 404 {
		// /users is not registered in this app; 404 through the envelope is
		// proof the server is still serving.
		t.Logf("status during drain = %d", res.StatusCode)
	}

	close(c.release)

	got := <-done
	if got.err != nil {
		t.Fatalf("the in-flight request was dropped: %v", got.err)
	}
	if got.status != 200 || !strings.Contains(got.body, "finished") {
		t.Errorf("in-flight request = %d %q, want the handler's own answer", got.status, got.body)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// waitFor polls until cond holds, with a deadline. Polling, not sleeping:
// the test never waits longer than the condition takes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// warrenCorrelationID reads the ID the edge ring seeded, the way a user's
// middleware would.
func warrenCorrelationID(r *http.Request) string {
	return wlog.CorrelationID(r.Context())
}

// --- regressions found by field-testing the framework, 2026-08-02 ----------

// A body over the limit used to render byte-for-byte identically to malformed
// JSON, which made MaxBodyBytes unobservable to a client.
func TestOversizedBodyIs413AndSaysSo(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()}, whttp.MaxBodyBytes(32))
	res, body := do(t, "POST", base+"/users", `{"email":"`+strings.Repeat("a", 200)+`"}`)

	if res.StatusCode != 413 {
		t.Errorf("status = %d, want 413 — a transport limit is not a domain verdict", res.StatusCode)
	}
	if !strings.Contains(body, "32-byte limit") {
		t.Errorf("body = %s — the message must name the limit, or it is\nindistinguishable from malformed JSON", body)
	}
}

// A wrong verb on a raw route reported 404, as though the path did not exist.
func TestRawRouteGetsA405WithAllow(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("files", warren.Controllers(func() *rawController { return &rawController{up: &uploader{}} }))
	base := serve(t, []warren.Module{m})

	res, _ := do(t, "DELETE", base+"/uploads", "")
	if res.StatusCode != 405 {
		t.Errorf("status = %d, want 405 — the path exists, the method does not", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// Handle's patterns carry a method too, so they contribute to Allow.
func TestHandlePatternContributesToAllow(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	base := serve(t, []warren.Module{userModule()}, whttp.Handle("GET /ping", h))

	res, _ := do(t, "POST", base+"/ping", "")
	if res.StatusCode != 405 {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

// --- telemetry -------------------------------------------------------------

// spanRecorder is an app.Telemetry that records the SERVER spans the edge
// opens, so the test can assert on route, status and kind without a collector.
type spanRecorder struct {
	mu     sync.Mutex
	server []serverSpan
}

type serverSpan struct {
	info   app.RequestInfo
	status int
}

func (s *spanRecorder) Span(ctx context.Context, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (s *spanRecorder) Record(string, time.Duration, error)             {}
func (s *spanRecorder) Inject(context.Context, func(key, value string)) {}
func (s *spanRecorder) Extract(ctx context.Context, get func(string) string) context.Context {
	if get("x-trace") != "" {
		return context.WithValue(ctx, traceKey{}, get("x-trace"))
	}
	return ctx
}

type traceKey struct{}

func (s *spanRecorder) ServerSpan(ctx context.Context, info app.RequestInfo) (context.Context, func(int, error)) {
	return ctx, func(status int, _ error) {
		s.mu.Lock()
		s.server = append(s.server, serverSpan{info: info, status: status})
		s.mu.Unlock()
	}
}

func (s *spanRecorder) seen() []serverSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]serverSpan(nil), s.server...)
}

// The SERVER span must carry the matched PATTERN, not the concrete path: its
// cardinality has to be bounded by the route table, not by traffic.
func TestServerSpanCarriesTheRouteNotThePath(t *testing.T) {
	t.Parallel()

	rec := &spanRecorder{}
	base := serveWithTelemetry(t, rec, userModule())
	do(t, "GET", base+"/users/u-42", "")

	spans := rec.seen()
	if len(spans) != 1 {
		t.Fatalf("server spans = %d, want 1", len(spans))
	}
	if spans[0].info.Route != "/users/{id}" {
		t.Errorf("route = %q, want the pattern — a concrete path is unbounded cardinality", spans[0].info.Route)
	}
	if spans[0].info.Path != "/users/u-42" {
		t.Errorf("path = %q", spans[0].info.Path)
	}
	if spans[0].info.Method != "GET" || spans[0].status != 200 {
		t.Errorf("method/status = %s/%d", spans[0].info.Method, spans[0].status)
	}
}

// The span wraps EVERYTHING, so requests that never reach the handler are
// visible: a malformed body, a 404, a validation failure. A handler-only span
// shows none of them, and "clients started getting 400s at 14:00" is then
// unanswerable from traces.
func TestServerSpanCoversRequestsThatNeverReachTheHandler(t *testing.T) {
	t.Parallel()

	rec := &spanRecorder{}
	base := serveWithTelemetry(t, rec, userModule())

	do(t, "POST", base+"/users", `{not json`)    // 400, decode fails
	do(t, "POST", base+"/users", `{"name":"x"}`) // 400, validation fails
	do(t, "GET", base+"/nope", "")               // 404, no route at all

	spans := rec.seen()
	if len(spans) != 3 {
		t.Fatalf("server spans = %d, want 3 — pre-handler failures must be traced", len(spans))
	}
	for _, s := range spans {
		if s.status != 400 && s.status != 404 {
			t.Errorf("status = %d, want the real response status", s.status)
		}
	}
	// The 404 is served by the catch-all pattern, so its route is "/" — one
	// bounded value, not the concrete path. That is the property that
	// matters: a flood of requests to random URLs produces ONE route label,
	// not one per URL, so a 404 storm cannot become a cardinality incident.
	for _, s := range spans {
		if s.status == 404 && s.info.Route == "/nope" {
			t.Errorf("the 404's span named the concrete path %q — a 404 flood would then explode cardinality", s.info.Route)
		}
	}
}

func TestServerSpanRecordsTheErrorStatus(t *testing.T) {
	t.Parallel()

	rec := &spanRecorder{}
	base := serveWithTelemetry(t, rec, userModule())
	do(t, "POST", base+"/fail", `{"code":"other"}`)

	spans := rec.seen()
	if len(spans) != 1 || spans[0].status != 500 {
		t.Fatalf("spans = %+v, want one 500", spans)
	}
}

// Probes bypass the edge ring, so they must not produce spans: one every two
// seconds is a telemetry bill, not a signal.
func TestProbesProduceNoSpans(t *testing.T) {
	t.Parallel()

	rec := &spanRecorder{}
	base := serveWithTelemetry(t, rec, userModule())
	do(t, "GET", base+"/healthz", "")
	do(t, "GET", base+"/readyz", "")

	if spans := rec.seen(); len(spans) != 0 {
		t.Errorf("probes produced %d spans", len(spans))
	}
}

func serveWithTelemetry(t *testing.T, tel app.Telemetry, modules ...warren.Module) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := warren.New(append(modules, whttp.Server(whttp.Listener(ln), whttp.DrainDelay(0)))...)
	if err := a.Telemetry(tel); err != nil {
		t.Fatalf("Telemetry: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return "http://" + ln.Addr().String()
}

// TestAPanicKeepsTheCorrelationID — a panic's 500 body and its "handler
// panicked" log record both carried NO correlation ID, while the response
// header did. The two places an operator actually looks were the only ones
// in the service that could not be joined to the request that caused them,
// at the moment that link is worth most.
//
// The cause was composition: recoverer(correlate(h)). correlate passes a
// NEW request down, so the recoverer's deferred func closes over the one
// whose context was never given an ID. Fixed the way broker.Pipeline
// already does it — Recover twice, inner and outer. The inner sees the
// correlated context; the outer still catches a panic in correlate itself.
//
// The log record is the assertion that bites. An earlier version of this
// test checked only the body, and passed against the broken code twice
// over: once because it sent a body that returns an ERROR rather than
// panicking — rendered inside correlate, so it always had the ID — and once
// because the body is not where this reliably shows.
func TestAPanicKeepsTheCorrelationID(t *testing.T) {
	// Not parallel: it swaps slog.Default to capture what a real main
	// installs.
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(wlog.Handler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(old) })

	// {"code":"PANIC"} is what makes the handler panic. Any other value
	// returns an ERROR instead, which is rendered inside correlate and so
	// always carried the ID — sending the wrong key is how an earlier
	// version of this test passed against the broken code.
	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "POST", base+"/fail", `{"code":"PANIC"}`)

	if res.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	id := res.Header.Get(whttp.CorrelationHeader)
	if id == "" {
		t.Fatal("no correlation header on a panicking request")
	}
	if !strings.Contains(body, id) {
		t.Errorf("the 500 body does not carry the correlation ID %q:\n%s\n\nAn operator handed this body has no way back to the log line.", id, body)
	}

	// The log record is what an operator greps. Give the write a moment:
	// it happens on the server's goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "handler panicked") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logged := buf.String()
	if !strings.Contains(logged, "handler panicked") {
		t.Fatal("the panic was never logged")
	}
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, "handler panicked") {
			continue
		}
		if !strings.Contains(line, id) {
			t.Errorf("the panic log record does not carry the correlation ID %q:\n%s", id, line)
		}
	}
}

// syncBuffer is a bytes.Buffer safe to write from the server goroutine and
// read from the test's.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// doWithHeaders is do with request headers — the identity seam is seeded
// from one, so the tests need to set it.
func doWithHeaders(t *testing.T, method, url, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return res, string(out)
}

// --- app.RequireScope over HTTP --------------------------------------------
//
// These are the first tests in the repository able to produce a 401 at all.
// Until app.RequireAuthenticated and app.RequireScope shipped, nothing could
// construct an UNAUTHENTICATED error, so statusFor's CodeUnauthenticated arm
// and warren.md §2.6's 401 row were unreachable — mapped, documented, and
// never exercised.

type scopedController struct{ reached bool }

func (c *scopedController) get(context.Context, getUser) (userDTO, error) {
	c.reached = true
	return userDTO{ID: "u-1"}, nil
}

func (c *scopedController) Register(r transport.Registrar) {
	transport.Get(r, "/scoped/{id}", app.HandlerFunc[getUser, userDTO](c.get),
		transport.Guard(app.RequireScope("users:read")))
}

// identityMiddleware is the fifteen lines a v0.1 user writes at the edge: it
// reads a header and seeds app.WithIdentity. In v0.2 warren/auth does this
// after verifying a signature; the seam does not change.
func identityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sub := r.Header.Get("X-Subject"); sub != "" {
			id := app.Identity{Subject: sub}
			if scope := r.Header.Get("X-Scope"); scope != "" {
				id.Scopes = []string{scope}
			}
			r = r.WithContext(app.WithIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

func TestRequireScopeMapsTo401And403(t *testing.T) {
	t.Parallel()

	c := &scopedController{}
	m := warren.NewModule("scoped", warren.Controllers(func() *scopedController { return c }))
	base := serve(t, []warren.Module{m}, whttp.Middleware(identityMiddleware))

	t.Run("no identity is 401", func(t *testing.T) {
		res, body := doWithHeaders(t, "GET", base+"/scoped/u-1", "", nil)
		if res.StatusCode != 401 {
			t.Errorf("status = %d, want 401 — the caller has not proved who they are\n%s", res.StatusCode, body)
		}
		if !strings.Contains(body, `"code":"UNAUTHENTICATED"`) {
			t.Errorf("body does not carry the code:\n%s", body)
		}
	})

	t.Run("wrong scope is 403", func(t *testing.T) {
		res, body := doWithHeaders(t, "GET", base+"/scoped/u-1", "",
			map[string]string{"X-Subject": "u-1", "X-Scope": "users:write"})
		if res.StatusCode != 403 {
			t.Errorf("status = %d, want 403 — a known caller who may not act\n%s", res.StatusCode, body)
		}
		if !strings.Contains(body, `"code":"PERMISSION_DENIED"`) {
			t.Errorf("body does not carry the code:\n%s", body)
		}
	})

	t.Run("the right scope is served", func(t *testing.T) {
		res, body := doWithHeaders(t, "GET", base+"/scoped/u-1", "",
			map[string]string{"X-Subject": "u-1", "X-Scope": "users:read"})
		if res.StatusCode != 200 {
			t.Errorf("status = %d, want 200\n%s", res.StatusCode, body)
		}
	})

	if c.reached != true {
		t.Error("the handler never ran, so the allowed case proved nothing")
	}
}

// TestGuardDeniesBeforeDecodeWithARealPolicy — transport.Guard's doc comment
// has always claimed an unauthenticated caller's malformed body is a 401 and
// not a 400. Nothing could prove it until a policy existed that produces
// UNAUTHENTICATED.
func TestGuardDeniesBeforeDecodeWithARealPolicy(t *testing.T) {
	t.Parallel()

	c := &scopedController{}
	m := warren.NewModule("scoped2", warren.Controllers(func() *scopedController { return c }))
	base := serve(t, []warren.Module{m}, whttp.Middleware(identityMiddleware))

	res, body := doWithHeaders(t, "GET", base+"/scoped/u-1", `{not json at all`, nil)
	if res.StatusCode != 401 {
		t.Errorf("a malformed body with no identity = %d, want 401 not 400 — the guard runs before decode\n%s",
			res.StatusCode, body)
	}
	if c.reached {
		t.Error("the handler ran behind a denying guard")
	}
}

// TestWriteErrorIsUsableFromEdgeMiddleware — field test #6's highest-value
// gap. An authenticator that rejects a forged token never reaches a route, so
// nothing in the framework rendered its refusal, and every edge middleware
// hand-copied the envelope out of a golden file. Two copies already differed
// in key order.
func TestWriteErrorIsUsableFromEdgeMiddleware(t *testing.T) {
	t.Parallel()

	reject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Subject") == "" {
				whttp.WriteError(w, r, errors.Unauthenticated("invalid or expired credential"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	c := &scopedController{}
	m := warren.NewModule("edgewrite", warren.Controllers(func() *scopedController { return c }))
	base := serve(t, []warren.Module{m}, whttp.Middleware(reject))

	res, body := doWithHeaders(t, "GET", base+"/scoped/u-1", "", nil)
	if res.StatusCode != 401 {
		t.Errorf("status = %d, want 401\n%s", res.StatusCode, body)
	}
	// The SAME envelope the framework writes: same keys, same order, same
	// correlation id — which is the entire point of exporting it.
	if !strings.Contains(body, `{"error":{"code":"UNAUTHENTICATED","message":"invalid or expired credential"`) {
		t.Errorf("the edge envelope is not the framework's:\n%s", body)
	}
	if !strings.Contains(body, `"correlation_id"`) {
		t.Errorf("the edge envelope carries no correlation id:\n%s", body)
	}
	if res.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("content type = %q", res.Header.Get("Content-Type"))
	}
}

// tenantGuard allows only when the {tenant} path parameter matches the
// caller's — the shape a real multi-tenant policy has.
type tenantGuard struct{}

func (tenantGuard) Authorize(ctx context.Context) error {
	id, ok := app.IdentityFromContext(ctx)
	if !ok {
		return errors.Unauthenticated("no caller identity")
	}
	p := transport.ParamsFromContext(ctx)
	if p == nil {
		// The FAIL-OPEN reading this test exists to make impossible: "no
		// param, nothing to compare, not applicable". Written the other way
		// on purpose, so the test fails loudly if params go missing again.
		return nil
	}
	tenant, _ := p.Path("tenant")
	if want, _ := app.Claim[string](id, "tid"); want != tenant {
		return errors.PermissionDenied("tenant " + tenant)
	}
	return nil
}

type rawTenantController struct{}

func (rawTenantController) Register(r transport.Registrar) {
	transport.Raw(r, transport.ProtocolHTTP, "GET /raw/t/{tenant}/doc",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
		transport.Guard(tenantGuard{}))
	transport.Get(r, "/typed/t/{tenant}/doc",
		app.HandlerFunc[tenantReq, userDTO](func(context.Context, tenantReq) (userDTO, error) {
			return userDTO{ID: "ok"}, nil
		}),
		transport.Guard(tenantGuard{}))
}

type tenantReq struct {
	Tenant string `param:"tenant"`
}

// TestGuardsSeeParamsOnRawRoutesToo — field test #6, defect B4. Params were
// seeded before guards on typed routes and NOT on raw ones, so the same
// policy on the same URL shape behaved differently. The tester's policy
// failed closed because they wrote it that way; the natural alternative is a
// silent cross-tenant bypass on raw routes only.
func TestGuardsSeeParamsOnRawRoutesToo(t *testing.T) {
	t.Parallel()

	m := warren.NewModule("rawtenant", warren.Controllers(func() *rawTenantController { return &rawTenantController{} }))
	base := serve(t, []warren.Module{m}, whttp.Middleware(tenantIdentity))

	for _, path := range []string{"/raw/t/acme/doc", "/typed/t/acme/doc"} {
		t.Run(path, func(t *testing.T) {
			// Same tenant: allowed.
			res, body := doWithHeaders(t, "GET", base+path, "", map[string]string{"X-Tenant": "acme"})
			if res.StatusCode != 200 {
				t.Errorf("same tenant = %d, want 200\n%s", res.StatusCode, body)
			}
			// Different tenant: MUST be refused on both route shapes.
			res, body = doWithHeaders(t, "GET", base+path, "", map[string]string{"X-Tenant": "other"})
			if res.StatusCode != 403 {
				t.Errorf("cross-tenant = %d, want 403 — the guard could not read {tenant}\n%s", res.StatusCode, body)
			}
		})
	}
}

func tenantIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tid := r.Header.Get("X-Tenant"); tid != "" {
			r = r.WithContext(app.WithIdentity(r.Context(),
				app.Identity{Subject: "u-1", Claims: map[string]any{"tid": tid}}))
		}
		next.ServeHTTP(w, r)
	})
}

// Run is what every generated main.go calls, and until now nothing tested it.
// Start and Stop are covered thoroughly; the signal path that JOINS them was
// not, so a service could have a perfect drain that SIGTERM never reached.
//
// These two are deliberately NOT parallel: they signal the test process
// itself, which every goroutine in the binary shares.
func TestRunDrainsOnSIGTERM(t *testing.T) {
	c := &blockingController{entered: make(chan struct{}), release: make(chan struct{})}
	m := warren.NewModule("slow", warren.Controllers(func() *blockingController { return c }))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()
	// A drain WINDOW, not zero: during it the listener still accepts, which
	// is what makes the 503 observable. With DrainDelay(0) the socket closes
	// the instant readiness does, and polling /readyz gets a connection
	// error instead of the status — which is a test artefact, not a drop.
	a := warren.New(m, whttp.Server(whttp.Listener(ln), whttp.DrainDelay(3*time.Second)))

	ran := make(chan error, 1)
	go func() { ran <- a.Run() }()

	// Only signal once Run has installed its handler — before that, SIGTERM
	// has default disposition and would kill the test binary.
	waitFor(t, "the server to be ready", func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == 200
	})

	done := make(chan int, 1)
	go func() {
		res, err := http.Get(base + "/slow/u-1")
		if err != nil {
			done <- -1
			return
		}
		defer func() { _ = res.Body.Close() }()
		done <- res.StatusCode
	}()
	<-c.entered

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// The drain has begun: readiness closes before the listener does.
	waitFor(t, "readiness to close", func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == 503
	})

	close(c.release)

	if got := <-done; got != 200 {
		t.Errorf("the in-flight request got %d, want 200 — SIGTERM dropped it", got)
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned after SIGTERM")
	}
}

// TestSecondSignalCancelsTheDrain covers the force-exit short-circuit Run
// documents: an operator who signals twice has decided not to wait.
//
// It also covers the re-arm Run performs before releasing its first
// registration. Without that, a signal landing in the gap gets DEFAULT
// disposition — killing the process outright, mid-drain, which is the
// opposite of what pressing Ctrl-C twice should mean.
func TestSecondSignalCancelsTheDrain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()
	// A drain long enough that returning early can only be the short-circuit.
	a := warren.New(whttp.Server(whttp.Listener(ln), whttp.DrainDelay(60*time.Second)))

	ran := make(chan error, 1)
	go func() { ran <- a.Run() }()

	waitFor(t, "the server to be ready", func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == 200
	})

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitFor(t, "the drain to start", func() bool {
		res, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode == 503
	})

	started := time.Now()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("second kill: %v", err)
	}

	select {
	case <-ran:
		// Whatever Run returns, it must not have waited out the 60s drain.
		if elapsed := time.Since(started); elapsed > 20*time.Second {
			t.Errorf("the second signal took %v to end the drain; it must short-circuit", elapsed)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the second signal did not cancel the drain — Run is still draining")
	}
}

// TestAStuckHandlerCannotPreventShutdown covers what ShutdownTimeout is for.
//
// A handler that never returns must not hold the process open: the drain
// gives in-flight requests their chance, and then the server stops anyway.
// Without that bound a single wedged request outlives the grace period and
// the orchestrator SIGKILLs the pod — losing every OTHER in-flight request
// too, which is the opposite of a graceful drain.
func TestAStuckHandlerCannotPreventShutdown(t *testing.T) {
	t.Parallel()

	c := &blockingController{entered: make(chan struct{}), release: make(chan struct{})}
	// Never released: this handler is wedged for the rest of the test.
	m := warren.NewModule("slow", warren.Controllers(func() *blockingController { return c }))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()
	a := warren.New(m, whttp.Server(
		whttp.Listener(ln),
		whttp.DrainDelay(0),
		whttp.ShutdownTimeout(time.Second),
	))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}

	go func() {
		res, err := http.Get(base + "/slow/u-1")
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	<-c.entered

	stopped := make(chan error, 1)
	started := time.Now()
	go func() { stopped <- a.Stop(context.Background()) }()

	select {
	case err := <-stopped:
		// It may report the timeout or not; what matters is that it RETURNED.
		t.Logf("Stop returned after %v: %v", time.Since(started), err)
		if err == nil || !strings.Contains(err.Error(), "ShutdownTimeout") {
			t.Errorf("the error does not name the knob an operator would reach for: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a single wedged handler held shutdown open past ShutdownTimeout; " +
			"the orchestrator would SIGKILL the process and drop every other in-flight request")
	}
}
