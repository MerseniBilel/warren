// Package servertest boots a Warren module graph behind a real HTTP server
// and returns its base URL, so a test can exercise a route the way a client
// will: over the wire, through decode, validation, parameter binding, guards,
// the status defaults and the error-code column.
//
// It exists because warrentest.Invoke calls a handler directly. That is the
// right tool for a use case's own logic, and it is the wrong tool for a
// controller: it crosses none of the transport, so a route that 400s on every
// request can ship with a passing test. This package is the other half.
//
// It is warrentest with a listener attached. Everything warrentest offers —
// Replace, WithMemoryBroker, WithMemoryPersistence, Resolve, AssertPublished —
// reaches through the App field.
//
// It is a SUBPACKAGE of transport/http rather than part of it, so that test
// helpers never compile into a production binary that imports the adapter.
package servertest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MerseniBilel/warren"
	warrentest "github.com/MerseniBilel/warren/testing"
	whttp "github.com/MerseniBilel/warren/transport/http"
)

// Server is a booted graph served over HTTP for the duration of a test.
type Server struct {
	// App is the harness underneath, for Resolve, Published, AssertPublished
	// and everything else warrentest offers.
	App *warrentest.App

	// URL is the base URL, with no trailing slash: "http://127.0.0.1:54321".
	URL string

	client *http.Client
}

// Option configures New.
type Option struct{ apply func(*config) }

type config struct {
	server []whttp.Option
	inner  []warrentest.Option
}

// Options passes options through to the HTTP server module — for a test that
// needs a real drain delay, a body limit, TLS, or StrictJSON.
func Options(opts ...whttp.Option) Option {
	return Option{apply: func(c *config) { c.server = append(c.server, opts...) }}
}

// With passes options through to warrentest.NewModuleTest, so Replace,
// WithMemoryBroker and WithMemoryPersistence all reach the graph.
func With(opts ...warrentest.Option) Option {
	return Option{apply: func(c *config) { c.inner = append(c.inner, opts...) }}
}

// New boots m behind an HTTP server on a port the OS chooses, and registers
// cleanup.
//
// THE DRAIN DELAY IS ZERO by default. whttp's own default is 5s, which is
// correct for production — a load balancer needs a full poll period to notice
// readiness closing — and wrong for every test, where it is five seconds of
// wall clock per booted server. A default that depends on which of the two
// you are cannot be chosen by whttp; it is chosen here, by the package that
// only ever runs in tests. A test that genuinely exercises the drain asks for
// it back:
//
//	servertest.New(t, m, servertest.Options(whttp.DrainDelay(2*time.Second)))
func New(t *testing.T, m warren.Module, opts ...Option) *Server {
	t.Helper()

	var cfg config
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("servertest: listen: %v", err)
	}

	// DrainDelay AND ShutdownTimeout. The drain is how long the stop hook
	// WAITS before closing the listener; the timeout bounds the whole stop.
	// Zeroing only the first leaves a test suite's teardown bounded by a
	// production-sized number, which is the same mistake one knob along —
	// nothing in a test should be able to cost seconds by default.
	//
	// An external reviewer measured a hard 5s teardown stall in a HAND-ROLLED
	// harness and attributed it to this timeout with two idle keep-alive
	// connections. I could NOT reproduce it through servertest — a barrier
	// forcing two genuinely concurrent connections tears down in
	// milliseconds with or without this line, because http.Server.Shutdown
	// closes idle connections immediately and waits only on active ones. So
	// this is a defensive bound, not a fix for a demonstrated bug, and it is
	// recorded that way rather than credited with more than it earned.
	//
	// Explicit Options(...) still win: options apply in order and these are
	// first.
	server := append([]whttp.Option{
		whttp.Listener(l),
		whttp.DrainDelay(0),
		whttp.ShutdownTimeout(200 * time.Millisecond),
	}, cfg.server...)
	inner := append([]warrentest.Option{warrentest.WithModules(whttp.Server(server...))}, cfg.inner...)

	s := &Server{
		App: warrentest.NewModuleTest(t, m, inner...),
		URL: "http://" + l.Addr().String(),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A test asserting a 303 must see the 303, not its target.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return s
}

// Client returns a client bound to this server. It follows no redirects.
func (s *Server) Client() *http.Client { return s.client }

// Request builds a request against this server, for the cases the five verbs
// do not cover: a custom header, a raw body, an odd method.
func (s *Server) Request(t testing.TB, method, path string, body io.Reader) *http.Request {
	t.Helper()
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequest(method, s.URL+path, body)
	if err != nil {
		t.Fatalf("servertest: building %s %s: %v", method, path, err)
	}
	return req
}

// Do sends req and reads the whole response. A transport error fails the test.
func (s *Server) Do(t testing.TB, req *http.Request) *Response {
	t.Helper()
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("servertest: %s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("servertest: reading %s %s: %v", req.Method, req.URL.Path, err)
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// send builds and sends in one step. A nil body sends none; any other value is
// marshalled as JSON.
func (s *Server) send(t testing.TB, method, path string, body any) *Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("servertest: marshalling the body for %s %s: %v", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req := s.Request(t, method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.Do(t, req)
}

// Get sends a GET.
func (s *Server) Get(t testing.TB, path string) *Response {
	t.Helper()
	return s.send(t, http.MethodGet, path, nil)
}

// Post sends a POST, marshalling body as JSON unless it is nil.
func (s *Server) Post(t testing.TB, path string, body any) *Response {
	t.Helper()
	return s.send(t, http.MethodPost, path, body)
}

// Put sends a PUT, marshalling body as JSON unless it is nil.
func (s *Server) Put(t testing.TB, path string, body any) *Response {
	t.Helper()
	return s.send(t, http.MethodPut, path, body)
}

// Patch sends a PATCH, marshalling body as JSON unless it is nil.
func (s *Server) Patch(t testing.TB, path string, body any) *Response {
	t.Helper()
	return s.send(t, http.MethodPatch, path, body)
}

// Delete sends a DELETE.
func (s *Server) Delete(t testing.TB, path string) *Response {
	t.Helper()
	return s.send(t, http.MethodDelete, path, nil)
}

// Response is one answer, already read.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// String renders the response for a failure message.
func (r *Response) String() string {
	return fmt.Sprintf("%d %s: %s", r.Status, http.StatusText(r.Status), r.Body)
}

// Decode unmarshals the body into v, failing the test with the body's TEXT
// when it does not fit — because "unexpected end of JSON input" over an
// unprinted 500 is the least useful failure a test can produce.
func (r *Response) Decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("servertest: decoding a %d response: %v\nbody was:\n%s", r.Status, err, r.Body)
	}
}

// errorBody is Warren's rendered error envelope.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Code returns the error code from a Warren error body, or "" if the body is
// not one.
//
// A test asserts on this rather than on the status because the CODE is the
// contract a client switches on and the status is the adapter's rendering of
// it. CONFLICT and CONTENTION share 409 deliberately; only the code tells
// them apart.
func (r *Response) Code(t testing.TB) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(r.Body, &b); err != nil {
		return ""
	}
	return b.Error.Code
}

// Message returns the message from a Warren error body, or "" if the body is
// not one.
func (r *Response) Message(t testing.TB) string {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(r.Body, &b); err != nil {
		return ""
	}
	return b.Error.Message
}
