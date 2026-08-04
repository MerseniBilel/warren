package http_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/transport"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

// --- the fixture ----------------------------------------------------------

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

type deleteUser struct {
	ID string `param:"id"`
}

// failing drives one route per row of the error table.
type failing struct {
	Code string `json:"code"`
}

type controller struct{}

func (c *controller) register(_ context.Context, cmd registerUser) (userDTO, error) {
	return userDTO{ID: "u-1", Email: cmd.Email}, nil
}

func (c *controller) get(_ context.Context, q getUser) (userDTO, error) {
	return userDTO{ID: q.ID, Email: q.Trace}, nil
}

func (c *controller) remove(context.Context, deleteUser) (struct{}, error) {
	return struct{}{}, nil
}

func (c *controller) fail(_ context.Context, f failing) (struct{}, error) {
	switch f.Code {
	case "INVALID":
		return struct{}{}, errors.Invalid("email", io.EOF).WithDetail("email", "must be an address")
	case "NOT_FOUND":
		return struct{}{}, errors.NotFound("user", "u-9")
	case "CONFLICT":
		return struct{}{}, errors.Conflict("user already exists")
	case "UNAUTHENTICATED":
		return struct{}{}, errors.Unauthenticated("no bearer token")
	case "PERMISSION_DENIED":
		return struct{}{}, errors.PermissionDenied("users:delete")
	case "UNAVAILABLE":
		return struct{}{}, errors.Unavailable("postgres", io.EOF)
	case "PANIC":
		panic("boom: password=hunter2")
	}
	// A plain error, not from the vocabulary: INTERNAL, and its text carries
	// a secret that must never reach the client.
	return struct{}{}, io.ErrUnexpectedEOF
}

func (c *controller) Register(r transport.Registrar) {
	transport.Post(r, "/users", app.HandlerFunc[registerUser, userDTO](c.register))
	transport.Get(r, "/users/{id}", app.HandlerFunc[getUser, userDTO](c.get))
	transport.Delete(r, "/users/{id}", app.HandlerFunc[deleteUser, struct{}](c.remove))
	transport.Post(r, "/fail", app.HandlerFunc[failing, struct{}](c.fail))
}

func userModule() warren.Module {
	return warren.NewModule("user",
		warren.Controllers(func() *controller { return &controller{} }),
	)
}

// --- harness --------------------------------------------------------------

// serve boots a real application on a loopback listener bound to port 0 and
// returns its base URL. No Docker, no network beyond loopback, no sleeps —
// which is exactly what httptest does, with the boot sequence included.
func serve(t *testing.T, modules []warren.Module, opts ...whttp.Option) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opts = append([]whttp.Option{whttp.Listener(ln), whttp.DrainDelay(0)}, opts...)
	a := warren.New(append(modules, whttp.Server(opts...))...)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("request: %v", err)
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

// --- end to end -----------------------------------------------------------

func TestPostServesTheHandler(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "POST", base+"/users", `{"email":"bob@example.com","name":"Bob"}`)

	if res.StatusCode != 201 {
		t.Errorf("status = %d, want 201 — Post's default success", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", got)
	}
	if body != `{"id":"u-1","email":"bob@example.com"}` {
		t.Errorf("body = %s", body)
	}
}

func TestPathAndQueryAreBound(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "GET", base+"/users/u-42?trace=abc", "")

	if res.StatusCode != 200 {
		t.Errorf("status = %d", res.StatusCode)
	}
	if body != `{"id":"u-42","email":"abc"}` {
		t.Errorf("body = %s — path and query must reach the request struct", body)
	}
}

func TestQueryUnescapes(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	// encoding/json escapes & as & on the way out; what is under test is
	// that the scanner decoded %20 and %26 on the way in.
	_, body := do(t, "GET", base+"/users/u-1?trace=a%20b%26c", "")
	if body != `{"id":"u-1","email":"a b\u0026c"}` {
		t.Errorf("body = %s — the query scanner must unescape", body)
	}
}

func TestDeleteWritesNoBody(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "DELETE", base+"/users/u-1", "")

	if res.StatusCode != 204 {
		t.Errorf("status = %d, want 204 — Delete's default success", res.StatusCode)
	}
	if body != "" {
		t.Errorf("204 wrote a body: %q", body)
	}
	if got := res.Header.Get("Content-Type"); got != "" {
		t.Errorf("204 wrote a Content-Type: %q", got)
	}
}

func TestValidationFailsBeforeTheHandler(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "POST", base+"/users", `{"name":"Bob"}`)

	if res.StatusCode != 400 {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
	if !strings.Contains(body, `"code":"INVALID"`) || !strings.Contains(body, "email") {
		t.Errorf("body = %s — a validation failure must name the field", body)
	}
}

// --- the error table ------------------------------------------------------

func TestErrorTable(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"INVALID", 400},
		{"NOT_FOUND", 404},
		{"CONFLICT", 409},
		{"UNAUTHENTICATED", 401},
		{"PERMISSION_DENIED", 403},
		{"UNAVAILABLE", 503},
	} {
		t.Run(tc.code, func(t *testing.T) {
			res, body := do(t, "POST", base+"/fail", `{"code":"`+tc.code+`"}`)
			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Errorf("body = %s — code must be the errors.Code verbatim", body)
			}
			if !strings.Contains(body, `"correlation_id":"`) {
				t.Errorf("body = %s — every error carries the correlation ID", body)
			}
		})
	}
}

func TestInternalLeaksNothing(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "POST", base+"/fail", `{"code":"other"}`)

	if res.StatusCode != 500 {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
	if strings.Contains(body, "unexpected EOF") {
		t.Errorf("INTERNAL leaked the cause to the client: %s", body)
	}
	if !strings.Contains(body, `"message":"internal error"`) {
		t.Errorf("body = %s — INTERNAL renders a fixed message", body)
	}
}

func TestPanicIsA500AndLeaksNoStack(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "POST", base+"/fail", `{"code":"PANIC"}`)

	if res.StatusCode != 500 {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, "goroutine") {
		t.Errorf("the panic reached the client: %s", body)
	}
}

// --- 404, 405, redirects --------------------------------------------------

func TestUnknownPathIsAJSON404(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "GET", base+"/nope", "")

	if res.StatusCode != 404 {
		t.Errorf("status = %d", res.StatusCode)
	}
	if !strings.Contains(body, `"code":"NOT_FOUND"`) {
		t.Errorf("body = %s — 404 is the same envelope as every other error", body)
	}
}

func TestWrongMethodIs405WithAllow(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, body := do(t, "PATCH", base+"/users/u-1", "")

	if res.StatusCode != 405 {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
	// Registering the shim costs ServeMux's own free Allow, so the adapter
	// computes it — including the HEAD that every GET pattern serves.
	if got := res.Header.Get("Allow"); got != "DELETE, GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "DELETE, GET, HEAD")
	}
	// NOT_FOUND would be a lie with consequences: a client switching on
	// error.code would conclude the resource is gone and stop retrying,
	// when in fact it exists and answers other verbs — the Allow header
	// two lines up says which. §2.6's table maps codes raised by HANDLERS;
	// 405 is raised by the adapter, before any handler exists.
	if !strings.Contains(body, `"code":"INVALID"`) {
		t.Errorf("a 405 must not claim the resource does not exist: %s", body)
	}
	if strings.Contains(body, "NOT_FOUND") {
		t.Errorf("405 still carries NOT_FOUND: %s", body)
	}
}

// net/http's redirect is 307, not 301 — and it fires on path cleaning, not on
// a trailing slash the pattern did not ask for.
func TestPathCleaningRedirectsWith307(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Get(base + "//users/u-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 307 {
		t.Errorf("status = %d, want 307 — net/http uses a TEMPORARY redirect", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/users/u-1" {
		t.Errorf("Location = %q", got)
	}
}

func TestTrailingSlashOnAnExactPatternIs404(t *testing.T) {
	t.Parallel()

	base := serve(t, []warren.Module{userModule()})
	res, _ := do(t, "GET", base+"/users/u-1/", "")
	if res.StatusCode != 404 {
		t.Errorf("status = %d, want 404 — no redirect is added for a suffix slash", res.StatusCode)
	}
}
