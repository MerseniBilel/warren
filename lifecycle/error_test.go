package lifecycle_test

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/lifecycle"
)

// goldenPerm is the mode for a regenerated golden file. They are fixtures read
// by tests, never executed.
const goldenPerm = 0o600

// update rewrites the golden files instead of comparing against them. It backs
// `make golden-update`, which is why it has to be a package-level flag.
//
//nolint:gochecknoglobals // a test flag must be registered at package level for the flag package to parse it.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// realSite matches a site this machine produced, so that a golden pins the shape
// of a registration's location without pinning an absolute path or a line number
// that shifts whenever the file above it is edited.
//
// SPEC.md §6 shows these as a service's own files — internal/platform/module.go —
// because that is what a user sees. A test can only produce its own path, so the
// goldens pin the shape and the spec shows the intent.
var realSite = regexp.MustCompile(`/\S*/warren/lifecycle/(\S+\.go):\d+`)

// cancelled returns a context that is already over, which is how AGENT.md says
// to write a deadline in a test: never a sleep.
func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}

// service registers the three hooks a real service has, in dependency order.
func service(h *lifecycle.Hooks) {
	h.Append(lifecycle.Close(poolHook, func() {}))
	h.Append(lifecycle.Close(consumerHook, func() {}))
	h.Append(lifecycle.Close(serverHook, func() {}))
}

// TestErrorGolden pins every message in SPEC.md §6. A lifecycle failure is read
// by an operator during an incident, so the text is the thing under test.
func TestErrorGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]func() error{
		// §6.1 — a hook failed to start.
		"start_failed": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Close(poolHook, func() {}))
			h.Append(lifecycle.Hook{
				Name: consumerHook,
				OnStart: func(context.Context) error {
					return errors.Internal("dial tcp 127.0.0.1:5432: connection refused")
				},
			})
			h.Append(lifecycle.Close(serverHook, func() {}))

			return h.Start(context.Background())
		},

		// §6.2 — the start failed and the unwind failed too, joined cause first.
		"start_failed_and_unwind_failed": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Closer(poolHook, func() error {
				return errors.Internal("context deadline exceeded")
			}))
			h.Append(lifecycle.Hook{
				Name: consumerHook,
				OnStart: func(context.Context) error {
					return errors.Internal("no brokers configured")
				},
			})

			return h.Start(context.Background())
		},

		// §6.3 — the grace period expired.
		"stop_deadline": func() error {
			h := lifecycle.New()
			service(h)

			err := h.Start(context.Background())
			if err != nil {
				return err
			}

			return h.Stop(cancelled())
		},

		// §6.4 — a hook failed to stop, and Stop carried on.
		"stop_failed": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Close(poolHook, func() {}))
			h.Append(lifecycle.Closer(consumerHook, func() error {
				return errors.Internal("pool is already closed")
			}))
			h.Append(lifecycle.Close(serverHook, func() {}))

			ctx := context.Background()

			err := h.Start(ctx)
			if err != nil {
				return err
			}

			return h.Stop(ctx)
		},

		// §6.5 — a hook with no name.
		"no_name": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Hook{OnStart: func(context.Context) error { return nil }})

			return h.Start(context.Background())
		},

		// §6.6 — a hook with neither callback.
		"nothing_to_do": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Hook{Name: schedulerHook})

			return h.Start(context.Background())
		},

		// §6.7 — started twice.
		"already_started": func() error {
			h := lifecycle.New()
			service(h)

			ctx := context.Background()

			err := h.Start(ctx)
			if err != nil {
				return err
			}

			return h.Start(ctx)
		},

		// §6.8 — a hook appended after the lifecycle started.
		"appended_late": func() error {
			h := lifecycle.New()
			service(h)

			ctx := context.Background()

			err := h.Start(ctx)
			if err != nil {
				return err
			}

			h.Append(lifecycle.Close(schedulerHook, func() {}))

			return h.Stop(ctx)
		},

		// §6.9 — a hook panicked.
		"panicked": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Close(poolHook, func() {}))
			h.Append(lifecycle.Hook{
				Name: consumerHook,
				OnStart: func(context.Context) error {
					panic("runtime error: invalid memory address or nil pointer dereference")
				},
			})

			return h.Start(context.Background())
		},

		// §6.10 — the start deadline expired.
		"start_deadline": func() error {
			h := lifecycle.New()
			service(h)

			return h.Start(cancelled())
		},

		// §6.11 — started after stopping.
		"already_stopped": func() error {
			h := lifecycle.New()
			service(h)

			ctx := context.Background()

			err := h.Start(ctx)
			if err != nil {
				return err
			}

			err = h.Stop(ctx)
			if err != nil {
				return err
			}

			return h.Start(ctx)
		},

		// §6.12 — a hook ended its goroutine without returning.
		"exited": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Hook{
				Name:    schedulerHook,
				OnStart: func(context.Context) error { runtime.Goexit(); return nil },
			})

			return h.Start(context.Background())
		},
	}

	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := build()
			if err == nil {
				t.Fatalf("%s produced no error", name)
			}

			got := realSite.ReplaceAllString(errors.Detail(err), "warren/lifecycle/$1:NN")
			path := filepath.Join("testdata", name+".golden")

			if *update {
				writeErr := os.WriteFile(path, []byte(got), goldenPerm)
				if writeErr != nil {
					t.Fatalf("writing golden file: %v", writeErr)
				}
			}

			want, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("reading golden file: %v (run: make golden-update)", readErr)
			}

			if got != string(want) {
				t.Errorf("Detail() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// TestEveryFailureCarriesAFix is the structural half of the same requirement:
// AGENT.md § Errors says a message must name the fix, and a test can only assert
// that when the fix is a field rather than prose inside a sentence.
//
// The two joined cases are excluded because errors.Join reports no fix for the
// whole — SPEC.md §5.4 — and each branch carries its own.
func TestEveryFailureCarriesAFix(t *testing.T) {
	t.Parallel()

	failures := map[string]func() error{
		"a hook that failed to start": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Hook{
				Name:    poolHook,
				OnStart: func(context.Context) error { return errors.Internal("refused") },
			})

			return h.Start(context.Background())
		},
		"a hook with no name": func() error {
			h := lifecycle.New()
			h.Append(lifecycle.Hook{OnStop: func(context.Context) error { return nil }})

			return h.Start(context.Background())
		},
		"an expired start deadline": func() error {
			h := lifecycle.New()
			service(h)

			return h.Start(cancelled())
		},
		"an expired grace period": func() error {
			h := lifecycle.New()
			service(h)

			err := h.Start(context.Background())
			if err != nil {
				return err
			}

			return h.Stop(cancelled())
		},
	}

	for name, build := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := build()
			if err == nil {
				t.Fatalf("%s produced no error", name)
			}

			if errors.Fix(err) == "" {
				t.Errorf("%s carries no fix: %v", name, err)
			}
		})
	}
}
