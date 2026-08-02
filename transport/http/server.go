package http

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
)

// server is the eager singleton the module builds: a mux of pre-composed
// closures, and a lifecycle hook that opens and drains the listener.
type server struct {
	cfg  config
	mux  *http.ServeMux
	http *http.Server
	ln   net.Listener

	// serveErr carries a listener failure from the serving goroutine to
	// shutdown, so a port already in use surfaces as an error rather than a
	// line on stderr.
	serveErr atomic.Pointer[error]
}

func newServer(cfg config, tbl *transport.Table, lc lifecycle.Lifecycle, reg health.Registry) (*server, error) {
	tbl.Claim(transport.ProtocolHTTP, ModuleName)

	s := &server{cfg: cfg, mux: http.NewServeMux()}
	if err := s.build(tbl, reg); err != nil {
		return nil, err
	}

	s.http = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		ReadTimeout:       cfg.readTimeout,
		WriteTimeout:      cfg.writeTimeout,
		IdleTimeout:       cfg.idleTimeout,
		MaxHeaderBytes:    cfg.maxHeaderBytes,
		// net/http writes connection-level errors to stderr unless told
		// otherwise; route them through warren/log so one service has one
		// log stream.
		ErrorLog: connErrorLog(),
	}
	if cfg.tls != nil {
		s.http.TLSConfig = cfg.tls
	}
	if cfg.h2c {
		// Since Go 1.24 unencrypted HTTP/2 needs no golang.org/x/net.
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		s.http.Protocols = p
	}

	lc.Append(lifecycle.Hook{
		Name:    ModuleName,
		OnStart: s.start,
		OnStop:  s.stop,
	})
	return s, nil
}

// build registers every pattern on the mux. ServeMux PANICS on a conflicting
// pattern rather than returning an error — its message is excellent (both
// patterns, both registration sites) but AGENT.md forbids a panic reaching a
// user, so every registration is recovered and re-rendered as a Warren boot
// diagnostic.
func (s *server) build(tbl *transport.Table, reg health.Registry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errPatternConflict(r)
		}
	}()

	// Typed routes: decode, bind, validate, handler, encode — all of it
	// composed at boot into one closure per route.
	verbs := map[string][]string{}
	for _, rt := range tbl.HTTP() {
		s.mux.Handle(rt.Verb+" "+rt.Pattern, s.wrap(s.typed(rt)))
		verbs[rt.Pattern] = append(verbs[rt.Pattern], rt.Verb)
	}

	// Raw routes: the escape hatch. The handler is opaque to core, so this is
	// where the type assertion happens — and where it fails the boot.
	for _, rr := range tbl.Raw() {
		if rr.Protocol != transport.ProtocolHTTP {
			continue
		}
		h, ok := rr.Handler.(http.Handler)
		if !ok {
			return errNotAHandler(rr)
		}
		s.mux.Handle(rr.Pattern, s.wrap(s.raw(rr, h)))
	}

	// Handle(...) options: the main-side door, for handlers with no
	// module-scoped dependency.
	for _, e := range s.cfg.extra {
		s.mux.Handle(e.pattern, s.wrap(e.handler))
	}

	// 405 with a computed Allow. ServeMux already returns 405 with a correct
	// Allow for free — but registering this method-less shim to render the
	// JSON envelope DESTROYS that, so the shim computes Allow itself,
	// including the implicit HEAD that every GET pattern provides.
	rootAllow := ""
	for pattern, vs := range verbs {
		allow := allowHeader(vs)
		if pattern == "/" {
			// "/" is also the catch-all; one handler must serve both.
			rootAllow = allow
			continue
		}
		s.mux.Handle(pattern, s.wrap(s.methodNotAllowed(allow)))
	}

	// The catch-all: a JSON 404 envelope instead of net/http's bare text. It
	// also answers a wrong method on "/" itself, which has no shim of its own
	// because its pattern is this one.
	s.mux.Handle("/", s.wrap(s.notFound(rootAllow)))

	// Probes bypass the edge ring — except recover, because a panicking check
	// must not kill the process that is answering the probe. No auth, no rate
	// limit, no tracing: a span every two seconds is a telemetry bill, not a
	// signal.
	s.mux.Handle("GET /healthz", s.recoverer(probe(reg.Live)))
	s.mux.Handle("GET /readyz", s.recoverer(probe(reg.Ready)))
	return nil
}

// wrap composes the edge ring around one handler. It is applied at boot, per
// route, rather than around the whole mux — which is what lets the probes
// opt out of everything but recover while every other route still pays for
// exactly one mux dispatch.
//
// Order: recover (outermost, not removable) → correlation ID → user
// middleware in argument order → the handler. User middleware cannot precede
// correlation-ID seeding or its log lines would have none.
func (s *server) wrap(h http.Handler) http.Handler {
	for i := len(s.cfg.middleware) - 1; i >= 0; i-- {
		h = s.cfg.middleware[i](h)
	}
	return s.recoverer(s.correlate(h))
}

func (s *server) start(context.Context) error {
	ln := s.cfg.listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.cfg.addr)
		if err != nil {
			return fmt.Errorf("warren/transport/http: listening on %s: %w", s.cfg.addr, err)
		}
	}
	s.ln = ln

	go func() {
		var err error
		switch {
		case s.cfg.certFile != "" || s.cfg.keyFile != "":
			err = s.http.ServeTLS(ln, s.cfg.certFile, s.cfg.keyFile)
		case s.cfg.tls != nil:
			err = s.http.ServeTLS(ln, "", "")
		default:
			err = s.http.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr.Store(&err)
		}
	}()
	return nil
}

// stop is shutdown steps 9b and 10. Readiness has already closed — lifecycle
// does that first — so this waits DrainDelay, interruptibly, for the load
// balancer to observe the 503, and only then stops accepting.
func (s *server) stop(ctx context.Context) error {
	if err := s.serveErr.Load(); err != nil {
		return *err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.shutdownTimeout)
	defer cancel()

	if s.cfg.drainDelay > 0 {
		t := time.NewTimer(s.cfg.drainDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			// A second signal, or the force-exit deadline: skip the courtesy
			// wait and drain now.
		}
	}
	// Shutdown does NOT cancel in-flight request contexts: a handler that
	// ignores ctx blocks here until ShutdownTimeout expires.
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("warren/transport/http: draining: %w", err)
	}
	return nil
}

// Addr reports the address the server is listening on, or "" before start.
// It is how a test that bound port 0 finds the port.
func (s *server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// allowHeader renders the Allow header for a path: the registered verbs, plus
// the HEAD that net/http serves for free on every GET, sorted so the value is
// deterministic and golden-testable.
func allowHeader(verbs []string) string {
	set := slices.Clone(verbs)
	if slices.Contains(set, http.MethodGet) && !slices.Contains(set, http.MethodHead) {
		set = append(set, http.MethodHead)
	}
	slices.Sort(set)
	return strings.Join(slices.Compact(set), ", ")
}

// diagnostic carries a rendered multi-line block; the text is the contract,
// covered by golden files like every other Warren diagnostic.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

// muxSite strips ServeMux's "(registered at <file>:<line>)" clauses. They
// name THIS file — every pattern is registered from one loop in build — so
// they point a user at the framework's source instead of at their own
// Register method, which is the one place they can fix it.
var muxSite = regexp.MustCompile(` \(registered at [^)]*\)`)

func errPatternConflict(r any) error {
	return diagnostic(fmt.Sprintf(
		"✗ conflicting HTTP route patterns\n\n    %s\n\n"+
			"  net/http.ServeMux refuses two patterns that can match the same request,\n"+
			"  because which one wins would be arbitrary. Two routes with\n"+
			"  differently-named wildcards on the same path — \"/users/{id}\" and\n"+
			"  \"/users/{uid}\" — are the usual cause: pick one name.\n\n"+
			"  Both patterns come from a controller's Register method; grep for them.",
		muxSite.ReplaceAllString(fmt.Sprint(r), "")))
}

func errNotAHandler(rr transport.RawRoute) error {
	return diagnostic(fmt.Sprintf(
		"✗ raw route is not an http.Handler\n\n"+
			"    route %q, registered by %s,\n"+
			"    was given a value of type %T.\n\n"+
			"  transport.Raw hands its handler to whichever adapter serves the\n"+
			"  protocol, and this one is HTTP: the value must implement\n"+
			"  net/http.Handler. Wrap a function with http.HandlerFunc(fn).",
		rr.Pattern, rr.Name, rr.Handler))
}

// connErrorLog routes net/http's connection-level errors — TLS handshake
// failures, malformed requests — into warren/log rather than stderr, so one
// service has one log stream.
func connErrorLog() *stdlog.Logger {
	return stdlog.New(slogWriter{}, "", 0)
}

type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Warn(strings.TrimRight(string(p), "\n"), "source", "net/http")
	return len(p), nil
}
