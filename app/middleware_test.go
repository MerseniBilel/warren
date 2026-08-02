package app_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
)

// countingPolicy retries up to max times with the given delay — the test
// double for the resilience port. Zero delays keep the suite sleep-free.
type countingPolicy struct {
	max   int
	delay time.Duration
	asked []int
}

func (p *countingPolicy) Next(attempt int) (time.Duration, bool) {
	p.asked = append(p.asked, attempt)
	return p.delay, attempt < p.max
}

func TestRetrying(t *testing.T) {
	t.Parallel()

	t.Run("retries UNAVAILABLE and nothing else", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name      string
			err       error
			wantCalls int
		}{
			{"unavailable retries", werrors.Unavailable("db", stderrors.New("down")), 3},
			{"invalid does not", werrors.Invalid("email", stderrors.New("bad")), 1},
			{"not found does not", werrors.NotFound("user", 42), 1},
			{"conflict does not", werrors.Conflict("taken"), 1},
			{"unauthenticated does not", werrors.Unauthenticated("no token"), 1},
			{"permission denied does not", werrors.PermissionDenied("delete"), 1},
			{"internal does not", werrors.Internal(stderrors.New("boom")), 1},
			{"non-Warren error does not", stderrors.New("plain"), 1},
			{"success does not", nil, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				calls := 0
				h := app.Chain(
					app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
						calls++
						return "ok", tc.err
					}),
					app.Retrying[string, string](&countingPolicy{max: 3}),
				)
				_, err := h.Handle(context.Background(), "x")
				if calls != tc.wantCalls {
					t.Errorf("handler ran %d times, want %d", calls, tc.wantCalls)
				}
				if tc.err != nil && !stderrors.Is(err, tc.err) {
					t.Errorf("the last error did not surface: %v", err)
				}
			})
		}
	})

	t.Run("exhaustion returns the last error with its code intact", func(t *testing.T) {
		t.Parallel()
		attempt := 0
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				attempt++
				if attempt < 100 {
					return "", werrors.Unavailable("db", stderrors.New("still down"))
				}
				return "up", nil
			}),
			app.Retrying[string, string](&countingPolicy{max: 2}),
		)
		_, err := h.Handle(context.Background(), "x")
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("exhaustion did not return the retryable last error: %v", err)
		}
		if attempt != 2 {
			t.Errorf("handler ran %d times, want the policy's 2", attempt)
		}
	})

	t.Run("recovery mid-loop returns the success", func(t *testing.T) {
		t.Parallel()
		attempt := 0
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				attempt++
				if attempt < 3 {
					return "", werrors.Unavailable("db", stderrors.New("down"))
				}
				return "up", nil
			}),
			app.Retrying[string, string](&countingPolicy{max: 5}),
		)
		res, err := h.Handle(context.Background(), "x")
		if err != nil || res != "up" {
			t.Errorf("Handle() = %q, %v — recovery on attempt 3 was lost", res, err)
		}
	})

	t.Run("cancellation during the wait aborts with the last error, no sleeps", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the wait must observe this instantly — the delay is an hour
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				return "", werrors.Unavailable("db", stderrors.New("down"))
			}),
			app.Retrying[string, string](&countingPolicy{max: 10, delay: time.Hour}),
		)
		_, err := h.Handle(ctx, "x")
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("cancellation did not return the handler's last error: %v", err)
		}
	})

	t.Run("a nil policy panics at composition", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "Retrying composed with a nil policy", func() {
			app.Retrying[string, string](nil)
		})
	})

	t.Run("the outermost code decides: a recategorized Internal is final", func(t *testing.T) {
		t.Parallel()
		calls := 0
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				calls++
				// The handler deliberately recategorizes a driver outage as
				// Internal — declaring it non-retryable, per the errors
				// package's "wrapping is recategorization" doctrine.
				return "", werrors.Internal(werrors.Unavailable("db", stderrors.New("down")))
			}),
			app.Retrying[string, string](&countingPolicy{max: 5}),
		)
		_, err := h.Handle(context.Background(), "x")
		if calls != 1 {
			t.Errorf("handler ran %d times — a recategorized failure was retried against the handler's declared intent", calls)
		}
		if !werrors.Is(err, werrors.CodeInternal) {
			t.Errorf("the recategorized error was altered: %v", err)
		}
	})

	t.Run("a plain %w wrap stays retryable — wrapping without recategorizing", func(t *testing.T) {
		t.Parallel()
		calls := 0
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				calls++
				return "", fmt.Errorf("loading profile: %w", werrors.Unavailable("db", stderrors.New("down")))
			}),
			app.Retrying[string, string](&countingPolicy{max: 3}),
		)
		_, _ = h.Handle(context.Background(), "x")
		if calls != 3 {
			t.Errorf("handler ran %d times, want 3 — a %%w wrap leaves the outermost Warren code Unavailable", calls)
		}
	})
}

// allowDeny is the test double for the auth port.
type allowDeny struct{ err error }

func (p allowDeny) Authorize(context.Context) error { return p.err }

func TestAuthorized(t *testing.T) {
	t.Parallel()

	t.Run("a denial short-circuits with the code unchanged", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			deny error
			code werrors.Code
		}{
			{"known caller, forbidden action", werrors.PermissionDenied("delete user 42"), werrors.CodePermissionDenied},
			{"identity absent", werrors.Unauthenticated("token expired"), werrors.CodeUnauthenticated},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				invoked := false
				h := app.Chain(
					app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
						invoked = true
						return "ok", nil
					}),
					app.Authorized[string, string](allowDeny{err: tc.deny}),
				)
				_, err := h.Handle(context.Background(), "x")
				if invoked {
					t.Error("the handler ran despite the denial — Authorized must short-circuit")
				}
				if !werrors.Is(err, tc.code) {
					t.Errorf("the policy's code did not survive: %v", err)
				}
			})
		}
	})

	t.Run("allow proceeds untouched", func(t *testing.T) {
		t.Parallel()
		h := app.Chain(echo(), app.Authorized[string, string](allowDeny{}))
		res, err := h.Handle(context.Background(), "x")
		if err != nil || res != "echo:x" {
			t.Errorf("Handle() = %q, %v", res, err)
		}
	})

	t.Run("a consumer-shaped call path is the same check", func(t *testing.T) {
		t.Parallel()
		// The reason Authorized is core-ring (§7.2): the identical policy
		// runs for a message-shaped request, no HTTP anywhere.
		type message struct{ Payload []byte }
		invoked := false
		h := app.Chain(
			app.HandlerFunc[message, struct{}](func(context.Context, message) (struct{}, error) {
				invoked = true
				return struct{}{}, nil
			}),
			app.Authorized[message, struct{}](allowDeny{err: werrors.PermissionDenied("consume billing.events")}),
		)
		_, err := h.Handle(context.Background(), message{Payload: []byte("evt")})
		if invoked || !werrors.Is(err, werrors.CodePermissionDenied) {
			t.Errorf("consumer-shaped denial failed: invoked=%v err=%v", invoked, err)
		}
	})

	t.Run("a nil policy panics at composition", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "Authorized composed with a nil policy", func() {
			app.Authorized[string, string](nil)
		})
	})
}

// recordingTelemetry is the test double for the observability port.
type recordingTelemetry struct {
	spans    []string
	ended    []error
	recorded []struct {
		name string
		err  error
	}
}

func (r *recordingTelemetry) Span(ctx context.Context, name string) (context.Context, func(error)) {
	r.spans = append(r.spans, name)
	return ctx, func(err error) { r.ended = append(r.ended, err) }
}

func (r *recordingTelemetry) Record(name string, _ time.Duration, err error) {
	r.recorded = append(r.recorded, struct {
		name string
		err  error
	}{name, err})
}

func TestTracedAndMetered(t *testing.T) {
	t.Parallel()

	t.Run("span and record carry the seeded handler name, error path included", func(t *testing.T) {
		t.Parallel()
		tel := &recordingTelemetry{}
		boom := werrors.Conflict("taken")
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				return "", boom
			}),
			app.Traced[string, string](),
			app.Metered[string, string](),
		)
		ctx := app.WithTelemetry(app.WithHandlerName(context.Background(), "user.RegisterUser"), tel)
		_, err := h.Handle(ctx, "x")
		if !werrors.Is(err, werrors.CodeConflict) {
			t.Fatalf("code lost: %v", err)
		}
		if len(tel.spans) != 1 || tel.spans[0] != "user.RegisterUser" {
			t.Errorf("spans = %v, want one named user.RegisterUser", tel.spans)
		}
		if len(tel.ended) != 1 || !werrors.Is(tel.ended[0], werrors.CodeConflict) {
			t.Errorf("span end did not record the handler error: %v", tel.ended)
		}
		if len(tel.recorded) != 1 || tel.recorded[0].name != "user.RegisterUser" || !werrors.Is(tel.recorded[0].err, werrors.CodeConflict) {
			t.Errorf("Record = %+v, want one entry keyed by the name with the coded error", tel.recorded)
		}
	})

	t.Run("without telemetry on the context both are exact pass-throughs", func(t *testing.T) {
		t.Parallel()
		h := app.Chain(echo(), app.Traced[string, string](), app.Metered[string, string]())
		res, err := h.Handle(context.Background(), "x")
		if err != nil || res != "echo:x" {
			t.Errorf("Handle() = %q, %v — the uninstrumented path changed behaviour", res, err)
		}
	})

	t.Run("a nil telemetry is not carried", func(t *testing.T) {
		t.Parallel()
		ctx := app.WithTelemetry(context.Background(), nil)
		if app.TelemetryFromContext(ctx) != nil {
			t.Error("a nil Telemetry was carried — the never-nil discipline from log applies here too")
		}
	})

	t.Run("a typed-nil telemetry is not carried either", func(t *testing.T) {
		t.Parallel()
		// The construction bug an edge integration can ship: a nil pointer
		// inside a non-nil interface. Without the guard, every instrumented
		// request panics in Span.
		var broken *recordingTelemetry
		ctx := app.WithTelemetry(context.Background(), broken)
		if app.TelemetryFromContext(ctx) != nil {
			t.Fatal("a typed-nil Telemetry passed the guard — Traced would panic on the first request")
		}
		h := app.Chain(echo(), app.Traced[string, string](), app.Metered[string, string]())
		res, err := h.Handle(ctx, "x")
		if err != nil || res != "echo:x" {
			t.Errorf("the typed-nil path did not pass through: %q, %v", res, err)
		}
	})
}

func BenchmarkTracedMeteredUninstrumented(b *testing.B) {
	// The uninstrumented request path: two context lookups, zero allocs.
	h := app.Chain(identity(), app.Traced[string, string](), app.Metered[string, string]())
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.Handle(ctx, "req")
	}
}

// recordingUoW is the in-test unit of work: it records commits and rollbacks
// so the middleware's contract is asserted without a database.
type recordingUoW struct {
	commits   int
	rollbacks int
	failWith  error
}

func (f *recordingUoW) Do(ctx context.Context, fn func(context.Context) error) error {
	if f.failWith != nil {
		return f.failWith
	}
	if err := fn(ctx); err != nil {
		f.rollbacks++
		return err
	}
	f.commits++
	return nil
}

func TestTransactional(t *testing.T) {
	t.Parallel()

	t.Run("a successful handler commits and its response passes through", func(t *testing.T) {
		t.Parallel()
		uow := &recordingUoW{}
		h := app.Chain(echo(), app.Transactional[string, string](uow))
		res, err := h.Handle(context.Background(), "x")
		if err != nil || res != "echo:x" {
			t.Fatalf("Handle() = %q, %v", res, err)
		}
		if uow.commits != 1 || uow.rollbacks != 0 {
			t.Errorf("commits=%d rollbacks=%d, want 1/0", uow.commits, uow.rollbacks)
		}
	})

	t.Run("a failing handler rolls back and keeps its code", func(t *testing.T) {
		t.Parallel()
		uow := &recordingUoW{}
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				return "", werrors.Conflict("already exists")
			}),
			app.Transactional[string, string](uow),
		)
		_, err := h.Handle(context.Background(), "x")
		if !werrors.Is(err, werrors.CodeConflict) {
			t.Errorf("err = %v, want the handler's CONFLICT intact for the adapter to map", err)
		}
		if uow.commits != 0 || uow.rollbacks != 1 {
			t.Errorf("commits=%d rollbacks=%d, want 0/1", uow.commits, uow.rollbacks)
		}
	})

	t.Run("a commit failure surfaces with its code and a zero response", func(t *testing.T) {
		t.Parallel()
		uow := &recordingUoW{failWith: werrors.Unavailable("postgres", stderrors.New("serialization failure"))}
		h := app.Chain(echo(), app.Transactional[string, string](uow))
		res, err := h.Handle(context.Background(), "x")
		if !werrors.Is(err, werrors.CodeUnavailable) {
			t.Errorf("err = %v, want UNAVAILABLE so Retrying re-runs the whole transaction", err)
		}
		if res != "" {
			t.Errorf("res = %q, want the zero value when the transaction failed", res)
		}
	})

	t.Run("retry outside the transaction re-runs the whole transaction", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		uow := &countingUoW{}
		h := app.Chain(
			app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
				attempts++
				if attempts < 3 {
					return "", werrors.Unavailable("postgres", stderrors.New("serialization failure"))
				}
				return "ok", nil
			}),
			app.Retrying[string, string](&countingPolicy{max: 5}), // outermost
			app.Transactional[string, string](uow),
		)
		res, err := h.Handle(context.Background(), "x")
		if err != nil || res != "ok" {
			t.Fatalf("Handle() = %q, %v", res, err)
		}
		if uow.opened != 3 {
			t.Errorf("transactions opened = %d, want 3 — each retry is its own transaction", uow.opened)
		}
	})

	t.Run("a nil unit of work panics at composition", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "Transactional composed with a nil unit of work", func() {
			app.Transactional[string, string](nil)
		})
	})
}

type countingUoW struct{ opened int }

func (c *countingUoW) Do(ctx context.Context, fn func(context.Context) error) error {
	c.opened++
	return fn(ctx)
}

// TestStampHandlerNameAllocatesOnce pins the reason StampHandlerName exists.
// A benchmark that drifts tells nobody; an exact count fails CI.
func TestStampHandlerNameAllocatesOnce(t *testing.T) {
	// Not t.Parallel: AllocsPerRun panics in a parallel test, and an
	// allocation count measured against concurrent work would be noise.
	stamp := app.StampHandlerName("user.RegisterUser")
	ctx := context.Background()

	if n := testing.AllocsPerRun(100, func() { _ = stamp(ctx) }); n != 1 {
		t.Errorf("stamping the handler name allocates %v times, want 1", n)
	}
	// And the value still round-trips.
	if got := app.HandlerName(stamp(ctx)); got != "user.RegisterUser" {
		t.Errorf("HandlerName = %q", got)
	}
}
