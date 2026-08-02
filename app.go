package warren

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
)

// App is a bootstrapped application: the flattened module graph, its scoped
// containers, and its run loop.
type App struct {
	modules []Module
	subs    []Substitution

	mu       sync.Mutex
	booted   bool
	stopping bool
	lc       lifecycle.Lifecycle
	scopes   map[string]di.Container
}

// New builds an App from the given module declarations. It does no fallible
// work — it collects the inert values and allocates the lifecycle; the boot
// sequence runs in Run or Start.
func New(modules ...Module) *App {
	return &App{modules: modules, lc: lifecycle.New()}
}

// Substitute applies substitutions before boot: Substitute[T] replaces every
// provider of T, Bind[T] adds one in the root scope. It must be called
// before Start.
func (a *App) Substitute(subs ...Substitution) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.booted {
		return errors.New("warren: Substitute after Start — substitutions are boot-time")
	}
	a.subs = append(a.subs, subs...)
	return nil
}

// Run boots the application and blocks until SIGINT or SIGTERM, then runs
// the shutdown sequence and returns. It returns the boot error if boot
// fails, otherwise whatever Stop returns. A second signal during shutdown
// cancels the drain — the force-exit short-circuit.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := a.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	// Re-arm before releasing the first registration: a second signal in the
	// gap would otherwise get default disposition and kill the process before
	// the drain starts. Registered this way, it cancels the drain instead —
	// the force-exit short-circuit — and a third signal kills.
	stopCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	stop()
	return a.Stop(stopCtx)
}

// Start runs boot steps 0–7 — flatten, scope, copy exports, validate,
// instantiate, hook up, open readiness — and returns once the application is
// serving. It exists so tests can drive boot without signals. Failure at any
// step is a startup failure; nothing is left half-started.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	if a.booted {
		a.mu.Unlock()
		return errors.New("warren: Start called again — an App boots once; construct a new one")
	}
	a.booted = true
	a.mu.Unlock()

	// Step 1 — flatten the module graph. Cycles are unrepresentable (Module
	// is a value); duplicates and export mistakes are not.
	ordered, err := flatten(a.modules)
	if err != nil {
		return err
	}

	// Step 2 — one scope per module; copy in only what imports export.
	root := di.New()
	matched := map[reflect.Type]bool{}
	a.mu.Lock()
	lc := a.lc
	a.mu.Unlock()
	if err := root.Provide(func() lifecycle.Lifecycle { return lc }); err != nil {
		return err
	}
	// The readiness gate is the lifecycle's; health only reads it, so the
	// two can never drift. Adapters register their pings against this
	// registry from their own constructors.
	if err := root.Provide(func() health.Registry { return health.New(lc.Ready) }); err != nil {
		return err
	}

	scopes := map[string]di.Container{}
	a.mu.Lock()
	a.scopes = scopes
	a.mu.Unlock()
	for _, m := range ordered {
		scope := root.Scope(m.name)
		scopes[m.name] = scope

		provided := map[reflect.Type]bool{}
		for _, group := range [][]any{m.providers, m.controllers, m.consumers} {
			for _, ctor := range group {
				opts := []di.ProvideOption{di.DeclaredAt(splitSite(m.declared))}
				exported := false
				outs := outputsOf(ctor)
				if sub, ok := a.replacementFor(outs); ok {
					// A substituted provider is replaced wholesale: the
					// value is provided instead, and the real constructor
					// never runs (so its own dependencies need not resolve).
					ctor = constantProvider(sub.t, sub.value)
					opts = append(opts, di.Named(sub.name()))
					if sub.site != "" {
						opts = append(opts, di.DeclaredAt(splitSite(sub.site)))
					}
					outs = []reflect.Type{sub.t}
					matched[sub.t] = true
				}
				for _, out := range outs {
					provided[out] = true
					exported = exported || slices.Contains(m.exports, out)
				}
				if exported {
					// Marks the whole constructor; a multi-output provider
					// with one exported output over-marks the rest — a known
					// cosmetic limit of the candidate text, not of visibility.
					opts = append(opts, di.Exported())
				}
				if err := scope.Provide(ctor, opts...); err != nil {
					return err
				}
			}
		}
		for _, t := range m.exports {
			if !provided[t] {
				return errExportWithoutProvider(m.name, m.declared, t)
			}
		}
	}
	for _, m := range ordered {
		seenImports := map[*int]bool{}
		for _, imp := range m.imports {
			// The same module listed twice in Imports is one import, not an
			// ambiguity.
			if seenImports[imp.id] {
				continue
			}
			seenImports[imp.id] = true
			from := scopes[imp.name]
			to := scopes[m.name]
			for _, t := range imp.exports {
				name := fmt.Sprintf("%s (exported by module %q)", t, imp.name)
				if err := to.Provide(forwarder(t, from), di.DeclaredAt(splitSite(imp.declared)), di.Named(name)); err != nil {
					return err
				}
			}
		}
	}

	// Bind substitutions land in the root scope, where every module sees
	// them; a Substitute that matched no provider is a boot error, because a
	// fake that was silently ignored is worse than no fake.
	for _, sub := range a.subs {
		if sub.replace && !matched[sub.t] {
			return errUnmatchedSubstitution(sub.t)
		}
		if matched[sub.t] {
			// Already applied in place of the provider it replaced.
			continue
		}
		opts := []di.ProvideOption{di.Named(sub.name())}
		if sub.site != "" {
			opts = append(opts, di.DeclaredAt(splitSite(sub.site)))
		}
		if err := root.Provide(constantProvider(sub.t, sub.value), opts...); err != nil {
			return err
		}
	}

	// Step 3 — validate the whole graph before anything is instantiated.
	if err := root.Validate(); err != nil {
		return err
	}

	// Steps 4–6 — instantiate each module's entry points in dependency
	// order (singletons materialise topologically behind them), append its
	// hooks, then start everything.
	for _, m := range ordered {
		scope := scopes[m.name]
		for _, group := range [][]any{m.controllers, m.consumers} {
			for _, ctor := range group {
				for _, t := range outputsOf(ctor) {
					if err := resolveDynamic(scope, t); err != nil {
						return err
					}
				}
			}
		}
		for _, t := range m.eager {
			if err := resolveDynamic(scope, t); err != nil {
				return err
			}
		}
		for _, fn := range m.onStart {
			lc.Append(lifecycle.Hook{Name: m.name, OnStart: fn})
		}
		for _, fn := range m.onStop {
			lc.Append(lifecycle.Hook{Name: m.name, OnStop: fn})
		}
	}

	// Steps 6–7 — OnStart in registration order; readiness opens when every
	// hook has started. A Stop that arrived anywhere during boot wins:
	// readiness never opens and the boot reports itself abandoned.
	a.mu.Lock()
	stopping := a.stopping
	a.mu.Unlock()
	if stopping {
		return errBootAbandoned()
	}
	if err := lc.Start(ctx); err != nil {
		a.mu.Lock()
		stopping = a.stopping
		a.mu.Unlock()
		if stopping {
			return errBootAbandoned()
		}
		return err
	}
	return nil
}

func errBootAbandoned() error {
	return errors.New("warren: Stop arrived during boot — boot abandoned, readiness never opened")
}

// Invoke resolves fn's parameters from the named module's scope and calls
// fn — the seam tests and pre-transport mains reach the components the boot
// built, without constructing second instances. Module encapsulation holds:
// fn sees exactly what the module's own constructors see, own bindings and
// imported exports, nothing else. It is boot-time machinery (invariant 7 is
// about the request path); a transport adapter, once one exists, is the
// production caller of your handlers.
func (a *App) Invoke(module string, fn any) error {
	a.mu.Lock()
	booted := a.booted
	scope := a.scopes[module]
	known := make([]string, 0, len(a.scopes))
	for name := range a.scopes {
		known = append(known, name)
	}
	a.mu.Unlock()
	if !booted {
		return errors.New("warren: Invoke before Start — boot the app first")
	}
	if scope == nil {
		slices.Sort(known)
		return fmt.Errorf("warren: Invoke on unknown module %q — the graph has: %s", module, strings.Join(known, ", "))
	}
	return scope.Invoke(fn)
}

// Stop runs the shutdown sequence: readiness closes first, then hooks stop
// in reverse order, bounded by the force-exit deadline.
func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	a.stopping = true
	lc := a.lc
	a.mu.Unlock()
	return lc.Stop(ctx)
}

// name renders a substitution the way diagnostics should print it — never
// as reflect.makeFuncStub, which is what a synthetic provider is called
// unless it is given a name.
func (s Substitution) name() string {
	if s.replace {
		return fmt.Sprintf("warren.Substitute[%s]", s.t)
	}
	return fmt.Sprintf("warren.Bind[%s]", s.t)
}

// replacementFor reports the substitution matching any of these output
// types.
func (a *App) replacementFor(outs []reflect.Type) (Substitution, bool) {
	for _, sub := range a.subs {
		// Bind shadows an existing provider too — see its doc comment.
		if slices.Contains(outs, sub.t) {
			return sub, true
		}
	}
	return Substitution{}, false
}

// constantProvider builds a func() T returning v, so a substituted value
// enters the graph as an ordinary provider.
func constantProvider(t reflect.Type, v any) any {
	fnType := reflect.FuncOf(nil, []reflect.Type{t}, false)
	val := reflect.ValueOf(v)
	return reflect.MakeFunc(fnType, func([]reflect.Value) []reflect.Value {
		if !val.IsValid() {
			return []reflect.Value{reflect.Zero(t)}
		}
		return []reflect.Value{val}
	}).Interface()
}

func errUnmatchedSubstitution(t reflect.Type) error {
	return diagnostic(fmt.Sprintf(
		"✗ substitution matched no provider\n\n    Substitute[%s] was applied, but no module in the graph provides %s.\n\n"+
			"  Check the type — a substitution that silently does nothing is a fake\n"+
			"  you would trust and never get. Use Bind[%s] to add a binding the\n"+
			"  graph does not already have.", t, t, t))
}

// flatten walks the New list and every transitive import into one
// deduplicated, dependency-ordered list — imports before importers.
// Deduplication is by IDENTITY, not call site: copies of one NewModule value
// reached through several paths are one module; two NewModule calls — a
// factory called twice, two config.Module instantiations — are two modules,
// and sharing a name is then the duplicate-name boot error. Call sites do
// not identify modules once closures or type parameters are involved.
func flatten(modules []Module) ([]Module, error) {
	var ordered []Module
	seen := map[*int]bool{}
	sites := map[string]string{}

	var visit func(m Module) error
	visit = func(m Module) error {
		if m.id != nil && seen[m.id] {
			return nil
		}
		if site, ok := sites[m.name]; ok {
			return errDuplicateModule(m.name, site, m.declared)
		}
		if m.id != nil {
			seen[m.id] = true
		}
		sites[m.name] = m.declared
		for _, imp := range m.imports {
			if err := visit(imp); err != nil {
				return err
			}
		}
		ordered = append(ordered, m)
		return nil
	}
	for _, m := range modules {
		if err := visit(m); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// forwarder returns a constructor of dynamic type func() (T, error) that
// resolves T from the exporting module's scope — the copy-in mechanism of
// boot step 2. Reflection here is boot-time only (invariant 7 concerns the
// request path).
func forwarder(t reflect.Type, from di.Container) any {
	fnType := reflect.FuncOf(nil, []reflect.Type{t, errType}, false)
	return reflect.MakeFunc(fnType, func([]reflect.Value) []reflect.Value {
		out := reflect.Zero(t)
		captureType := reflect.FuncOf([]reflect.Type{t}, nil, false)
		capture := reflect.MakeFunc(captureType, func(args []reflect.Value) []reflect.Value {
			out = args[0]
			return nil
		})
		errVal := reflect.Zero(errType)
		if err := from.Invoke(capture.Interface()); err != nil {
			errVal = reflect.ValueOf(err)
		}
		return []reflect.Value{out, errVal}
	}).Interface()
}

// resolveDynamic materialises the singleton of type t from scope — how entry
// points (controllers, consumers) are instantiated at boot step 4 without
// constructing a second instance outside the container.
func resolveDynamic(scope di.Container, t reflect.Type) error {
	captureType := reflect.FuncOf([]reflect.Type{t}, nil, false)
	capture := reflect.MakeFunc(captureType, func([]reflect.Value) []reflect.Value { return nil })
	return scope.Invoke(capture.Interface())
}

// splitSite turns "module.go:14" back into DeclaredAt's (file, line) pair.
func splitSite(site string) (string, int) {
	i := strings.LastIndex(site, ":")
	if i < 0 {
		return site, 0
	}
	line, err := strconv.Atoi(site[i+1:])
	if err != nil {
		return site, 0
	}
	return site[:i], line
}
