// Package http exposes Warren handlers over HTTP. It is the only package in
// an HTTP service that imports net/http, and it imports nothing else that is
// not the core module — a service that serves HTTP has two modules in its
// go.mod and no third party at all.
//
// The router is net/http.ServeMux, deliberately and not swappably. The sealed
// transport.Registrar already owns decode, validation, parameter binding and
// status defaults, so the adapter's entire use of a router is to match a
// method and pattern to one pre-built closure and hand back the path
// segments — which is exactly what ServeMux has done since Go 1.22, at half
// chi's allocations and none of its dependencies.
//
// Everything is decided at boot. By the time a request arrives the mux holds
// closures with middleware already composed, the DI container is never
// consulted, and nothing is resolved.
package http

import (
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/MerseniBilel/warren"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
)

// ModuleName is the name of the module Server returns — the scope name that
// appears in boot diagnostics.
const ModuleName = "warren/transport/http"

// Option configures the server.
type Option struct{ apply func(*config) }

type config struct {
	addr              string
	listener          net.Listener
	middleware        []func(http.Handler) http.Handler
	extra             []handleRoute
	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	maxHeaderBytes    int
	maxBodyBytes      int64
	drainDelay        time.Duration
	shutdownTimeout   time.Duration
	tls               *tls.Config
	certFile, keyFile string
	h2c               bool
	codec             transport.Codec
}

type handleRoute struct {
	pattern string
	handler http.Handler
}

func defaults() config {
	return config{
		addr:              ":8080",
		readHeaderTimeout: 10 * time.Second,
		readTimeout:       30 * time.Second,
		writeTimeout:      0, // see WriteTimeout
		idleTimeout:       120 * time.Second,
		maxHeaderBytes:    1 << 20,
		maxBodyBytes:      1 << 20,
		drainDelay:        5 * time.Second,
		shutdownTimeout:   15 * time.Second,
		codec:             transport.JSON(),
	}
}

// Server returns a warren.Module that serves the application's registered
// HTTP routes — transport.Table.HTTP() and the ProtocolHTTP entries of
// Table.Raw(), both frozen at boot step 5 — over a net/http.ServeMux.
//
// Its constructor claims transport.ProtocolHTTP, so an application that
// registers HTTP routes without this module fails the boot rather than 404ing
// in production. It registers a lifecycle hook: the listener opens after every
// dependency has started, and on shutdown it waits DrainDelay after readiness
// closed — so the load balancer observes the 503 before the listener goes
// away — and then drains in-flight requests.
func Server(opts ...Option) warren.Module {
	cfg := defaults()
	for _, o := range opts {
		o.apply(&cfg)
	}
	return warren.NewModule(ModuleName,
		warren.Providers(func(tbl *transport.Table, lc lifecycle.Lifecycle, reg health.Registry) (*server, error) {
			return newServer(cfg, tbl, lc, reg)
		}),
		// Eager: nothing in a user's graph depends on the server, and boot
		// builds eager singletons last — after step 5 has filled the table,
		// and after every other module's lifecycle hooks. Servers start last.
		warren.Eager[*server](),
	)
}

// Port sets the TCP port the server listens on. The default is 8080.
func Port(n int) Option {
	return Option{apply: func(c *config) { c.addr = ":" + strconv.Itoa(n) }}
}

// Addr sets the full listen address, "host:port", and overrides Port. The
// default host is empty — every interface. Bind "127.0.0.1:8080" to serve
// loopback only.
func Addr(addr string) Option {
	return Option{apply: func(c *config) { c.addr = addr }}
}

// Listener serves l instead of binding an address, and overrides Addr and
// Port. It exists so a test can bind port 0 and still know the address;
// production code has no reason to reach for it.
func Listener(l net.Listener) Option {
	return Option{apply: func(c *config) { c.listener = l }}
}

// Middleware appends edge middleware, in the standard library's
// func(http.Handler) http.Handler shape, so anything written for net/http —
// chi/v5/middleware included — runs unmodified. It applies to every typed and
// raw route, to the 404 and 405 responses, and to nothing else: the health
// probes bypass it.
//
// It runs after recover and correlation-ID seeding, in argument order, and
// before the handler. It is global in v0.1; per-route authorization is
// transport.Guard, which runs before decode.
//
// Core middleware — the kind that must also apply to gRPC and consumers —
// belongs in app, not here.
func Middleware(mw ...func(http.Handler) http.Handler) Option {
	return Option{apply: func(c *config) { c.middleware = append(c.middleware, mw...) }}
}

// ReadHeaderTimeout bounds reading the request headers. The default is 10s.
// An unbounded header read is Slowloris.
func ReadHeaderTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.readHeaderTimeout = d }}
}

// ReadTimeout bounds reading the entire request. The default is 30s.
func ReadTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.readTimeout = d }}
}

// WriteTimeout bounds writing the response. The default is 0 — deliberately
// off, because a global write timeout kills server-sent events and large
// downloads. Set it per deployment when every route is a short command.
func WriteTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.writeTimeout = d }}
}

// IdleTimeout bounds a keep-alive connection between requests. The default is
// 120s.
func IdleTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.idleTimeout = d }}
}

// MaxHeaderBytes bounds the request headers. The default is 1 MiB.
func MaxHeaderBytes(n int) Option {
	return Option{apply: func(c *config) { c.maxHeaderBytes = n }}
}

// MaxBodyBytes bounds the request body of a typed route. The default is 1 MiB:
// transport.Invoker takes the whole body as a []byte, so an unbounded body is
// a one-request OOM. A body over the limit is INVALID — 400, not 413, because
// the error table is the only thing that chooses a status and its codes are a
// closed set clients switch on.
//
// Raw routes are exempt and own their own limit: an upload is the reason they
// exist.
func MaxBodyBytes(n int64) Option {
	return Option{apply: func(c *config) { c.maxBodyBytes = n }}
}

// DrainDelay is how long the stop hook waits, interruptibly, between readiness
// closing and the listener stopping. The default is 5s: a load balancer polls
// /readyz on an interval and keeps routing for up to one full poll period
// after the 503, so stopping the listener immediately drops those requests.
func DrainDelay(d time.Duration) Option {
	return Option{apply: func(c *config) { c.drainDelay = d }}
}

// ShutdownTimeout bounds the whole stop hook — DrainDelay plus the drain. The
// default is 15s, which must stay inside lifecycle's 30s force-exit deadline.
//
// Server.Shutdown does not cancel in-flight request contexts, so a handler
// that ignores ctx blocks the drain until this expires.
func ShutdownTimeout(d time.Duration) Option {
	return Option{apply: func(c *config) { c.shutdownTimeout = d }}
}

// TLS serves HTTPS with cfg. *tls.Config is the standard library, not a driver
// type, and it is the option an mTLS mesh needs; TLSFiles is the two-file
// shorthand. TLS implies HTTP/2.
func TLS(cfg *tls.Config) Option {
	return Option{apply: func(c *config) { c.tls = cfg }}
}

// TLSFiles serves HTTPS from a certificate and a key file.
func TLSFiles(certFile, keyFile string) Option {
	return Option{apply: func(c *config) { c.certFile, c.keyFile = certFile, keyFile }}
}

// H2C accepts unencrypted HTTP/2 alongside HTTP/1.1, through
// http.Protocols.SetUnencryptedHTTP2. Service meshes speak it, and since Go
// 1.24 it needs no golang.org/x/net.
func H2C() Option {
	return Option{apply: func(c *config) { c.h2c = true }}
}

// Handle registers h at pattern directly on the mux, for handlers that need no
// module-scoped dependency: net/http/pprof, static assets, a vendor SDK's
// webhook receiver. It gets the edge ring, the correlation ID and the drain,
// and no decode, validate or encode.
//
// A raw handler that needs a repository or a storage port is registered from
// the controller instead — transport.Raw(r, transport.ProtocolHTTP,
// "POST /uploads", h) — so that the module's own container builds it.
func Handle(pattern string, h http.Handler) Option {
	return Option{apply: func(c *config) {
		c.extra = append(c.extra, handleRoute{pattern: pattern, handler: h})
	}}
}

// Codec chooses the codec every typed route on this server decodes and
// encodes with. The default is transport.JSON().
//
// The one Warren ships as an alternative is transport.StrictJSON(), which
// rejects a body carrying a member the request type does not declare:
//
//	http.Server(http.Codec(transport.StrictJSON()))
//
// That is a decision about your API's compatibility policy, which is why it
// is a line in main.go rather than a default. It costs 3 allocations per
// request, and it means a client that adds a field before this server is
// deployed gets a 400 rather than being ignored — right when every HTTP
// client is first-party and versioned with the server, wrong when they are
// not.
//
// It applies to HTTP routes only. The event path always decodes leniently
// and there is no option here that changes it: a decode failure on a
// consumer is INVALID, which dead-letters without retry, so a strict event
// codec turns a producer's additive change into a dead-letter storm.
//
// A nil codec is ignored rather than installed, because a nil one would
// panic on the first request instead of at boot.
func Codec(c transport.Codec) Option {
	return Option{apply: func(cfg *config) {
		if c != nil {
			cfg.codec = c
		}
	}}
}
