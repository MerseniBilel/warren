// Package health holds the registry of checks a service can be asked about
// and renders the two probe verdicts. It serves nothing: the kernel has no
// knowledge that HTTP exists. warren/transport/http serves /healthz and
// /readyz from this registry, and warren/transport/grpc serves the gRPC
// health service from the same one, so the two can never disagree.
//
// The split that matters: liveness runs NO dependency checks and is up for
// the whole life of the process. Restarting a process does not fix a dead
// database, so a liveness probe wired to Postgres kills every replica when
// Postgres blips. Readiness is the probe with a job — the lifecycle gate,
// then every critical check.
package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Status is a probe verdict.
type Status string

const (
	// StatusUp means serving.
	StatusUp Status = "up"
	// StatusDown means not serving — for readiness, take me out of the load
	// balancer.
	StatusDown Status = "down"
)

// Check is one thing a service can be asked about — a database ping, a
// broker metadata fetch. Adapters implement it and register from their
// module's constructor.
type Check interface {
	Name() string
	Check(ctx context.Context) error
}

// NewCheck adapts a bare function to Check, so an adapter contributes a ping
// without declaring a struct.
func NewCheck(name string, fn func(context.Context) error) Check {
	return funcCheck{name: name, fn: fn}
}

type funcCheck struct {
	name string
	fn   func(context.Context) error
}

func (c funcCheck) Name() string                    { return c.name }
func (c funcCheck) Check(ctx context.Context) error { return c.fn(ctx) }

// Result is one check's outcome in a probe response.
type Result struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Error    string `json:"error,omitempty"`
	Critical bool   `json:"critical"`
}

// Report is a probe response — the body every transport renders. The kernel
// owns the shape so HTTP and gRPC never disagree about it.
type Report struct {
	Status Status   `json:"status"`
	Reason string   `json:"reason,omitempty"` // "starting" or "draining"
	Checks []Result `json:"checks,omitempty"`
}

// Registry is where adapters register checks and transports read verdicts.
// The bootstrapper provides one in the root container, as it does
// lifecycle.Lifecycle, so any constructor in any module can inject it.
type Registry interface {
	// Register adds a check. A name already registered is an error, not a
	// silent replacement.
	Register(c Check, opts ...RegisterOption) error

	// Live answers liveness. It runs no checks and is always up: the fact
	// that this code ran is the answer.
	Live(ctx context.Context) Report

	// Ready answers readiness: the lifecycle gate first, then every critical
	// check, run concurrently under their timeouts. With the gate closed no
	// check runs — the verdict is already decided.
	Ready(ctx context.Context) Report
}

// Option configures a Registry.
type Option func(*registry)

// DefaultTimeout bounds each check unless the check overrides it. The
// effective deadline is the shorter of this and the caller's context, so an
// adapter's own request timeout wins when it is tighter. Default 2s.
func DefaultTimeout(d time.Duration) Option {
	return func(r *registry) { r.timeout = d }
}

// RegisterOption configures one check.
type RegisterOption func(*registration)

// Timeout overrides DefaultTimeout for one check.
func Timeout(d time.Duration) RegisterOption {
	return func(r *registration) { r.timeout = d }
}

// Informational marks a check reported but not readiness-gating — a warm
// cache, an optional downstream. Checks are critical by default.
func Informational() RegisterOption {
	return func(r *registration) { r.critical = false }
}

// New returns a Registry whose readiness gate is ready — in a booted
// application, lifecycle.Lifecycle.Ready. This package never owns a gate of
// its own: two gates drift, and only the lifecycle observes the transitions
// at boot step 7 and shutdown step 9.
func New(ready func() bool, opts ...Option) Registry {
	r := &registry{ready: ready, timeout: 2 * time.Second}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

type registration struct {
	check    Check
	timeout  time.Duration
	critical bool
}

type registry struct {
	ready   func() bool
	timeout time.Duration

	mu       sync.Mutex
	checks   []*registration
	byName   map[string]bool
	wasReady bool
}

func (r *registry) Register(c Check, opts ...RegisterOption) error {
	if c == nil {
		return fmt.Errorf("health: Register(nil) — pass a Check, or health.NewCheck(name, fn)")
	}
	reg := &registration{check: c, timeout: r.timeout, critical: true}
	for _, opt := range opts {
		opt(reg)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = map[string]bool{}
	}
	if r.byName[c.Name()] {
		return errDuplicate(c.Name())
	}
	r.byName[c.Name()] = true
	r.checks = append(r.checks, reg)
	return nil
}

func (r *registry) Live(context.Context) Report {
	return Report{Status: StatusUp}
}

func (r *registry) Ready(ctx context.Context) Report {
	if !r.ready() {
		r.mu.Lock()
		reason := "starting"
		if r.wasReady {
			reason = "draining"
		}
		r.mu.Unlock()
		return Report{Status: StatusDown, Reason: reason}
	}

	r.mu.Lock()
	r.wasReady = true
	checks := append([]*registration(nil), r.checks...)
	r.mu.Unlock()

	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, reg := range checks {
		wg.Go(func() { results[i] = run(ctx, reg) })
	}
	wg.Wait()

	status := StatusUp
	for _, res := range results {
		if res.Status == StatusDown && res.Critical {
			status = StatusDown
			break
		}
	}
	return Report{Status: status, Checks: results}
}

// run executes one check under its timeout, in its own goroutine so a check
// that ignores its context cannot wedge the probe. A panic is reported, not
// fatal: a bug in one check must not take down the process answering the
// probe.
func run(ctx context.Context, reg *registration) Result {
	res := Result{Name: reg.check.Name(), Status: StatusUp, Critical: reg.critical}

	cctx := ctx
	if reg.timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, reg.timeout)
		defer cancel()
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("panic: %v", p)
			}
		}()
		done <- reg.check.Check(cctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			res.Status = StatusDown
			res.Error = err.Error()
			if cctx.Err() != nil && ctx.Err() == nil {
				res.Error = fmt.Sprintf("health: check %q exceeded its %v timeout", reg.check.Name(), reg.timeout)
			}
		}
	case <-cctx.Done():
		res.Status = StatusDown
		if ctx.Err() != nil {
			res.Error = fmt.Sprintf("health: check %q was cancelled: %v", reg.check.Name(), ctx.Err())
		} else {
			res.Error = fmt.Sprintf("health: check %q exceeded its %v timeout", reg.check.Name(), reg.timeout)
		}
	}
	return res
}

// diagnostic carries a rendered multi-line block; the text is the contract.
type diagnostic string

func (d diagnostic) Error() string { return string(d) }

func errDuplicate(name string) error {
	return diagnostic(fmt.Sprintf(
		"✗ duplicate health check\n\n    %q is already registered.\n\n"+
			"  Two dependencies of the same kind need distinct check names — name the\n"+
			"  check after the instance (\"postgres.billing\"), not the driver.", name))
}
