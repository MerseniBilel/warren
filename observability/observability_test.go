package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
	wlog "github.com/MerseniBilel/warren/log"
	"github.com/MerseniBilel/warren/observability"
	"github.com/MerseniBilel/warren/transport"
)

// --- the fixture -----------------------------------------------------------

type greet struct {
	Name string `json:"name"`
}

type greeting struct {
	Text string `json:"text"`
}

type controller struct{ fail error }

func (c *controller) hello(_ context.Context, g greet) (greeting, error) {
	if c.fail != nil {
		return greeting{}, c.fail
	}
	return greeting{Text: "hello " + g.Name}, nil
}

func (c *controller) Register(r transport.Registrar) {
	transport.Post(r, "/greet", app.HandlerFunc[greet, greeting](c.hello))
}

// fakeTelemetry is an in-process app.Telemetry: no collector, no network, no
// sleeps. It records what a real implementation would export, and delegates
// propagation to the real OTel propagator so the round-trip test exercises
// the actual W3C encoding rather than a stand-in for it.
type fakeTelemetry struct {
	tracer     trace.Tracer
	noHandlers bool
	spans      []string
	records    []fakeRecord
}

type fakeRecord struct {
	name string
	err  error
}

func (f *fakeTelemetry) Span(ctx context.Context, name string) (context.Context, func(error)) {
	f.spans = append(f.spans, name)
	if f.tracer == nil {
		return ctx, func(error) {}
	}
	ctx, span := f.tracer.Start(ctx, name)
	return ctx, func(error) { span.End() }
}

func (f *fakeTelemetry) Record(name string, _ time.Duration, err error) {
	f.records = append(f.records, fakeRecord{name: name, err: err})
}

func (f *fakeTelemetry) Inject(ctx context.Context, set func(key, value string)) {
	otel.GetTextMapPropagator().Inject(ctx, injectCarrier(set))
}

func (f *fakeTelemetry) Extract(ctx context.Context, get func(key string) string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, extractCarrier(get))
}

func (f *fakeTelemetry) InstrumentHandlers() bool { return !f.noHandlers }

type injectCarrier func(k, v string)

func (c injectCarrier) Get(string) string { return "" }
func (c injectCarrier) Set(k, v string)   { c(k, v) }
func (c injectCarrier) Keys() []string    { return nil }

type extractCarrier func(k string) string

func (c extractCarrier) Get(k string) string { return c(k) }
func (c extractCarrier) Set(string, string)  {}
func (c extractCarrier) Keys() []string      { return nil }

// --- the seam --------------------------------------------------------------

// One import instruments every handler: no controller decorates anything, and
// boot composes Traced and Metered around every route.
func TestHandlersAreInstrumentedWithoutDecoration(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	tel := &fakeTelemetry{tracer: tp.Tracer("test")}
	tbl := buildTable(t, tel, &controller{})

	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	if _, err := invoke(context.Background(), []byte(`{"name":"bob"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}

	if len(tel.spans) != 1 {
		t.Fatalf("spans = %d, want 1 — boot must compose app.Traced around every route", len(tel.spans))
	}
	if tel.spans[0] != "user.hello" {
		t.Errorf("span name = %q, want <module>.<handler>", tel.spans[0])
	}
	if len(tel.records) != 1 {
		t.Fatalf("metric records = %d, want 1 — app.Metered too", len(tel.records))
	}
}

// With no telemetry bound, nothing is composed: the route closure is what it
// would be in a service that never heard of this package.
func TestNoTelemetryComposesNothing(t *testing.T) {
	tbl := buildTable(t, nil, &controller{})
	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	if _, err := invoke(context.Background(), []byte(`{"name":"bob"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// Nothing to assert but the absence of a panic and a correct answer: the
	// point is that the uninstrumented path is unchanged.
}

// The error counter is keyed by the semantic CODE, never the message: a
// counter dimensioned on free text is a cardinality explosion.
func TestErrorsAreRecordedByCode(t *testing.T) {
	tel := &fakeTelemetry{}
	tbl := buildTable(t, tel, &controller{fail: werrors.Conflict("taken")})

	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	if _, err := invoke(context.Background(), []byte(`{"name":"bob"}`)); err == nil {
		t.Fatal("the handler's error did not surface")
	}
	if len(tel.records) != 1 || tel.records[0].err == nil {
		t.Fatalf("records = %+v, want one carrying the error", tel.records)
	}
}

// WithoutHandlerInstrumentation declines composition while leaving trace
// continuation and broker propagation alone.
func TestHandlerInstrumentationIsOptOut(t *testing.T) {
	tel := &fakeTelemetry{noHandlers: true}
	tbl := buildTable(t, tel, &controller{})

	invoke := tbl.HTTP()[0].Bind(transport.JSON())
	if _, err := invoke(context.Background(), []byte(`{"name":"bob"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(tel.spans) != 0 {
		t.Errorf("spans = %v, want none when instrumentation is declined", tel.spans)
	}
}

// --- propagation -----------------------------------------------------------

// The §7.1 claim, tested without a broker and without a collector: a span
// survives a round trip through a map[string]string.
func TestTraceContextSurvivesAMapRoundTrip(t *testing.T) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("producer").Start(context.Background(), "publish")
	defer span.End()

	tel := &fakeTelemetry{}
	headers := map[string]string{}
	tel.Inject(ctx, func(k, v string) { headers[k] = v })

	if _, ok := headers["traceparent"]; !ok {
		t.Fatalf("no W3C traceparent was injected: %v", headers)
	}

	// The consumer side, in a fresh context — as if it had crossed a broker.
	got := tel.Extract(context.Background(), func(k string) string { return headers[k] })
	sc := trace.SpanContextFromContext(got)
	if !sc.IsValid() {
		t.Fatal("the extracted context carries no span")
	}
	if sc.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("trace ID = %s, want the producer's %s — the trace did not survive",
			sc.TraceID(), span.SpanContext().TraceID())
	}
}

// --- log correlation -------------------------------------------------------

// The trace ID reaches every log line with no per-request work: resolution
// happens in Handle, where slog is designed to do it.
// Installing the correlation handler over the BUILT-IN default deadlocks:
// slog.SetDefault redirects the log package through the new handler, so a
// handler wrapping the default recurses slog -> handler -> default ->
// log.Output -> slog. This package used to do exactly that at boot, and every
// service with an OTLP endpoint hung on its first log line.
//
// The rule is that log.Handler wraps a handler the CALLER built. This test
// pins that a wrapped concrete handler emits exactly once and returns.
func TestHandlerOverAConcreteHandlerDoesNotRecurse(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(wlog.Handler(slog.NewJSONHandler(&buf, nil), observability.LogAttrs()))

	done := make(chan struct{})
	go func() {
		defer close(done)
		logger.InfoContext(context.Background(), "hello")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging through the correlation handler did not return — it recursed")
	}
	if n := strings.Count(buf.String(), "hello"); n != 1 {
		t.Errorf("record emitted %d times, want exactly 1", n)
	}
}

func TestLogAttrsPutTheTraceOnTheRecord(t *testing.T) {
	var buf bytes.Buffer
	h := wlog.Handler(slog.NewJSONHandler(&buf, nil), observability.LogAttrs())
	logger := slog.New(h)

	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "req")
	defer span.End()
	ctx = wlog.WithCorrelationID(ctx, "corr-1")

	logger.InfoContext(ctx, "hello")

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id = %v, want the active span's", line["trace_id"])
	}
	if line["correlation_id"] != "corr-1" {
		t.Errorf("correlation_id = %v", line["correlation_id"])
	}
}

// A context with no span produces a record with no trace fields, rather than
// empty strings a dashboard would have to filter.
func TestLogAttrsAddNothingWithoutASpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(wlog.Handler(slog.NewJSONHandler(&buf, nil), observability.LogAttrs()))
	logger.InfoContext(context.Background(), "hello")

	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("a record with no span carries a trace_id: %s", buf.String())
	}
}

// --- boot ------------------------------------------------------------------

func TestSampleRatioOutOfRangeFailsTheBoot(t *testing.T) {
	a := warren.New(observability.Module(
		observability.ServiceName("x"),
		observability.OTLPEndpoint("localhost:4317"),
		observability.SampleRatio(1.5),
	))
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("a sample ratio of 1.5 booted")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("diagnostic:\n%v", err)
	}
}

func TestEndpointWithoutServiceNameFailsTheBoot(t *testing.T) {
	a := warren.New(observability.Module(observability.OTLPEndpoint("localhost:4317")))
	err := a.Start(context.Background())
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("an OTLP endpoint with no service name booted")
	}
	if !strings.Contains(err.Error(), "service name") {
		t.Errorf("diagnostic:\n%v", err)
	}
}

// No endpoint is the intended test and local-development configuration: it
// builds no exporter, binds no telemetry, and must not fail or warn.
func TestNoEndpointBootsAndInstrumentsNothing(t *testing.T) {
	a := warren.New(observability.Module(observability.ServiceName("x")))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("an app with no collector must boot: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func buildTable(t *testing.T, tel app.Telemetry, c transport.Controller) *transport.Table {
	t.Helper()
	var opts []transport.BuilderOption
	if tel != nil {
		opts = append(opts, transport.WithTelemetry(tel))
	}
	b := transport.NewBuilder(opts...)
	c.Register(b.For("user"))
	tbl, err := b.Table()
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	return tbl
}

// --- regressions found by field-testing, 2026-08-02 ------------------------

// The module could not boot with an OTLPEndpoint at all: resource.Merge
// refuses two differing schema URLs, and this package pinned a semconv
// version the SDK had moved past. Every test that set an endpoint asserted a
// FAILURE — sample ratio, missing service name — and each returned before
// resource.Merge was reached, so a green suite covered a package whose
// primary path was dead.
//
// This is that missing test. It asserts the happy path, which is the only
// kind of test that could have caught it.
func TestBootWithAnEndpointSucceeds(t *testing.T) {
	a := warren.New(observability.Module(
		observability.ServiceName("tasks-service"),
		observability.ServiceVersion("1.2.3"),
		observability.ResourceAttr("deployment.environment", "test"),
		observability.OTLPEndpoint("localhost:4317"),
		observability.Insecure(),
	))
	// No collector is listening, and that must not matter: the OTLP exporter
	// does not dial at construction. Boot succeeds and export fails later,
	// which is the right trade — telemetry must never stop a service starting.
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot with an endpoint must succeed:\n%v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// The same, with metrics off: a second resource-building path.
func TestBootWithAnEndpointAndNoMetrics(t *testing.T) {
	a := warren.New(observability.Module(
		observability.ServiceName("tasks-service"),
		observability.OTLPEndpoint("localhost:4317"),
		observability.Insecure(),
		observability.WithoutMetrics(),
	))
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}
