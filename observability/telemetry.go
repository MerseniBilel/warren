package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/MerseniBilel/warren/app"
	werrors "github.com/MerseniBilel/warren/errors"
	"github.com/MerseniBilel/warren/log"
)

// telemetry implements app.Telemetry over OpenTelemetry. It is the only
// implementation of that seam in the framework, and the only place a span or
// a histogram is created.
type telemetry struct {
	cfg config

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	tracer         trace.Tracer

	duration metric.Float64Histogram
	failures metric.Int64Counter
}

var (
	_ app.Telemetry              = (*telemetry)(nil)
	_ app.HandlerInstrumentation = (*telemetry)(nil)
	_ app.RequestSpan            = (*telemetry)(nil)
)

// ServerSpan opens the transport-level SERVER span around a whole request.
//
// SpanKind Server is what makes a trace UI render this service as an entry
// point rather than an internal step, and the http.* attributes are what let
// you filter by route and by status. Without them a trace answers only
// "which handler was slow" — the handler span nested inside this one — and
// not "which route", "which status", or "how much of the latency was
// transport".
//
// The span is named "<METHOD> <route>", the OTel HTTP server convention, and
// route is the matched PATTERN, so cardinality is bounded by the size of the
// route table rather than by traffic.
func (t *telemetry) ServerSpan(ctx context.Context, info app.RequestInfo) (context.Context, func(int, error)) {
	if t.tracer == nil {
		return ctx, func(int, error) {}
	}
	name := info.Method
	if info.Route != "" {
		name += " " + info.Route
	}
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(info.Method),
		semconv.URLScheme(info.Scheme),
		semconv.URLPath(info.Path),
	}
	if info.Route != "" {
		attrs = append(attrs, semconv.HTTPRoute(info.Route))
	}
	if info.Host != "" {
		attrs = append(attrs, semconv.ServerAddress(info.Host))
	}
	ctx, span := t.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...))

	return ctx, func(status int, err error) {
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		// Only 5xx marks the SPAN as failed. A 404 or a 409 is the server
		// working correctly, and marking those Error makes every error-rate
		// dashboard read the client's mistakes as the service's.
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
}

// InstrumentHandlers reports whether boot composes Traced and Metered around
// every route — false when WithoutHandlerInstrumentation was passed. Trace
// continuation at the edge and broker propagation are unaffected either way.
func (t *telemetry) InstrumentHandlers() bool { return !t.cfg.noHandlerInstr }

func (t *telemetry) buildInstruments() error {
	m := t.meterProvider.Meter(ModuleName)
	var err error
	t.duration, err = m.Float64Histogram("warren.handler.duration",
		metric.WithUnit("s"),
		metric.WithDescription("handler invocation duration"))
	if err != nil {
		return fmt.Errorf("observability: building the duration histogram: %w", err)
	}
	t.failures, err = m.Int64Counter("warren.handler.errors",
		metric.WithDescription("handler invocations that returned an error, by warren/errors code"))
	if err != nil {
		return fmt.Errorf("observability: building the error counter: %w", err)
	}
	return nil
}

// Span opens a span named name — "<module>.<handler>", the low-cardinality
// label a metrics backend can group by — and returns the derived context and
// the function that ends it.
//
// The returned context derives from ctx, preserving its values, which
// app.Telemetry's own contract requires: Metered and HandlerName read through
// it downstream.
func (t *telemetry) Span(ctx context.Context, name string) (context.Context, func(err error)) {
	if t.tracer == nil {
		return ctx, func(error) {}
	}
	ctx, span := t.tracer.Start(ctx, name)
	return ctx, func(err error) {
		if err != nil {
			// The semantic code, not the message: a span attribute is a
			// dimension, and a message is unbounded cardinality.
			span.SetAttributes(attribute.String("warren.error.code", string(codeOf(err))))
			span.SetStatus(codes.Error, string(codeOf(err)))
			span.RecordError(err)
		}
		span.End()
	}
}

// Record records one handler invocation: name, duration, and the error, nil
// on success.
//
// The error counter is keyed by the warren/errors CODE — the closed
// seven-value set — never by the message. A counter dimensioned on free text
// is a cardinality explosion, and the code is what an alert should fire on
// anyway.
func (t *telemetry) Record(name string, d time.Duration, err error) {
	if t.duration == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("handler", name))
	t.duration.Record(context.Background(), d.Seconds(), attrs)
	if err != nil && t.failures != nil {
		t.failures.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("handler", name),
			attribute.String("code", string(codeOf(err))),
		))
	}
}

// Inject writes the trace context on ctx through set, one key at a time.
//
// The carrier is a function, so an HTTP header map, a broker.Message.Headers
// and gRPC metadata are all reachable without an intermediate allocation —
// and so core never names a header key. The keys here are W3C's
// (traceparent, tracestate, baggage), which is what every collector, sidecar
// and hosted backend already speaks.
func (t *telemetry) Inject(ctx context.Context, set func(key, value string)) {
	otel.GetTextMapPropagator().Inject(ctx, setterCarrier(set))
}

// Extract returns a context continuing the trace the pairs describe, or ctx
// unchanged when there is none.
func (t *telemetry) Extract(ctx context.Context, get func(key string) string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, getterCarrier(get))
}

// setterCarrier and getterCarrier adapt the core seam's plain functions to
// OTel's TextMapCarrier. Keys() is unused on the inject path and returns nil
// on the extract path because W3C propagation asks for named keys, never
// enumerates.
type setterCarrier func(key, value string)

func (s setterCarrier) Get(string) string { return "" }
func (s setterCarrier) Set(k, v string)   { s(k, v) }
func (s setterCarrier) Keys() []string    { return nil }

type getterCarrier func(key string) string

func (g getterCarrier) Get(k string) string { return g(k) }
func (g getterCarrier) Set(string, string)  {}
func (g getterCarrier) Keys() []string      { return nil }

// codeOf is the semantic classification an error carries, or INTERNAL — the
// same default every adapter applies to an error outside the vocabulary.
func codeOf(err error) werrors.Code {
	var werr *werrors.Error
	if errors.As(err, &werr) {
		return werr.Code()
	}
	return werrors.CodeInternal
}

// LogAttrs returns the extractor that puts trace_id and span_id on a log
// record, for a main that builds its own handler:
//
//	slog.SetDefault(slog.New(log.Handler(
//	    slog.NewJSONHandler(os.Stdout, nil), observability.LogAttrs())))
//
// Module installs this for you unless you pass WithoutLogCorrelation.
//
// The return type is log.ContextAttrs, a core function type — no
// OpenTelemetry type crosses this boundary. Reading the span context off ctx
// is a value lookup that allocates nothing, and the IDs are formatted only
// when a record is actually emitted, above the level threshold. That is the
// whole reason the correlation seam is a slog.Handler rather than a logger
// derived at the edge: log.With costs 8 allocations on every request,
// measured, whether or not the request logs anything.
func LogAttrs() log.ContextAttrs {
	return func(ctx context.Context, add func(slog.Attr)) {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return
		}
		add(slog.String("trace_id", sc.TraceID().String()))
		add(slog.String("span_id", sc.SpanID().String()))
	}
}
