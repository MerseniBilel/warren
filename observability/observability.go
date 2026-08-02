// Package observability wires OpenTelemetry into a Warren application: every
// handler gets a span and a duration histogram, every HTTP request continues
// the caller's trace, and trace context travels through a broker's message
// headers — by importing this module and listing it in warren.New.
//
// # It wraps setup, not the API
//
// No OpenTelemetry type appears in any signature here. This module installs
// the global TracerProvider, MeterProvider and propagator, and a user who
// wants a manual span reaches for OTel directly:
//
//	ctx, span := otel.Tracer("billing").Start(ctx, "settle")
//
// — holding the SDK by choice, one call site at a time. That is what keeps
// invariant 3 intact without granting OTel an exemption from it.
//
// # It implements app.Telemetry
//
// app.Traced and app.Metered are core middleware that speak to the
// app.Telemetry seam and are exact pass-throughs when nothing is bound. This
// module binds one, and boot step 5 composes them around every route. The
// kernel never learns a telemetry SDK exists.
//
// # What it costs
//
// Measured 2026-08-02: 15 third-party modules, including google.golang.org/grpc
// and protobuf. That is the going rate for OTLP in Go — the HTTP exporter
// reaches grpc through its own config package, so it is not lighter — and it
// is why this is an opt-in module of its own. A service that does not import
// it pays nothing.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/log"
)

// ModuleName is the name of the module Module returns — the scope name that
// appears in boot diagnostics.
const ModuleName = "warren/observability"

// Option configures the observability module.
type Option struct{ apply func(*config) }

type config struct {
	serviceName    string
	serviceVersion string
	resourceAttrs  []attribute.KeyValue
	endpoint       string
	insecure       bool
	headers        map[string]string
	sampleRatio    float64
	metricInterval time.Duration
	exportTimeout  time.Duration

	noMetrics        bool
	noHandlerInstr   bool
	noLogCorrelation bool
}

func defaults() config {
	return config{
		sampleRatio:    1,
		metricInterval: 60 * time.Second,
		exportTimeout:  10 * time.Second,
	}
}

// Module returns a Warren module that installs tracing and metrics across
// every instrumented boundary: one span and one duration histogram per
// handler invocation, a continued trace per HTTP request, and trace context
// carried into and out of broker.Message.Headers.
//
// LIST IT FIRST in warren.New. Declared shutdown hooks unwind in reverse
// module order, so a module listed first flushes LAST — after consumers have
// stopped, after the outbox relay, after the pool closes. Spans emitted
// during shutdown are the ones worth having, and flushing earlier drops
// exactly the teardown bugs telemetry exists to catch.
//
// Database query spans are the one boundary this module cannot reach on its
// own: the pgx tracer seam is a pgx type, and an adapter may not import
// another adapter. One line in main, beside the postgres module:
//
//	postgres.Configure(func(c *pgxpool.Config) error {
//	    c.ConnConfig.Tracer = otelpgx.NewTracer()
//	    return nil
//	})
//
// A missing or empty OTLPEndpoint builds no exporter and binds no telemetry:
// boot composes no instrumentation and the request path costs exactly what an
// uninstrumented service costs. That is the intended configuration for tests
// and for local runs without a collector — not an error, and not a warning on
// every boot.
func Module(opts ...Option) warren.Module {
	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}
	return warren.NewModule(ModuleName,
		warren.Providers(func(lc lifecycle.Lifecycle) (app.Telemetry, error) {
			return newTelemetry(cfg, lc)
		}),
		warren.Exports[app.Telemetry](),
		// Eager so a misconfiguration fails the boot even when nothing
		// injects it — the bootstrapper resolves it before step 5 anyway, but
		// an app with no routes at all would otherwise never build it.
		warren.Eager[app.Telemetry](),
	)
}

// ServiceName sets the service name reported on every span and metric. It is
// required: an OTLP resource with no service.name is unroutable at the
// collector, so an empty name fails the boot rather than exporting telemetry
// nobody can find.
func ServiceName(name string) Option {
	return Option{apply: func(c *config) { c.serviceName = name }}
}

// ServiceVersion sets service.version on the resource. Without it a trace
// cannot be attributed to a deploy, which is the first question asked of any
// latency regression.
func ServiceVersion(version string) Option {
	return Option{apply: func(c *config) { c.serviceVersion = version }}
}

// ResourceAttr adds one attribute to the OTLP resource — deployment
// environment, region, tenant. Repeatable.
func ResourceAttr(key, value string) Option {
	return Option{apply: func(c *config) {
		c.resourceAttrs = append(c.resourceAttrs, attribute.String(key, value))
	}}
}

// OTLPEndpoint sets the OTLP/gRPC collector address, "host:port", for both
// traces and metrics. Empty — the default — disables export entirely; see
// Module.
//
// The exporter is OTLP over gRPC and is not swappable. Measured 2026-08-02:
// the gRPC and HTTP exporters compile the same 15 third-party modules,
// because otlptracehttp reaches google.golang.org/grpc through its own config
// package regardless. One transport means one code path to test.
func OTLPEndpoint(addr string) Option {
	return Option{apply: func(c *config) { c.endpoint = addr }}
}

// Insecure disables TLS to the collector. It is the right setting for a
// sidecar or a collector on the cluster network, and the wrong one for
// anything crossing a public boundary.
func Insecure() Option {
	return Option{apply: func(c *config) { c.insecure = true }}
}

// OTLPHeader adds a header to every export request — the API-key mechanism
// every hosted OTLP backend uses. Repeatable.
func OTLPHeader(key, value string) Option {
	return Option{apply: func(c *config) {
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[key] = value
	}}
}

// SampleRatio sets the head sampling ratio, 0 to 1 inclusive; the default
// is 1. A value outside the range fails the boot, not request one.
//
// Sampling is parent-based: a request arriving with a sampled traceparent is
// sampled whatever this says, so a trace is never half-recorded across a
// service boundary. The ratio decides only the traces this service starts.
func SampleRatio(ratio float64) Option {
	return Option{apply: func(c *config) { c.sampleRatio = ratio }}
}

// MetricInterval is how often metrics are pushed to the collector. The
// default is 60s, OTel's own; lowering it multiplies the export bill without
// making a histogram any sharper.
func MetricInterval(d time.Duration) Option {
	return Option{apply: func(c *config) { c.metricInterval = d }}
}

// ExportTimeout bounds one export attempt and the shutdown flush. The default
// is 10s. A flush that exceeds it logs and returns nil: a saturated collector
// must not hold a pod in Terminating past its grace period.
func ExportTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.exportTimeout = d }}
}

// WithoutMetrics installs tracing only. app.Metered then costs nothing and
// the meter provider is never built.
func WithoutMetrics() Option {
	return Option{apply: func(c *config) { c.noMetrics = true }}
}

// WithoutHandlerInstrumentation stops boot composing app.Traced and
// app.Metered around every route. Transport-level trace continuation and
// broker propagation are unaffected. Use it when handler middleware is
// composed by hand.
func WithoutHandlerInstrumentation() Option {
	return Option{apply: func(c *config) { c.noHandlerInstr = true }}
}

// WithoutLogCorrelation stops this module re-wrapping slog.Default's handler
// at boot. By default it wraps it with log.Handler and LogAttrs, so every
// record emitted inside a request carries correlation_id, trace_id and
// span_id with no per-request work at all. Set this when main builds its own
// log.Handler.
func WithoutLogCorrelation() Option {
	return Option{apply: func(c *config) { c.noLogCorrelation = true }}
}

// newTelemetry builds the providers and registers the flush. It returns a nil
// app.Telemetry when no endpoint is configured — app.WithTelemetry's
// typed-nil probe and boot's own nil check both treat that as "uninstrumented",
// so the request path is unchanged.
func newTelemetry(cfg config, lc lifecycle.Lifecycle) (app.Telemetry, error) {
	if cfg.sampleRatio < 0 || cfg.sampleRatio > 1 {
		return nil, errBadSampleRatio(cfg.sampleRatio)
	}
	// Propagation and log correlation are free and useful even with no
	// exporter: a service behind a mesh still continues an inbound trace, and
	// still writes the trace ID on its log lines.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !cfg.noLogCorrelation {
		slog.SetDefault(slog.New(log.Handler(slog.Default().Handler(), LogAttrs())))
	}

	if cfg.endpoint == "" {
		return nil, nil
	}
	if cfg.serviceName == "" {
		return nil, errNoServiceName()
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		append([]attribute.KeyValue{
			semconv.ServiceName(cfg.serviceName),
			semconv.ServiceVersion(cfg.serviceVersion),
		}, cfg.resourceAttrs...)...,
	))
	if err != nil {
		return nil, fmt.Errorf("observability: building the OTLP resource: %w", err)
	}

	t := &telemetry{cfg: cfg}
	lc.Append(lifecycle.Hook{
		Name:    ModuleName,
		OnStart: func(ctx context.Context) error { return t.start(ctx, res) },
		OnStop:  t.stop,
	})
	return t, nil
}

// start builds the exporters and installs the global providers. It is an
// OnStart, not a constructor: an exporter opens a connection, and the
// lifecycle is what can roll that back if a later hook fails.
func (t *telemetry) start(ctx context.Context, res *resource.Resource) error {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.exportTimeout)
	defer cancel()

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(t.cfg.endpoint)}
	if t.cfg.insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	if len(t.cfg.headers) > 0 {
		traceOpts = append(traceOpts, otlptracegrpc.WithHeaders(t.cfg.headers))
	}
	spanExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return errCannotExport(t.cfg.endpoint, err)
	}
	t.tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExp),
		sdktrace.WithResource(res),
		// Parent-based: an inbound sampled traceparent is honoured whatever
		// the ratio says, so a trace is never half-recorded across a service
		// boundary.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(t.cfg.sampleRatio))),
	)
	otel.SetTracerProvider(t.tracerProvider)
	t.tracer = t.tracerProvider.Tracer(ModuleName)

	if !t.cfg.noMetrics {
		metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(t.cfg.endpoint)}
		if t.cfg.insecure {
			metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		}
		if len(t.cfg.headers) > 0 {
			metricOpts = append(metricOpts, otlpmetricgrpc.WithHeaders(t.cfg.headers))
		}
		metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
		if err != nil {
			return errCannotExport(t.cfg.endpoint, err)
		}
		t.meterProvider = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
				sdkmetric.WithInterval(t.cfg.metricInterval))),
		)
		otel.SetMeterProvider(t.meterProvider)
		if err := t.buildInstruments(); err != nil {
			return err
		}
	}

	slog.Info("telemetry exporting",
		"module", ModuleName,
		"endpoint", t.cfg.endpoint,
		"service", t.cfg.serviceName,
		"sample_ratio", t.cfg.sampleRatio,
		"metrics", !t.cfg.noMetrics)
	return nil
}

// stop flushes. It runs LAST — see Module — because the spans emitted while
// everything else shuts down are the ones nobody can reproduce.
//
// A flush that exceeds ExportTimeout logs and returns nil: a saturated
// collector must not hold a pod in Terminating past its grace period.
func (t *telemetry) stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.exportTimeout)
	defer cancel()

	if t.meterProvider != nil {
		if err := t.meterProvider.Shutdown(ctx); err != nil {
			slog.Warn("telemetry: metric flush did not complete", "module", ModuleName, "error", err)
		}
	}
	if t.tracerProvider != nil {
		if err := t.tracerProvider.Shutdown(ctx); err != nil {
			slog.Warn("telemetry: span flush did not complete", "module", ModuleName, "error", err)
		}
	}
	return nil
}
