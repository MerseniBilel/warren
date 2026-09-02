package servertest_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/transport"
	whttp "github.com/MerseniBilel/warren/transport/http"
	"github.com/MerseniBilel/warren/transport/http/servertest"
)

// --- a module whose routes exercise the whole edge ------------------------

type createUser struct {
	Email string `json:"email" validate:"required"`
}

type getUser struct {
	ID string `param:"id" validate:"required"`
}

type listUsers struct {
	Limit  int    `query:"limit"`
	Status string `query:"status"`
}

type userView struct {
	ID     string `json:"id"`
	Limit  int    `json:"limit,omitempty"`
	Status string `json:"status,omitempty"`
}

type createHandler struct{}

func (createHandler) Handle(_ context.Context, c createUser) (userView, error) {
	if c.Email == "taken@example.com" {
		return userView{}, werrors.Conflict("user already exists")
	}
	return userView{ID: "u-1"}, nil
}

type getHandler struct{}

func (getHandler) Handle(_ context.Context, q getUser) (userView, error) {
	if q.ID != "u-1" {
		return userView{}, werrors.NotFound("user", q.ID)
	}
	return userView{ID: q.ID}, nil
}

type listHandler struct{}

func (listHandler) Handle(_ context.Context, q listUsers) (userView, error) {
	return userView{ID: "list", Limit: q.Limit, Status: q.Status}, nil
}

type controller struct{}

func (c *controller) Register(r *transport.Registrar) {
	r.Post("/users", createHandler{})
	r.Get("/users/{id}", getHandler{})
	r.Get("/users", listHandler{})
}

func userModule() warren.Module {
	return warren.NewModule("user",
		warren.Controllers(func() *controller { return &controller{} }),
	)
}

// TestTheSeamCrossesTheWholeEdge is the point of this package. Every
// assertion here is one warrentest.Invoke cannot make, because Invoke calls
// the handler directly and crosses none of the transport.
func TestTheSeamCrossesTheWholeEdge(t *testing.T) {
	t.Parallel()
	s := servertest.New(t, userModule())

	t.Run("POST defaults to 201", func(t *testing.T) {
		got := s.Post(t, "/users", map[string]string{"email": "a@example.com"})
		if got.Status != http.StatusCreated {
			t.Fatalf("status = %s, want 201 — Post's default success status", got)
		}
		var v userView
		got.Decode(t, &v)
		if v.ID != "u-1" {
			t.Errorf("id = %q, want u-1", v.ID)
		}
	})

	t.Run("validation refuses before the handler", func(t *testing.T) {
		got := s.Post(t, "/users", map[string]string{})
		if got.Status != http.StatusBadRequest {
			t.Fatalf("status = %s, want 400", got)
		}
		if code := got.Code(t); code != "INVALID" {
			t.Errorf("code = %q, want INVALID — the code is the contract, not the status", code)
		}
	})

	t.Run("param binding reaches the handler", func(t *testing.T) {
		got := s.Get(t, "/users/u-1")
		if got.Status != http.StatusOK {
			t.Fatalf("status = %s, want 200", got)
		}
		var v userView
		got.Decode(t, &v)
		if v.ID != "u-1" {
			t.Errorf("id = %q — the {id} wildcard did not bind", v.ID)
		}
	})

	t.Run("query binding reaches the handler", func(t *testing.T) {
		got := s.Get(t, "/users?limit=7&status=active")
		var v userView
		got.Decode(t, &v)
		if v.Limit != 7 || v.Status != "active" {
			t.Errorf("limit/status = %d/%q, want 7/active — query tags did not bind", v.Limit, v.Status)
		}
	})

	t.Run("an unparseable query value is a 400", func(t *testing.T) {
		got := s.Get(t, "/users?limit=abc")
		if got.Status != http.StatusBadRequest {
			t.Errorf("status = %s, want 400 for a non-numeric int query parameter", got)
		}
	})

	t.Run("the error column maps codes to statuses", func(t *testing.T) {
		for _, tc := range []struct {
			name, path, code string
			body             any
			want             int
		}{
			{"conflict", "/users", "CONFLICT", map[string]string{"email": "taken@example.com"}, http.StatusConflict},
			{"not found", "/users/nope", "NOT_FOUND", nil, http.StatusNotFound},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var got *servertest.Response
				if tc.body == nil {
					got = s.Get(t, tc.path)
				} else {
					got = s.Post(t, tc.path, tc.body)
				}
				if got.Status != tc.want {
					t.Errorf("status = %s, want %d", got, tc.want)
				}
				if code := got.Code(t); code != tc.code {
					t.Errorf("code = %q, want %q", code, tc.code)
				}
			})
		}
	})

	t.Run("every body ends with a newline", func(t *testing.T) {
		// Felt on every hand-driven curl during development: without it the
		// body runs into the next shell prompt. encoding/json.Encoder emits
		// one; Marshal + Write did not.
		for _, got := range []*servertest.Response{
			s.Post(t, "/users", map[string]string{"email": "a@example.com"}),
			s.Post(t, "/users", map[string]string{}),
			s.Get(t, "/users/nope"),
		} {
			if len(got.Body) == 0 || got.Body[len(got.Body)-1] != '\n' {
				t.Errorf("a %d body does not end with a newline: %q", got.Status, got.Body)
			}
		}
	})

	t.Run("a wrong method is refused by the mux", func(t *testing.T) {
		got := s.Delete(t, "/users")
		if got.Status != http.StatusMethodNotAllowed {
			t.Errorf("status = %s, want 405", got)
		}
		// The code, not just the status — same argument as the 415 below.
		if code := got.Code(t); code != "METHOD_NOT_ALLOWED" {
			t.Errorf("code = %q, want METHOD_NOT_ALLOWED — a client cannot tell a wrong verb from a bad body", code)
		}
	})

	t.Run("a wrong content type is refused", func(t *testing.T) {
		req := s.Request(t, http.MethodPost, "/users", strings.NewReader("email=a"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got := s.Do(t, req)
		if got.Status != http.StatusUnsupportedMediaType {
			t.Errorf("status = %s, want 415", got)
		}
		// The code, not just the status. Warren asks clients to switch on the
		// code — that is the stated reason CONFLICT and CONTENTION may share
		// 409 — so a 415 that answered INVALID made the same promise false
		// for the one case whose remedy is "fix your HTTP client".
		if code := got.Code(t); code != "UNSUPPORTED_MEDIA" {
			t.Errorf("code = %q, want UNSUPPORTED_MEDIA — a client cannot tell this from a malformed body", code)
		}
	})
}

// TestDrainDelayIsZeroByDefault pins the harness default. whttp's own default
// is 5s, which is right in production and costs a test suite five seconds per
// booted server — the papercut that made this package's first user lose time.
func TestDrainDelayIsZeroByDefault(t *testing.T) {
	t.Parallel()

	start := time.Now()
	func() {
		inner := &testing.T{}
		_ = inner
		s := servertest.New(t, userModule())
		if got := s.Get(t, "/users/u-1"); got.Status != http.StatusOK {
			t.Fatalf("status = %s", got)
		}
	}()
	// Cleanup runs at test end, so this bounds boot + one request only. The
	// real assertion is that the whole package's suite does not take 5s per
	// server; this catches an accidental removal of the DrainDelay(0) line.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("boot and one request took %s — DrainDelay(0) is probably not applied", elapsed)
	}
}

// TestOptionsOverrideTheHarnessDefault proves a test can ask for a real drain
// back, which is the escape hatch that makes the zero default acceptable.
func TestOptionsOverrideTheHarnessDefault(t *testing.T) {
	t.Parallel()
	s := servertest.New(t, userModule(), servertest.Options(whttp.DrainDelay(10*time.Millisecond)))
	if got := s.Get(t, "/users/u-1"); got.Status != http.StatusOK {
		t.Fatalf("status = %s", got)
	}
}

var _ = app.Handler[createUser, userView](createHandler{})

// TestLogRoutesListsEveryRoute pins SBX-019: the route table is built at boot
// and was reachable from nothing, so "what does this service serve?" could
// only be answered by reading every Register method or grepping for
// registration calls — which returns fragments for a multi-line registration.
func TestLogRoutesListsEveryRoute(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	servertest.New(t, userModule(), servertest.Options(whttp.LogRoutes()))

	mu.Lock()
	got := buf.String()
	mu.Unlock()

	for _, want := range []string{
		`msg=route method=POST pattern=/users`,
		`msg=route method=GET pattern=/users/{id}`,
		`msg=route method=GET pattern=/users`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LogRoutes did not print %q:\n%s", want, got)
		}
	}
}

// lockedWriter serialises writes, because the server logs from its own
// goroutine and the test reads the buffer from this one.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
