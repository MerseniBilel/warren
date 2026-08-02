package http_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
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

// --- body limit -----------------------------------------------------------

func TestBodyOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()}, whttp.MaxBodyBytes(32))
	big := `{"email":"` + strings.Repeat("a", 200) + `"}`
	res, body := do(t, "POST", base+"/users", big)

	if res.StatusCode != 400 {
		t.Errorf("status = %d, want 400 — the error table chooses the status", res.StatusCode)
	}
	if !strings.Contains(body, `"code":"INVALID"`) {
		t.Errorf("body = %s", body)
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

func (c *conflictController) Register(r transport.Registrar) {
	transport.Get(r, "/users/{id}", app.HandlerFunc[getUser, userDTO](
		func(context.Context, getUser) (userDTO, error) { return userDTO{}, nil }))
	transport.Get(r, "/users/{uid}", app.HandlerFunc[getUser, userDTO](
		func(context.Context, getUser) (userDTO, error) { return userDTO{}, nil }))
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
