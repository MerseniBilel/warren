package warren

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/MerseniBilel/warren/app"
	"github.com/MerseniBilel/warren/di"
	"github.com/MerseniBilel/warren/health"
	"github.com/MerseniBilel/warren/lifecycle"
	"github.com/MerseniBilel/warren/transport"
	"github.com/MerseniBilel/warren/validate"
)

// App is a bootstrapped application: the flattened module graph, its scoped
// containers, and its run loop.
type App struct {
	modules []Module
	subs    []Substitution

	mu        sync.Mutex
	booted    bool
	stopping  bool
	validator validate.Validator
	telemetry app.Telemetry
	lc        lifecycle.Lifecycle
	scopes    map[string]di.Container
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

// Validator sets the validator whose rules are compiled into every route
// closure at boot step 5. The default is validate.Required(). It must be
// called before Start.
//
// It is the reachable form of the fix transport's own diagnostic promises:
// "transport.WithValidator(validate.None())" names a Builder that, until this
// existed, only the bootstrapper held.
func (a *App) Validator(v validate.Validator) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.booted {
		return errors.New("warren: Validator after Start — the validator is compiled into routes at boot")
	}
	a.validator = v
	return nil
}

// Telemetry sets the instrumentation compiled into every route at boot: with
// one bound, boot step 5 wraps app.Traced and app.Metered around every
// handler once, and the request path decides nothing.
//
// It is the non-DI path — a test, or a main that constructs its own. A
// service that lists observability.Module needs none of it: the bootstrapper
// resolves an exported app.Telemetry from the graph and uses that. It must be
// called before Start.
func (a *App) Telemetry(t app.Telemetry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.booted {
		return errors.New("warren: Telemetry after Start — instrumentation is compiled into routes at boot")
	}
	a.telemetry = t
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
	// The route table is provided EMPTY here and filled at step 5. An adapter
	// injects it, so its constructor's input must resolve at step 3 — long
	// before any controller has registered. The pointer is the seam.
	table := &transport.Table{}
	if err := root.Provide(func() *transport.Table { return table }); err != nil {
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
				// Checked BEFORE anything reflects on it. A nil in a
				// Providers list used to reach sameFunc, which calls
				// reflect.Value.Pointer on a zero Value and panics with a
				// framework stack trace naming neither the module nor the
				// word "provider" — the one boot mistake that did not
				// produce a Warren diagnostic.
				if err := checkConstructor(ctor, m.name, m.declared); err != nil {
					return err
				}
				opts := []di.ProvideOption{di.DeclaredAt(splitSite(m.declared))}
				exported := false
				named := false
				outs := outputsOf(ctor)
				if sub, ok := a.replacementFor(outs); ok {
					// A substituted provider is replaced wholesale: the
					// value is provided instead, and the real constructor
					// never runs (so its own dependencies need not resolve).
					ctor = constantProvider(sub.t, sub.value)
					opts = append(opts, di.Named(sub.name()))
					named = true
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
				// Wrapped so a nil return fails the boot rather than the
				// first request that touches it. outputsOf ran above, on the
				// ORIGINAL, so substitution matching is unaffected.
				//
				// The wrapper is a reflect.MakeFunc value, whose runtime name
				// is the assembly stub reflect.makeFuncStub. Carry the
				// original constructor's name across, or every diagnostic
				// that names a provider names the stub instead (field-test
				// defect B3). A substitution has already set its own name.
				wrapped := nilChecked(ctor, m.name, m.optional)
				if !named && !sameFunc(wrapped, ctor) {
					opts = append(opts, di.Named(constructorName(ctor)))
				}
				if err := scope.Provide(wrapped, opts...); err != nil {
					return err
				}
			}
		}
		for _, t := range m.exports {
			if !provided[t] {
				return errExportWithoutProvider(m.name, m.declared, t)
			}
		}
		// A waiver for a type this module does not provide is dead — and a
		// dead waiver is worse than a dead export, because nothing downstream
		// ever fails to notice it: the nil check for that type is simply off
		// for ever, silently, which is the failure the check exists to catch.
		for _, t := range m.optional {
			if !provided[t] {
				return errOptionalWithoutProvider(m.name, m.declared, t)
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
				if err := to.Provide(forwarder(t, from),
					di.DeclaredAt(splitSite(imp.declared)),
					di.Named(name),
					di.ForwardedFrom(imp.name),
				); err != nil {
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

	// Step 3b — resolve the telemetry BEFORE anything else is instantiated.
	//
	// Two things depend on it happening here. Step 5 composes app.Traced and
	// app.Metered into every route closure, so the binding must exist by
	// then. And a telemetry provider registers its flush hook from its
	// constructor — resolving it first means that hook is appended FIRST, so
	// it unwinds LAST: after the servers stop, after the consumers drain,
	// after the pools close. The spans emitted while everything else shuts
	// down are the ones nobody can reproduce, and they are only captured if
	// the exporter is still alive to take them.
	//
	// It is a scan over module SCOPES, not the root: a module's providers are
	// private to it, so an exported app.Telemetry lives in the exporting
	// module's scope. Where the user listed that module makes no difference —
	// the ordering is a property of this phase, not of argument order, which
	// is exactly why it cannot be got wrong.
	a.mu.Lock()
	telemetry := a.telemetry
	a.mu.Unlock()
	if telemetry == nil {
		for _, m := range ordered {
			v, err := resolveDynamic(scopes[m.name], telemetryType)
			if err != nil {
				continue
			}
			if t, ok := v.(app.Telemetry); ok && t != nil {
				telemetry = t
				break
			}
		}
	}

	// Step 4 — instantiate each module's entry points in dependency order
	// (singletons materialise topologically behind them), KEEPING the values:
	// step 5 registers them, and a discarded controller can register nothing.
	type entryPoints struct {
		module      string
		controllers []transport.Controller
	}
	var built []entryPoints
	for _, m := range ordered {
		scope := scopes[m.name]
		found := entryPoints{module: m.name}
		for i, group := range [][]any{m.controllers, m.consumers} {
			option := "warren.Controllers"
			if i == 1 {
				option = "warren.Consumers"
			}
			for _, ctor := range group {
				registers := false
				for _, t := range outputsOf(ctor) {
					v, err := resolveDynamic(scope, t)
					if err != nil {
						return err
					}
					if c, ok := v.(transport.Controller); ok {
						found.controllers = append(found.controllers, c)
						registers = true
					}
				}
				if !registers {
					return errRegistersNothing(m, option, outputsOf(ctor))
				}
			}
		}
		built = append(built, found)

		// Declared hooks are appended HERE, in module order and after this
		// module's own entry points, so a consumer that appends its drain
		// from its constructor still precedes the platform module's — the
		// ordering §1.3 fixes, unchanged by the pass split.
		for _, fn := range m.onStart {
			lc.Append(lifecycle.Hook{Name: m.name, OnStart: fn})
		}
		for _, fn := range m.onStop {
			lc.Append(lifecycle.Hook{Name: m.name, OnStop: fn})
		}
	}

	// Step 5 — registration. Every controller in the graph builds its routes
	// into ONE builder, which is then frozen into the table provided at
	// step 2. Every registration problem is reported together.
	a.mu.Lock()
	validator := a.validator
	a.mu.Unlock()
	// App.Validator wins; failing that, a validate.Validator in the graph is
	// used — the same seam as telemetry, and for a sharper reason. A harness
	// that boots the app itself (warrentest.NewModuleTest) never calls
	// App.Validator, so without this a module whose requests carry tags only
	// warren/validate/playground can enforce serves in production and cannot
	// be booted in a test. The scan is over module scopes because a module's
	// providers are private to it, and root-scope bindings are visible from
	// every one of them.
	if validator == nil {
		for _, m := range ordered {
			v, err := resolveDynamic(scopes[m.name], validatorType)
			if err != nil {
				continue
			}
			if vv, ok := v.(validate.Validator); ok && vv != nil {
				validator = vv
				break
			}
		}
	}
	var bopts []transport.BuilderOption
	if validator != nil {
		bopts = append(bopts, transport.WithValidator(validator))
	}
	if telemetry != nil {
		bopts = append(bopts, transport.WithTelemetry(telemetry))
	}
	b := transport.NewBuilder(bopts...)
	for _, ep := range built {
		r := b.For(ep.module)
		for _, c := range ep.controllers {
			c.Register(r)
		}
	}
	if err := b.Fill(table); err != nil {
		return err
	}

	// Step 5b — eager singletons, LAST. A transport or broker adapter is an
	// eager singleton whose constructor injects the table, claims its
	// protocol, and appends its own lifecycle hook. Building them here means
	// two things: an adapter reads a COMPLETE table, and its hook lands after
	// every other module's — which is what makes §1.3's
	// "pool → repos → consumers → servers" true by construction rather than
	// by the order the user happened to list modules in New.
	for _, m := range ordered {
		scope := scopes[m.name]
		for _, t := range m.eager {
			v, err := resolveDynamic(scope, t)
			if err != nil {
				return err
			}
			// An Eager type that IS a controller was never registered — step
			// 5 only walks Controllers and Consumers. Listing a controller
			// under Providers boots clean and serves nothing, which is the
			// silent 404 this whole ordering exists to prevent.
			if c, ok := v.(transport.Controller); ok {
				return errControllerNotRegistered(m, t, c)
			}
		}
	}

	// A controller listed under Providers that nothing depends on can never
	// be constructed, so step 5 never registers it and every route it meant
	// to declare 404s. The Eager scan above catches the variant that IS
	// built; this catches the one a user actually mistypes, and it is a
	// static fact about the declarations, not about any value.
	for _, m := range ordered {
		if err := checkUnreachableControllers(m); err != nil {
			return err
		}
	}

	// A route nobody serves is a route that silently 404s in production, and
	// it is detectable here — so it fails here, not there.
	if err := table.Unserved(); err != nil {
		return err
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

// resolveDynamic materialises the singleton of type t from scope and returns
// it — how entry points (controllers, consumers) are instantiated at boot
// step 4 without constructing a second instance outside the container. The
// value is what step 5 calls Register on; discarding it is why step 5 did not
// exist.
func resolveDynamic(scope di.Container, t reflect.Type) (any, error) {
	var out any
	captureType := reflect.FuncOf([]reflect.Type{t}, nil, false)
	capture := reflect.MakeFunc(captureType, func(args []reflect.Value) []reflect.Value {
		if args[0].IsValid() && args[0].CanInterface() {
			out = args[0].Interface()
		}
		return nil
	})
	return out, scope.Invoke(capture.Interface())
}

// errRegistersNothing names the type, the option the user actually wrote, and
// the two ways out. A controller whose Register has the wrong signature is
// accepted by the compiler, registers nothing, and every route it meant to
// declare 404s in production — which is the exact failure boot step 3 exists
// to prevent, one step further along.
func errRegistersNothing(m Module, option string, outs []reflect.Type) error {
	what := "nothing"
	if len(outs) > 0 {
		names := make([]string, 0, len(outs))
		for _, t := range outs {
			names = append(names, t.String())
		}
		what = strings.Join(names, ", ")
	}
	return diagnostic(fmt.Sprintf(
		"✗ controller registers nothing\n\n"+
			"    module %q (%s)\n"+
			"      └─ %s lists a constructor returning %s,\n"+
			"           which has no Register(transport.Registrar) method.\n\n"+
			"  A controller whose Register has the wrong signature compiles, registers\n"+
			"  no routes, and 404s in production. Add:\n\n"+
			"      func (c *%s) Register(r transport.Registrar) {\n"+
			"          transport.Post(r, \"/things\", c.create)\n"+
			"      }\n\n"+
			"  Or, if it registers nothing and only needs building at boot, declare it\n"+
			"  with warren.Providers and warren.Eager[%s]() instead.",
		m.name, m.declared, option, what, shortTypeName(outs), what))
}

// errControllerNotRegistered catches the mirror image of errRegistersNothing:
// a type that DOES implement transport.Controller, declared with Providers
// and Eager instead of Controllers. Nothing calls its Register, so its routes
// do not exist, and with no routes at all Table.Unserved has nothing to
// report either — both safety nets miss it, and the service ships dead.
func errControllerNotRegistered(m Module, t reflect.Type, _ transport.Controller) error {
	return diagnostic(fmt.Sprintf(
		"✗ controller declared as a plain provider\n\n"+
			"    module %q (%s)\n"+
			"      └─ warren.Eager[%s]() builds a type that implements\n"+
			"           Register(transport.Registrar), but only warren.Controllers\n"+
			"           and warren.Consumers are registered at boot step 5.\n\n"+
			"  Its routes would never exist, and with no routes registered nothing\n"+
			"  else would notice. Move the constructor:\n\n"+
			"      warren.Controllers(%s)\n\n"+
			"  and drop the warren.Eager — Controllers instantiates it already.",
		m.name, m.declared, t, "New"+shortTypeName([]reflect.Type{t})))
}

// shortTypeName renders the first output's bare type name for the example
// receiver in the diagnostic above — "*user.Controller" reads badly inside a
// method declaration.
func shortTypeName(outs []reflect.Type) string {
	if len(outs) == 0 {
		return "Controller"
	}
	t := outs[0]
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() == "" {
		return "Controller"
	}
	return t.Name()
}

// checkConstructor refuses a value that is not a usable constructor, naming
// the module it came from — which is the part the reader needs and the part
// the reflect panic did not have.
func checkConstructor(ctor any, module, declared string) error {
	if ctor == nil {
		return diagnostic(fmt.Sprintf(
			"✗ nil constructor\n\n    module %q (%s) lists a nil in one of its constructor lists.\n\n"+
				"  warren.Providers, Controllers and Consumers take FUNCTIONS. A nil is\n"+
				"  usually a name that does not exist yet, or a trailing comma after a\n"+
				"  deleted line.", module, declared))
	}
	v := reflect.ValueOf(ctor)
	// A TYPED nil func — `var newStore func() *Store`, declared and never
	// assigned — has Kind() == Func and is not == nil as an `any`, so it
	// passes both of the other checks. It then reaches dig and panics inside
	// nilChecked's fn.Call with a raw reflect stack carrying dig frames,
	// which is invariant 2's "no dig error message reaches a user" as well as
	// the missing diagnostic. It is also the shape a real project produces,
	// where an untyped nil is mostly a typo.
	if v.Kind() == reflect.Func && v.IsNil() {
		return diagnostic(fmt.Sprintf(
			"✗ nil constructor\n\n    module %q (%s) lists a %T that is nil.\n\n"+
				"  The name exists but nothing was assigned to it — a package-level\n"+
				"  constructor variable an init never set, or a build tag that left it\n"+
				"  empty. Assign it, or remove it from the list.", module, declared, ctor))
	}
	if v.Kind() != reflect.Func {
		return diagnostic(fmt.Sprintf(
			"✗ not a constructor\n\n    module %q (%s) lists a %T where a constructor belongs.\n\n"+
				"  warren.Providers, Controllers and Consumers take a function whose\n"+
				"  returns are the types it provides, optionally with a trailing error.",
			module, declared, ctor))
	}
	return nil
}

// sameFunc reports whether two constructor values are the same function —
// how the bootstrapper asks whether nilChecked wrapped this one or handed it
// back untouched. Funcs are not comparable, so the code pointers are compared
// instead.
func sameFunc(a, b any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// constructorName renders a constructor the way diagnostics print it:
// "postgres.NewUserRepository". It is what a wrapped constructor is Named,
// because the wrapper's own runtime name is reflect.makeFuncStub.
func constructorName(ctor any) string {
	v := reflect.ValueOf(ctor)
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return "unknown function"
	}
	name := f.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
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

// controllerType is what a constructor's return value is measured against
// when deciding whether a plain provider is a controller in disguise.
var controllerType = reflect.TypeFor[transport.Controller]()

// checkUnreachableControllers refuses a controller declared under
// warren.Providers that nothing in the module can consume.
//
// The rule is deliberately narrow. Merely being a controller under Providers
// is not an error — a sub-controller its parent delegates to is constructed
// and registered THROUGH that parent, and refusing it would make the guard
// worse than the bug it prevents. What cannot be right is a controller
// nothing depends on: dig builds a provider only on demand, so it is never
// constructed, step 5 never registers it, and every route it meant to
// declare 404s while the app logs a clean boot.
//
// Reachability is read off the declarations — the parameter types of every
// constructor in the module, plus its eager list — so nothing is
// instantiated to find out.
func checkUnreachableControllers(m Module) error {
	if len(m.providers) == 0 {
		return nil
	}
	consumed := map[reflect.Type]bool{}
	for _, group := range [][]any{m.providers, m.controllers, m.consumers} {
		for _, ctor := range group {
			t := reflect.TypeOf(ctor)
			if t == nil || t.Kind() != reflect.Func {
				continue
			}
			for i := range t.NumIn() {
				consumed[t.In(i)] = true
			}
		}
	}
	for _, t := range m.eager {
		consumed[t] = true
	}

	for _, ctor := range m.providers {
		t := reflect.TypeOf(ctor)
		if t == nil || t.Kind() != reflect.Func || t.NumOut() == 0 {
			continue
		}
		out := t.Out(0)
		if !out.Implements(controllerType) || consumed[out] {
			continue
		}
		// An EXPORTED type is reachable from outside: an importing module's
		// constructor can consume it, and this module cannot see that. The
		// export list is the only way out of a module, so it is the complete
		// escape — and a type that carries a Register method for a consumer's
		// benefit is exactly the shape that would otherwise be refused here.
		if slices.Contains(m.exports, out) {
			continue
		}
		return errControllerIsAPlainProvider(m, out)
	}
	return nil
}

// errControllerIsAPlainProvider names the module, the type, and the one-word
// edit — because the symptom is a 404 with nothing in the logs, and the
// distance between `warren.Providers` and `warren.Controllers` is where the
// user's eye will not go on its own.
func errControllerIsAPlainProvider(m Module, t reflect.Type) error {
	return diagnostic(fmt.Sprintf(
		"✗ controller declared as a plain provider\n\n"+
			"    module %q (%s)\n"+
			"      └─ warren.Providers lists a constructor returning %s,\n"+
			"           which implements Register(transport.Registrar) — and nothing\n"+
			"           in the module depends on it.\n\n"+
			"  Only warren.Controllers is registered at boot step 5. Declared this way\n"+
			"  the constructor is never called, no route is registered, and every one\n"+
			"  of them 404s while the app logs a clean start.\n\n"+
			"      warren.Controllers(%s)\n\n"+
			"  If it really is a plain dependency, something has to depend on it.",
		m.name, m.declared, t, constructorHint(t)))
}

// constructorHint guesses the constructor's name from the type it returns —
// New<Type> is what every generator writes and what the manual documents.
func constructorHint(t reflect.Type) string {
	name := t.String()
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return "New" + strings.TrimPrefix(name, "*")
}
