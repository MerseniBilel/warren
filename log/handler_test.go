package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/MerseniBilel/warren/log"
)

// The handler is the mechanism that puts correlation_id on every record —
// warren.md §7.4's claim that "a service with no telemetry still gets
// correlation_id on every record" is this file's subject. It is also the one
// piece of core that a user composes with slog's own combinators, so what
// With and WithGroup do to the ID is part of the contract, not an accident.

// record logs one line through h and returns it decoded.
func record(t *testing.T, build func(slog.Handler) *slog.Logger, ctx context.Context, extra ...log.ContextAttrs) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	h := log.Handler(slog.NewJSONHandler(&buf, nil), extra...)
	build(h).InfoContext(ctx, "msg", "status", 200)
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	return got
}

func TestTheCorrelationIDIsAddedWhenTheContextCarriesOne(t *testing.T) {
	t.Parallel()
	ctx := log.WithCorrelationID(context.Background(), "abc-1")
	got := record(t, slog.New, ctx)
	if got["correlation_id"] != "abc-1" {
		t.Errorf("correlation_id = %v, want abc-1 — in %v", got["correlation_id"], got)
	}
}

func TestAnUnseededContextGetsNoCorrelationKeyAtAll(t *testing.T) {
	t.Parallel()
	// Not an empty string: a key that is always present but sometimes blank
	// is one every dashboard has to special-case.
	got := record(t, slog.New, context.Background())
	if _, ok := got["correlation_id"]; ok {
		t.Errorf("unseeded context produced correlation_id = %v", got["correlation_id"])
	}
}

func TestWithAttrsKeepsTheCorrelationID(t *testing.T) {
	t.Parallel()
	// slog.New(h).With(...) derives a NEW handler from the wrapper. A
	// WithAttrs that returned the inner handler would compile, pass every
	// other test here, and silently drop the ID for every service that
	// labels its logger — which is most of them.
	ctx := log.WithCorrelationID(context.Background(), "abc-2")
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h).With("service", "billing")
	}, ctx)
	if got["correlation_id"] != "abc-2" {
		t.Errorf("correlation_id = %v, want abc-2 — in %v", got["correlation_id"], got)
	}
	if got["service"] != "billing" {
		t.Errorf("the caller's own attribute was lost: %v", got)
	}
}

func TestWithGroupKeepsTheCorrelationIDOutOfTheGroup(t *testing.T) {
	t.Parallel()
	// The ID is an INDEX KEY: the one field you query to pull a request's
	// records out of a log store. Nested under a group it is still present
	// and still wrong — `correlation_id:"abc-3"` matches nothing, and the
	// records look perfectly healthy while the search comes back empty.
	ctx := log.WithCorrelationID(context.Background(), "abc-3")
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h).WithGroup("http")
	}, ctx)

	if got["correlation_id"] != "abc-3" {
		t.Errorf("correlation_id = %v, want it at the TOP level — in %v", got["correlation_id"], got)
	}
	group, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("the group is gone: %v", got)
	}
	if _, nested := group["correlation_id"]; nested {
		t.Errorf("correlation_id is inside the group: %v", got)
	}
	if group["status"] != float64(200) {
		t.Errorf("the caller's own attribute left the group: %v", got)
	}
}

func TestAttributesAddedAfterAGroupStayInsideIt(t *testing.T) {
	t.Parallel()
	// The other half of the previous test. Deferring the group so the ID can
	// go above it must not lift the CALLER's attributes out of it too: they
	// were grouped on purpose.
	ctx := log.WithCorrelationID(context.Background(), "abc-4")
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h).WithGroup("http").With("method", "POST")
	}, ctx)

	if got["correlation_id"] != "abc-4" {
		t.Errorf("correlation_id = %v, want it at the top level — in %v", got["correlation_id"], got)
	}
	if _, lifted := got["method"]; lifted {
		t.Errorf("method was lifted out of the group it was added to: %v", got)
	}
	group, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("the group is gone: %v", got)
	}
	if group["method"] != "POST" {
		t.Errorf("method = %v, want POST inside the group — in %v", group["method"], got)
	}
}

func TestNestedGroupsKeepTheirNesting(t *testing.T) {
	t.Parallel()
	ctx := log.WithCorrelationID(context.Background(), "abc-5")
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h).WithGroup("http").WithGroup("request")
	}, ctx)

	if got["correlation_id"] != "abc-5" {
		t.Errorf("correlation_id = %v, want it at the top level — in %v", got["correlation_id"], got)
	}
	outer, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("the outer group is gone: %v", got)
	}
	inner, ok := outer["request"].(map[string]any)
	if !ok {
		t.Fatalf("the inner group is gone: %v", got)
	}
	if inner["status"] != float64(200) {
		t.Errorf("the record's attribute is not in the innermost group: %v", got)
	}
}

func TestAnEmptyGroupNameIsIgnored(t *testing.T) {
	t.Parallel()
	// slog.Handler's contract: "If name is empty, WithGroup returns the
	// receiver." A wrapper that queued it instead would open an unnamed
	// group and swallow every attribute after it.
	ctx := log.WithCorrelationID(context.Background(), "abc-6")
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h.WithGroup(""))
	}, ctx)

	if got["correlation_id"] != "abc-6" {
		t.Errorf("correlation_id = %v, want abc-6 — in %v", got["correlation_id"], got)
	}
	if got["status"] != float64(200) {
		t.Errorf("status = %v, want it at the top level — in %v", got["status"], got)
	}
}

func TestTwoLoggersDerivedFromOneGroupedParentDoNotShareIt(t *testing.T) {
	t.Parallel()
	// The classic append-aliasing bug: two children built from one parent
	// write to the same backing array, so the second's group overwrites the
	// first's and records land in a group their logger never opened.
	//
	// The parent needs THREE deferred operations for the array to have spare
	// capacity to alias — append grows 1 → 2 → 4, so a shorter chain is full
	// and reallocates on every derivation, hiding the bug behind Go's growth
	// pattern rather than fixing it.
	var buf bytes.Buffer
	h := log.Handler(slog.NewJSONHandler(&buf, nil))
	ctx := log.WithCorrelationID(context.Background(), "abc-10")

	parent := slog.New(h).WithGroup("outer").With("tier", "gold").WithGroup("store")
	first := parent.WithGroup("db")
	second := parent.WithGroup("cache")
	// With attributes, because a group holding nothing is elided — by this
	// handler and by the unwrapped one alike.
	second.InfoContext(ctx, "second", "hit", true)
	first.InfoContext(ctx, "first", "rows", 1)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 records, got %d: %q", len(lines), buf.String())
	}
	for i, want := range []string{"cache", "db"} {
		var got map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatalf("decoding %q: %v", lines[i], err)
		}
		outer, ok := got["outer"].(map[string]any)
		if !ok {
			t.Fatalf("record %d lost the outer group: %v", i, got)
		}
		store, ok := outer["store"].(map[string]any)
		if !ok {
			t.Fatalf("record %d lost the store group: %v", i, got)
		}
		if _, ok := store[want]; !ok {
			t.Errorf("record %d is not in the %q group: %v", i, want, got)
		}
	}
}

func TestExtraContextAttrsAreResolvedAtEmitTime(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), tenantKey{}, "acme")
	got := record(t, slog.New, ctx, func(ctx context.Context, add func(slog.Attr)) {
		if v, ok := ctx.Value(tenantKey{}).(string); ok {
			add(slog.String("tenant", v))
		}
	})
	if got["tenant"] != "acme" {
		t.Errorf("tenant = %v, want acme — in %v", got["tenant"], got)
	}
}

func TestExtraContextAttrsAlsoStayOutOfAGroup(t *testing.T) {
	t.Parallel()
	// trace_id arrives this way, and it is an index key for the same reason
	// correlation_id is.
	got := record(t, func(h slog.Handler) *slog.Logger {
		return slog.New(h).WithGroup("http")
	}, context.Background(), func(_ context.Context, add func(slog.Attr)) {
		add(slog.String("trace_id", "t-1"))
	})
	if got["trace_id"] != "t-1" {
		t.Errorf("trace_id = %v, want it at the top level — in %v", got["trace_id"], got)
	}
}

func TestANilContextAttrsIsSkipped(t *testing.T) {
	t.Parallel()
	// A caller composing extractors conditionally — observability passes one
	// only when it is configured — must not take the whole process down.
	ctx := log.WithCorrelationID(context.Background(), "abc-7")
	got := record(t, slog.New, ctx, nil)
	if got["correlation_id"] != "abc-7" {
		t.Errorf("correlation_id = %v, want abc-7 — in %v", got["correlation_id"], got)
	}
}

func TestBelowTheThresholdNothingIsResolved(t *testing.T) {
	t.Parallel()
	// The doc's claim: "a request that logs nothing pays nothing". Enabled
	// must delegate, so slog drops the record before Handle runs and the
	// extractors never touch the context.
	var buf bytes.Buffer
	ran := 0
	h := log.Handler(
		slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}),
		func(context.Context, func(slog.Attr)) { ran++ },
	)
	l := slog.New(h)
	ctx := log.WithCorrelationID(context.Background(), "abc-8")

	l.InfoContext(ctx, "dropped")
	if ran != 0 {
		t.Errorf("the extractor ran %d time(s) for a record below the threshold", ran)
	}
	if buf.Len() != 0 {
		t.Errorf("a record below the threshold was written: %q", buf.String())
	}

	l.ErrorContext(ctx, "kept")
	if ran != 1 {
		t.Errorf("the extractor ran %d time(s) for a record above the threshold, want 1", ran)
	}
	if !strings.Contains(buf.String(), "abc-8") {
		t.Errorf("the record above the threshold lost the correlation ID: %q", buf.String())
	}
}

func TestHandlerRefusesANilHandler(t *testing.T) {
	t.Parallel()
	// Wrapping nil yields a handler that panics on the first record instead
	// of at the line that built it — in main, at boot, where the fix is.
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("log.Handler(nil) did not panic")
		}
		if !strings.Contains(r.(string), "nil slog.Handler") {
			t.Errorf("panic = %v, want it to name the nil handler", r)
		}
	}()
	log.Handler(nil)
}

type tenantKey struct{}

// The ungrouped path is what every Warren service logs through, and it is
// held to a fixed cost: the handler resolves the ID onto the record rather
// than deriving a logger per request, which is the whole reason it exists.
func BenchmarkHandleUngrouped(b *testing.B) {
	h := log.Handler(slog.NewJSONHandler(discard{}, nil))
	l := slog.New(h)
	ctx := log.WithCorrelationID(context.Background(), "abc-9")
	b.ReportAllocs()
	for b.Loop() {
		l.InfoContext(ctx, "msg", "status", 200)
	}
}

func BenchmarkHandleGrouped(b *testing.B) {
	h := log.Handler(slog.NewJSONHandler(discard{}, nil))
	l := slog.New(h).WithGroup("http")
	ctx := log.WithCorrelationID(context.Background(), "abc-9")
	b.ReportAllocs()
	for b.Loop() {
		l.InfoContext(ctx, "msg", "status", 200)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
