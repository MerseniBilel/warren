package app_test

import (
	"context"
	stderrors "errors"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
)

var update = flag.Bool("update", false, "rewrite golden files")

// tracing returns a pass-through middleware that appends to trace on the way
// in and on the way out.
func tracing(name string, trace *[]string) app.Middleware[string, string] {
	return func(next app.Handler[string, string]) app.Handler[string, string] {
		return app.HandlerFunc[string, string](func(ctx context.Context, req string) (string, error) {
			*trace = append(*trace, name+"-in")
			res, err := next.Handle(ctx, req)
			*trace = append(*trace, name+"-out")
			return res, err
		})
	}
}

func echo() app.Handler[string, string] {
	return app.HandlerFunc[string, string](func(_ context.Context, req string) (string, error) {
		return "echo:" + req, nil
	})
}

// TestChainOrdering is the golden ordering test: mw[0] outermost, reverse on
// the way out. A silent reordering moves the transaction boundary relative
// to the retry boundary, so the order is contract.
func TestChainOrdering(t *testing.T) {
	t.Parallel()

	var trace []string
	h := app.Chain(
		app.HandlerFunc[string, string](func(_ context.Context, req string) (string, error) {
			trace = append(trace, "handle")
			return req, nil
		}),
		tracing("a", &trace),
		tracing("b", &trace),
		tracing("c", &trace),
	)
	if _, err := h.Handle(context.Background(), "x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := strings.Join(trace, "\n")
	path := filepath.Join("testdata", "chain_order.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("chain order changed:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTransparency(t *testing.T) {
	t.Parallel()

	t.Run("a successful response passes through identically", func(t *testing.T) {
		t.Parallel()
		var trace []string
		h := app.Chain(echo(), tracing("a", &trace), tracing("b", &trace))
		res, err := h.Handle(context.Background(), "req")
		if err != nil || res != "echo:req" {
			t.Errorf("Handle() = %q, %v — the chain altered a successful response", res, err)
		}
	})

	t.Run("every error code survives the chain", func(t *testing.T) {
		t.Parallel()
		codes := []struct {
			name string
			err  error
			code werrors.Code
		}{
			{"invalid", werrors.Invalid("email", stderrors.New("bad")), werrors.CodeInvalid},
			{"not found", werrors.NotFound("user", 42), werrors.CodeNotFound},
			{"conflict", werrors.Conflict("taken"), werrors.CodeConflict},
			{"unauthenticated", werrors.Unauthenticated("no token"), werrors.CodeUnauthenticated},
			{"permission denied", werrors.PermissionDenied("delete"), werrors.CodePermissionDenied},
			{"unavailable", werrors.Unavailable("db", stderrors.New("down")), werrors.CodeUnavailable},
			{"internal", werrors.Internal(stderrors.New("boom")), werrors.CodeInternal},
		}
		for _, tc := range codes {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var trace []string
				failing := app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
					return "", tc.err
				})
				h := app.Chain(failing, tracing("a", &trace), tracing("b", &trace))
				_, err := h.Handle(context.Background(), "req")
				if !werrors.Is(err, tc.code) {
					t.Errorf("code %s did not survive the chain: %v — the adapter downstream would pick the wrong status", tc.code, err)
				}
			})
		}
	})
}

func TestContextDiscipline(t *testing.T) {
	t.Parallel()

	type key struct{}
	sawValue := false
	var sawCancel error

	adds := func(next app.Handler[string, string]) app.Handler[string, string] {
		return app.HandlerFunc[string, string](func(ctx context.Context, req string) (string, error) {
			return next.Handle(context.WithValue(ctx, key{}, "outer"), req)
		})
	}
	h := app.Chain(
		app.HandlerFunc[string, string](func(ctx context.Context, req string) (string, error) {
			sawValue = ctx.Value(key{}) == "outer"
			sawCancel = ctx.Err()
			return req, nil
		}),
		adds,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Handle(ctx, "x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !sawValue {
		t.Error("the context reaching Handle is not derived from the outermost middleware's")
	}
	if sawCancel == nil {
		t.Error("cancellation did not propagate through the chain")
	}
}

// TestChainOrderingOnErrorPath pins that unwinding order holds when the
// handler fails: middleware see the error on the way out in reverse order,
// with the code intact.
func TestChainOrderingOnErrorPath(t *testing.T) {
	t.Parallel()

	var trace []string
	failing := app.HandlerFunc[string, string](func(context.Context, string) (string, error) {
		trace = append(trace, "handle")
		return "", werrors.Conflict("taken")
	})
	h := app.Chain(failing, tracing("a", &trace), tracing("b", &trace))
	_, err := h.Handle(context.Background(), "x")
	if !werrors.Is(err, werrors.CodeConflict) {
		t.Fatalf("code did not survive: %v", err)
	}
	want := []string{"a-in", "b-in", "handle", "b-out", "a-out"}
	if strings.Join(trace, ",") != strings.Join(want, ",") {
		t.Errorf("error-path order = %v, want %v", trace, want)
	}
}

// assertPanics runs fn and asserts it panics with a message containing want —
// the composition-time guards are boot-time panics with named messages
// (AGENT.md § General's second sanctioned exception).
func assertPanics(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one mentioning %q", want)
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, want) {
			t.Errorf("panic = %v, want a named message containing %q", r, want)
		}
	}()
	fn()
}

func TestChainRefusesNilAtComposition(t *testing.T) {
	t.Parallel()

	pass := func(next app.Handler[string, string]) app.Handler[string, string] { return next }
	returnsNil := func(app.Handler[string, string]) app.Handler[string, string] { return nil }

	t.Run("nil handler", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "nil handler", func() { app.Chain[string, string](nil) })
	})

	t.Run("nil middleware names its position", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "middleware 2 of 3 is nil", func() {
			app.Chain(echo(), pass, nil, pass)
		})
	})

	t.Run("middleware returning nil names its position", func(t *testing.T) {
		t.Parallel()
		assertPanics(t, "middleware 1 of 2 returned a nil handler", func() {
			app.Chain(echo(), returnsNil, pass)
		})
	})
}

// TestTransportIndependence is the compile-time check the spec requires: the
// package's own dependency graph is the standard library and nothing else,
// guarded against regression rather than asserted once by hand.
func TestTransportIndependence(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"context": true, "fmt": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !allowed[path] {
				t.Errorf("%s imports %q — the app package's graph must stay stdlib-only, no transport, broker, or persistence anything", e.Name(), path)
			}
		}
	}
}

func TestChainOfNothingIsTheHandler(t *testing.T) {
	t.Parallel()

	h := echo()
	if got := app.Chain(h); got == nil {
		t.Fatal("Chain(h) returned nil")
	}
	res, err := app.Chain(h).Handle(context.Background(), "x")
	if err != nil || res != "echo:x" {
		t.Errorf("Chain with no middleware altered the handler: %q, %v", res, err)
	}
}

// identity is the benchmark handler: allocation-free, so any alloc the
// benchmarks report belongs to the chain — and the claim is that there are
// none.
func identity() app.Handler[string, string] {
	return app.HandlerFunc[string, string](func(_ context.Context, req string) (string, error) {
		return req, nil
	})
}

func BenchmarkBareHandler(b *testing.B) {
	h := identity()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.Handle(ctx, "req")
	}
}

func BenchmarkChainOfFive(b *testing.B) {
	// Composition allocates here, at boot — not per request.
	pass := func(next app.Handler[string, string]) app.Handler[string, string] {
		return app.HandlerFunc[string, string](func(ctx context.Context, req string) (string, error) {
			return next.Handle(ctx, req)
		})
	}
	h := app.Chain(identity(), pass, pass, pass, pass, pass)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.Handle(ctx, "req")
	}
}
